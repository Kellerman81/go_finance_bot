package ml

import (
	"math"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// learnable builds candles whose direction is genuinely predictable from the
// features: price trends up while RSI is low and down while it is high, so a
// classifier has something real to find. Without a learnable fixture, a test of
// "does training work" cannot distinguish a working fitter from a broken one.
func learnable(n int) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px := 100.0

	for i := range out {
		// Alternating regimes of 40 bars: up-trending then down-trending.
		if (i/40)%2 == 0 {
			px *= 1.004
		} else {
			px *= 0.997
		}

		out[i] = market.Candle{
			Symbol: "TST", Time: base.Add(time.Duration(i) * time.Minute),
			Open: px, High: px * 1.002, Low: px * 0.998, Close: px, Volume: 1000,
		}
	}

	return out
}

// TestBuildSamplesLeavesTheUnlabelableTail is the look-ahead guard. The final
// `horizon` bars cannot be labelled — their forward window has not happened — so
// they must produce no rows. A builder that labelled them would be reading data
// that does not exist yet.
func TestBuildSamplesLeavesTheUnlabelableTail(t *testing.T) {
	t.Parallel()

	const (
		bars    = 900
		horizon = 10
	)

	p := ai.Params{}.WithDefaults()
	candles := learnable(bars)

	samples := BuildSamples(candles, p, horizon, 0.002)
	if len(samples) == 0 {
		t.Fatal("no samples built")
	}

	// The last labelable bar is bars-horizon-1; the first is the warmup.
	maxPossible := bars - horizon - 1 - p.WarmupBars() + 1
	if len(samples) > maxPossible {
		t.Errorf("built %d samples, but only %d bars have a complete forward window — "+
			"the tail was labelled from data that does not exist", len(samples), maxPossible)
	}
}

// TestBuildSamplesIgnoresFutureBars proves the input side is causal: extending
// the data with future bars must not change the samples already built for the
// earlier ones.
func TestBuildSamplesIgnoresFutureBars(t *testing.T) {
	t.Parallel()

	p := ai.Params{}.WithDefaults()
	short := learnable(600)
	long := learnable(900) // identical prefix, more history after

	a := BuildSamples(short, p, 10, 0.002)
	b := BuildSamples(long, p, 10, 0.002)

	if len(a) == 0 || len(b) <= len(a) {
		t.Fatalf("fixture problem: %d vs %d samples", len(a), len(b))
	}

	// Every sample the shorter run produced must appear identically in the
	// longer one.
	for i := range a {
		for j := range a[i].X {
			if math.Abs(a[i].X[j]-b[i].X[j]) > 1e-9 {
				t.Fatalf("sample %d feature %d changed when future bars were added: %v vs %v — "+
					"features are not causal", i, j, a[i].X[j], b[i].X[j])
			}
		}

		if a[i].Y != b[i].Y {
			t.Fatalf("sample %d label changed with more history: %v vs %v", i, a[i].Y, b[i].Y)
		}
	}
}

// TestBuildSamplesTooShort checks history below the horizon yields nothing.
func TestBuildSamplesTooShort(t *testing.T) {
	t.Parallel()

	p := ai.Params{}.WithDefaults()

	if s := BuildSamples(learnable(5), p, 10, 0); len(s) != 0 {
		t.Errorf("built %d samples from 5 bars with a 10-bar horizon", len(s))
	}

	if s := BuildSamples(learnable(500), p, 0, 0); len(s) != 0 {
		t.Error("built samples with a zero horizon")
	}
}

// TestTrainLearnsALearnableSignal checks the fitter works: on data where the
// label really is a function of the features, accuracy must beat the base rate.
func TestTrainLearnsALearnableSignal(t *testing.T) {
	t.Parallel()

	p := ai.Params{}.WithDefaults()

	samples := BuildSamples(learnable(1500), p, 10, 0.002)
	if len(samples) < 200 {
		t.Fatalf("only %d samples; fixture too small", len(samples))
	}

	m, err := Train(samples, ai.FeatureNames(), TrainOpts{})
	if err != nil {
		t.Fatal(err)
	}

	if m.Skill() <= 0 {
		t.Errorf("accuracy %.3f does not beat the base rate %.3f on learnable data — "+
			"the fitter is not learning", m.Accuracy, m.BaseRate)
	}

	if len(m.W) != len(ai.FeatureNames()) {
		t.Errorf("weights = %d, want one per feature (%d)", len(m.W), len(ai.FeatureNames()))
	}

	if m.TopFeatures(3) == "" {
		t.Error("TopFeatures returned nothing; the reason string would be empty")
	}
}

// TestPredictIsStandardised checks the scaler travels with the model. Applying
// raw features to standardised weights is a silent, confident error, so the
// model must own the transformation.
func TestPredictIsStandardised(t *testing.T) {
	t.Parallel()

	samples := []Sample{
		{X: []float64{0, 1000}, Y: 0},
		{X: []float64{1, 2000}, Y: 1},
		{X: []float64{0, 1100}, Y: 0},
		{X: []float64{1, 2100}, Y: 1},
	}

	m, err := Train(samples, []string{"a", "b"}, TrainOpts{Epochs: 500})
	if err != nil {
		t.Fatal(err)
	}

	if len(m.Mean) != 2 || len(m.Std) != 2 {
		t.Fatalf("scaler = %v/%v, want two columns", m.Mean, m.Std)
	}

	// The wildly-scaled second column must not swamp the first: after
	// standardisation both weights should be comparable in magnitude.
	if math.Abs(m.W[1]) > 50*math.Abs(m.W[0]) {
		t.Errorf("weights %v are dominated by the large-scale column; standardisation failed",
			m.W)
	}

	lo, ok := m.Predict([]float64{0, 1000})
	if !ok {
		t.Fatal("Predict rejected a well-formed vector")
	}

	hi, _ := m.Predict([]float64{1, 2100})
	if !(lo < hi) {
		t.Errorf("p(low)=%.3f is not below p(high)=%.3f", lo, hi)
	}

	if _, ok := m.Predict([]float64{1}); ok {
		t.Error("Predict accepted a wrong-width vector")
	}
}

// TestTrainRejectsDegenerate checks a one-class window is reported as such rather
// than producing a model that always says the same thing with high confidence.
func TestTrainRejectsDegenerate(t *testing.T) {
	t.Parallel()

	allZero := []Sample{
		{X: []float64{1, 2}, Y: 0},
		{X: []float64{3, 4}, Y: 0},
	}

	if _, err := Train(allZero, nil, TrainOpts{}); err == nil {
		t.Error("trained on a single-class window")
	}

	if _, err := Train(nil, nil, TrainOpts{}); err == nil {
		t.Error("trained on no samples")
	}

	ragged := []Sample{{X: []float64{1, 2}, Y: 0}, {X: []float64{1}, Y: 1}}
	if _, err := Train(ragged, nil, TrainOpts{}); err == nil {
		t.Error("trained on ragged samples")
	}
}

// TestTrainIsDeterministic checks two fits of the same data agree — full-batch
// descent with no shuffling, so a model can be reasoned about.
func TestTrainIsDeterministic(t *testing.T) {
	t.Parallel()

	samples := BuildSamples(learnable(1200), ai.Params{}.WithDefaults(), 10, 0.002)

	a, err := Train(samples, nil, TrainOpts{})
	if err != nil {
		t.Fatal(err)
	}

	b, _ := Train(samples, nil, TrainOpts{})

	for i := range a.W {
		if a.W[i] != b.W[i] {
			t.Fatalf("weight %d differs between fits: %v vs %v", i, a.W[i], b.W[i])
		}
	}
}

// mlCfg is an enabled ML config with a small window so tests run quickly.
func mlCfg() config.MLConfig {
	return config.MLConfig{
		Enabled: true, Horizon: 10, Threshold: 0.002,
		TrainBars: 1200, RetrainEvery: 200, MinSamples: 150,
		BuyProb: 0.55, SellProb: 0.45, MaxStrength: 1,
		Epochs: 200, LearningRate: 0.05, L2: 0.01,
	}
}

// TestPredictorVerdict checks the end-to-end path produces an action with a
// bounded strength and an explanatory reason.
func TestPredictorVerdict(t *testing.T) {
	t.Parallel()

	p := New(mlCfg(), ai.Params{})
	if p == nil {
		t.Fatal("New returned nil for an enabled config")
	}

	v, ok := p.Verdict("TST", learnable(1500))
	if !ok {
		t.Fatal("no verdict on 1500 bars")
	}

	if v.Strength < 0 || v.Strength > 1 {
		t.Errorf("strength = %v, want 0..1", v.Strength)
	}

	if v.Probability < 0 || v.Probability > 1 {
		t.Errorf("probability = %v, want 0..1", v.Probability)
	}

	if v.Reason == "" {
		t.Error("verdict has no reason; the detector would explain nothing")
	}

	if s := p.Stats(); !s.Enabled || s.Symbols != 1 {
		t.Errorf("stats = %+v, want one tracked symbol", s)
	}
}

// TestPredictorRetrainsOnSchedule checks the cadence is measured in bars, so a
// backtest and a live run refit on the same schedule rather than the backtest
// refitting once per bar.
func TestPredictorRetrainsOnSchedule(t *testing.T) {
	t.Parallel()

	cfg := mlCfg()
	cfg.RetrainEvery = 300

	p := New(cfg, ai.Params{})
	candles := learnable(2000)

	p.Verdict("TST", candles[:1500])

	if got := p.Stats().Trained; got != 1 {
		t.Fatalf("trained %d times on the first call, want 1", got)
	}

	// Well inside the cadence: no refit.
	p.Verdict("TST", candles[:1600])

	if got := p.Stats().Trained; got != 1 {
		t.Errorf("trained %d times after 100 more bars, want 1 (cadence is 300)", got)
	}

	// Past the cadence: refit.
	p.Verdict("TST", candles[:1900])

	if got := p.Stats().Trained; got != 2 {
		t.Errorf("trained %d times after 400 more bars, want 2", got)
	}
}

// TestMinSkillGatesWorthlessModels is the guard that matters most. A model with
// no measurable edge must abstain, not emit confident probabilities from noise.
func TestMinSkillGatesWorthlessModels(t *testing.T) {
	t.Parallel()

	cfg := mlCfg()
	cfg.MinSkill = 0.99 // unreachable, so every model is gated

	p := New(cfg, ai.Params{})

	v, ok := p.Verdict("TST", learnable(1500))
	if !ok {
		t.Fatal("expected a verdict explaining the gate")
	}

	if v.Action != Hold || v.Strength != 0 {
		t.Errorf("verdict = %+v, want HOLD with no strength", v)
	}

	if want := "min_skill"; !contains(v.Reason, want) {
		t.Errorf("reason = %q, want it to mention %q", v.Reason, want)
	}
}

// TestPredictorAbstainsWithoutHistory checks too little data yields no verdict
// rather than a model fitted on nothing.
func TestPredictorAbstainsWithoutHistory(t *testing.T) {
	t.Parallel()

	p := New(mlCfg(), ai.Params{})

	if _, ok := p.Verdict("TST", learnable(100)); ok {
		t.Error("produced a verdict from 100 bars")
	}

	if p.Stats().Failures == 0 {
		t.Error("the failed fit was not counted")
	}
}

// TestNilPredictorIsSafe checks the disabled state needs no special case.
func TestNilPredictorIsSafe(t *testing.T) {
	t.Parallel()

	if New(config.MLConfig{Enabled: false}, ai.Params{}) != nil {
		t.Error("a disabled config built a predictor")
	}

	var p *Predictor

	if _, ok := p.Verdict("TST", learnable(500)); ok {
		t.Error("nil predictor returned a verdict")
	}

	if p.WarmupBars() != 0 || p.Horizon() != 0 {
		t.Error("nil predictor reports a warmup or horizon")
	}

	if s := p.Stats(); s.Enabled {
		t.Error("nil predictor reports enabled")
	}

	p.Forget("TST") // must not panic
}

// TestDecideScalesFromTheThreshold checks conviction measures distance past the
// threshold, not the raw probability: 0.56 against a 0.55 threshold is a weak
// lean and must not be reported as 56% conviction.
func TestDecideScalesFromTheThreshold(t *testing.T) {
	t.Parallel()

	p := New(mlCfg(), ai.Params{})
	m := &Model{Names: []string{"a"}, W: []float64{1}, Mean: []float64{0}, Std: []float64{1}}

	weak := p.decide(0.56, m)
	strong := p.decide(0.95, m)

	if weak.Action != Buy || strong.Action != Buy {
		t.Fatalf("actions = %v/%v, want both BUY", weak.Action, strong.Action)
	}

	if !(weak.Strength < 0.2) {
		t.Errorf("a 0.56 probability against a 0.55 threshold gave strength %.2f; too confident",
			weak.Strength)
	}

	if !(strong.Strength > weak.Strength) {
		t.Errorf("strength did not increase with probability: %.2f vs %.2f",
			strong.Strength, weak.Strength)
	}

	if hold := p.decide(0.50, m); hold.Action != Hold || hold.Strength != 0 {
		t.Errorf("a coin-flip probability gave %+v, want HOLD", hold)
	}

	if sell := p.decide(0.10, m); sell.Action != Sell || sell.Strength <= 0 {
		t.Errorf("a low probability gave %+v, want a SELL with conviction", sell)
	}
}

// TestMaxStrengthCaps checks the detector's influence ceiling is honoured.
func TestMaxStrengthCaps(t *testing.T) {
	t.Parallel()

	cfg := mlCfg()
	cfg.MaxStrength = 0.3

	p := New(cfg, ai.Params{})
	m := &Model{Names: []string{"a"}, W: []float64{1}, Mean: []float64{0}, Std: []float64{1}}

	if v := p.decide(1.0, m); v.Strength > 0.3+1e-9 {
		t.Errorf("strength = %v, want it capped at max_strength 0.3", v.Strength)
	}
}

// TestPredictorRetrainsOnAFixedSizeSlidingWindow reproduces CRIT-8: both the
// backtest window and the live bar-store ring hand modelFor a CAPPED-length
// window that slides forward (old bars drop off the front as new ones
// arrive), not a slice that keeps growing. A retrain trigger measured by
// slice length alone never sees length change once the window saturates, so
// it fires exactly once and then never again for the rest of the run. The
// trigger must instead advance with the window's newest timestamp.
func TestPredictorRetrainsOnAFixedSizeSlidingWindow(t *testing.T) {
	t.Parallel()

	cfg := mlCfg()
	cfg.RetrainEvery = 200

	p := New(cfg, ai.Params{})

	const windowLen = 1500

	all := learnable(windowLen + 1000)

	// First fit, on the initial fixed-size window.
	p.Verdict("TST", all[:windowLen])

	if got := p.Stats().Trained; got != 1 {
		t.Fatalf("trained %d times on the first window, want 1", got)
	}

	// Slide the window forward one bar at a time — length never changes, only
	// which bars are in it, exactly like a saturated ring buffer or a
	// saturated backtest lookback window.
	trainedAgain := false

	for i := 1; i <= 1000; i++ {
		p.Verdict("TST", all[i:i+windowLen])

		if p.Stats().Trained >= 2 {
			trainedAgain = true

			break
		}
	}

	if !trainedAgain {
		t.Fatal("the model never retrained again after the window saturated at a fixed length " +
			"— the retrain trigger is stuck measuring a slice length that stopped changing")
	}
}

// TestResetClearsTrainedModels reproduces CRIT-7: the process-wide predictor
// is shared across every backtest/optimizer trial (see ai.Params / ml.Default
// wiring), so without an explicit reset between runs, a later trial silently
// reuses an earlier trial's fitted model regardless of what it's actually
// testing. Backtester.Run calls Reset for exactly this reason.
func TestResetClearsTrainedModels(t *testing.T) {
	t.Parallel()

	p := New(mlCfg(), ai.Params{})

	p.Verdict("TST", learnable(1500))

	if got := p.Stats().Trained; got != 1 {
		t.Fatalf("trained %d times before Reset, want 1", got)
	}

	p.Reset()

	stats := p.Stats()
	if stats.Trained != 0 || stats.Symbols != 0 {
		t.Fatalf("Stats after Reset = %+v, want a clean slate", stats)
	}

	// The next Verdict call must fit a brand new model rather than serving the
	// pre-Reset one — Trained increments from 0 again, not from where it left
	// off, proving the old entry (and its "already trained" bookkeeping) is
	// actually gone rather than just hidden.
	p.Verdict("TST", learnable(1500))

	if got := p.Stats().Trained; got != 1 {
		t.Fatalf("trained %d times after Reset + one Verdict call, want 1 (a fresh fit)", got)
	}
}

// contains is a small substring helper.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}

// TestDecideReadsTheThresholdsAgainstThePositiveRate is the regression for
// ML2-2. The label is one-sided (1 = rose by at least threshold, 0 = anything
// else, flat included), so p(up) for a calibrated model sits near the
// window's positive rate — about 0.06 with the shipped 0.005 threshold. Read
// against the absolute 0.42 sell line that was SELL on almost every bar: on a
// random walk, all 3,301 verdicts. "Unlikely to rally" is not "will fall".
func TestDecideReadsTheThresholdsAgainstThePositiveRate(t *testing.T) {
	t.Parallel()

	p := New(mlCfg(), ai.Params{})
	m := &Model{Samples: 1000, Positives: 64} // positive rate 0.064

	if v := p.decide(0.07, m); v.Action != Hold {
		t.Errorf("p(up)=0.07 at a 0.064 base rate voted %v, want HOLD — it is the base rate, not a fall", v.Action)
	}

	if v := p.decide(0.30, m); v.Action != Buy {
		t.Errorf("p(up)=0.30, nearly 5x the base rate, voted %v, want BUY", v.Action)
	}

	balanced := &Model{Samples: 1000, Positives: 500}
	if v := p.decide(0.30, balanced); v.Action != Sell {
		t.Errorf("p(up)=0.30 on a balanced label voted %v, want SELL (the old reading, unchanged)", v.Action)
	}
}
