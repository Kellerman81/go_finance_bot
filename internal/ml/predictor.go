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
	// params are the feature periods the model was fitted on. A caller
	// asking with different ones (a strategy edit, a per-symbol override, an
	// optimizer trial) gets a refit, never a model scoring features it was
	// not trained on (ML3-5).
	params ai.Params
}

// Stats reports the predictor's state for the API.
type Stats struct {
	Enabled  bool    `json:"enabled"`
	Symbols  int     `json:"symbols"`
	Trained  int64   `json:"trained"`
	Failures int64   `json:"failures"`
	Horizon  int     `json:"horizon"`
	AvgSkill float64 `json:"avg_skill"` // mean Model.Skill (vote hit rate − chance) across models
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
		c.BuyProb = config.DefaultMLBuyProb
	}

	if c.SellProb <= 0 {
		c.SellProb = config.DefaultMLSellProb
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
	if p == nil {
		return Verdict{}, false
	}

	return p.VerdictWith(symbol, candles, p.params)
}

// VerdictWith is Verdict on features built with params — the periods of the
// strategy asking, which the "ml" detector passes so the model sees the
// readings its own strategy's rule-based detectors vote on. The predictor's
// own params were fixed at ml.Configure, from the base strategy as it was
// then: a strategy edit, every per-symbol override and every optimizer trial
// that varied a period trained and predicted on stale ones (ML3-5).
func (p *Predictor) VerdictWith(
	symbol string,
	candles []market.Candle,
	params ai.Params,
) (Verdict, bool) {
	if p == nil || len(candles) == 0 {
		return Verdict{}, false
	}

	params = params.WithDefaults()

	model, ok := p.modelFor(symbol, candles, params)
	if !ok {
		return Verdict{}, false
	}

	// A model with no measurable skill is worse than none: it produces
	// confident-looking probabilities from noise. Report the fact instead.
	if p.cfg.MinSkill > 0 && model.Skill() < p.cfg.MinSkill {
		return Verdict{
			Action: Hold,
			Reason: fmt.Sprintf(
				"ml: skill %.3f below min_skill %.3f (votes hit %.3f vs chance %.3f, %d votes)",
				model.Skill(),
				p.cfg.MinSkill,
				model.VoteHitRate,
				model.VoteChance,
				model.Votes,
			),
		}, true
	}

	f, ok := FeaturesAt(candles, symbol, params)
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
//
// The thresholds are read RELATIVE TO THE TRAINING POSITIVE RATE, not to 0.5.
// The label is one-sided — 1 means "rose by at least threshold", 0 means
// everything else, flat included — so a well-calibrated model's p(up) sits
// near the window's positive rate. With the shipped 0.005 threshold that rate
// was about 0.06, far below the absolute 0.42 sell line, and the model voted
// SELL on almost every bar: "unlikely to rally" read as "will fall", and ties
// prefer SELL (ML2-2). Now buy_prob/sell_prob are distances from 0.5 applied
// around the positive rate: with 0.58/0.42 the model needs a p(up) 0.08 above
// the base rate to BUY and 0.08 below it to SELL. When rallies are rare the
// SELL line falls below zero and the model never sells — which is honest, as
// a "rally or not" label cannot learn a fall. With threshold 0 the positive
// rate is near 0.5 and this is the old absolute reading.
func (p *Predictor) decide(prob float64, m *Model) Verdict {
	v := Verdict{Probability: prob, Action: Hold}

	base := 0.5
	if m.Samples > 0 {
		base = float64(m.Positives) / float64(m.Samples)
	}

	buyAt, sellAt := Cutoffs(base, p.cfg.BuyProb, p.cfg.SellProb)

	switch {
	case prob >= buyAt && buyAt < 1:
		v.Action = Buy
		v.Strength = clamp01((prob-buyAt)/(1-buyAt)) * p.cfg.MaxStrength

	case prob <= sellAt && sellAt > 0:
		v.Action = Sell
		v.Strength = clamp01((sellAt-prob)/sellAt) * p.cfg.MaxStrength
	}

	v.Reason = fmt.Sprintf("ml: p(up %d bars)=%.3f %s (acc %.2f vs base %.2f, n=%d) [%s]",
		p.cfg.Horizon, prob, v.Action, m.Accuracy, m.BaseRate, m.Samples, m.TopFeatures(3))

	return v
}

// modelFor returns symbol's model, retraining when the schedule says so.
func (p *Predictor) modelFor(
	symbol string,
	candles []market.Candle,
	params ai.Params,
) (*Model, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, seen := p.models[symbol]
	if !seen || e.params != params {
		e = &entry{params: params}
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

	model, err := p.train(candles, params)

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
//
// The rows are the last TrainBars, but their features are built from the
// featureContext bars before each of them too. Building them from the window
// alone gave its first ~120 rows a trend regression over fewer bars than
// TrendPeriod and RSI/ADX seeds that had not decayed: readings no inference,
// which sees the whole history, ever produces — trend_r2 off by up to 0.47 on
// a 0..1 feature, in 158 of the first 163 rows (ML3-6).
func (p *Predictor) train(candles []market.Candle, params ai.Params) (*Model, error) {
	ctx := featureContext(params)

	window := candles
	if len(window) > p.cfg.TrainBars+ctx {
		window = window[len(window)-p.cfg.TrainBars-ctx:]
	}

	first := max(len(window)-p.cfg.TrainBars, ctx)

	samples := buildSamples(window, first, params, p.cfg.Horizon, p.cfg.Threshold)
	if len(samples) < p.cfg.MinSamples {
		return nil, fmt.Errorf("%w: %d < %d", errTooFewSamples, len(samples), p.cfg.MinSamples)
	}

	return Train(samples, ai.FeatureNames(), TrainOpts{
		Epochs:       p.cfg.Epochs,
		LearningRate: p.cfg.LearningRate,
		L2:           p.cfg.L2,
		BuyProb:      p.cfg.BuyProb,
		SellProb:     p.cfg.SellProb,
		Embargo:      p.cfg.Horizon,
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

	return p.WarmupBarsFor(p.params)
}

// WarmupBarsFor is WarmupBars for features built with params (see
// VerdictWith): the feature context the first training row needs, the
// window, and the unlabelable tail.
func (p *Predictor) WarmupBarsFor(params ai.Params) int {
	if p == nil {
		return 0
	}

	return featureContext(params.WithDefaults()) + p.cfg.TrainBars + p.cfg.Horizon + 1
}

// featureContext is how many bars of history a training row's features are
// built over at least: the trend regression's whole TrendPeriod or the
// indicator warmup, whichever is longer, plus the warmup again so the
// Wilder/EMA seeds have decayed toward what a long history reads.
func featureContext(params ai.Params) int {
	return max(params.WarmupBars(), params.TrendPeriod) + params.WarmupBars()
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
// actually testing. A replay should not share the process-wide predictor at
// all: Backtester.Run trains into Fresh() instead, because resetting the
// shared one from inside the live bot wiped its live models (ML2-3).
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

// Fresh returns a new, untrained predictor with p's settings, or nil when p is
// nil. A backtest trains into one of these rather than into the process-wide
// predictor, so a replay can never wipe or overwrite the models the live bot
// trades on (see backtest.Backtester.Run).
func (p *Predictor) Fresh() *Predictor {
	if p == nil {
		return nil
	}

	return &Predictor{cfg: p.cfg, params: p.params, models: make(map[string]*entry)}
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
