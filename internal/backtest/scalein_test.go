package backtest

import (
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// TestScaleInAddsOnDrawdown reproduces the "engine.scale_in (averaging down)
// is entirely unmodeled in backtest" finding: a config with scale_in enabled
// used to trade identically here whether it was on or off, since the
// backtest had no code path for it at all — measuring a different
// position-sizing regime than what actually runs live. A position that
// falls step_pct below its average entry must now add, exactly as the live
// engine's checkScaleIn does.
func TestScaleInAddsOnDrawdown(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 8, 100)

	// Open pinned at the prior level (100) so the add fills exactly at its
	// 90 threshold (100*(1-0.10)), not gapped — same convention used
	// throughout newexits_test.go.
	bars[4] = openBar(base.Add(4*time.Minute), 100, 100, 85, 90)

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000, // 10 shares @ 100
		ScaleIn: config.ScaleInConfig{
			Enabled: true, StepPct: 0.10, MaxAdds: 1, SizePct: 1.0,
		},
	}

	res := bt.Run(data(bars))

	var add *struct {
		qty, price float64
	}

	for _, tr := range res.Trades {
		if tr.Side == "buy" && strings.Contains(tr.Reason, "SCALE-IN") {
			add = &struct{ qty, price float64 }{tr.Qty, tr.Price}
		}
	}

	if add == nil {
		t.Fatalf("no scale-in add fired; trades: %+v", res.Trades)
	}

	if add.price != 90 {
		t.Errorf("add filled at %v, want 90 (the 10%% threshold, not the bar's low or close)",
			add.price)
	}

	// SizePct 1.0 of the 1000 OrderSize, at a fill of 90.
	wantQty := 1000.0 / 90.0
	if add.qty < wantQty-0.01 || add.qty > wantQty+0.01 {
		t.Errorf("add qty = %v, want ~%v (SizePct 1.0 of order_size_usd at the fill price)",
			add.qty, wantQty)
	}
}

// The ladder must stop adding once MaxAdds is reached, even though the price
// keeps falling well past every subsequent threshold measured off the newly
// lowered average cost.
func TestScaleInRespectsMaxAdds(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 12, 100)

	bars[4] = openBar(base.Add(4*time.Minute), 100, 100, 85, 90) // first add: threshold 90
	for i := 5; i < 8; i++ {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 90, 90, 90, 90)
	}
	// Deep enough to clear a second rung off ANY plausible new average cost,
	// if the cap were not respected.
	bars[8] = openBar(base.Add(8*time.Minute), 90, 90, 50, 60)
	for i := 9; i < 12; i++ {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 60, 60, 60, 60)
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		ScaleIn: config.ScaleInConfig{
			Enabled: true, StepPct: 0.10, MaxAdds: 1, SizePct: 1.0,
		},
	}

	res := bt.Run(data(bars))

	adds := 0

	for _, tr := range res.Trades {
		if tr.Side == "buy" && strings.Contains(tr.Reason, "SCALE-IN") {
			adds++
		}
	}

	if adds != 1 {
		t.Errorf("scale-in fired %d time(s), want exactly 1 (max_adds: 1): %+v", adds, res.Trades)
	}
}

// require_signal must block an add once the strategy no longer says BUY —
// buyOnce returns HOLD on every bar after its first, so the ladder must stay
// silent despite the price qualifying.
func TestScaleInRequireSignalBlocksWhenSignalIsNotBuy(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 8, 100)
	bars[4] = openBar(base.Add(4*time.Minute), 100, 100, 85, 90)

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		ScaleIn: config.ScaleInConfig{
			Enabled: true, StepPct: 0.10, MaxAdds: 1, SizePct: 1.0, RequireSignal: true,
		},
	}

	res := bt.Run(data(bars))

	for _, tr := range res.Trades {
		if strings.Contains(tr.Reason, "SCALE-IN") {
			t.Fatalf("a scale-in add fired despite require_signal and a non-BUY signal: %+v", tr)
		}
	}
}

// The mirror image: require_signal must ALLOW the add when the strategy is
// still signalling BUY on the qualifying bar, even with pyramiding off (the
// two are unrelated gates — scale_in fires on drawdown, not on a repeated
// BUY signal, but require_signal only asks whether the strategy would still
// enter here).
func TestScaleInRequireSignalAllowsWhenStillBuy(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 8, 100)
	bars[4] = openBar(base.Add(4*time.Minute), 100, 100, 85, 90)

	bt := &Backtester{
		Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		AllowPyramiding: false,
		ScaleIn: config.ScaleInConfig{
			Enabled: true, StepPct: 0.10, MaxAdds: 1, SizePct: 1.0, RequireSignal: true,
		},
	}

	res := bt.Run(data(bars))

	for _, tr := range res.Trades {
		if strings.Contains(tr.Reason, "SCALE-IN") {
			return
		}
	}

	t.Fatalf("no scale-in add fired despite an ongoing BUY signal; trades: %+v", res.Trades)
}

// scale_in must stay a complete no-op when disabled, and existing "unmodeled"
// backtests keep behaving exactly as they always did.
func TestScaleInDisabledDoesNothing(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 8, 100)
	bars[4] = openBar(base.Add(4*time.Minute), 100, 100, 85, 90)

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		// ScaleIn left at its zero value: Enabled defaults false.
	}

	res := bt.Run(data(bars))

	if res.NumTrades != 1 {
		t.Errorf("num trades = %d, want exactly 1 (the initial buy, no scale-in)", res.NumTrades)
	}
}
