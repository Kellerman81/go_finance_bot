package ai

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/strategy/indicators"
)

// Params are the indicator periods the feature snapshot is computed with. They
// mirror the strategy's own settings so the model sees exactly the readings the
// rule-based detectors are voting on, not a second, differently-tuned set.
type Params struct {
	FastMA, SlowMA     int
	RSIPeriod          int
	MACDFast, MACDSlow int
	MACDSignal         int
	BBPeriod           int
	BBStdDev           float64
	ATRPeriod          int
	ADXPeriod          int
	StochKPeriod       int
	StochDPeriod       int
	RVOLPeriod         int
	RangePeriod        int // lookback for the high/low range position
	TrendPeriod        int
}

// WithDefaults fills in any unset period with a conventional value, so a
// caller can pass a partially-populated Params (or the zero value) without the
// indicator calls silently returning "undefined" for everything.
func (p Params) WithDefaults() Params {
	setInt := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}

	setInt(&p.FastMA, 12)
	setInt(&p.SlowMA, 26)
	setInt(&p.RSIPeriod, 14)
	setInt(&p.MACDFast, 12)
	setInt(&p.MACDSlow, 26)
	setInt(&p.MACDSignal, 9)
	setInt(&p.BBPeriod, 20)
	setInt(&p.ATRPeriod, 14)
	setInt(&p.ADXPeriod, 14)
	setInt(&p.StochKPeriod, 14)
	setInt(&p.StochDPeriod, 3)
	setInt(&p.RVOLPeriod, 20)
	setInt(&p.RangePeriod, 20)
	setInt(&p.TrendPeriod, 120)

	if p.BBStdDev <= 0 {
		p.BBStdDev = 2
	}

	return p
}

// WarmupBars is the fewest bars needed before a snapshot has enough defined
// readings to be worth asking a model about.
func (p Params) WarmupBars() int {
	need := p.SlowMA + p.MACDSignal + 2
	for _, n := range []int{
		p.RSIPeriod + 2,
		p.BBPeriod + 2,
		p.ATRPeriod + 2,
		2*p.ADXPeriod + 2,
		p.StochKPeriod + p.StochDPeriod + 2,
		p.RVOLPeriod + 2,
		p.RangePeriod + 2,
	} {
		if n > need {
			need = n
		}
	}

	return need
}

// Features is a compact, model-friendly snapshot of one symbol's technical
// state. Every field is normalised (percentages, 0..1 positions, oscillator
// levels) rather than raw price, so the same prompt reads the same way for a
// $3 stock and a $3000 one — and a small local model has a fighting chance.
//
// An "ok" flag accompanies each optional reading: a value that could not be
// computed is left out of the prompt entirely rather than sent as a zero the
// model would read as real data.
type Features struct {
	// At is the time of the bar the features were read at, when the caller
	// knows it. A blocking (backtest) advisor times its refreshes on it — see
	// Advisor.clockFor. Never sent to the model.
	At time.Time `json:"-"`

	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	Bars   int     `json:"bars"`

	Ret1  float64 `json:"ret_1_pct"`  // % change over the last bar
	Ret5  float64 `json:"ret_5_pct"`  // % change over the last 5 bars
	Ret20 float64 `json:"ret_20_pct"` // % change over the last 20 bars

	EMASpreadPct float64 `json:"ema_spread_pct"` // (fast-slow)/slow, %
	emaOK        bool

	RSI   float64 `json:"rsi"`
	rsiOK bool

	MACDHistPct float64 `json:"macd_hist_pct"` // histogram as % of price
	macdOK      bool

	BBPos float64 `json:"bb_pos"` // 0 = lower band, 1 = upper band
	bbOK  bool

	ATRPct float64 `json:"atr_pct"` // ATR as % of price
	atrOK  bool

	ADX     float64 `json:"adx"`
	PlusDI  float64 `json:"di_plus"`
	MinusDI float64 `json:"di_minus"`
	adxOK   bool

	StochK  float64 `json:"stoch_k"`
	StochD  float64 `json:"stoch_d"`
	stochOK bool

	RVOL   float64 `json:"rvol"` // volume vs its average
	rvolOK bool

	RangePos float64 `json:"range_pos"` // 0 = period low, 1 = period high
	rangeOK  bool

	TrendPct float64 `json:"trend_pct"` // fitted regression move over the window, %
	TrendR2  float64 `json:"trend_r2"`
	trendOK  bool
}

// BuildFeatures computes the snapshot from aligned OHLCV columns. ok is false
// when there is not enough history for even the basic readings, in which case
// the caller should not consult the model at all.
//
//nolint:cyclop // one guarded assignment per reading; a flat, readable list
func BuildFeatures(symbol string, high, low, closes, volume []float64, p Params) (Features, bool) {
	n := len(closes)
	if n < 2 || closes[n-1] <= 0 {
		return Features{}, false
	}

	price := closes[n-1]
	f := Features{Symbol: symbol, Price: price, Bars: n}

	f.Ret1 = pctChange(closes, 1)
	f.Ret5 = pctChange(closes, 5)
	f.Ret20 = pctChange(closes, 20)

	if fast, ok1 := indicators.EMALast(closes, p.FastMA); ok1 {
		if slow, ok2 := indicators.EMALast(closes, p.SlowMA); ok2 && slow != 0 {
			f.EMASpreadPct, f.emaOK = (fast-slow)/slow*100, true
		}
	}

	if r, ok := indicators.RSILast(closes, p.RSIPeriod); ok {
		f.RSI, f.rsiOK = r, true
	}

	if _, _, hist, ok := indicators.MACDLast(closes, p.MACDFast, p.MACDSlow, p.MACDSignal); ok {
		f.MACDHistPct, f.macdOK = hist/price*100, true
	}

	if _, up, lo, ok := indicators.BollingerLast(closes, p.BBPeriod, p.BBStdDev); ok && up > lo {
		f.BBPos, f.bbOK = (price-lo)/(up-lo), true
	}

	if a, ok := indicators.ATRLast(high, low, closes, p.ATRPeriod); ok {
		f.ATRPct, f.atrOK = a/price*100, true
	}

	if adx, plus, minus, ok := indicators.ADXLast(high, low, closes, p.ADXPeriod); ok {
		f.ADX, f.PlusDI, f.MinusDI, f.adxOK = adx, plus, minus, true
	}

	k, dv, stochOK := indicators.StochasticLast(high, low, closes, p.StochKPeriod, p.StochDPeriod)
	if stochOK {
		f.StochK, f.StochD, f.stochOK = k, dv, true
	}

	if rv, ok := indicators.RVOLLast(volume, p.RVOLPeriod); ok {
		f.RVOL, f.rvolOK = rv, true
	}

	if up, lo, ok := indicators.DonchianLast(high, low, p.RangePeriod); ok && up > lo {
		f.RangePos, f.rangeOK = (price-lo)/(up-lo), true
	}

	if slope, intercept, r2, ok := indicators.LinReg(closes, p.TrendPeriod); ok {
		used := min(n, p.TrendPeriod)

		// Same fix as strategy/detectors.go's trendDetector — the singularity
		// is at intercept -> 0 from either side, not just at or below it. A
		// sign-only guard misses a small positive intercept, which blows this
		// up into a nonsense TrendPct that then poisons the ML model's
		// z-standardization for every other row of that symbol's feature
		// column. See CODE_REVIEW_FINDINGS.md's trend-formula-singularity
		// finding.
		// And a negative intercept is no anchor either (ST2-2): it flips
		// the sign, so a strong rally fed the model a negative TrendPct.
		base := intercept
		if base < 0.01*price {
			base = price
		}

		f.TrendPct, f.TrendR2, f.trendOK = slope*float64(used-1)/base*100, r2, true
	}

	return f, true
}

// pctChange returns the percentage change of the last value versus the one
// `back` bars earlier, or 0 when that bar is unavailable.
func pctChange(v []float64, back int) float64 {
	n := len(v)
	if n <= back || v[n-1-back] <= 0 {
		return 0
	}

	return (v[n-1]/v[n-1-back] - 1) * 100
}

// Prompt renders the snapshot as the user message: one `key: value` per line,
// omitting readings that could not be computed. Plain lines beat JSON here —
// small instruct models follow a short labelled list more reliably, and the
// saved tokens go to the answer.
//
//nolint:cyclop // one guarded line per optional reading; a flat, readable list
func (f Features) Prompt() string {
	var b strings.Builder

	add := func(k string, format string, args ...any) {
		fmt.Fprintf(&b, "%s: %s\n", k, fmt.Sprintf(format, args...))
	}

	add("symbol", "%s", f.Symbol)
	add("price", "%.4f", f.Price)
	add("return_last_bar_pct", "%+.2f", f.Ret1)
	add("return_5_bars_pct", "%+.2f", f.Ret5)
	add("return_20_bars_pct", "%+.2f", f.Ret20)

	if f.emaOK {
		add("ema_fast_vs_slow_pct", "%+.2f", f.EMASpreadPct)
	}

	if f.rsiOK {
		add("rsi", "%.1f", f.RSI)
	}

	if f.macdOK {
		add("macd_histogram_pct_of_price", "%+.3f", f.MACDHistPct)
	}

	if f.bbOK {
		add("bollinger_position", "%.2f (0=lower band, 1=upper band)", f.BBPos)
	}

	if f.atrOK {
		add("atr_pct_of_price", "%.2f", f.ATRPct)
	}

	if f.adxOK {
		add("adx", "%.1f (+DI %.1f, -DI %.1f)", f.ADX, f.PlusDI, f.MinusDI)
	}

	if f.stochOK {
		add("stochastic", "%%K %.1f, %%D %.1f", f.StochK, f.StochD)
	}

	if f.rvolOK {
		add("relative_volume", "%.2f (1.0 = average)", f.RVOL)
	}

	if f.rangeOK {
		add("range_position", "%.2f (0=period low, 1=period high)", f.RangePos)
	}

	if f.trendOK {
		add("regression_trend_pct", "%+.2f (fit quality R2 %.2f)", f.TrendPct, f.TrendR2)
	}

	return b.String()
}

// FeatureNames lists the vector columns, in the order Vector returns them. It is
// the schema a trained model is tied to: a model trained on one order and
// evaluated on another is silently wrong, so anything that persists a model must
// persist these alongside it.
func FeatureNames() []string {
	return []string{
		"ret_1", "ret_5", "ret_20",
		"ema_spread_pct", "rsi", "macd_hist_pct", "bb_pos", "atr_pct",
		"adx", "di_spread", "stoch_k", "stoch_d", "rvol", "range_pos",
		"trend_pct", "trend_r2",
	}
}

// Vector renders the snapshot as a fixed-length numeric vector for a model,
// matching FeatureNames.
//
// Undefined readings become 0, which is not the same lie it would be in the
// prompt: a model sees the *distribution* of each column over its training set
// and standardises against it, so a consistently-imputed value is a category the
// model can learn rather than a fabricated measurement. (The LLM path omits
// them instead, because a language model reads "rsi: 0" as genuinely oversold.)
//
// The two directional-index readings are collapsed into +DI − DI: their levels
// carry little information on their own, and the difference is what the ADX
// detector actually votes on.
func (f Features) Vector() []float64 {
	return []float64{
		f.Ret1, f.Ret5, f.Ret20,
		f.EMASpreadPct, f.RSI, f.MACDHistPct, f.BBPos, f.ATRPct,
		f.ADX, f.PlusDI - f.MinusDI, f.StochK, f.StochD, f.RVOL, f.RangePos,
		f.TrendPct, f.TrendR2,
	}
}

// Defined reports whether enough readings were computable for the snapshot to be
// worth training on or predicting from. A row of mostly-zero imputations teaches
// a model nothing except what a warmup period looks like.
func (f Features) Defined() bool {
	return f.rsiOK && f.macdOK && f.emaOK && f.atrOK && f.bbOK
}

// Fingerprint is a coarse hash of the snapshot's decision-relevant readings. Two
// snapshots with the same fingerprint would almost certainly get the same
// verdict, so the advisor can skip a call when nothing has moved.
func (f Features) Fingerprint() string {
	round := func(v float64, step float64) int64 {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0
		}

		return int64(math.Round(v / step))
	}

	return fmt.Sprintf("%d/%d/%d/%d/%d/%d/%d",
		round(f.Ret5, 0.25), round(f.EMASpreadPct, 0.1), round(f.RSI, 2),
		round(f.MACDHistPct, 0.05), round(f.BBPos, 0.05), round(f.ADX, 2),
		round(f.RangePos, 0.05))
}
