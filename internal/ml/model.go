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

// Skill is how far the model's training accuracy exceeds simply always guessing
// the majority class. At or below zero it has learned nothing worth acting on,
// which is the common case on noisy financial data and must be visible rather
// than hidden behind a probability that looks meaningful.
func (m *Model) Skill() float64 {
	if m == nil {
		return 0
	}

	return m.Accuracy - m.BaseRate
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
}

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

	return m, nil
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
