package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// trip builds a round trip for the aggregation tests.
func trip(sym string, pnl, pct float64, kind string, held time.Duration) RoundTrip {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	return RoundTrip{
		Symbol: sym, Entry: base, Exit: base.Add(held), Held: held,
		PnL: pnl, PnLPct: pct, ExitKind: kind,
	}
}

// TestClassifyExit checks the raw reason strings the engine and backtester
// produce map onto stable kinds. Without this the breakdown aggregates nothing,
// because "stop-loss 92.40<=100.00" is unique per trade.
func TestClassifyExit(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"EXIT stop-loss 92.40<=100.00":                        ExitStopLoss,
		"EXIT trailing-stop 110.00<=peak 120":                 ExitTrailingStop,
		"EXIT take-profit +20.0% 120.00>=100":                 ExitTakeProfit,
		"EXIT roi 2h +2.0% 102.00>=100.00":                    ExitROI,
		"rsi:SELL(0.60), macd:SELL(0.12) => SELL by majority": ExitStrategy,
		"": ExitStrategy,
	}

	for reason, want := range cases {
		if got := classifyExit(reason); got != want {
			t.Errorf("classifyExit(%q) = %q, want %q", reason, got, want)
		}
	}
}

// TestClassifyExitPrefersTrailingStop checks the ordering trap: "trailing-stop"
// contains no "stop-loss" substring, but a naive check on "stop" would collapse
// the two rules that most need telling apart.
func TestClassifyExitPrefersTrailingStop(t *testing.T) {
	t.Parallel()

	if got := classifyExit("EXIT trailing-stop 110.00<=peak 120.00"); got != ExitTrailingStop {
		t.Errorf("got %q, want %q — the trailing stop must not be counted as a stop-loss",
			got, ExitTrailingStop)
	}
}

// TestBySymbolAggregates checks per-symbol totals, win rate and ordering.
func TestBySymbolAggregates(t *testing.T) {
	t.Parallel()

	trips := []RoundTrip{
		trip("AAPL", 100, 10, ExitTakeProfit, 2*time.Hour),
		trip("AAPL", -40, -4, ExitStopLoss, 30*time.Minute),
		trip("MSFT", 250, 25, ExitTakeProfit, 4*time.Hour),
	}

	rows := bySymbol(trips)
	if len(rows) != 2 {
		t.Fatalf("got %d symbols, want 2", len(rows))
	}

	// Best P&L first.
	if rows[0].Symbol != "MSFT" {
		t.Errorf("first row = %s, want MSFT (the higher P&L)", rows[0].Symbol)
	}

	aapl := rows[1]
	if aapl.Trades != 2 || aapl.Wins != 1 || aapl.Losses != 1 {
		t.Errorf("AAPL = %+v, want 2 trades / 1 win / 1 loss", aapl)
	}

	if math.Abs(aapl.PnL-60) > 1e-9 {
		t.Errorf("AAPL P&L = %v, want 60", aapl.PnL)
	}

	if math.Abs(aapl.WinRatePct-50) > 1e-9 {
		t.Errorf("AAPL win rate = %v, want 50", aapl.WinRatePct)
	}

	if aapl.AvgHeld != 75*time.Minute {
		t.Errorf("AAPL avg held = %v, want 1h15m", aapl.AvgHeld)
	}
}

// TestByExitKindSurfacesLosingRules is the point of the exit table: a rule that
// loses money must be visible as such, not buried in an aggregate.
func TestByExitKindSurfacesLosingRules(t *testing.T) {
	t.Parallel()

	trips := []RoundTrip{
		trip("A", 100, 10, ExitTakeProfit, time.Hour),
		trip("B", 80, 8, ExitTakeProfit, time.Hour),
		trip("C", -50, -5, ExitStopLoss, time.Hour),
		trip("D", -60, -6, ExitStopLoss, time.Hour),
		trip("E", -5, -0.5, ExitStrategy, time.Hour),
	}

	rows := byExitKind(trips)
	if len(rows) != 3 {
		t.Fatalf("got %d exit kinds, want 3", len(rows))
	}

	if rows[0].Kind != ExitTakeProfit {
		t.Errorf("first row = %s, want the most profitable rule", rows[0].Kind)
	}

	last := rows[len(rows)-1]
	if last.Kind != ExitStopLoss || last.PnL >= 0 {
		t.Errorf("last row = %+v, want the stop-loss with negative P&L", last)
	}

	for _, e := range rows {
		if e.Kind == ExitStopLoss && (e.Trades != 2 || e.Wins != 0) {
			t.Errorf("stop-loss row = %+v, want 2 trades and no wins", e)
		}
	}
}

// TestMonthlyReturns checks the equity curve is split by calendar month and
// each month reports its own return rather than a cumulative one — measured
// from the previous month's close (the run's starting cash for the first), so
// the months chain to the whole run. See TestMonthlyReturnsChainToTheTotal.
func TestMonthlyReturns(t *testing.T) {
	t.Parallel()

	jan := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)

	equity := []EquityPoint{
		{Time: jan, Equity: 100000},
		{Time: jan.AddDate(0, 0, 10), Equity: 110000}, // +10% in January
		{Time: feb, Equity: 110000},
		{Time: feb.AddDate(0, 0, 10), Equity: 99000}, // -10% in February
	}

	rows := monthlyReturns(equity, 100000)
	if len(rows) != 2 {
		t.Fatalf("got %d months, want 2: %+v", len(rows), rows)
	}

	if rows[0].Month != "2026-01" || math.Abs(rows[0].ReturnPct-10) > 1e-9 {
		t.Errorf("January = %+v, want +10%%", rows[0])
	}

	if rows[1].Month != "2026-02" || math.Abs(rows[1].ReturnPct+10) > 1e-9 {
		t.Errorf("February = %+v, want -10%%", rows[1])
	}
}

// TestHoldStatsFlagsAsymmetry checks the win/loss holding split, which exposes
// cutting winners early while sitting on losers.
func TestHoldStatsFlagsAsymmetry(t *testing.T) {
	t.Parallel()

	trips := []RoundTrip{
		trip("A", 10, 1, ExitTakeProfit, 1*time.Hour),
		trip("B", 10, 1, ExitTakeProfit, 2*time.Hour),
		trip("C", -10, -1, ExitStopLoss, 20*time.Hour),
		trip("D", -10, -1, ExitStopLoss, 30*time.Hour),
	}

	h := holdStats(trips)
	if h.Trades != 4 {
		t.Fatalf("trades = %d, want 4", h.Trades)
	}

	if h.Min != time.Hour || h.Max != 30*time.Hour {
		t.Errorf("min/max = %v/%v, want 1h/30h", h.Min, h.Max)
	}

	if h.AvgWinHeld != 90*time.Minute {
		t.Errorf("avg win held = %v, want 1h30m", h.AvgWinHeld)
	}

	if h.AvgLossHeld != 25*time.Hour {
		t.Errorf("avg loss held = %v, want 25h", h.AvgLossHeld)
	}

	if h.AvgLossHeld <= h.AvgWinHeld {
		t.Error("the fixture should show losers held longer than winners")
	}
}

// TestEmptyBreakdownsAreSafe checks a run with no closed trades produces no
// tables rather than dividing by zero.
func TestEmptyBreakdownsAreSafe(t *testing.T) {
	t.Parallel()

	if rows := bySymbol(nil); rows != nil {
		t.Errorf("bySymbol(nil) = %v, want nil", rows)
	}

	if rows := byExitKind(nil); rows != nil {
		t.Errorf("byExitKind(nil) = %v, want nil", rows)
	}

	if rows := monthlyReturns(nil, 0); rows != nil {
		t.Errorf("monthlyReturns(nil, 0) = %v, want nil", rows)
	}

	if h := holdStats(nil); h.Trades != 0 {
		t.Errorf("holdStats(nil) = %+v, want the zero value", h)
	}
}

// TestRoundTripsRecordedInRun proves the records are produced end-to-end, with
// entry/exit times, a classified exit and a holding period.
func TestRoundTripsRecordedInRun(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Enter at 100 on bar 1's open, stop out on bar 2.
	data := map[string][]market.Candle{"TST": {
		openBar(base, 100, 100, 100, 100),
		openBar(base.Add(time.Minute), 100, 101, 99, 100),
		openBar(base.Add(2*time.Minute), 100, 101, 88, 89),
		openBar(base.Add(3*time.Minute), 89, 90, 88, 89),
	}}

	bt := &Backtester{
		Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
		Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.05},
		Cash:  100000, OrderSize: 1000,
	}

	res := bt.Run(data)
	if len(res.RoundTrips) == 0 {
		t.Fatal("no round trips recorded")
	}

	rt := res.RoundTrips[0]
	if rt.Symbol != "TST" {
		t.Errorf("symbol = %q, want TST", rt.Symbol)
	}

	if rt.ExitKind != ExitStopLoss {
		t.Errorf("exit kind = %q, want %q", rt.ExitKind, ExitStopLoss)
	}

	if rt.Entry.IsZero() || rt.Exit.IsZero() || rt.Held <= 0 {
		t.Errorf("times = %v → %v (held %v), want a real holding period",
			rt.Entry, rt.Exit, rt.Held)
	}

	if rt.PnL >= 0 || rt.PnLPct >= 0 {
		t.Errorf("P&L = %v (%.2f%%), want a loss on a stop-out", rt.PnL, rt.PnLPct)
	}

	// The breakdowns must agree with the records they come from.
	if len(res.ByExit) == 0 || res.ByExit[0].Kind != ExitStopLoss {
		t.Errorf("ByExit = %+v, want a stop-loss row", res.ByExit)
	}

	if len(res.BySymbol) != 1 || res.BySymbol[0].Symbol != "TST" {
		t.Errorf("BySymbol = %+v, want one TST row", res.BySymbol)
	}
}

// TestMonthlyReturnsChainToTheTotal reproduces the BT-3 finding: each month was
// measured from its OWN first bar rather than from the previous month's close,
// so the move across every boundary belonged to no month at all and the
// reported months did not compound to the run's actual return.
func TestMonthlyReturnsChainToTheTotal(t *testing.T) {
	t.Parallel()

	const (
		startCash = 100.0
		days      = 90
		daily     = 1.01
	)

	var (
		equity []EquityPoint
		v      = startCash
	)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for d := range days {
		v *= daily
		equity = append(equity, EquityPoint{Time: start.AddDate(0, 0, d), Equity: v})
	}

	rows := monthlyReturns(equity, startCash)
	if len(rows) != 3 {
		t.Fatalf("got %d months, want 3", len(rows))
	}

	chained := 1.0
	for _, m := range rows {
		chained *= 1 + m.ReturnPct/100
	}

	want := equity[len(equity)-1].Equity / startCash

	if math.Abs(chained-want) > 1e-9 {
		t.Errorf("compounded monthly = %+.4f%%, actual = %+.4f%% — the months must chain "+
			"to the whole run, or the boundary moves belong to no month",
			(chained-1)*100, (want-1)*100)
	}

	// Each month must start where the previous one ended.
	for i := 1; i < len(rows); i++ {
		if math.Abs(rows[i].StartEq-rows[i-1].EndEq) > 1e-9 {
			t.Errorf("%s starts at %.4f but %s ended at %.4f",
				rows[i].Month, rows[i].StartEq, rows[i-1].Month, rows[i-1].EndEq)
		}
	}

	if math.Abs(rows[0].StartEq-startCash) > 1e-9 {
		t.Errorf("the first month starts at %.4f, want the run's starting cash %.2f — "+
			"otherwise bar 0's move is the one remaining orphan", rows[0].StartEq, startCash)
	}
}

// With no usable starting cash the first month falls back to its own opening
// equity rather than reporting an infinite return.
func TestMonthlyReturnsWithoutStartCash(t *testing.T) {
	t.Parallel()

	jan := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

	rows := monthlyReturns([]EquityPoint{
		{Time: jan, Equity: 100},
		{Time: jan.AddDate(0, 0, 10), Equity: 110},
	}, 0)

	if len(rows) != 1 {
		t.Fatalf("got %d months, want 1", len(rows))
	}

	if math.Abs(rows[0].ReturnPct-10) > 1e-9 {
		t.Errorf("January = %+v, want +10%% measured from its own open", rows[0])
	}
}
