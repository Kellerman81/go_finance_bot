package ml

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// Action mirrors the strategy's three outcomes without importing it.
type Action string

const (
	Hold Action = "HOLD"
	Buy  Action = "BUY"
	Sell Action = "SELL"
)

// Verdict is one prediction.
type Verdict struct {
	Action      Action  `json:"action"`
	Probability float64 `json:"probability"` // modelled P(up over the horizon)
	Strength    float64 `json:"strength"`    // 0..1 conviction after gating
	Reason      string  `json:"reason"`
}

// Predictor holds one model per symbol and retrains each on a schedule.
//
// The retraining discipline is the whole design. A model is fitted on a rolling
// window that ends `horizon` bars before the present, because rows nearer than
// that have no realised label yet. It then predicts the current bar. That makes
// every prediction a genuine out-of-sample one — the walk-forward arrangement,
// applied continuously — which is the only way an ML signal in a backtest means
// anything about live behaviour.
//
// A nil Predictor is valid and always abstains, so callers need no special case.
type Predictor struct {
	cfg    config.MLConfig
	params ai.Params

	mu     sync.Mutex
	models map[string]*entry
	stats  Stats
}

// entry is one symbol's model plus its retraining bookkeeping.
type entry struct {
	model *Model
	// trainedAt is the newest candle's Time when the model was last (attempted
	// to be) fitted. The retrain trigger counts candles newer than this,
	// rather than the window's slice length, because both the backtest window
	// and the live bar-store ring are capped — length alone stops advancing
	// once either saturates, and a retrain trigger measured that way silently
	// stops firing forever (see CODE_REVIEW_FINDINGS.md CRIT-8). Counting by
	// timestamp instead keeps working identically live and in a backtest no
	// matter how long the window has been running, and needs no assumption
	// about exactly one new bar arriving between calls.
	trainedAt time.Time
	failed    bool // the last attempt could not fit (e.g. one-class window)
}

// Stats reports the predictor's state for the API.
type Stats struct {
	Enabled  bool    `json:"enabled"`
	Symbols  int     `json:"symbols"`
	Trained  int64   `json:"trained"`
	Failures int64   `json:"failures"`
	Horizon  int     `json:"horizon"`
	AvgSkill float64 `json:"avg_skill"` // mean (accuracy − base rate) across models
}

// ParamsFor maps a strategy's indicator periods onto the feature parameters, so
// the model sees exactly the readings the rule-based detectors vote on rather
// than a second, differently-tuned set.
func ParamsFor(c config.StrategyConfig) ai.Params {
	return ai.Params{
		FastMA:       c.FastMA,
		SlowMA:       c.SlowMA,
		RSIPeriod:    c.RSIPeriod,
		MACDFast:     c.MACDFast,
		MACDSlow:     c.MACDSlow,
		MACDSignal:   c.MACDSignal,
		BBPeriod:     c.BBPeriod,
		BBStdDev:     c.BBStdDev,
		ATRPeriod:    c.ATRPeriod,
		ADXPeriod:    c.ADXPeriod,
		StochKPeriod: c.StochKPeriod,
		StochDPeriod: c.StochDPeriod,
		RVOLPeriod:   c.RVOLPeriod,
		RangePeriod:  c.DonchianPeriod,
		TrendPeriod:  c.TrendPeriod,
	}.WithDefaults()
}

// New builds a Predictor, or nil when ML is disabled.
func New(cfg config.MLConfig, params ai.Params) *Predictor {
	if !cfg.Enabled {
		return nil
	}

	return &Predictor{
		cfg:    withDefaults(cfg),
		params: params.WithDefaults(),
		models: make(map[string]*entry),
	}
}

// withDefaults fills in unset ML settings.
func withDefaults(c config.MLConfig) config.MLConfig {
	if c.Horizon <= 0 {
		c.Horizon = 10
	}

	if c.TrainBars <= 0 {
		c.TrainBars = 1500
	}

	if c.RetrainEvery <= 0 {
		c.RetrainEvery = 250
	}

	if c.MinSamples <= 0 {
		c.MinSamples = 200
	}

	if c.BuyProb <= 0 {
		c.BuyProb = 0.58
	}

	if c.SellProb <= 0 {
		c.SellProb = 0.42
	}

	if c.MaxStrength <= 0 || c.MaxStrength > 1 {
		c.MaxStrength = 1
	}

	return c
}

// Verdict returns the model's read on symbol given its candle history, retraining
// first if due. ok is false when there is no usable model — the caller must then
// abstain rather than guess.
func (p *Predictor) Verdict(symbol string, candles []market.Candle) (Verdict, bool) {
	if p == nil || len(candles) == 0 {
		return Verdict{}, false
	}

	model, ok := p.modelFor(symbol, candles)
	if !ok {
		return Verdict{}, false
	}

	// A model with no measurable skill is worse than none: it produces
	// confident-looking probabilities from noise. Report the fact instead.
	if p.cfg.MinSkill > 0 && model.Skill() < p.cfg.MinSkill {
		return Verdict{
			Action: Hold,
			Reason: fmt.Sprintf("ml: skill %.3f below min_skill %.3f (acc %.3f vs base %.3f)",
				model.Skill(), p.cfg.MinSkill, model.Accuracy, model.BaseRate),
		}, true
	}

	f, ok := FeaturesAt(candles, symbol, p.params)
	if !ok {
		return Verdict{}, false
	}

	prob, ok := model.Predict(f.Vector())
	if !ok {
		return Verdict{}, false
	}

	return p.decide(prob, model), true
}

// decide maps a probability onto an action and a conviction.
//
// The strength scales with how far the probability sits past its threshold, not
// with the probability itself: 0.60 against a 0.58 threshold is a weak lean, and
// calling it 60% conviction would overstate a coin flip.
func (p *Predictor) decide(prob float64, m *Model) Verdict {
	v := Verdict{Probability: prob, Action: Hold}

	switch {
	case prob >= p.cfg.BuyProb:
		span := 1 - p.cfg.BuyProb
		if span <= 0 {
			span = 1
		}

		v.Action = Buy
		v.Strength = clamp01((prob-p.cfg.BuyProb)/span) * p.cfg.MaxStrength

	case prob <= p.cfg.SellProb:
		span := p.cfg.SellProb
		if span <= 0 {
			span = 1
		}

		v.Action = Sell
		v.Strength = clamp01((p.cfg.SellProb-prob)/span) * p.cfg.MaxStrength
	}

	v.Reason = fmt.Sprintf("ml: p(up %d bars)=%.3f %s (acc %.2f vs base %.2f, n=%d) [%s]",
		p.cfg.Horizon, prob, v.Action, m.Accuracy, m.BaseRate, m.Samples, m.TopFeatures(3))

	return v
}

// modelFor returns symbol's model, retraining when the schedule says so.
func (p *Predictor) modelFor(symbol string, candles []market.Candle) (*Model, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, seen := p.models[symbol]
	if !seen {
		e = &entry{}
		p.models[symbol] = e
	}

	// Count candles newer than the model's training cutoff (binary search: both
	// the backtest window and the live ring are time-ordered) rather than
	// diffing slice lengths — see the entry.trainedAt doc comment.
	sinceTrain := len(candles) - sort.Search(len(candles), func(i int) bool {
		return candles[i].Time.After(e.trainedAt)
	})

	// Retrain on the first sight of a symbol and every RetrainEvery bars after.
	// Measuring cadence in bars (not wall clock) keeps a backtest and a live
	// run on the same schedule, where a wall-clock timer would retrain
	// thousands of times in a backtest and once an hour live.
	due := e.model == nil || sinceTrain >= p.cfg.RetrainEvery
	if !due {
		return e.model, e.model != nil
	}

	// A failed fit backs off by the same cadence rather than retrying per bar.
	if e.failed && sinceTrain < p.cfg.RetrainEvery {
		return e.model, e.model != nil
	}

	model, err := p.train(candles)

	e.trainedAt = candles[len(candles)-1].Time

	if err != nil {
		e.failed = true
		p.stats.Failures++

		// A one-class window is ordinary and not worth logging; anything else is.
		if !errors.Is(err, ErrDegenerate) && !errors.Is(err, errTooFewSamples) {
			errlog.Recordf("ml", "%s: train: %v", symbol, err)
		}

		return e.model, e.model != nil
	}

	e.model, e.failed = model, false
	p.stats.Trained++

	return model, true
}

// errTooFewSamples means the window did not yield enough labelled rows yet.
var errTooFewSamples = errors.New("ml: too few samples")

// train fits a model on the most recent TrainBars of history.
//
// Note what is *not* here: the whole candle slice is passed to BuildSamples,
// which itself refuses to label the final `horizon` bars. So the newest rows the
// model sees end well before the bar it will predict, and no future information
// reaches the fit.
func (p *Predictor) train(candles []market.Candle) (*Model, error) {
	window := candles
	if len(window) > p.cfg.TrainBars {
		window = window[len(window)-p.cfg.TrainBars:]
	}

	samples := BuildSamples(window, p.params, p.cfg.Horizon, p.cfg.Threshold)
	if len(samples) < p.cfg.MinSamples {
		return nil, fmt.Errorf("%w: %d < %d", errTooFewSamples, len(samples), p.cfg.MinSamples)
	}

	return Train(samples, ai.FeatureNames(), TrainOpts{
		Epochs:       p.cfg.Epochs,
		LearningRate: p.cfg.LearningRate,
		L2:           p.cfg.L2,
	})
}

// WarmupBars is the history needed before a model can be fitted at all: a full
// TrainBars-sized training window's worth of rows plus the unlabelable tail.
//
// This must scale with TrainBars, not MinSamples: train (below) fits on the
// most recent TrainBars candles it's handed, but a caller sizing its window
// from MinSamples instead (as this did previously) hands it far fewer than
// TrainBars whenever MinSamples < TrainBars — the configured default is
// MinSamples=200 vs TrainBars=1500, so every backtest fit a model on ~7x less
// data than train_bars asked for, silently, with no way to observe it from
// the backtest's own output (see CODE_REVIEW_FINDINGS.md CRIT-9).
func (p *Predictor) WarmupBars() int {
	if p == nil {
		return 0
	}

	return p.params.WarmupBars() + p.cfg.TrainBars + p.cfg.Horizon + 1
}

// Horizon is the forward window the model predicts over.
func (p *Predictor) Horizon() int {
	if p == nil {
		return 0
	}

	return p.cfg.Horizon
}

// Reset clears every trained model and retraining bookkeeping, so the next
// Verdict for any symbol starts from a blank slate.
//
// A backtest or optimizer trial that reuses the process-wide predictor (see
// Default) without this shares trained models across trials: the first
// trial fits a model, and because both the backtest window and the live
// bar-store ring are capped, the retrain trigger's bar-count never advances
// far enough again to re-fire — every later trial silently reuses trial
// one's model regardless of what strategy/indicator parameters it's
// actually testing. Call this at the start of every independent replay (see
// Backtester.Run). Live trading never calls it — ml.Configure is called once
// at startup and the model legitimately persists for the process lifetime.
func (p *Predictor) Reset() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.models = make(map[string]*entry)
	p.stats = Stats{}
}

// Forget drops a symbol's model (called when it leaves the watchlist).
func (p *Predictor) Forget(symbol string) {
	if p == nil {
		return
	}

	p.mu.Lock()

	delete(p.models, symbol)
	p.mu.Unlock()
}

// Stats returns a snapshot of the predictor's state.
func (p *Predictor) Stats() Stats {
	if p == nil {
		return Stats{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	s := p.stats

	s.Enabled = true
	s.Symbols = len(p.models)
	s.Horizon = p.cfg.Horizon

	var (
		sum float64
		n   int
	)

	for _, e := range p.models {
		if e.model != nil {
			sum += e.model.Skill()
			n++
		}
	}

	if n > 0 {
		s.AvgSkill = sum / float64(n)
	}

	return s
}

// ----- process-wide default predictor -----

var (
	defaultMu sync.RWMutex
	defaultP  *Predictor
)

// Configure builds the process-wide predictor and installs it. Called once at
// startup so live strategy edits can rebuild detectors without discarding every
// trained model.
func Configure(cfg config.MLConfig, params ai.Params) {
	p := New(cfg, params)

	defaultMu.Lock()

	defaultP = p
	defaultMu.Unlock()
}

// Default returns the process-wide predictor, or nil when none is configured.
func Default() *Predictor {
	defaultMu.RLock()
	defer defaultMu.RUnlock()

	return defaultP
}

// clamp01 clamps v into [0, 1].
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}

	if v > 1 {
		return 1
	}

	return v
}

// Config returns the predictor's live settings, or the disabled zero value when
// none is configured.
func (p *Predictor) Config() config.MLConfig {
	if p == nil {
		return config.MLConfig{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.cfg
}
