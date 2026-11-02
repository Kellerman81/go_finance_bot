package ml

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestParamsForMapsEveryStrategyPeriod guards ML3-5's premise: the model must
// see the readings the strategy's own detectors vote on. A period left out of
// ParamsFor would silently fall back to its default, and a strategy tuned to
// RSI(30) would get a model trained on RSI(14). Donchian's period is the one
// renamed field (RangePeriod) and the easiest to drop.
func TestParamsForMapsEveryStrategyPeriod(t *testing.T) {
	t.Parallel()

	c := config.StrategyConfig{
		FastMA: 7, SlowMA: 31, RSIPeriod: 9, MACDFast: 8, MACDSlow: 21, MACDSignal: 5,
		BBPeriod: 17, BBStdDev: 2.5, ATRPeriod: 11, ADXPeriod: 13, StochKPeriod: 12,
		StochDPeriod: 4, RVOLPeriod: 25, DonchianPeriod: 33, TrendPeriod: 77,
	}

	want := ai.Params{
		FastMA: 7, SlowMA: 31, RSIPeriod: 9, MACDFast: 8, MACDSlow: 21, MACDSignal: 5,
		BBPeriod: 17, BBStdDev: 2.5, ATRPeriod: 11, ADXPeriod: 13, StochKPeriod: 12,
		StochDPeriod: 4, RVOLPeriod: 25, RangePeriod: 33, TrendPeriod: 77,
	}

	if got := ParamsFor(c); got != want {
		t.Errorf("ParamsFor =\n %+v\nwant\n %+v", got, want)
	}

	// A zero strategy gets the feature defaults, not zero periods.
	if got := ParamsFor(config.StrategyConfig{}); got != (ai.Params{}).WithDefaults() {
		t.Errorf("ParamsFor(zero) = %+v, want the defaults", got)
	}
}

// TestWarmupBarsForCoversWindowAndTail guards the ml detector's warmup: it
// needs the feature context, the TrainBars window and the unlabelable
// horizon tail. Under-reporting it lets the strategy leave warmup before a
// model can be trained — every early verdict then abstains — and a nil
// predictor (ml disabled) must need nothing.
func TestWarmupBarsForCoversWindowAndTail(t *testing.T) {
	t.Parallel()

	var nilP *Predictor
	if got := nilP.WarmupBarsFor(ai.Params{}); got != 0 {
		t.Errorf("nil WarmupBarsFor = %d, want 0", got)
	}

	p := New(config.MLConfig{Enabled: true, TrainBars: 500, Horizon: 10}, ai.Params{})

	base := ai.Params{}.WithDefaults()
	got := p.WarmupBarsFor(base)

	if want := featureContext(base) + 500 + 10 + 1; got != want {
		t.Errorf("WarmupBarsFor = %d, want %d", got, want)
	}

	// A longer trend period needs more context.
	longer := base
	longer.TrendPeriod = base.TrendPeriod * 4

	if p.WarmupBarsFor(longer) <= got {
		t.Errorf("WarmupBarsFor did not grow with TrendPeriod %d -> %d", base.TrendPeriod, longer.TrendPeriod)
	}
}

// TestFreshSharesNoModels guards LR2 ML2-3: a backtest trains into Fresh(),
// and a Fresh that shared the live predictor's model map would let a replay
// overwrite the models the live bot is trading on. It must keep the settings
// and start empty.
func TestFreshSharesNoModels(t *testing.T) {
	t.Parallel()

	var nilP *Predictor
	if nilP.Fresh() != nil {
		t.Error("nil.Fresh() is not nil")
	}

	live := New(mlCfg(), ai.Params{})
	if _, ok := live.Verdict("TST", learnable(1500)); !ok {
		t.Fatal("setup: no verdict on a learnable series")
	}

	f := live.Fresh()

	if f.Config() != live.Config() {
		t.Errorf("Fresh config = %+v, want %+v", f.Config(), live.Config())
	}

	if got := f.Stats().Symbols; got != 0 {
		t.Errorf("Fresh has %d models, want 0", got)
	}

	f.Reset()

	if live.Stats().Symbols != 1 {
		t.Error("resetting the fresh predictor dropped the live model")
	}
}

// TestConfigAndDefault guards the process-wide predictor the API and the
// detectors read: Configure installs it with defaults filled in, a disabled
// config installs nil (ml off reads as "no predictor", which every caller
// handles), and a nil predictor's Config is the disabled zero value.
//
// Not parallel: it swaps the package-level default.
func TestConfigAndDefault(t *testing.T) {
	prev := Default()
	t.Cleanup(func() {
		defaultMu.Lock()
		defaultP = prev
		defaultMu.Unlock()
	})

	Configure(config.MLConfig{Enabled: true, Horizon: 7}, ai.Params{})

	p := Default()
	if p == nil {
		t.Fatal("Default is nil after an enabled Configure")
	}

	if c := p.Config(); !c.Enabled || c.Horizon != 7 || c.TrainBars != 1500 {
		t.Errorf("Config = %+v, want enabled, horizon 7, default train_bars 1500", c)
	}

	Configure(config.MLConfig{Enabled: false}, ai.Params{})

	if Default() != nil {
		t.Error("Default is not nil after a disabled Configure")
	}

	var nilP *Predictor
	if c := nilP.Config(); c.Enabled {
		t.Errorf("nil Config = %+v, want the disabled zero value", c)
	}
}
