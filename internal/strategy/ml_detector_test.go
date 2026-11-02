package strategy

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
)

// recordingPredictor is a predictor stand-in that records the candles it was
// last asked to predict on, so a test can inspect exactly what mlDetector
// handed it.
type recordingPredictor struct {
	warmup, horizon int
	lastCandles     []market.Candle
	lastParams      ai.Params
	verdict         ml.Verdict
	ok              bool
}

func (p *recordingPredictor) VerdictWith(_ string, candles []market.Candle, params ai.Params) (ml.Verdict, bool) {
	p.lastCandles = candles
	p.lastParams = params

	return p.verdict, p.ok
}

func (p *recordingPredictor) WarmupBarsFor(ai.Params) int { return p.warmup }
func (p *recordingPredictor) Horizon() int                { return p.horizon }

// TestDetectForDropsTheNewestCandle reproduces the "Predictions computed on
// the in-progress (partial) bar while training used completed bars" finding:
// a live evaluation's series always ends in the current, still-forming bar
// (see engine's barStore.tail), which every ml.BuildSamples training row
// excludes by construction. DetectFor must drop that trailing candle before
// asking the predictor for a verdict, so it evaluates on the same kind of
// feature distribution — always-completed bars — the model was trained on.
func TestDetectForDropsTheNewestCandle(t *testing.T) {
	pred := &recordingPredictor{warmup: 5, horizon: 1, ok: true}
	d := &mlDetector{pred: pred, warmup: 10}

	candles := closesToCandles(risingCloses(12, 100, 1))
	s := toSeries(candles)

	d.DetectFor("TST", s)

	if pred.lastCandles == nil {
		t.Fatal("predictor was never asked for a verdict")
	}

	if got, want := len(pred.lastCandles), len(candles)-1; got != want {
		t.Fatalf("predictor received %d candles, want %d (the newest dropped)", got, want)
	}

	// The dropped candle must be the newest (last) one, not an arbitrary
	// one from the middle or the oldest.
	wantLast := candles[len(candles)-2]
	gotLast := pred.lastCandles[len(pred.lastCandles)-1]

	if gotLast.Close != wantLast.Close {
		t.Errorf("predictor's last candle close = %v, want %v (the second-to-newest "+
			"of the original series — the newest one must be the one dropped)",
			gotLast.Close, wantLast.Close)
	}
}

// TestMLDetectorWarmupAccountsForTheDroppedCandle confirms WarmupBars was
// bumped to compensate for DetectFor's drop: the engine must feed one more
// candle than the predictor's own floor, or the predictor would perpetually
// see one bar short of what it asked for.
func TestMLDetectorWarmupAccountsForTheDroppedCandle(t *testing.T) {
	cfg := config.StrategyConfig{SlowMA: 5, MACDSignal: 2} // indicator floor: 5+2+2=9
	d := newMLDetector(cfg)

	predictorFloor := d.pred.WarmupBarsFor(d.params)
	indicatorFloor := cfg.SlowMA + cfg.MACDSignal + 2
	want := max(predictorFloor, indicatorFloor) + 1

	if d.warmup != want {
		t.Errorf("WarmupBars() = %d, want %d (the higher of the predictor's own floor "+
			"and the indicator floor, plus 1 for the candle DetectFor always drops)",
			d.WarmupBars(), want)
	}
}

// TestDetectForHoldsWithFewerThanWarmupCandles confirms the "not enough
// history" gate is still checked against the (bumped) warmup — a series
// exactly at the predictor's own bare floor, before the +1 bump, must not
// be accepted.
func TestDetectForHoldsWithFewerThanWarmupCandles(t *testing.T) {
	pred := &recordingPredictor{warmup: 5, horizon: 1, ok: true}
	d := &mlDetector{pred: pred, warmup: 10}

	// One short of d.warmup.
	s := toSeries(closesToCandles(risingCloses(9, 100, 1)))

	action, _, reason := d.DetectFor("TST", s)
	if action != Hold {
		t.Errorf("action = %s, want HOLD with fewer than warmup candles", action)
	}

	if pred.lastCandles != nil {
		t.Errorf("predictor was called (%q) despite too little history", reason)
	}
}

// TestDetectForPassesTheBarTimes is the regression for ML2-1 — the CRIT-8
// retrain fix doing nothing in production. The predictor decides when to
// retrain by counting candles whose Time is after its last fit, and every
// production call reaches it through DetectFor, which rebuilt the candles from
// the series' OHLCV columns: every Time was zero, the count was always 0, and
// a fitted model was never refitted. Through DetectFor over 2,500 bars with
// retrain_every 50 it trained once; fed timestamped candles, 51 times. The
// existing tests called Predictor.Verdict directly and could not see it.
func TestDetectForPassesTheBarTimes(t *testing.T) {
	pred := &recordingPredictor{warmup: 5, horizon: 1, ok: true}
	d := &mlDetector{pred: pred, warmup: 10}

	candles := closesToCandles(risingCloses(12, 100, 1))
	base := time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)

	for i := range candles {
		candles[i].Time = base.Add(time.Duration(i) * time.Minute)
	}

	d.DetectFor("TST", toSeries(candles))

	for i, c := range pred.lastCandles {
		if !c.Time.Equal(candles[i].Time) {
			t.Fatalf("candle %d reached the predictor with Time %v, want %v — "+
				"without bar times the retrain schedule never fires", i, c.Time, candles[i].Time)
		}
	}
}

// ML3-5: the detector asked the predictor for a verdict on the periods
// ml.Configure fixed at startup, so after `rsi_period: 30` the rsi detector
// voted on RSI(30) while the model kept reading RSI(14) until a restart —
// and per-symbol overrides and optimizer trials never reached it at all.
// The detector hands its own strategy's periods with every verdict.
func TestDetectForHandsTheStrategysPeriods(t *testing.T) {
	c := config.Default().Strategy
	c.RSIPeriod = 30

	pred := &recordingPredictor{warmup: 5, horizon: 1, ok: true}
	d := newMLDetector(c)
	d.pred, d.warmup = pred, 10

	d.DetectFor("TST", toSeries(closesToCandles(risingCloses(12, 100, 1))))

	if got := pred.lastParams.RSIPeriod; got != 30 {
		t.Errorf("predictor was asked on RSI(%d), want the strategy's RSI(30)", got)
	}
}
