package strategy

import (
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// htfSeries builds n bars that fall for the first half and rise sharply in the
// last `rally` bars — so the base timeframe sees an up-move while the higher
// timeframe still sees the larger decline.
func htfSeries(n, rally int) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px := 200.0

	for i := range out {
		if i >= n-rally {
			px *= 1.004 // late rally
		} else {
			px *= 0.9990 // long decline
		}

		out[i] = market.Candle{
			Symbol: "TST", Time: base.Add(time.Duration(i) * time.Minute),
			Open: px, High: px * 1.001, Low: px * 0.999, Close: px, Volume: 100,
		}
	}

	return out
}

// htfConfig is a base config with multi-timeframe confirmation configured.
func htfConfig(multiple int, mode string) config.StrategyConfig {
	cfg := config.Default().Strategy

	cfg.Detectors = []string{"ema_cross"}
	cfg.MinStrength = 0
	cfg.MinStrengthSell = 0
	cfg.TrendGate = false // isolate the higher-timeframe behaviour
	cfg.HTFMultiple = multiple
	cfg.HTFDetectors = []string{"trend"}
	cfg.HTFMode = mode
	cfg.TrendPeriod = 40
	cfg.TrendThreshold = 0.005

	return cfg
}

// TestHTFGateBlocksCounterTrendBuys checks the point of the feature: a base
// timeframe buy inside a larger downtrend is vetoed.
func TestHTFGateBlocksCounterTrendBuys(t *testing.T) {
	t.Parallel()

	candles := htfSeries(1200, 40)

	gated := New(htfConfig(20, "gate")).Evaluate("TST", candles)

	ungated := htfConfig(20, "gate")
	ungated.HTFMultiple = 0 // same settings, confirmation off

	plain := New(ungated).Evaluate("TST", candles)

	if plain.Action != Buy {
		t.Skipf("base timeframe did not produce a BUY to veto (got %s); "+
			"the fixture no longer exercises the gate", plain.Action)
	}

	if gated.Action != Hold {
		t.Errorf("action = %s, want HOLD: a BUY inside a higher-timeframe downtrend "+
			"should be vetoed (%s)", gated.Action, gated.Reason)
	}

	if !strings.Contains(gated.Reason, "blocked by htf") {
		t.Errorf("reason = %q, want it to name the higher-timeframe veto", gated.Reason)
	}
}

// TestHTFVoteAddsAnOpinion checks "vote" mode folds the higher timeframe into
// the tally instead of vetoing, so its call shows up among the detector votes.
func TestHTFVoteAddsAnOpinion(t *testing.T) {
	t.Parallel()

	sig := New(htfConfig(20, "vote")).Evaluate("TST", htfSeries(1200, 40))

	if !strings.Contains(sig.Reason, "htf") {
		t.Errorf("reason = %q, want the htf vote listed among the detectors", sig.Reason)
	}

	if strings.Contains(sig.Reason, "blocked by htf") {
		t.Error("vote mode vetoed a signal; only gate mode may block")
	}
}

// decliningSeries is a monotonic downtrend, so both the base timeframe and
// any higher-timeframe reading agree on SELL.
func decliningSeries(n int) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px := 200.0

	for i := range out {
		px *= 0.999

		out[i] = market.Candle{
			Symbol: "TST", Time: base.Add(time.Duration(i) * time.Minute),
			Open: px, High: px * 1.001, Low: px * 0.999, Close: px, Volume: 100,
		}
	}

	return out
}

// TestHTFVoteAppliesToASplitSellDetectorSet reproduces the
// HTF-vote-dropped-from-sell finding: addHTFVote was only ever applied to
// the buy-side tally. When the sell side uses the SAME detector set
// (sellSame, the common case) it aliases the buy tally and sees the HTF vote
// by accident — but a config with a distinct sell detector set (one line
// away from any profile) builds its own fresh tally that never saw it, so
// htf_mode: vote silently only ever influenced entries, never exits.
func TestHTFVoteAppliesToASplitSellDetectorSet(t *testing.T) {
	t.Parallel()

	cfg := htfConfig(20, "vote")
	cfg.DetectorsSell = []string{"ema_cross"} // forces sellSame=false

	sig := New(cfg).Evaluate("TST", decliningSeries(1200))

	if sig.Action != Sell {
		t.Skipf("fixture did not produce a SELL to check (got %s); "+
			"the fixture no longer exercises the sell path", sig.Action)
	}

	if !strings.Contains(sig.Reason, "htf") {
		t.Errorf("reason = %q, want the htf vote listed among the SELL-side detectors "+
			"(it was silently dropped for any config with a split sell detector set)", sig.Reason)
	}
}

// TestHTFWarmupRequiresAggregatedHistory checks the strategy waits for enough
// base bars to form real higher-timeframe candles, rather than gating on a
// half-formed reading.
func TestHTFWarmupRequiresAggregatedHistory(t *testing.T) {
	t.Parallel()

	cfg := htfConfig(20, "gate")

	withHTF := New(cfg)

	cfg.HTFMultiple = 0
	without := New(cfg)

	if withHTF.WarmupBars() <= without.WarmupBars() {
		t.Errorf("warmup with confirmation (%d) should exceed without it (%d)",
			withHTF.WarmupBars(), without.WarmupBars())
	}

	// Too little history: the strategy says it is warming up rather than trading.
	short := htfSeries(50, 10)
	if sig := withHTF.Evaluate("TST", short); sig.Action != Hold {
		t.Errorf("action = %s on %d bars, want HOLD while warming up", sig.Action, len(short))
	}
}

// TestHTFBaseBarsScalesWithTrendPeriod reproduces "higherTimeframe.BaseBars()
// under-reports": with the default "trend" HTF detector, WarmupBars is only
// trendMinBars (20) regardless of the configured TrendPeriod — sizing
// BaseBars off that alone left it far too small for a large TrendPeriod, so
// a backtest fed only that many base bars gave the HTF trend detector a
// ~20-aggregated-bar micro-trend to read instead of its full configured
// lookback, which live (fed by an ever-growing candle history) never faces.
func TestHTFBaseBarsScalesWithTrendPeriod(t *testing.T) {
	t.Parallel()

	cfg := htfConfig(30, "gate")
	cfg.TrendPeriod = 1200

	h := newHigherTimeframe(cfg)
	if h == nil {
		t.Fatal("newHigherTimeframe returned nil")
	}

	if got, want := h.BaseBars(), (1200+2)*30; got < want {
		t.Errorf("BaseBars() = %d, want >= %d — it must scale with the trend "+
			"detector's configured TrendPeriod (via MaxLookback), not just its "+
			"WarmupBars floor (trendMinBars=20)", got, want)
	}
}

// TestHTFDisabledByDefault checks nothing changes unless it is configured.
func TestHTFDisabledByDefault(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Strategy
	if newHigherTimeframe(cfg) != nil {
		t.Error("multi-timeframe confirmation is on by default; it must be opt-in")
	}

	cfg.HTFMultiple = 1
	if newHigherTimeframe(cfg) != nil {
		t.Error("a multiple of 1 is not a higher timeframe; it must be ignored")
	}

	var nilHTF *higherTimeframe

	if nilHTF.BaseBars() != 0 {
		t.Error("nil higher timeframe reports a warmup")
	}

	if nilHTF.vote(htfSeries(100, 10)).ok {
		t.Error("nil higher timeframe returned a usable vote")
	}
}

// TestResampleEveryAggregates checks the bar-count resampler: complete buckets
// only, anchored to the newest bar, with OHLCV folded correctly.
func TestResampleEveryAggregates(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := make([]market.Candle, 10)

	for i := range in {
		in[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: float64(100 + i), High: float64(110 + i),
			Low: float64(90 + i), Close: float64(105 + i), Volume: 10,
		}
	}

	// 10 bars in groups of 4 => 2 complete buckets; the oldest 2 bars are
	// dropped so every emitted bar aggregates exactly 4.
	out := market.ResampleEvery(in, 4)
	if len(out) != 2 {
		t.Fatalf("got %d aggregated bars, want 2", len(out))
	}

	// First bucket is bars 2..5.
	if out[0].Open != in[2].Open {
		t.Errorf("open = %v, want the first bar of the bucket (%v)", out[0].Open, in[2].Open)
	}

	if out[0].Close != in[5].Close {
		t.Errorf("close = %v, want the last bar of the bucket (%v)", out[0].Close, in[5].Close)
	}

	if out[0].High != in[5].High || out[0].Low != in[2].Low {
		t.Errorf("high/low = %v/%v, want the bucket extremes %v/%v",
			out[0].High, out[0].Low, in[5].High, in[2].Low)
	}

	if out[0].Volume != 40 {
		t.Errorf("volume = %v, want the sum 40", out[0].Volume)
	}

	if !out[0].Time.Equal(in[5].Time) {
		t.Errorf("time = %v, want the bucket's last bar %v (when it became knowable)",
			out[0].Time, in[5].Time)
	}

	// The newest bar must always end a bucket, so the reading never flickers.
	if !out[len(out)-1].Time.Equal(in[len(in)-1].Time) {
		t.Error("the last aggregate does not end on the newest bar")
	}

	if got := market.ResampleEvery(in, 20); got != nil {
		t.Errorf("got %d bars with too little history, want none", len(got))
	}

	if got := market.ResampleEvery(in, 1); len(got) != len(in) {
		t.Error("a multiple of 1 must pass the series through unchanged")
	}
}
