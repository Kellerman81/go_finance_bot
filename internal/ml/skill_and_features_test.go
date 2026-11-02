package ml

import (
	"math"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// randomWalk is a pure random walk: nothing in it is predictable, so a model
// of it that claims skill is measuring its own overfit.
func randomWalk(n int, vol float64, seed int64) []market.Candle {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // a reproducible fixture, not a secret
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px := 100.0

	for i := range out {
		px *= 1 + r.NormFloat64()*vol
		out[i] = market.Candle{
			Symbol: "RW", Time: base.Add(time.Duration(i) * time.Minute),
			Open: px, High: px * (1 + vol/2), Low: px * (1 - vol/2), Close: px,
			Volume: 1000 + r.Float64()*500,
		}
	}

	return out
}

// rareRallies is a genuinely informative but imbalanced label, the shape the
// shipped 0.5% threshold produces: P(y) = σ(-3 + 1.2·x0), about 7% positives.
func rareRallies(n int, seed int64) []Sample {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // a reproducible fixture, not a secret
	out := make([]Sample, 0, n)

	for range n {
		x := make([]float64, 16)
		for j := range x {
			x[j] = r.NormFloat64()
		}

		y := 0.0
		if r.Float64() < 1/(1+math.Exp(3-1.2*x[0])) {
			y = 1
		}

		out = append(out, Sample{X: x, Y: y})
	}

	return out
}

// ML3-1: Skill was training accuracy at a 0.5 cutoff minus the majority
// rate. With rallies this rare no model outputs p > 0.5, so accuracy WAS the
// base rate and skill exactly 0 — the shipped min_skill 0.02 held every
// verdict at HOLD, even for this model, whose BUY votes hit about three times
// the base rate. Skill must measure the votes the predictor casts.
func TestSkillSeesAnInformativeRareRallyModel(t *testing.T) {
	t.Parallel()

	m, err := Train(rareRallies(1400, 11), ai.FeatureNames(), TrainOpts{})
	if err != nil {
		t.Fatal(err)
	}

	if got := m.Skill(); got < 0.02 {
		t.Errorf("Skill = %.4f on a model that really predicts rallies (votes %d, hit %.3f vs chance %.3f); "+
			"the shipped min_skill 0.02 would silence it", got, m.Votes, m.VoteHitRate, m.VoteChance)
	}
}

// The other half of ML3-1: the votes are, by construction, the rows the
// model fits best, so scored on its own training rows a model of a pure
// random walk read a skill of 0.20 — ten times min_skill, and noise would
// have voted. Skill is scored on a held-out tail, embargoed by the horizon.
func TestSkillOfARandomWalkModelIsNotAboveMinSkill(t *testing.T) {
	t.Parallel()

	for seed := int64(1); seed <= 4; seed++ {
		samples := BuildSamples(randomWalk(1600, 0.001, seed), ai.Params{}.WithDefaults(), 10, 0.005)

		m, err := Train(samples, ai.FeatureNames(), TrainOpts{Embargo: 10})
		if err != nil {
			t.Fatal(err)
		}

		if got := m.Skill(); got >= 0.02 {
			t.Errorf("seed %d: a random-walk model reads skill %.4f (votes %d, hit %.3f vs chance %.3f): "+
				"min_skill would let noise vote", seed, got, m.Votes, m.VoteHitRate, m.VoteChance)
		}
	}
}

// ML3-2: from an intercept of 0, 300 epochs at lr 0.05 stopped near -2.2
// where a 5% rally rate needs -2.9, so p(up) averaged about twice the rate
// decide centres its lines on and noise voted BUY ~4× as often as a
// converged fit. Starting at logit(rate) the default fit is calibrated.
func TestTheDefaultFitIsCalibratedToTheRallyRate(t *testing.T) {
	t.Parallel()

	samples := BuildSamples(randomWalk(1500, 0.001, 7), ai.Params{}.WithDefaults(), 10, 0.005)

	m, err := Train(samples, ai.FeatureNames(), TrainOpts{})
	if err != nil {
		t.Fatal(err)
	}

	sum := 0.0

	for _, s := range samples {
		p, _ := m.Predict(s.X)
		sum += p
	}

	mean := sum / float64(len(samples))
	rate := float64(m.Positives) / float64(m.Samples)

	if mean > 1.25*rate || mean < 0.75*rate {
		t.Errorf("mean p(up) %.4f vs rally rate %.4f: the 300-epoch fit is not calibrated, "+
			"so the vote lines around the rate read a biased probability", mean, rate)
	}
}

// ML3-5: the predictor's feature periods were fixed at ml.Configure. A model
// asked with different periods — a strategy edit, a per-symbol override, an
// optimizer trial — kept scoring the features of the periods it was trained
// on. A change of periods must refit the symbol.
func TestChangedFeaturePeriodsRefitTheModel(t *testing.T) {
	t.Parallel()

	p := New(mlCfg(), ai.Params{})
	candles := learnable(1500)

	base := ai.Params{}.WithDefaults()
	wider := base
	wider.RSIPeriod = 30

	if _, ok := p.VerdictWith("TST", candles, base); !ok {
		t.Fatal("setup: no verdict on the base periods")
	}

	if _, ok := p.VerdictWith("TST", candles, wider); !ok {
		t.Fatal("no verdict on the wider periods")
	}

	if got := p.Stats().Trained; got != 2 {
		t.Errorf("Trained = %d after the RSI period changed, want 2: the model fitted on RSI(14) "+
			"was asked to score RSI(30) features", got)
	}
}

// ML3-6: the training rows' features were built from the TrainBars window
// alone, so its first ~120 rows saw a trend regression over fewer bars than
// TrendPeriod — readings no live prediction, which sees the whole history,
// ever produces (trend_r2 off by up to 0.47). The rows are still the last
// TrainBars, but their trend features must match the full history's.
func TestTrainingRowsReadTheHistoryBeforeTheWindow(t *testing.T) {
	t.Parallel()

	cfg := withDefaults(mlCfg())
	cfg.TrainBars = 1500
	p := New(cfg, ai.Params{})
	params := ai.Params{}.WithDefaults()
	candles := randomWalk(3000, 0.001, 5)

	m, err := p.train(candles, params)
	if err != nil {
		t.Fatal(err)
	}

	want := buildSamples(candles, len(candles)-cfg.TrainBars, params, cfg.Horizon, cfg.Threshold)
	if m.Samples != len(want) {
		t.Fatalf("trained on %d rows, want %d (the last train_bars, horizon excluded)", m.Samples, len(want))
	}

	names := ai.FeatureNames()

	for _, name := range []string{"trend_pct", "trend_r2"} {
		j := slices.Index(names, name)

		sum := 0.0
		for _, s := range want {
			sum += s.X[j]
		}

		if got, exp := m.Mean[j], sum/float64(len(want)); math.Abs(got-exp) > 1e-9 {
			t.Errorf("%s mean over the training rows = %.6f, full history reads %.6f: "+
				"early rows saw a truncated trend window", name, got, exp)
		}
	}
}

// ML3-7: ml.max_strength outside 0..1 ran silently as full strength — the
// AI2-4 defect, fixed for ai and never for ml. It is refused at load now.
func TestMLMaxStrengthIsRangeChecked(t *testing.T) {
	t.Parallel()

	for _, v := range []float64{-0.5, 5} {
		if err := (config.MLConfig{MaxStrength: v}).Validate(); err == nil {
			t.Errorf("max_strength %v was accepted; it would run as 1", v)
		}
	}

	if err := (config.MLConfig{MaxStrength: 0}).Validate(); err != nil {
		t.Errorf("max_strength 0 (= the default) was refused: %v", err)
	}
}
