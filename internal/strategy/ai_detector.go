package strategy

import (
	"fmt"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// advisor is the slice of *ai.Advisor the detector needs, as an interface so
// tests can supply a deterministic stand-in for a live model.
type advisor interface {
	// Verdict returns the current opinion for symbol; ok is false when there is
	// no usable (fresh enough) verdict.
	Verdict(symbol string, f ai.Features) (ai.Verdict, bool)
	// MaxStrength caps the strength a verdict may contribute.
	MaxStrength() float64
	// Model names the model behind the verdicts, for the reason string.
	Model() string
}

// aiDetector votes with an LLM's read of the same normalised indicator snapshot
// the other detectors work from. It is a detector like any other — it can be
// weighted, muted, combined, and gated by min_strength — with one difference:
// it can decline to answer. Every failure mode (advisor off, warming up, model
// unreachable, verdict stale, confidence below the gate) is a HOLD.
//
// The verdict itself comes from a cache the advisor refreshes in the
// background, so Detect never waits on the network.
type aiDetector struct {
	adv    advisor
	params ai.Params
	warmup int
}

// newAIDetector builds the LLM detector, wiring it to the process-wide advisor
// installed by ai.Configure at startup. With no advisor configured the detector
// still exists (so the UI can list it) and always votes HOLD.
func newAIDetector(c config.StrategyConfig) *aiDetector {
	p := ai.Params{
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

	d := &aiDetector{params: p, warmup: p.WarmupBars()}
	// A nil *ai.Advisor is a valid "disabled" advisor, but assigning it to the
	// interface would produce a non-nil interface holding a nil pointer — its
	// methods are nil-safe, so that is fine and keeps the nil check in one place.
	d.adv = ai.Default()

	return d
}

// Name returns the canonical detector name.
func (*aiDetector) Name() string { return "ai" }

// WarmupBars reports the minimum bars for a stable read. The snapshot is built
// from the same indicators as the other detectors, so it needs the longest of
// their warmups before it is worth asking a model about.
func (d *aiDetector) WarmupBars() int { return d.warmup }

// MaxLookback reports the most history the snapshot uses (the regression trend
// window), so the backtester sizes its per-bar window to match.
func (d *aiDetector) MaxLookback() int {
	if d.params.TrendPeriod > d.warmup {
		return d.params.TrendPeriod
	}

	return d.warmup
}

// Detect without a symbol cannot address the per-symbol verdict cache, so it
// abstains. Every real evaluation path goes through DetectFor.
func (*aiDetector) Detect(series) (Action, float64, string) {
	return Hold, 0, "ai: no symbol context"
}

// DetectFor votes with the advisor's current verdict for symbol.
func (d *aiDetector) DetectFor(symbol string, s series) (Action, float64, string) {
	if d.adv == nil || d.adv.Model() == "" {
		return Hold, 0, "ai: advisor disabled"
	}

	f, ok := ai.BuildFeatures(symbol, s.high, s.low, s.close, s.volume, d.params)
	if !ok {
		return Hold, 0, "ai: not enough history"
	}

	v, ok := d.adv.Verdict(symbol, f)
	if !ok {
		return Hold, 0, "ai: awaiting model"
	}

	strength := clamp01(v.Confidence) * d.adv.MaxStrength()

	switch v.Action {
	case ai.Buy:
		return Buy, strength, fmt.Sprintf(
			"ai(%s): BUY %.2f — %s",
			d.adv.Model(),
			strength,
			v.Reason,
		)

	case ai.Sell:
		return Sell, strength, fmt.Sprintf(
			"ai(%s): SELL %.2f — %s",
			d.adv.Model(),
			strength,
			v.Reason,
		)

	case ai.Hold:
	}

	return Hold, 0, fmt.Sprintf("ai(%s): HOLD — %s", d.adv.Model(), v.Reason)
}
