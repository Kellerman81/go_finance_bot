// Package backtest replays historical candles through the same strategy, risk
// limits, protective exits and simulated broker used in live trading, then
// reports performance metrics. Bars are processed in index lockstep across
// symbols (assuming a shared timeframe), with fills at each bar's close.
package backtest

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// Backtester runs a strategy over historical data.
type Backtester struct {
	Strategy        strategy.Strategy
	Limits          config.Limits
	Exits           config.ExitsConfig
	Costs           config.CostsConfig
	Cash            float64
	OrderSize       float64
	AllowPyramiding bool

	// SizeByStrength scales each BUY's notional by the signal's conviction
	// (Signal.Strength) instead of always using the full OrderSize. A strength of
	// 1 buys the full OrderSize; a strength of 0 buys MinSizeFraction of it.
	SizeByStrength bool
	// MinSizeFraction is the floor fraction of OrderSize used at zero strength
	// when SizeByStrength is set. Defaults to 0.25 when <= 0.
	MinSizeFraction float64

	// ConfirmBuy/ConfirmSell mirror engine.confirm_buy/confirm_sell: a signal
	// must repeat on N consecutive bars before it is acted on (the live engine
	// counts evaluation ticks; here a bar is the analogue). Values below 1 act
	// immediately. KeepInterval mirrors engine.keep_interval: strategy sells are
	// blocked until the position has been held this long (protective exits
	// still fire). Modelling these keeps backtest/optimizer results honest
	// about the churn damping the live engine applies.
	ConfirmBuy   int
	ConfirmSell  int
	KeepInterval time.Duration
}

// btStreak tracks consecutive same-action bars per symbol (confirmation).
type btStreak struct {
	action strategy.Action
	count  int
}

// sizeForStrength returns the BUY notional for a signal of the given conviction,
// scaling linearly from MinSizeFraction·OrderSize (at strength 0) to OrderSize
// (at strength 1) when SizeByStrength is enabled; otherwise the full OrderSize.
func (bt *Backtester) sizeForStrength(strength float64) float64 {
	if !bt.SizeByStrength {
		return bt.OrderSize
	}

	frac := bt.MinSizeFraction
	if frac <= 0 {
		frac = 0.25
	}

	if frac > 1 {
		frac = 1
	}

	if strength < 0 {
		strength = 0
	} else if strength > 1 {
		strength = 1
	}

	return bt.OrderSize * (frac + (1-frac)*strength)
}

// EquityPoint is one sample of the equity curve.
type EquityPoint struct {
	Time   time.Time `json:"time"`
	Equity float64   `json:"equity"`
}

// Result holds backtest outcomes and metrics.
type Result struct {
	StartingCash     float64              `json:"starting_cash"`
	FinalEquity      float64              `json:"final_equity"`
	TotalReturnPct   float64              `json:"total_return_pct"`
	BuyHoldReturnPct float64              `json:"buy_hold_return_pct"`
	MaxDrawdownPct   float64              `json:"max_drawdown_pct"`
	NumTrades        int                  `json:"num_trades"`
	Wins             int                  `json:"wins"`
	Losses           int                  `json:"losses"`
	WinRatePct       float64              `json:"win_rate_pct"`
	Sharpe           float64              `json:"sharpe"`
	Bars             int                  `json:"bars"`
	Trades           []engine.TradeRecord `json:"trades"`
	Equity           []EquityPoint        `json:"equity"`
}

// Run executes the backtest over per-symbol candle series.
//
//nolint:cyclop,funlen // the bar-by-bar simulation loop mirrors the live engine's flow
func (bt *Backtester) Run(data map[string][]market.Candle) Result {
	ctx := context.Background()
	symbols := make([]string, 0, len(data))
	maxLen := 0

	for s, c := range data {
		symbols = append(symbols, s)

		if len(c) > maxLen {
			maxLen = len(c)
		}
	}

	slices.Sort(symbols)

	// Bound the per-bar lookback so each Evaluate is O(window) rather than
	// O(i): without this the backtest is O(n^2) and large datasets hang. The
	// window must cover the most history any detector can use, so long-lookback
	// detectors (e.g. trend, which reads up to a week) see the same data they
	// would live; otherwise changing their period would not affect results.
	lookback := bt.Strategy.WarmupBars() + 50
	if lb, ok := bt.Strategy.(interface{ MaxLookback() int }); ok {
		if m := lb.MaxLookback(); m > lookback {
			lookback = m
		}
	}

	if lookback < 100 {
		lookback = 100
	}

	prices := make(map[string]float64)
	priceOf := func(s string) (float64, bool) { p, ok := prices[s]; return p, ok && p > 0 }
	brk := broker.NewSim(bt.Cash, priceOf, bt.Costs.Commission)
	pf := portfolio.New()
	rm := risk.New(bt.Limits, bt.Costs)
	peaks := make(map[string]float64)
	streaks := make(map[string]btStreak)
	entryTime := make(map[string]time.Time) // opening-buy bar time per held symbol

	var (
		trades       []engine.TradeRecord
		wins, losses int
		ts           time.Time
	)

	equity := make([]EquityPoint, 0, maxLen)

	syncPF := func() {
		acct, _ := brk.GetAccount(ctx)
		pf.SetCash(acct.Cash)

		ps, _ := brk.GetPositions(ctx)
		hs := make([]portfolio.Holding, len(ps))

		for i, p := range ps {
			px := p.Current
			if v, ok := prices[p.Symbol]; ok {
				px = v
			}

			hs[i] = portfolio.Holding{
				Symbol: p.Symbol, Qty: p.Qty, AvgPrice: p.AvgPrice,
				LastPrice: px, MarketValue: px * p.Qty, UnrealizedPL: (px - p.AvgPrice) * p.Qty,
			}
		}

		pf.SyncPositions(hs)
	}

	exec := func(order broker.Order, reason string) {
		price := prices[order.Symbol]
		snap := pf.Snapshot()
		authOrder, dec := rm.Authorize(order, price, snap)

		if !dec.Approved {
			return
		}

		avgCost := snap.Holdings[order.Symbol].AvgPrice

		res, err := brk.SubmitOrder(ctx, authOrder)
		if err != nil {
			return
		}

		fillPx := res.FilledPx
		if fillPx <= 0 {
			fillPx = price
		}

		val := fillPx * res.FilledQty
		if order.Side == broker.Buy {
			rm.RecordBuy(val)

			if _, held := entryTime[order.Symbol]; !held {
				entryTime[order.Symbol] = ts
			}
		} else {
			// Win/loss net of round-trip commission, matching the equity curve
			// (which the sim broker debits fees from): a gross gain smaller than
			// its costs is a loss.
			cost := bt.Costs.Commission(val) + bt.Costs.Commission(avgCost*res.FilledQty)
			if (fillPx-avgCost)*res.FilledQty-cost >= 0 {
				wins++
			} else {
				losses++
			}

			delete(peaks, order.Symbol)
		}

		trades = append(trades, engine.TradeRecord{
			Time: ts, Symbol: order.Symbol, Side: order.Side, Qty: res.FilledQty,
			Price: fillPx, Value: val, Status: "filled", Reason: reason, OrderID: res.ID,
		})

		syncPF()

		if order.Side == broker.Sell && pf.PositionQty(order.Symbol) <= 0 {
			delete(entryTime, order.Symbol)
		}
	}

	syncPF() // initialise cash/positions from the broker before the run

	for i := range maxLen {
		for _, sym := range symbols {
			c := data[sym]
			if i < len(c) {
				prices[sym] = c[i].Close
				ts = c[i].Time
			}

			pf.MarkPrice(sym, prices[sym]) // cheap mark-to-market; cash unchanged between fills
		}

		// Protective exits first.
		if bt.Exits.Enabled {
			for sym, h := range pf.Snapshot().Holdings {
				if h.Qty <= 0 {
					continue
				}

				price := prices[sym]
				entry := h.AvgPrice
				peak := math.Max(math.Max(peaks[sym], price), entry)

				peaks[sym] = peak

				if r := bt.exitReason(price, entry, peak); r != "" {
					exec(
						broker.Order{
							Symbol: sym,
							Side:   broker.Sell,
							Qty:    h.Qty,
							Type:   broker.Market,
						},
						"EXIT "+r,
					)
				}
			}
		}

		// Strategy signals.
		for _, sym := range symbols {
			c := data[sym]
			if i >= len(c) {
				continue
			}

			start := i + 1 - lookback
			if start < 0 {
				start = 0
			}

			sig := bt.Strategy.Evaluate(sym, c[start:i+1])
			// Confirmation: the action must repeat on N consecutive bars before
			// acting (a HOLD or flip resets the run), like the live engine.
			st := streaks[sym]
			if st.action == sig.Action {
				st.count++
			} else {
				st = btStreak{action: sig.Action, count: 1}
			}

			streaks[sym] = st

			confirm := bt.ConfirmBuy

			if sig.Action == strategy.Sell {
				confirm = bt.ConfirmSell
			}

			if sig.Action != strategy.Hold && st.count < confirm {
				continue
			}

			price := prices[sym]
			switch sig.Action {
			case strategy.Hold:
				// nothing to do

			case strategy.Buy:
				held := pf.PositionQty(sym)
				if price > 0 && (bt.AllowPyramiding || held <= 0) {
					size := bt.sizeForStrength(sig.Strength)
					exec(
						broker.Order{
							Symbol: sym,
							Side:   broker.Buy,
							Qty:    size / price,
							Type:   broker.Market,
						},
						sig.Reason,
					)
				}

			case strategy.Sell:
				if held := pf.PositionQty(sym); held > 0 {
					// Minimum hold: strategy sells wait out keep_interval
					// (protective exits above are not gated by it).
					if bt.KeepInterval > 0 {
						if et, ok := entryTime[sym]; ok && c[i].Time.Sub(et) < bt.KeepInterval {
							continue
						}
					}

					exec(
						broker.Order{
							Symbol: sym,
							Side:   broker.Sell,
							Qty:    held,
							Type:   broker.Market,
						},
						sig.Reason,
					)
				}
			}
		}

		acct, _ := brk.GetAccount(ctx)

		equity = append(equity, EquityPoint{Time: ts, Equity: acct.Equity})
	}

	return bt.metrics(data, symbols, equity, trades, wins, losses, maxLen)
}

// exitReason reports which protective exit (if any) fires at price for a
// position with the given entry and peak, as a human-readable tag.
func (bt *Backtester) exitReason(price, entry, peak float64) string {
	switch {
	case bt.Exits.StopLossPct > 0 && price <= entry*(1-bt.Exits.StopLossPct):
		return fmt.Sprintf("stop-loss %.2f<=%.2f", price, entry)
	case bt.Exits.TakeProfitPct > 0 && price >= entry*(1+bt.Exits.TakeProfitPct):
		return fmt.Sprintf("take-profit %.2f>=%.2f", price, entry)
	case bt.Exits.TrailingStopPct > 0 && peak > entry && price <= peak*(1-bt.Exits.TrailingStopPct):
		return fmt.Sprintf("trailing-stop %.2f<=peak %.2f", price, peak)
	}

	return ""
}

// metrics assembles the final Result: returns, win rate, max drawdown, the
// buy & hold benchmark and Sharpe.
//
//nolint:cyclop // independent metric computations over the same inputs
func (bt *Backtester) metrics(data map[string][]market.Candle, symbols []string,
	equity []EquityPoint, trades []engine.TradeRecord, wins, losses, bars int,
) Result {
	r := Result{
		StartingCash: bt.Cash, Bars: bars, Trades: trades, Equity: equity,
		NumTrades: len(trades), Wins: wins, Losses: losses,
	}
	if len(equity) > 0 {
		r.FinalEquity = equity[len(equity)-1].Equity
	} else {
		r.FinalEquity = bt.Cash
	}

	if bt.Cash > 0 {
		r.TotalReturnPct = (r.FinalEquity/bt.Cash - 1) * 100
	}

	if wins+losses > 0 {
		r.WinRatePct = float64(wins) / float64(wins+losses) * 100
	}

	// Max drawdown from the equity curve.
	peak := math.Inf(-1)
	maxDD := 0.0

	for _, p := range equity {
		if p.Equity > peak {
			peak = p.Equity
		}

		if peak <= 0 {
			continue
		}

		dd := (peak - p.Equity) / peak
		if dd > maxDD {
			maxDD = dd
		}
	}

	r.MaxDrawdownPct = maxDD * 100

	// Equal-weighted buy & hold benchmark.
	var (
		sum float64
		n   int
	)

	for _, s := range symbols {
		c := data[s]
		if len(c) > 1 && c[0].Close > 0 {
			sum += (c[len(c)-1].Close/c[0].Close - 1)
			n++
		}
	}

	if n > 0 {
		r.BuyHoldReturnPct = sum / float64(n) * 100
	}

	// Sharpe over per-bar equity returns (not annualised).
	if len(equity) > 2 {
		rets := make([]float64, 0, len(equity)-1)
		for i := 1; i < len(equity); i++ {
			if equity[i-1].Equity > 0 {
				rets = append(rets, equity[i].Equity/equity[i-1].Equity-1)
			}
		}

		r.Sharpe = sharpe(rets)
	}

	return r
}

// sharpe computes a (non-annualised) Sharpe ratio over per-bar returns.
func sharpe(rets []float64) float64 {
	if len(rets) < 2 {
		return 0
	}

	var mean float64

	for _, x := range rets {
		mean += x
	}

	mean /= float64(len(rets))

	var variance float64

	for _, x := range rets {
		variance += (x - mean) * (x - mean)
	}

	variance /= float64(len(rets) - 1)

	sd := math.Sqrt(variance)

	if sd == 0 {
		return 0
	}

	return mean / sd * math.Sqrt(float64(len(rets))) // scaled by sqrt(N)
}
