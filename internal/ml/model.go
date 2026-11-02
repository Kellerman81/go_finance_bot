// Package ml turns the indicator snapshot into a learned prediction: a
// logistic-regression classifier, retrained on a rolling window, that estimates
// the probability the next N bars are up.
//
// Why regression rather than something fancier: with ~16 features and a few
// thousand noisy samples, the bottleneck is the signal in the data, not model
// capacity. A gradient-boosted forest fits this data better and generalises no
// better — it just overfits with more confidence. Logistic regression is also
// inspectable: the per-feature weights say *why* it leans, which matters when the
// alternative is an unexplainable number driving real orders.
//
// The label is the honest part and the easy part to get wrong. See BuildSamples.
package ml

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// Model is a fitted classifier plus the standardisation it was fitted under.
//
// The scaler is part of the model, not a preprocessing step: the features have
// wildly different scales (RSI spans 0–100, returns span ±2, bb_pos spans 0–1),
// so a model applied to unstandardised input — or standardised against different
// statistics than it was trained on — produces confident nonsense.
type Model struct {
	Names []string  `json:"names"`
	W     []float64 `json:"w"`
	B     float64   `json:"b"`

	Mean []float64 `json:"mean"`
	Std  []float64 `json:"std"`

	TrainedAt time.Time `json:"trained_at"`
	Samples   int       `json:"samples"`
	Positives int       `json:"positives"`
	// Accuracy and BaseRate are measured on the training data, so they describe
	// fit rather than skill. Accuracy at or below BaseRate means the model
	// learned nothing beyond "guess the majority class".
	Accuracy float64 `json:"accuracy"`
	BaseRate float64 `json:"base_rate"`
	LogLoss  float64 `json:"log_loss"`

	// Votes, VoteHitRate and VoteChance score the model the way the predictor
	// USES it, out of sample (ML3-1): a twin fitted on the older part of the
	// window votes on the newest holdoutFraction of it, at the cutoffs the
	// predictor trades on (see Cutoffs). Votes is how many holdout rows it
	// voted on, VoteHitRate the share of those votes that were right, and
	// VoteChance the share a vote on the same side would get right by chance
	// — the holdout's positive rate for a BUY, its complement for a SELL.
	// Accuracy above, scored at 0.5, cannot see a model whose every
	// probability sits below 0.5, which is every model of a rare-rally label.
	Votes       int     `json:"votes"`
	VoteHitRate float64 `json:"vote_hit_rate"`
	VoteChance  float64 `json:"vote_chance"`
}

// Sample is one training row: a standardisable feature vector and its label.
type Sample struct {
	X []float64
	Y float64 // 1 = the forward window was up enough, 0 = it was not
}

// Predict returns the modelled probability that the forward window is up.
func (m *Model) Predict(x []float64) (float64, bool) {
	if m == nil || len(x) != len(m.W) {
		return 0, false
	}

	z := m.B

	for i, v := range x {
		if m.Std[i] > 0 {
			z += m.W[i] * (v - m.Mean[i]) / m.Std[i]
		}
	}

	return sigmoid(z), true
}

// minSkillVotes is the fewest training votes a skill is measured on. A model
// that clears its cutoff on a handful of rows reads a hit rate from luck: 3
// right out of 4 is a 75% hit rate and says nothing.
const minSkillVotes = 20

// Skill is how far the model's votes beat a vote cast by chance: VoteHitRate
// minus VoteChance, on held-out rows, at the cutoffs the predictor actually
// trades on. At or below zero it has learned nothing worth acting on, which
// is the common case on noisy financial data and must be visible rather than
// hidden behind a probability that looks meaningful.
//
// Out of sample because the votes are, by construction, the rows the model
// fits best: scored on its own training rows a model of a pure random walk
// read a skill of 0.20, ten times the shipped min_skill.
//
// It used to be training accuracy at a 0.5 cutoff minus the majority-class
// rate. With the shipped label (a 0.5% rally, ~6% of rows) no model outputs
// p > 0.5, so accuracy WAS the base rate, skill was exactly 0 and the shipped
// min_skill 0.02 held every verdict at HOLD — a model whose BUYs hit 2.9× the
// base rate included (ML3-1).
func (m *Model) Skill() float64 {
	if m == nil || m.Votes < minSkillVotes {
		return 0
	}

	return m.VoteHitRate - m.VoteChance
}

// Cutoffs returns the BUY and SELL probability lines for a training positive
// rate of base: buyProb/sellProb are distances from 0.5 applied around base
// (see Predictor.decide). One function, so the votes Train scores are the
// votes the predictor casts.
func Cutoffs(base, buyProb, sellProb float64) (buyAt, sellAt float64) {
	return base + (buyProb - 0.5), base - (0.5 - sellProb)
}

// TopFeatures returns the n most influential features by absolute weight, with
// their signs, for the detector's reason string. Because the inputs are
// standardised, the weights are directly comparable.
func (m *Model) TopFeatures(n int) string {
	if m == nil || len(m.W) == 0 {
		return ""
	}

	type wf struct {
		name string
		w    float64
	}

	all := make([]wf, 0, len(m.W))
	for i, w := range m.W {
		name := fmt.Sprintf("f%d", i)
		if i < len(m.Names) {
			name = m.Names[i]
		}

		all = append(all, wf{name, w})
	}

	slices.SortFunc(all, func(a, b wf) int {
		switch {
		case math.Abs(b.w) > math.Abs(a.w):
			return 1
		case math.Abs(b.w) < math.Abs(a.w):
			return -1
		}

		return 0
	})

	if n > len(all) {
		n = len(all)
	}

	parts := make([]string, 0, n)
	for _, f := range all[:n] {
		parts = append(parts, fmt.Sprintf("%s%+.2f", f.name, f.w))
	}

	return strings.Join(parts, " ")
}

// TrainOpts are the fitting hyperparameters.
type TrainOpts struct {
	Epochs       int     // full passes over the training set
	LearningRate float64 // gradient step size
	L2           float64 // ridge penalty, the main defence against overfitting

	// BuyProb / SellProb are the predictor's vote thresholds (see Cutoffs),
	// so Skill is measured on the votes it will cast. 0 = the config
	// defaults.
	BuyProb  float64
	SellProb float64
	// Embargo is how many rows between the skill twin's fit and its holdout
	// are dropped: the label horizon, since a row's label reads closes up to
	// that many rows ahead and would otherwise leak the holdout into the fit.
	Embargo int

	// holdout is set on the twin's own fit so it does not score itself.
	holdout bool
}

// holdoutFraction is the newest share of the window the skill twin is
// scored on.
const holdoutFraction = 0.25

// withDefaults fills in unset hyperparameters with values that converge on this
// problem without tuning.
func (o TrainOpts) withDefaults() TrainOpts {
	if o.Epochs <= 0 {
		o.Epochs = 300
	}

	if o.LearningRate <= 0 {
		o.LearningRate = 0.05
	}

	if o.L2 <= 0 {
		o.L2 = 0.01
	}

	if o.BuyProb <= 0 {
		o.BuyProb = config.DefaultMLBuyProb
	}

	if o.SellProb <= 0 {
		o.SellProb = config.DefaultMLSellProb
	}

	return o
}

// ErrDegenerate is returned when the training set has only one class. It is not
// a failure to report loudly: a window in which price never rose by the label
// threshold is a perfectly normal quiet stretch, and the caller simply has no
// model for it yet.
var ErrDegenerate = errors.New("ml: training window has only one class")

// Train fits a model by full-batch gradient descent with an L2 penalty.
//
// Full batch rather than stochastic: the sample counts here are in the
// thousands, so a batch pass is microseconds and determinism is worth more than
// convergence speed. A model that differs run to run cannot be reasoned about.
//
//nolint:cyclop // validate → standardise → fit → score, one linear pipeline
func Train(samples []Sample, names []string, opts TrainOpts) (*Model, error) {
	if len(samples) == 0 {
		return nil, errors.New("ml: no samples")
	}

	dim := len(samples[0].X)
	if dim == 0 {
		return nil, errors.New("ml: zero-width feature vector")
	}

	opts = opts.withDefaults()

	positives := 0

	for _, s := range samples {
		if len(s.X) != dim {
			return nil, fmt.Errorf("ml: ragged samples (%d vs %d features)", len(s.X), dim)
		}

		if s.Y > 0.5 {
			positives++
		}
	}

	if positives == 0 || positives == len(samples) {
		return nil, ErrDegenerate
	}

	mean, std := standardise(samples, dim)

	m := &Model{
		Names: names, W: make([]float64, dim), Mean: mean, Std: std,
		TrainedAt: time.Now(), Samples: len(samples), Positives: positives,
	}

	// Pre-standardise once rather than inside every epoch. The rows are carved
	// from one backing array: a training window is thousands of samples, and a
	// separate small allocation per row costs both the allocations and the cache
	// locality of the epoch loop that then walks them all repeatedly.
	xs := make([][]float64, len(samples))
	flat := make([]float64, len(samples)*dim)

	for i, s := range samples {
		row := flat[i*dim : (i+1)*dim : (i+1)*dim]
		for j, v := range s.X {
			if std[j] > 0 {
				row[j] = (v - mean[j]) / std[j]
			}
		}

		xs[i] = row
	}

	n := float64(len(samples))
	gradW := make([]float64, dim)
	rate := float64(positives) / n

	// Start the intercept at logit(positive rate), where a model with no
	// weights is already calibrated. From 0 it had to walk to logit(0.05) ≈
	// -2.9 along a gradient of curvature p(1-p) ≈ 0.05, and 300 epochs at
	// lr 0.05 stopped near -2.2: p(up) sat about twice the rate decide
	// centres on, and noise voted BUY ~4× as often as a converged fit (ML3-2).
	m.B = math.Log(rate / (1 - rate))

	for range opts.Epochs {
		clear(gradW)

		gradB := 0.0

		for i, x := range xs {
			err := sigmoid(dot(m.W, x)+m.B) - samples[i].Y
			for j, v := range x {
				gradW[j] += err * v
			}

			gradB += err
		}

		for j := range m.W {
			// The L2 term shrinks weights toward zero, which on this data is
			// mostly what keeps the model from memorising the window.
			m.W[j] -= opts.LearningRate * (gradW[j]/n + opts.L2*m.W[j])
		}

		m.B -= opts.LearningRate * gradB / n
	}

	m.Accuracy, m.LogLoss = evaluate(m, xs, samples)
	m.BaseRate = math.Max(float64(positives), n-float64(positives)) / n

	if !opts.holdout {
		scoreHoldout(m, samples, names, opts)
	}

	return m, nil
}

// scoreHoldout fills m's vote fields from a twin fitted on the older rows and
// voting on the newest holdoutFraction, the embargo apart. A window too short
// to hold minSkillVotes rows out, or a one-class older part, leaves them zero:
// Skill then reads 0 and min_skill holds the model at HOLD, which is the safe
// answer to "could not measure".
func scoreHoldout(m *Model, samples []Sample, names []string, opts TrainOpts) {
	cut := len(samples) - int(float64(len(samples))*holdoutFraction)
	from := cut + max(opts.Embargo, 0)

	if len(samples)-from < minSkillVotes {
		return
	}

	twinOpts := opts

	twinOpts.holdout = true

	twin, err := Train(samples[:cut], names, twinOpts)
	if err != nil {
		return
	}

	held := samples[from:]
	rate := float64(twin.Positives) / float64(twin.Samples)
	buyAt, sellAt := Cutoffs(rate, opts.BuyProb, opts.SellProb)

	up := 0

	for _, s := range held {
		if s.Y > 0.5 {
			up++
		}
	}

	heldRate := float64(up) / float64(len(held))

	var votes, hits int

	chance := 0.0

	for _, s := range held {
		p, _ := twin.Predict(s.X)

		hit, chanceOfHit, voted := vote(p, s.Y > 0.5, buyAt, sellAt, heldRate)
		if !voted {
			continue
		}

		votes++

		chance += chanceOfHit

		if hit {
			hits++
		}
	}

	m.Votes = votes
	if votes > 0 {
		m.VoteHitRate = float64(hits) / float64(votes)
		m.VoteChance = chance / float64(votes)
	}
}

// vote is the predictor's vote on one held-out row at probability p: whether
// it voted, whether it was right, and the chance a vote on that side is right
// at the holdout's rally rate.
func vote(
	p float64,
	rallied bool,
	buyAt, sellAt, rate float64,
) (hit bool, chance float64, voted bool) {
	switch {
	case p >= buyAt && buyAt < 1:
		return rallied, rate, true
	case p <= sellAt && sellAt > 0:
		return !rallied, 1 - rate, true
	}

	return false, 0, false
}

// evaluate scores the fitted model on its own training rows.
func evaluate(m *Model, xs [][]float64, samples []Sample) (accuracy, logLoss float64) {
	const eps = 1e-12

	correct := 0
	loss := 0.0

	for i, x := range xs {
		p := sigmoid(dot(m.W, x) + m.B)
		if (p >= 0.5) == (samples[i].Y > 0.5) {
			correct++
		}

		if samples[i].Y > 0.5 {
			loss -= math.Log(math.Max(p, eps))
		} else {
			loss -= math.Log(math.Max(1-p, eps))
		}
	}

	n := float64(len(samples))

	return float64(correct) / n, loss / n
}

// standardise computes each column's mean and standard deviation. A column with
// no variation gets a zero deviation, which Predict treats as "ignore this
// feature" rather than dividing by zero.
func standardise(samples []Sample, dim int) (mean, std []float64) {
	mean = make([]float64, dim)
	std = make([]float64, dim)

	n := float64(len(samples))

	for _, s := range samples {
		for j, v := range s.X {
			mean[j] += v
		}
	}

	for j := range mean {
		mean[j] /= n
	}

	for _, s := range samples {
		for j, v := range s.X {
			d := v - mean[j]

			std[j] += d * d
		}
	}

	for j := range std {
		std[j] = math.Sqrt(std[j] / n)
		if std[j] < 1e-12 {
			std[j] = 0
		}
	}

	return mean, std
}

// dot returns the inner product of two equal-length vectors.
func dot(a, b []float64) float64 {
	var sum float64

	for i := range a {
		sum += a[i] * b[i]
	}

	return sum
}

// sigmoid maps a score to a probability, clamped to avoid overflow at the tails.
func sigmoid(z float64) float64 {
	switch {
	case z > 30:
		return 1
	case z < -30:
		return 0
	}

	return 1 / (1 + math.Exp(-z))
}
