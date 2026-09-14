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
	"github.com/Kellerman81/go_finance_bot/internal/exits"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/protect"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
	"github.com/Kellerman81/go_finance_bot/internal/strategy/indicators"
)

// Backtester runs a strategy over historical data.
type Backtester struct {
	// Strategy is the shared strategy. SymbolStrategies overrides it per symbol;
	// use strategyFor rather than reading Strategy directly, so a backtest of a
	// config with per-symbol parameters measures the bot that will actually run.
	Strategy         strategy.Strategy
	SymbolStrategies map[string]strategy.Strategy
	Limits           config.Limits
	Exits            config.ExitsConfig
	Costs            config.CostsConfig
	Cash             float64
	OrderSize        float64
	AllowPyramiding  bool

	// ScaleIn mirrors engine.scale_in: averaging into a losing position on
	// further drawdown, up to a capped number of adds. Modelled the same way
	// Protections is — not a search parameter, but its absence used to mean a
	// config with scale-in enabled traded identically with it on or off here,
	// while live actually adds to (and re-bases) the position. See
	// CODE_REVIEW_FINDINGS.md's backtest/live divergence cluster,
	// "engine.scale_in (averaging down) is entirely unmodeled in backtest".
	ScaleIn config.ScaleInConfig

	// SizeByStrength scales each BUY's notional by the signal's conviction
	// (Signal.Strength) instead of always using the full OrderSize. A strength of
	// 1 buys the full OrderSize; a strength of 0 buys MinSizeFraction of it.
	SizeByStrength bool
	// MinSizeFraction is the floor fraction of OrderSize used at zero strength
	// when SizeByStrength is set. Defaults to 0.25 when <= 0.
	MinSizeFraction float64

	MaxRiskSizeUSD  float64
	MinOrderSizeUSD float64

	// Portfolio, Sectors and AssetClasses configure the same concentration
	// caps the live engine applies (asset-class allocation, sector exposure,
	// correlated positions). Modelling them here matters: a backtest that
	// freely holds twenty correlated names, or puts the whole book into ETFs,
	// while the live bot refuses them is measuring a strategy nobody will run.
	//
	// AssetClasses is the live engine's merged map (provider/broker
	// classifications under config.yaml's asset_classes: overrides). Passing
	// it in rather than re-classifying here keeps the backtest from needing a
	// network lookup, and keeps both sides capping the same buckets.
	Portfolio    config.PortfolioConfig
	Sectors      map[string]string
	AssetClasses map[string]string

	// Protections applies the same outcome-driven circuit breakers the live bot
	// runs under: cooldown after a loss, the stop guard, and the drawdown and
	// daily-loss halts.
	//
	// These are NOT search parameters — a return-maximising optimiser would
	// always switch them off, since they can only ever cost return in-sample.
	// They are modelled so that a configuration which the live bot would halt
	// does not appear tradeable here.
	Protections config.ProtectionsConfig

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

	// spreads holds each symbol's measured bid-ask spread for the current run.
	// Populated by Run from the data it was given; not a configuration field.
	spreads map[string]float64
	// atrBuf is scratch space for the per-bar ATR window. Run is not re-entrant
	// and the optimizer builds a fresh Backtester per trial, so this is never
	// shared across goroutines.
	atrBuf columns
}

// btStreak tracks consecutive same-action bars per symbol (confirmation).
type btStreak struct {
	action strategy.Action
	count  int
}

// pendingOrder is a strategy decision made on one bar and filled at the next
// bar's open. Nothing can be decided and executed on the same bar: the decision
// uses that bar's close, which is not knowable until the bar is over.
type pendingOrder struct {
	side     broker.Side
	notional float64 // BUY: cash to deploy (qty is resolved at the fill price)
	reason   string
}

// sizingConfig is what risk.Manager.SizeBuy needs from this backtester.
//
// OrderSize folds into MaxRiskSizeUSD rather than acting as a target. Sizing
// starts from the money-limit headroom now, exactly as it does live, so a
// per-trade deployment figure can only be a CEILING under that — which is what
// the optimiser's capital-fraction axis has always effectively searched (it
// also sets max_order_value/max_per_position through relaxedLimits). The
// tighter of the two wins, so setting neither, either or both all behave
// sensibly.
func (bt *Backtester) sizingConfig() risk.SizingConfig {
	capUSD := bt.MaxRiskSizeUSD
	if bt.OrderSize > 0 && (capUSD <= 0 || bt.OrderSize < capUSD) {
		capUSD = bt.OrderSize
	}

	return risk.SizingConfig{
		Exits:           bt.Exits,
		MaxRiskSizeUSD:  capUSD,
		MinOrderSizeUSD: bt.MinOrderSizeUSD,
	}
}

// strengthFraction returns the share of the sized order a signal of the given
// conviction should actually take: linear from MinSizeFraction (at strength 0)
// to 1 (at strength 1) when SizeByStrength is enabled, otherwise 1.
//
// It is a MULTIPLIER now rather than a notional. Sizing itself comes from
// risk.Manager.SizeBuy, the same call the live engine makes, so the two cannot
// drift; conviction scaling is layered on top as the backtest-only knob it has
// always been (the live engine has no equivalent, so it runs at 1).
func (bt *Backtester) strengthFraction(strength float64) float64 {
	if !bt.SizeByStrength {
		return 1
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

	return frac + (1-frac)*strength
}

// strategyFor returns the strategy for symbol: its override if it has one,
// otherwise the shared one.
func (bt *Backtester) strategyFor(symbol string) strategy.Strategy {
	if s, ok := bt.SymbolStrategies[symbol]; ok {
		return s
	}

	return bt.Strategy
}

// maxLookbackOf returns the largest per-bar window any of the strategies needs,
// so a symbol with a longer-lookback override is not silently evaluated on a
// window sized for the shared config.
func (bt *Backtester) maxLookbackOf() int {
	most := lookbackOf(bt.Strategy)
	for _, s := range bt.SymbolStrategies {
		if n := lookbackOf(s); n > most {
			most = n
		}
	}

	return most
}

// lookbackOf reports how much history one strategy can make use of per bar.
func lookbackOf(s strategy.Strategy) int {
	if s == nil {
		return 0
	}

	n := s.WarmupBars() + 50
	if lb, ok := s.(interface{ MaxLookback() int }); ok {
		if m := lb.MaxLookback(); m > n {
			n = m
		}
	}

	return n
}

// measureSpreads estimates each symbol's bid-ask spread from its own bars, so a
// run charges the friction each instrument really costs instead of one global
// guess. It is a no-op unless costs.measure_spread is set.
//
// The estimate is taken once over the whole series rather than rolled forward.
// It is a property of the instrument's liquidity, not a signal, and nothing in
// the fill logic may read a bar that has not happened yet — but a *cost* fitted
// to the same history everywhere is not look-ahead in the way a price is: it
// cannot tell the strategy when to trade, only how much every trade costs.
func (bt *Backtester) measureSpreads(data map[string][]market.Candle) map[string]float64 {
	if !bt.Costs.MeasureSpread {
		return nil
	}

	out := make(map[string]float64, len(data))

	for sym, candles := range data {
		if len(candles) < 2 {
			continue
		}

		high := make([]float64, len(candles))
		low := make([]float64, len(candles))

		for i, c := range candles {
			high[i], low[i] = c.High, c.Low
		}

		if s, ok := indicators.HighLowSpread(high, low); ok {
			out[sym] = s
		}
	}

	return out
}

// costsFor returns the cost model charged to one symbol: the shared commission
// and slippage, with the spread replaced by that symbol's measured value when
// one is available. The configured spread_pct remains a floor.
func (bt *Backtester) costsFor(symbol string) config.CostsConfig {
	c := bt.Costs
	if !c.MeasureSpread {
		return c
	}

	c.SpreadPct = c.SpreadFor(bt.spreads[symbol])

	return c
}

// EquityPoint is one sample of the equity curve.
type EquityPoint struct {
	Time   time.Time `json:"time"`
	Equity float64   `json:"equity"`
}

// Result holds backtest outcomes and metrics.
//
// The risk-adjusted ratios are annualised from the bar interval inferred from
// the equity curve's timestamps, so they are comparable with the numbers other
// tools publish. A total return means little without them: two strategies with
// the same return are not equivalent if one earned it through a 40% drawdown.
type Result struct {
	StartingCash     float64 `json:"starting_cash"`
	FinalEquity      float64 `json:"final_equity"`
	TotalReturnPct   float64 `json:"total_return_pct"`
	CAGRPct          float64 `json:"cagr_pct"` // annualised return
	BuyHoldReturnPct float64 `json:"buy_hold_return_pct"`
	MaxDrawdownPct   float64 `json:"max_drawdown_pct"`
	NumTrades        int     `json:"num_trades"`
	Wins             int     `json:"wins"`
	Losses           int     `json:"losses"`
	WinRatePct       float64 `json:"win_rate_pct"`
	Sharpe           float64 `json:"sharpe"`  // annualised, excess over zero
	Sortino          float64 `json:"sortino"` // like Sharpe but only downside deviation
	Calmar           float64 `json:"calmar"`  // CAGR / max drawdown
	ProfitFactor     float64 `json:"profit_factor"`
	ExpectancyPct    float64 `json:"expectancy_pct"` // mean P&L per trade, % of entry
	AvgWinPct        float64 `json:"avg_win_pct"`
	AvgLossPct       float64 `json:"avg_loss_pct"`
	ExposurePct      float64 `json:"exposure_pct"` // share of bar-slots holding something
	// SpanDays is the wall-clock length of the run. The annualised figures above
	// are only meaningful over a decent span: scaling a six-hour result up to a
	// year produces impressive-looking nonsense.
	SpanDays float64              `json:"span_days"`
	Bars     int                  `json:"bars"`
	Trades   []engine.TradeRecord `json:"trades"`
	Equity   []EquityPoint        `json:"equity"`

	// The breakdowns. The aggregates above say whether the strategy made money;
	// these say which parts of it did, which is what actually changes a config.
	RoundTrips []RoundTrip    `json:"round_trips"`
	BySymbol   []SymbolResult `json:"by_symbol"`
	ByExit     []ExitResult   `json:"by_exit"`
	ByMonth    []PeriodReturn `json:"by_month"`
	Hold       HoldStats      `json:"hold"`
	// MonteCarlo is populated only when a bootstrap was requested.
	MonteCarlo *MonteCarlo `json:"monte_carlo,omitempty"`
	// HaltedAt records the first time a protection would have stopped the live
	// bot, if any. A curve that continues past this point is fiction.
	HaltedAt string `json:"halted_at,omitempty"`
}

// Run executes the backtest over per-symbol candle series.
//
// Each bar is simulated in the order a real session experiences it:
//
//  1. at the open, orders decided on the previous bar are filled;
//  2. during the bar, protective exits are tested against the high and low —
//     a stop that was pierced intrabar fires, exactly as the live tick-driven
//     engine would fire it, instead of being missed because the bar recovered
//     by its close;
//  3. at the close, the strategy is evaluated and its decision is queued for the
//     next bar's open.
//
// Nothing is ever decided and filled on the same bar. Deciding on a bar's close
// and filling at that same close is a look-ahead: at the moment the close is
// known, it is no longer tradeable.
//
//nolint:cyclop,funlen // the bar-by-bar simulation loop mirrors the live engine's flow
func (bt *Backtester) Run(data map[string][]market.Candle) Result {
	// The process-wide ML predictor (see internal/ml.Default, installed by
	// ConfigureAI once before an optimizer's whole trial loop) is otherwise
	// shared across every trial: without this, only the first trial ever
	// actually fits a model, and every later trial — regardless of what
	// strategy/indicator parameters it's testing — silently reuses it. See
	// CODE_REVIEW_FINDINGS.md CRIT-7. A nil predictor (ML not configured) is a
	// no-op.
	ml.Default().Reset()

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
	lookback := max(bt.maxLookbackOf(), 100)

	bt.spreads = bt.measureSpreads(data)

	prices := make(map[string]float64)
	priceOf := func(s string) (float64, bool) { p, ok := prices[s]; return p, ok && p > 0 }
	brk := broker.NewSim(bt.Cash, priceOf, bt.Costs.Commission)
	// Market fills cross the spread and slip; without this the backtest buys and
	// sells at the same price, which no venue offers. With costs.measure_spread
	// on, each symbol is charged the spread estimated from its OWN bars.
	brk.SetSlippage(func(symbol string, buy bool, price float64) float64 {
		return bt.costsFor(symbol).FillPrice(buy, price)
	})

	pf := portfolio.New()
	rm := risk.New(bt.Limits, bt.Costs)
	// Concentration caps, measured only on history that existed at each bar.
	retsAt := &barReturnsAt{data: data}
	guard := risk.NewPortfolioGuard(bt.Portfolio, bt.Sectors, bt.AssetClasses, retsAt)
	// The manager reads the per-class position slot off the guard when it
	// sizes a buy (min_positions_per_asset_class), exactly as the live engine
	// wires it in Engine.SetPortfolioGuard. Without this line the backtest
	// would size buys the live bot would clamp.
	rm.SetPortfolioGuard(guard)

	peaks := make(map[string]float64)
	streaks := make(map[string]btStreak)
	entryTime := make(map[string]time.Time) // opening-buy bar time per held symbol
	origQty := make(map[string]float64)     // largest size reached, for the scale-out ladder
	partials := make(map[string]int)        // scale-out rungs already taken, per symbol
	// entryCommission is the real commission actually debited by the sim
	// broker for the buy fill(s) that built the currently-open position,
	// summed as fills happen and drawn down proportionally as each
	// scale-out rung's RoundTrip attributes its fair share. This is what
	// stands in for a synthetic per-rung Commission(basis) recomputation,
	// which double-charges any flat/minimum commission component once per
	// rung instead of once for the whole position — see
	// CODE_REVIEW_FINDINGS.md's "Round-trip P&L re-charges the entry
	// commission on every scale-out rung".
	entryCommission := make(map[string]float64)
	adds := make(map[string]int)                   // scale-in adds already taken, per symbol
	lastSignal := make(map[string]strategy.Action) // most recently evaluated action, per symbol
	pending := make(map[string]pendingOrder)       // decided on this bar, filled at the next open
	exposed := 0                                   // bar-symbol slots spent holding something
	slots := 0                                     // bar-symbol slots in total (for exposure %)

	var (
		trades       []engine.TradeRecord
		wins, losses int
		roundTrips   []RoundTrip // one record per closed entry-to-exit cycle
		ts           time.Time
	)

	// Protections, driven by the simulated clock rather than the wall clock —
	// otherwise a cooldown measured in real seconds expires instantly while the
	// replay races through years of history, and the protections silently do
	// nothing.
	prot := protect.New(bt.Protections)
	prot.SetClock(func() time.Time { return ts })

	// max_daily_spend is a rolling per-calendar-day budget; driven from the
	// same simulated clock so it actually rolls over across a multi-day (or
	// multi-year) replay instead of measuring the whole backtest against the
	// first bar's calendar day.
	rm.SetClock(func() time.Time { return ts })

	// halted records that a drawdown or daily-loss limit tripped, so the run
	// reports it rather than quietly continuing to trade past the point the live
	// bot would have stopped.
	haltedAt := ""

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

	// exec submits an order at the price currently marked for its symbol, which
	// the caller sets to the bar price the fill really happens at (the open for a
	// queued decision, the trigger level for a protective exit).
	//
	// Reports whether the order actually filled: existing callers that don't
	// key off it (the fire-and-forget entries/exits) are unaffected, but the
	// scale-in ladder needs it — risk.Manager fails closed on a BUY it can't
	// authorize (unlike a sell, which "always goes through by design"), and
	// without checking this, a refused add would still consume one of the
	// ladder's capped MaxAdds for nothing.
	exec := func(order broker.Order, reason string) bool {
		price := prices[order.Symbol]
		snap := pf.Snapshot()
		authOrder, dec := rm.Authorize(order, price, snap)

		if !dec.Approved {
			return false
		}

		avgCost := snap.Holdings[order.Symbol].AvgPrice

		res, err := brk.SubmitOrder(ctx, authOrder)
		if err != nil {
			return false
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

			// The scale-out ladder sizes its rungs against the largest the
			// position has been, so adding to one re-bases them.
			if q := snap.Holdings[order.Symbol].Qty + res.FilledQty; q > origQty[order.Symbol] {
				origQty[order.Symbol] = q
			}

			// The real commission the sim broker just debited for this fill,
			// pooled so a later partial exit can draw its fair share out of
			// what was ACTUALLY charged rather than recomputing Commission()
			// against a synthetic per-rung basis (see entryCommission's
			// declaration comment).
			entryCommission[order.Symbol] += bt.Costs.Commission(val)
		} else {
			// Win/loss net of round-trip commission, matching the equity curve
			// (which the sim broker debits fees from): a gross gain smaller than
			// its costs is a loss.
			//
			// The entry side of that cost is this rung's proportional share of
			// the pooled real entry commission (qty share of the position as it
			// stood immediately before this fill) — not a fresh
			// Commission(avgCost*qty) call, which would re-apply any flat/
			// minimum commission component once per rung instead of once for
			// the whole position.
			qtyBeforeSell := snap.Holdings[order.Symbol].Qty

			var entryShare float64
			if qtyBeforeSell > 0 {
				entryShare = entryCommission[order.Symbol] * (res.FilledQty / qtyBeforeSell)
				entryCommission[order.Symbol] -= entryShare
			}

			basis := avgCost * res.FilledQty
			cost := bt.Costs.Commission(val) + entryShare
			net := (fillPx-avgCost)*res.FilledQty - cost

			if net >= 0 {
				wins++
			} else {
				losses++
			}

			// One record per closed cycle. The aggregates (profit factor,
			// expectancy) come from these, and so do the per-symbol and
			// per-exit-reason breakdowns that say *which* part worked.
			trip := RoundTrip{
				Symbol: order.Symbol, Entry: entryTime[order.Symbol], Exit: ts,
				EntryPx: avgCost, ExitPx: fillPx, Qty: res.FilledQty,
				PnL: net, ExitKind: classifyExit(reason), ExitLabel: reason,
			}
			if !trip.Entry.IsZero() {
				trip.Held = trip.Exit.Sub(trip.Entry)
			}

			if basis > 0 {
				trip.PnLPct = net / basis * 100
			}

			roundTrips = append(roundTrips, trip)

			// A scale-out rung realizes P&L but does not end the trade: the
			// per-symbol state (trailing peak, entry time, ladder progress) has to
			// survive it, and the protections must judge completed trades rather
			// than tranches of one.
			if snap.Holdings[order.Symbol].Qty-res.FilledQty <= 1e-9 {
				prot.RecordClose(order.Symbol, net,
					trip.ExitKind == ExitStopLoss || trip.ExitKind == ExitTrailingStop)

				delete(peaks, order.Symbol)
				// Without this a re-entry inherits the FIRST entry's timestamp, so
				// the ROI ladder and max_hold measure the age of a position that
				// closed long ago — and max_hold would exit the new one at once.
				delete(entryTime, order.Symbol)
				delete(origQty, order.Symbol)
				delete(partials, order.Symbol)
				delete(entryCommission, order.Symbol)
				delete(adds, order.Symbol)
			}
		}

		trades = append(trades, engine.TradeRecord{
			Time: ts, Symbol: order.Symbol, Side: order.Side, Qty: res.FilledQty,
			Price: fillPx, Value: val, Status: "filled", Reason: reason, OrderID: res.ID,
		})

		syncPF()

		if order.Side == broker.Sell && pf.PositionQty(order.Symbol) <= 0 {
			delete(entryTime, order.Symbol)
		}

		return true
	}

	syncPF() // initialise cash/positions from the broker before the run

	for i := range maxLen {
		// ----- 1. The open: fill what the previous bar decided -----
		for _, sym := range symbols {
			c := data[sym]
			if i < len(c) {
				prices[sym] = barOpen(c[i])

				if ts.IsZero() || c[i].Time.After(ts) {
					ts = c[i].Time
				}
			}

			pf.MarkPrice(sym, prices[sym]) // cheap mark-to-market; cash unchanged between fills
		}

		for _, sym := range symbols { // sorted, so fills are deterministic
			po, queued := pending[sym]
			if !queued {
				continue
			}

			delete(pending, sym)

			price := prices[sym]
			if price <= 0 || i >= len(data[sym]) {
				continue
			}

			switch po.side {
			case broker.Buy:
				if bt.AllowPyramiding || pf.PositionQty(sym) <= 0 {
					exec(broker.Order{
						Symbol: sym, Side: broker.Buy,
						Qty: po.notional / price, Type: broker.Market,
					}, po.reason)
				}

			case broker.Sell:
				if held := pf.PositionQty(sym); held > 0 {
					exec(broker.Order{
						Symbol: sym, Side: broker.Sell, Qty: held, Type: broker.Market,
					}, po.reason)
				}
			}
		}

		// ----- 2. During the bar: protective exits against the high/low -----
		if bt.Exits.Enabled {
			for _, sym := range symbols {
				c := data[sym]
				if i >= len(c) {
					continue
				}

				// Holding, not Snapshot: this runs per symbol per bar, and a
				// Snapshot would copy the whole book to read one entry.
				h, isHeld := pf.Holding(sym)
				if !isHeld || h.Qty <= 0 {
					continue
				}

				bar := c[i]
				entry := h.AvgPrice
				// The trail is tested against the peak as it stood BEFORE this
				// bar — not including this bar's own high. OHLC data does not
				// say whether the high or the low came first within the bar;
				// ratcheting the peak up to this bar's high and then testing
				// that same bar's low against the raised level silently
				// assumes "high, then low" every time, which is look-ahead: it
				// can report a trailing-stop fill (or save one that a
				// pre-ratchet level would have triggered) using a peak the
				// position could only be known to have reached partway through
				// the bar being tested. See CODE_REVIEW_FINDINGS.md's
				// backtest/live divergence cluster, "the trailing stop
				// ratchets on the same bar's high and then fills at the
				// ratcheted level".
				peak := math.Max(peaks[sym], entry)

				// Position age drives the ROI ladder.
				age := time.Duration(0)
				if et, ok := entryTime[sym]; ok {
					age = bar.Time.Sub(et)
				}

				if fill, reason := bt.exitFill(
					bar,
					entry,
					peak,
					bt.atrAt(c, i),
					age,
				); reason != "" {
					prices[sym] = fill // the exit fills at its trigger, not at the close

					exec(broker.Order{
						Symbol: sym, Side: broker.Sell, Qty: h.Qty, Type: broker.Market,
					}, "EXIT "+reason)

					continue
				}

				// Only now, having tested against the pre-bar peak, does this
				// bar's high get folded in — so it can ratchet the trail for
				// bars from here on, without having been used to judge itself.
				peaks[sym] = math.Max(peak, bar.High)

				// Only once no full exit fired, exactly as the live engine orders
				// them: a trailing stop on the way down from a rung closes the
				// position rather than shaving it.
				orig := max(origQty[sym], h.Qty)

				qty, fill, reason, nextTaken := bt.partialFill(bar, entry, h.Qty, orig, partials[sym])
				if reason == "" {
					continue
				}

				prices[sym] = fill
				partials[sym] = nextTaken

				exec(broker.Order{
					Symbol: sym, Side: broker.Sell, Qty: qty, Type: broker.Market,
				}, "EXIT "+reason)
			}
		}

		// ----- 2b. During the bar: scale in on further drawdown -----
		//
		// A separate pass from the exits above, on purpose: it fires below
		// entry rather than against it, so the two never compete for the
		// same bar's fill, and this mirrors the live engine's checkScaleIn
		// being a wholly separate pass from the exit check. Uses the bar's
		// low, like the stop-loss/trailing-stop check above, and fills at
		// the threshold price (or the open, if the open itself already
		// gapped past it) — the same resting-order convention exitFill uses.
		if bt.ScaleIn.Enabled && bt.ScaleIn.MaxAdds > 0 &&
			bt.ScaleIn.StepPct > 0 && bt.ScaleIn.SizePct > 0 {
			for _, sym := range symbols {
				c := data[sym]
				if i >= len(c) {
					continue
				}

				h, isHeld := pf.Holding(sym)
				if !isHeld || h.Qty <= 0 || h.AvgPrice <= 0 {
					continue
				}

				if adds[sym] >= bt.ScaleIn.MaxAdds {
					continue
				}

				bar := c[i]
				threshold := h.AvgPrice * (1 - bt.ScaleIn.StepPct)

				if bar.Low > threshold {
					continue
				}

				// The strategy's action as of the PRIOR bar's close — the
				// only signal known without looking ahead into this bar.
				if bt.ScaleIn.RequireSignal && lastSignal[sym] != strategy.Buy {
					continue
				}

				if ok, _ := prot.AllowEntry(sym); !ok {
					continue
				}

				add := rm.SizeBuy(
					sym, c[i].Close, pf.Snapshot(), bt.sizingConfig(), bt.atrAt(c, i))
				if !add.OK {
					continue
				}

				size := add.Notional * bt.ScaleIn.SizePct

				retsAt.bar = i

				if ok, _ := guard.AllowEntry(sym, pf.Snapshot(), size); !ok {
					continue
				}

				fill := gapAdjusted(barOpen(bar), threshold, true)
				if fill <= 0 {
					continue
				}

				prices[sym] = fill

				filled := exec(broker.Order{
					Symbol: sym, Side: broker.Buy, Qty: size / fill, Type: broker.Market,
				}, fmt.Sprintf("SCALE-IN add %d/%d", adds[sym]+1, bt.ScaleIn.MaxAdds))

				if filled {
					adds[sym]++
				}
			}
		}

		// ----- 3. The close: evaluate and queue for the next open -----
		for _, sym := range symbols {
			c := data[sym]
			if i >= len(c) {
				continue
			}

			prices[sym] = c[i].Close

			pf.MarkPrice(sym, c[i].Close)

			slots++

			if pf.PositionQty(sym) > 0 {
				exposed++
			}

			start := i + 1 - lookback
			if start < 0 {
				start = 0
			}

			sig := bt.strategyFor(sym).Evaluate(sym, c[start:i+1])

			// The raw, unconfirmed action — mirroring what the live engine's
			// signalFor reads (engine.go's setSignal stores Evaluate's output
			// directly, before any confirm-streak gating), since that is what
			// scale_in.require_signal checks live: "the strategy still says
			// BUY", not "a confirmed BUY has fired". Captured before the
			// confirm gate below so a bar that raises this but does not (yet)
			// confirm still updates it, exactly as live would on that tick.
			lastSignal[sym] = sig.Action

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

			switch sig.Action {
			case strategy.Hold:
				delete(pending, sym) // a HOLD cancels a decision not yet filled

			case strategy.Buy:
				if pf.PositionQty(sym) > 0 && !bt.AllowPyramiding {
					continue // already in a position; don't add to it
				}

				sized := rm.SizeBuy(
					sym, c[i].Close, pf.Snapshot(), bt.sizingConfig(), bt.atrAt(c, i))
				if !sized.OK {
					continue // no headroom, or risk-scaled below the tradable minimum
				}

				size := sized.Notional * bt.strengthFraction(sig.Strength)

				// Protections gate entries only, exactly as they do live: a
				// cooldown or a halt must never stop an exit.
				if ok, _ := prot.AllowEntry(sym); !ok {
					continue
				}

				// Concentration caps, measured only on history that exists now.
				retsAt.bar = i

				if ok, _ := guard.AllowEntry(sym, pf.Snapshot(), size); !ok {
					continue
				}

				pending[sym] = pendingOrder{
					side: broker.Buy, notional: size, reason: sig.Reason,
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

					pending[sym] = pendingOrder{side: broker.Sell, reason: sig.Reason}
				}
			}
		}

		acct, _ := brk.GetAccount(ctx)

		equity = append(equity, EquityPoint{Time: ts, Equity: acct.Equity})

		// Drawdown and daily-loss halts. Recording the first trip matters: a
		// configuration whose equity curve looks acceptable overall may have
		// crossed the halt threshold on the way, at which point the live bot
		// would have stopped and never reached the rest of this curve.
		if reason := prot.ObserveEquity(acct.Equity); reason != "" && haltedAt == "" {
			haltedAt = fmt.Sprintf("%s: %s", ts.Format("2006-01-02 15:04"), reason)
		}
	}

	return bt.metrics(data, symbols, equity, runStats{
		trades: trades, wins: wins, losses: losses, bars: maxLen,
		roundTrips: roundTrips, exposed: exposed, slots: slots, haltedAt: haltedAt,
	})
}

// runStats carries the raw tallies a completed run hands to metrics.
type runStats struct {
	trades         []engine.TradeRecord
	wins, losses   int
	bars           int
	roundTrips     []RoundTrip
	exposed, slots int
	haltedAt       string
}

// barOpen returns a candle's open, falling back to its close for data sources
// that only carry closes.
func barOpen(c market.Candle) float64 {
	if c.Open > 0 {
		return c.Open
	}

	return c.Close
}

// exitFill reports which protective exit fires somewhere inside bar (if any),
// and the price it fills at. The levels come from the same rule engine the live
// bot uses (internal/exits), so an ATR stop or an ROI-ladder target behaves
// identically here; only the *comparison* differs — the live engine sees a
// stream of ticks, a backtest sees one bar's high and low.
//
// Levels are tested against the high and low, not the close: a stop that was
// pierced intrabar is hit even if the bar recovered. Downside exits are tested
// first — within a bar the order of the high and low is unknowable, so a bar
// that touched both the stop and the target counts as the stop.
func (bt *Backtester) exitFill(
	bar market.Candle,
	entry, peak, atr float64,
	held time.Duration,
) (float64, string) {
	lv := exits.Compute(bt.Exits, entry, peak, atr, held)
	open := barOpen(bar)

	if lv.Stop > 0 && bar.Low <= lv.Stop {
		return gapAdjusted(open, lv.Stop, true),
			fmt.Sprintf("stop-loss %.2f<=%.2f", lv.Stop, entry)
	}

	if lv.Trail > 0 && bar.Low <= lv.Trail {
		return gapAdjusted(open, lv.Trail, true),
			fmt.Sprintf("trailing-stop %.2f<=peak %.2f", lv.Trail, peak)
	}

	if lv.Target > 0 && bar.High >= lv.Target {
		return gapAdjusted(open, lv.Target, false),
			fmt.Sprintf("%s %.2f>=%.2f", lv.TargetReason, lv.Target, entry)
	}

	// The time barrier last, and at the close: it has no price level to fill at,
	// and a bar that also broke a price level has already returned above with the
	// more accurate fill.
	if lv.TimeExit != "" {
		return bar.Close, lv.TimeExit
	}

	return 0, ""
}

// partialFill reports whether the scale-out ladder takes a tranche inside bar,
// and the quantity and price it fills at.
//
// A rung is a resting take-profit: it fills when the bar trades through the
// threshold, at the threshold, not at the close. Gaps are handled the same way
// as any other target — an open already past the level fills at the open, since
// that is where liquidity actually was.
//
// nextTaken reports the taken count the caller must store for next time. It is
// part.Rung+1 — the highest rung actually consumed by this fill, plus one —
// not simply taken+1: exits.NextPartial can combine several rungs into one
// Fraction when a bar's high gaps past more than one threshold at once (the
// live engine models the same combined fill — see engine/exits.go's
// PartialsTaken = rung + 1). Advancing the counter by only one regardless of
// how many rungs were actually consumed left the already-sold rung(s) still
// "due" on the next bar — NextPartial would report it as due again the
// moment price merely stayed at or above that already-satisfied level, which
// on the very next check tends to sell the rest of the position outright
// (the "never leave an unsellable crumb" guard below) — where the live
// engine correctly holds the remainder until a genuinely new rung is
// reached. See CODE_REVIEW_FINDINGS.md's backtest/live divergence cluster,
// "the scale-out ladder advances one rung per fill in backtest, the highest
// rung hit live".
func (bt *Backtester) partialFill(
	bar market.Candle,
	entry, held, orig float64,
	taken int,
) (qty, fill float64, reason string, nextTaken int) {
	if entry <= 0 || bar.High <= 0 {
		return 0, 0, "", taken
	}

	part, due := exits.NextPartial(bt.Exits, bar.High/entry-1, taken)
	if !due {
		return 0, 0, "", taken
	}

	qty = math.Min(orig*part.Fraction, held)
	if held-qty <= 1e-9 {
		qty = held // never leave an unsellable crumb
	}

	if qty <= 0 {
		return 0, 0, "", taken
	}

	// The fill price is the rung's own threshold, which is where the order would
	// have rested — not the bar's high, which is hindsight. Read through
	// exits.RungProfitPct so the index means the same thing here as it did to
	// NextPartial, which produced it.
	profit, ok := exits.RungProfitPct(bt.Exits, part.Rung)
	if !ok {
		return 0, 0, "", taken
	}

	level := entry * (1 + profit)

	return qty, gapAdjusted(barOpen(bar), level, false), part.Reason, part.Rung + 1
}

// atrAt returns the ATR over the candles up to and including index i, or 0 when
// the volatility-adaptive stops are not in use (so the cost is not paid) or
// there is too little history.
func (bt *Backtester) atrAt(candles []market.Candle, i int) float64 {
	if !exits.UsesATR(bt.Exits) {
		return 0
	}

	period := exits.ATRPeriod(bt.Exits)
	if i < period+1 {
		return 0
	}

	start := i + 1 - (period * 3)
	if start < 0 {
		start = 0
	}

	window := candles[start : i+1]

	// Three scratch columns, reused across bars. This runs once per symbol per
	// bar whenever an ATR-based stop is configured, and allocating the window
	// each time made the exit check cost three garbage slices per bar for a
	// single number. The buffers are owned by this Backtester and Run is not
	// re-entrant, so reuse is safe.
	bt.atrBuf = growColumns(bt.atrBuf, len(window))

	for j, c := range window {
		bt.atrBuf.high[j], bt.atrBuf.low[j], bt.atrBuf.closes[j] = c.High, c.Low, c.Close
	}

	atr, ok := indicators.ATRLast(bt.atrBuf.high, bt.atrBuf.low, bt.atrBuf.closes, period)
	if !ok {
		return 0
	}

	return atr
}

// columns is a reusable set of aligned OHLC columns.
type columns struct {
	high, low, closes []float64
}

// growColumns resizes c to hold exactly n values, reusing its arrays when they
// are already large enough.
func growColumns(c columns, n int) columns {
	if cap(c.high) >= n {
		return columns{c.high[:n], c.low[:n], c.closes[:n]}
	}

	// One allocation carved into three, and rounded up so a window that grows
	// bar by bar does not reallocate on every one.
	size := n + n/4 + 16
	buf := make([]float64, 3*size)

	return columns{
		high:   buf[0:n:size],
		low:    buf[size : size+n : 2*size],
		closes: buf[2*size : 2*size+n : 3*size],
	}
}

// gapAdjusted returns the price a resting exit really fills at. If the bar
// opened beyond the level the market gapped through it overnight and the fill
// happens at the open — worse than the level for a stop, better for a target.
// Otherwise the level itself is reached during the bar.
func gapAdjusted(open, level float64, down bool) float64 {
	if down && open <= level {
		return open
	}

	if !down && open >= level {
		return open
	}

	return level
}

// metrics assembles the final Result: returns, the buy & hold benchmark,
// drawdown, the risk-adjusted ratios and the trade-quality statistics.
func (bt *Backtester) metrics(data map[string][]market.Candle, symbols []string,
	equity []EquityPoint, st runStats,
) Result {
	r := Result{
		StartingCash: bt.Cash, Bars: st.bars, Trades: st.trades, Equity: equity,
		NumTrades: len(st.trades), Wins: st.wins, Losses: st.losses,
	}
	if len(equity) > 0 {
		r.FinalEquity = equity[len(equity)-1].Equity
	} else {
		r.FinalEquity = bt.Cash
	}

	if bt.Cash > 0 {
		r.TotalReturnPct = (r.FinalEquity/bt.Cash - 1) * 100
	}

	if st.wins+st.losses > 0 {
		r.WinRatePct = float64(st.wins) / float64(st.wins+st.losses) * 100
	}

	if st.slots > 0 {
		r.ExposurePct = float64(st.exposed) / float64(st.slots) * 100
	}

	r.MaxDrawdownPct = maxDrawdown(equity) * 100
	r.BuyHoldReturnPct = buyHold(data, symbols) * 100

	// Risk-adjusted ratios, annualised from the observed bar spacing so they are
	// comparable with published figures instead of being per-bar curiosities.
	rets := barReturns(equity)
	periods := periodsPerYear(equity)

	r.SpanDays = spanYears(equity) * 365.25

	r.Sharpe = annualisedRatio(rets, periods, false)
	r.Sortino = annualisedRatio(rets, periods, true)

	if years := spanYears(equity); years > 0 && bt.Cash > 0 && r.FinalEquity > 0 {
		r.CAGRPct = (math.Pow(r.FinalEquity/bt.Cash, 1/years) - 1) * 100
	}

	if r.MaxDrawdownPct > 0 {
		r.Calmar = r.CAGRPct / r.MaxDrawdownPct
	}

	r.ProfitFactor = profitFactor(pnlSeries(st.roundTrips))
	r.ExpectancyPct, r.AvgWinPct, r.AvgLossPct = tradeQuality(pctSeries(st.roundTrips))

	r.RoundTrips = st.roundTrips
	r.BySymbol = bySymbol(st.roundTrips)
	r.ByExit = byExitKind(st.roundTrips)
	r.ByMonth = monthlyReturns(equity, bt.Cash)
	r.Hold = holdStats(st.roundTrips)
	r.HaltedAt = st.haltedAt

	return r
}

// maxDrawdown returns the deepest peak-to-trough fall of the equity curve, as a
// fraction.
func maxDrawdown(equity []EquityPoint) float64 {
	peak := math.Inf(-1)
	worst := 0.0

	for _, p := range equity {
		if p.Equity > peak {
			peak = p.Equity
		}

		if peak <= 0 {
			continue
		}

		if dd := (peak - p.Equity) / peak; dd > worst {
			worst = dd
		}
	}

	return worst
}

// buyHold returns the equal-weighted buy & hold return of the universe, as a
// fraction — the benchmark any strategy has to beat to justify trading at all.
func buyHold(data map[string][]market.Candle, symbols []string) float64 {
	var (
		sum float64
		n   int
	)

	for _, s := range symbols {
		c := data[s]
		if len(c) > 1 && c[0].Close > 0 {
			sum += c[len(c)-1].Close/c[0].Close - 1
			n++
		}
	}

	if n == 0 {
		return 0
	}

	return sum / float64(n)
}

// barReturns converts the equity curve into per-bar simple returns.
func barReturns(equity []EquityPoint) []float64 {
	if len(equity) < 3 {
		return nil
	}

	rets := make([]float64, 0, len(equity)-1)
	for i := 1; i < len(equity); i++ {
		if equity[i-1].Equity > 0 {
			rets = append(rets, equity[i].Equity/equity[i-1].Equity-1)
		}
	}

	return rets
}

// spanYears is the wall-clock length of the run in years, from the equity
// curve's timestamps. Returns 0 when the data carries no usable times.
func spanYears(equity []EquityPoint) float64 {
	if len(equity) < 2 {
		return 0
	}

	first, last := equity[0].Time, equity[len(equity)-1].Time
	if first.IsZero() || last.IsZero() || !last.After(first) {
		return 0
	}

	const hoursPerYear = 24 * 365.25

	return last.Sub(first).Hours() / hoursPerYear
}

// periodsPerYear is how many bars fit in a year at the observed bar spacing. It
// is what turns a per-bar ratio into an annual one; without it a 1-minute
// strategy and a daily one produce Sharpe numbers that cannot be compared. When
// the timestamps are unusable it falls back to 252 (trading days).
func periodsPerYear(equity []EquityPoint) float64 {
	years := spanYears(equity)
	if years <= 0 {
		return 252
	}

	return float64(len(equity)-1) / years
}

// annualisedRatio computes the annualised Sharpe (or Sortino, when downsideOnly
// is set) of the return series against a zero risk-free rate. Sortino measures
// only the deviation of losing bars, on the argument that upside volatility is
// not risk.
//
// Both variants divide the accumulated squared deviation by a count of ALL
// observations — Sharpe by len-1 (the sample standard deviation), Sortino by
// len (downside deviation is conventionally a population figure over the whole
// series, with the non-losing bars contributing zero rather than being dropped).
//
// Dividing the Sortino sum by the number of LOSING bars instead, as this used
// to, is not a scale change that cancels out in a ranking: it multiplies the
// result by sqrt(losers/len), a factor that differs per strategy, so it
// reorders the leaderboard. Measured on two series with identical mean and
// identical total downside energy, one losing on 1 bar in 10 and the other on 6
// — the correct formula ranks the first ABOVE the second (17.57 vs 17.22), the
// old divisor ranked it BELOW (5.56 vs 13.34), penalising the strategy that
// lost on fewer bars, which is the opposite of what Sortino measures. It drives
// cmd/optimize's -rank sortino and internal/autotune's sortino objective. See
// CODE_REVIEW_FINDINGS.md's BT-1 finding.
//
// would duplicate the mean/deviation arithmetic
//
//nolint:cyclop // one accumulation loop with two guarded variants; splitting it
func annualisedRatio(rets []float64, periodsPerYear float64, downsideOnly bool) float64 {
	if len(rets) < 2 || periodsPerYear <= 0 {
		return 0
	}

	var mean float64

	for _, x := range rets {
		mean += x
	}

	mean /= float64(len(rets))

	var (
		sumSq float64
		// contributing counts the observations that added to sumSq. For
		// Sortino that is the losing bars only, and it is used solely to
		// detect "no losing bars at all" — never as the divisor.
		contributing int
	)

	for _, x := range rets {
		if downsideOnly {
			if x < 0 {
				sumSq += x * x
				contributing++
			}

			continue
		}

		sumSq += (x - mean) * (x - mean)
		contributing++
	}

	if contributing < 1 {
		// No losing bars at all: a Sortino of infinity is not a useful number, so
		// report 0 rather than something that would top every leaderboard.
		return 0
	}

	divisor := float64(len(rets)) // Sortino: population, over every observation
	if !downsideOnly {
		divisor = float64(len(rets) - 1) // Sharpe: sample standard deviation
	}

	if divisor <= 0 {
		return 0
	}

	sd := math.Sqrt(sumSq / divisor)
	if sd == 0 {
		return 0
	}

	return mean / sd * math.Sqrt(periodsPerYear)
}

// profitFactor is gross profit divided by gross loss across closed round trips.
// Below 1 the strategy loses money; a strategy with no losses at all reports 0
// rather than infinity, since that is almost always too few trades to mean
// anything.
func profitFactor(pnl []float64) float64 {
	var gross, loss float64

	for _, v := range pnl {
		if v >= 0 {
			gross += v
		} else {
			loss -= v
		}
	}

	if loss <= 0 {
		return 0
	}

	return gross / loss
}

// tradeQuality returns the mean P&L per round trip and the average win and loss,
// all as a percentage of the position's cost basis. Expectancy is the number
// that matters most: a 90% win rate is worthless if the losses are ten times the
// size of the wins.
func tradeQuality(pnlPct []float64) (expectancy, avgWin, avgLoss float64) {
	if len(pnlPct) == 0 {
		return 0, 0, 0
	}

	var (
		sum, winSum, lossSum float64
		nWin, nLoss          int
	)

	for _, v := range pnlPct {
		sum += v

		if v >= 0 {
			winSum += v
			nWin++
		} else {
			lossSum += v
			nLoss++
		}
	}

	expectancy = sum / float64(len(pnlPct))

	if nWin > 0 {
		avgWin = winSum / float64(nWin)
	}

	if nLoss > 0 {
		avgLoss = lossSum / float64(nLoss)
	}

	return expectancy, avgWin, avgLoss
}
