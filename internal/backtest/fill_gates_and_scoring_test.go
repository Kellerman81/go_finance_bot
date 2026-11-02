package backtest

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// TestRatioObjectivesScoreAFlawlessResultSensibly is the regression for
// AT2-4, in both directions. Profit factor is reported as 0 with no losing
// trade and Calmar as 0 with no drawdown — undefined, honestly displayed.
// Scored as 0, the best possible incumbent ranked as the worst, so any worse
// candidate cleared autotune's "beats the baseline" gate and was applied
// unsupervised. A flat bonus for "flawless" — the first attempt at a fix,
// caught in verification — inverts it: ten tiny winners would outrank a far
// more profitable record with one small loss. The smoothed scores must get
// both right.
func TestRatioObjectivesScoreAFlawlessResultSensibly(t *testing.T) {
	t.Parallel()

	flawless := Result{Wins: 5, AvgWinPct: 2, CAGRPct: 12}
	weaker := Result{Wins: 5, Losses: 3, AvgWinPct: 2, AvgLossPct: -2, CAGRPct: 9, MaxDrawdownPct: 6}

	if ScoreProfitFactor(flawless) <= ScoreProfitFactor(weaker) {
		t.Errorf("profitfactor: no losing trade scored %v, not above a record with losses (%v)",
			ScoreProfitFactor(flawless), ScoreProfitFactor(weaker))
	}

	if ScoreCalmar(flawless) <= ScoreCalmar(weaker) {
		t.Errorf("calmar: no drawdown scored %v, not above a weaker record with one (%v)",
			ScoreCalmar(flawless), ScoreCalmar(weaker))
	}

	tinyFlawless := Result{Wins: 10, AvgWinPct: 0.1, CAGRPct: 0.5}
	strongOneLoss := Result{Wins: 20, Losses: 1, AvgWinPct: 5, AvgLossPct: -0.5, CAGRPct: 40, MaxDrawdownPct: 0.5}

	if ScoreProfitFactor(tinyFlawless) >= ScoreProfitFactor(strongOneLoss) {
		t.Errorf("profitfactor: ten 0.1%% winners (%v) outranked twenty 5%% winners and one 0.5%% loss (%v)",
			ScoreProfitFactor(tinyFlawless), ScoreProfitFactor(strongOneLoss))
	}

	if ScoreCalmar(tinyFlawless) >= ScoreCalmar(strongOneLoss) {
		t.Errorf("calmar: a 0.5%% CAGR with no drawdown (%v) outranked 40%% CAGR with a 0.5%% one (%v)",
			ScoreCalmar(tinyFlawless), ScoreCalmar(strongOneLoss))
	}

	if idle := (Result{}); ScoreProfitFactor(idle) != 0 || ScoreCalmar(idle) != 0 {
		t.Error("a result with no trades scored above zero")
	}
}

// TestSameBarBuysRespectTheClassCapTogether is the regression for BT2-2. The
// portfolio guard ran only when a buy was DECIDED, against a snapshot that
// could not see the other buys queued on the same bar, and the next-open fill
// only went through Authorize, which has no class, sector or correlation caps.
// Two ETFs under a 600 class cap both filled on the first bar: 999.99 of
// exposure. Live, act handles one symbol at a time and refuses the second.
func TestSameBarBuysRespectTheClassCapTogether(t *testing.T) {
	t.Parallel()

	d := map[string][]market.Candle{
		"IWDA": risingCandles("IWDA", 400),
		"VWCE": risingCandles("VWCE", 400),
	}

	bt := assetClassBacktester(config.PortfolioConfig{
		Enabled: true, MaxPctPerAssetClass: map[string]float64{"etf": 0.006},
	})

	res := bt.Run(d)

	var first time.Time

	total := 0.0

	for _, tr := range res.Trades {
		if tr.Side != "buy" {
			continue
		}

		if first.IsZero() {
			first = tr.Time
		}

		if tr.Time.Equal(first) {
			total += tr.Value
		}
	}

	if total > 600.01 {
		t.Errorf("first-bar etf buys total %.2f, over the 600 class cap", total)
	}
}

// buyAt buys each symbol once, on the bar whose time matches its entry here.
type buyAt struct{ when map[string]time.Time }

func (buyAt) Name() string    { return "buyat" }
func (buyAt) WarmupBars() int { return 1 }

func (b buyAt) Evaluate(symbol string, candles []market.Candle) strategy.Signal {
	last := candles[len(candles)-1]

	action := strategy.Hold
	if last.Time.Equal(b.when[symbol]) {
		action = strategy.Buy
	}

	return strategy.Signal{Symbol: symbol, Action: action, Strength: 1, Price: last.Close}
}

// TestAHaltAtTheCloseStopsThatBarsQueuedBuy is the regression for BT2-5.
// Protections observe equity at a bar's close, after that bar's decisions,
// and the next-open fill never re-checked them — so the bar on which a
// drawdown halt tripped still opened one more position. Live checks drawdown
// before acting and would not.
func TestAHaltAtTheCloseStopsThatBarsQueuedBuy(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	a := make([]market.Candle, 10)
	b := make([]market.Candle, 10)

	for i := range a {
		a[i] = openBar(at(i), 100, 100, 100, 100)
		b[i] = openBar(at(i), 50, 50, 50, 50)
		b[i].Symbol = "BBB"
		a[i].Symbol = "AAA"
	}

	for i := 5; i < len(a); i++ { // AAA crashes at bar 5's close: -40% on the account
		a[i] = openBar(at(i), 100, 100, 20, 20)
		a[i].Symbol = "AAA"
	}

	lim := testLimits()
	lim.MaxOrderValue = 50000 // AAA takes half the account, leaving cash for BBB

	bt := &Backtester{
		Strategy: buyAt{when: map[string]time.Time{"AAA": at(0), "BBB": at(5)}},
		Limits:   lim, Cash: 100000,
		Protections: config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.2},
	}

	res := bt.Run(map[string][]market.Candle{"AAA": a, "BBB": b})

	for _, tr := range res.Trades {
		if tr.Symbol == "BBB" && tr.Side == "buy" {
			t.Fatalf("BBB was bought at %v, after the drawdown halt tripped at the close it was decided on", tr.Time)
		}
	}
}

// TestClosedTradesCountsRoundTripsNotFills is the regression for BT2-3.
// NumTrades is len(trades) — every buy, sell, scale-in add and scale-out rung
// — and every min_trades gate read it as a trade count, so
// autotune.min_trades: 10 was cleared by five round trips: the thin-sample
// safeguard was half as strict as the README says.
func TestClosedTradesCountsRoundTripsNotFills(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	bars := make([]market.Candle, 20)
	for i := range bars {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 100, 100.5, 99.5, 100)
	}

	bars[10] = openBar(base.Add(10*time.Minute), 100, 100, 90, 92) // one stop-out

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(), Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.05},
	}

	res := bt.Run(data(bars))
	if res.NumTrades != 2 {
		t.Fatalf("NumTrades = %d, want 2 fills (one buy, one stop); the scenario did not set up", res.NumTrades)
	}

	if res.ClosedTrades != 1 {
		t.Errorf("ClosedTrades = %d for one round trip, want 1", res.ClosedTrades)
	}
}

// TestOptimizeGatesOnClosedTrades is the gate half of BT2-3: no trial that
// survives min_trades may have fewer CLOSED trades than the floor.
func TestOptimizeGatesOnClosedTrades(t *testing.T) {
	t.Parallel()

	grid := ParamGrid{
		DetectorSets: [][]string{{"ema_cross", "macd"}, {"rsi"}, {"ema_cross"}},
		Combine:      []string{"majority"},
		MinStrength:  []float64{0.0, 0.03},
		CapitalFrac:  []float64{0.25},
		StopLoss:     []float64{0.02, 0.05},
		TrailingStop: []float64{0.08},
		TakeProfit:   []float64{0.05, 0.2},
	}

	const minTrades = 4

	trials := Optimize(context.Background(), config.Default(), Generate([]string{"AAPL"}, 800),
		grid, 50000, minTrades, ScoreBlend, false)

	for _, tr := range trials {
		if tr.Result.ClosedTrades < minTrades {
			t.Errorf("a trial with %d closed trades (%d fills) passed min_trades %d",
				tr.Result.ClosedTrades, tr.Result.NumTrades, minTrades)
		}
	}
}

// TestMisalignedSymbolsAreReported is the regression for BT2-4. The replay
// steps every symbol by bar index, so a holiday in one market, a later
// listing date or a gap silently paired bars from different moments — cash,
// the caps and the correlation filter then mixed them. The assumption is in
// the package doc; nothing checked it. A run must say when it does not hold.
func TestMisalignedSymbolsAreReported(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	day := func(i int) time.Time { return base.AddDate(0, 0, i) }

	a, b := make([]market.Candle, 5), make([]market.Candle, 5)
	for i := range a {
		a[i] = openBar(day(i), 100, 100, 100, 100)
		a[i].Symbol = "AAA"
		b[i] = openBar(day(i), 50, 50, 50, 50)
		b[i].Symbol = "BBB"
	}

	bt := &Backtester{Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(), Cash: 100000}

	if res := bt.Run(map[string][]market.Candle{"AAA": a, "BBB": b}); res.Misaligned != "" {
		t.Errorf("aligned data reported misaligned: %s", res.Misaligned)
	}

	b = append(b[:2:2], b[3:]...) // BBB skips a day: every later index is a day off
	b = append(b, openBar(day(5), 50, 50, 50, 50))

	if res := bt.Run(map[string][]market.Candle{"AAA": a, "BBB": b}); res.Misaligned == "" {
		t.Error("a symbol with a missing day was replayed as if aligned, with no warning")
	}
}
