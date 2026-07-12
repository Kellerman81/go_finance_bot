package strategy

import (
	"fmt"
	"math"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/strategy/indicators"
)

// ----- EMA crossover -----

type emaCrossDetector struct{ fast, slow int }

func newEMACrossDetector(c config.StrategyConfig) *emaCrossDetector {
	return &emaCrossDetector{fast: c.FastMA, slow: c.SlowMA}
}
func (d *emaCrossDetector) Name() string    { return "ema_cross" }
func (d *emaCrossDetector) WarmupBars() int  { return d.slow + 2 }
func (d *emaCrossDetector) Detect(s series) (Action, float64, string) {
	f, ok1 := indicators.EMALast(s.close, d.fast)
	sl, ok2 := indicators.EMALast(s.close, d.slow)
	if !ok1 || !ok2 || sl == 0 {
		return Hold, 0, "ema: undefined"
	}
	spread := (f - sl) / sl
	switch {
	case f > sl:
		return Buy, clamp01(math.Abs(spread) * 15), fmt.Sprintf("ema: %d>%d (+%.2f%%)", d.fast, d.slow, spread*100)
	case f < sl:
		return Sell, clamp01(math.Abs(spread) * 15), fmt.Sprintf("ema: %d<%d (%.2f%%)", d.fast, d.slow, spread*100)
	}
	return Hold, 0, "ema: flat"
}

// ----- Trend (linear-regression slope over a long lookback) -----

// trendMinBars is the fewest closes the trend detector needs for a meaningful
// fit. It is also the detector's warmup floor, so the trend uses up to `period`
// bars but does not hold the whole strategy in warmup for a week while history
// accrues — it works with whatever is available, up to a week.
const trendMinBars = 20

type trendDetector struct {
	period    int     // lookback in bars; uses all available up to this
	threshold float64 // min |fractional change over the window| to call a trend
}

func newTrendDetector(c config.StrategyConfig) *trendDetector {
	p := c.TrendPeriod
	if p < trendMinBars {
		p = trendMinBars
	}
	thr := c.TrendThreshold
	if thr <= 0 {
		thr = 0.01
	}
	return &trendDetector{period: p, threshold: thr}
}

func (d *trendDetector) Name() string   { return "trend" }
func (d *trendDetector) WarmupBars() int { return trendMinBars }

// MaxLookback reports the most bars the trend can use (its full lookback). It is
// larger than WarmupBars (the minimum to act): the detector works with as few as
// trendMinBars bars but reads the trend over up to `period`. The backtester uses
// this to size its window so the trend sees as much history as it would live.
func (d *trendDetector) MaxLookback() int { return d.period }
func (d *trendDetector) Detect(s series) (Action, float64, string) {
	n := len(s.close)
	if n < trendMinBars {
		return Hold, 0, "trend: warming up"
	}
	slope, intercept, r2, ok := indicators.LinReg(s.close, d.period)
	if !ok {
		return Hold, 0, "trend: undefined"
	}
	used := n
	if d.period < used {
		used = d.period
	}
	// Fractional change the fitted line implies across the window, anchored to
	// the line's start value so the magnitude is comparable across price levels.
	base := intercept
	if base <= 0 {
		base = s.close[n-1]
	}
	chg := slope * float64(used-1) / base
	// Confidence scales with the size of the move and how cleanly the series
	// follows the line (R²): a noisy series that merely drifts votes only weakly.
	str := clamp01(math.Abs(chg)*20) * r2
	switch {
	case chg >= d.threshold:
		return Buy, str, fmt.Sprintf("trend: +%.2f%% over %d bars (R²%.2f)", chg*100, used, r2)
	case chg <= -d.threshold:
		return Sell, str, fmt.Sprintf("trend: %.2f%% over %d bars (R²%.2f)", chg*100, used, r2)
	}
	return Hold, 0, fmt.Sprintf("trend: flat %.2f%% over %d bars (R²%.2f)", chg*100, used, r2)
}

// ----- RSI -----

type rsiDetector struct {
	period     int
	overbought float64
	oversold   float64
	midline    bool // true => graded mean-reversion vote around 50
}

func newRSIDetector(c config.StrategyConfig) *rsiDetector {
	return &rsiDetector{
		period: c.RSIPeriod, overbought: c.RSIOverbought, oversold: c.RSIOversold,
		midline: c.RSIMode != "extremes", // default "midline"
	}
}
func (d *rsiDetector) Name() string    { return "rsi" }
func (d *rsiDetector) WarmupBars() int { return d.period + 2 }
func (d *rsiDetector) Detect(s series) (Action, float64, string) {
	r, ok := indicators.RSILast(s.close, d.period)
	if !ok {
		return Hold, 0, "rsi: undefined"
	}
	if !d.midline {
		// "extremes" mode: only vote at oversold/overbought.
		switch {
		case r <= d.oversold:
			return Buy, clamp01((d.oversold - r) / 30), fmt.Sprintf("rsi %.1f oversold", r)
		case r >= d.overbought:
			return Sell, clamp01((r - d.overbought) / 30), fmt.Sprintf("rsi %.1f overbought", r)
		}
		return Hold, 0, fmt.Sprintf("rsi %.1f neutral", r)
	}
	// "midline" mode: graded mean-reversion. Below 50 is relatively weak
	// (buy bias), above 50 relatively strong (sell bias); strength reaches 1
	// at the oversold/overbought thresholds and saturates beyond them.
	switch {
	case r < 50:
		return Buy, clamp01((50 - r) / (50 - d.oversold)), fmt.Sprintf("rsi %.1f<50", r)
	case r > 50:
		return Sell, clamp01((r - 50) / (d.overbought - 50)), fmt.Sprintf("rsi %.1f>50", r)
	}
	return Hold, 0, "rsi 50.0"
}

// ----- MACD -----

type macdDetector struct{ fast, slow, signal int }

func newMACDDetector(c config.StrategyConfig) *macdDetector {
	return &macdDetector{fast: c.MACDFast, slow: c.MACDSlow, signal: c.MACDSignal}
}
func (d *macdDetector) Name() string   { return "macd" }
func (d *macdDetector) WarmupBars() int { return d.slow + d.signal + 2 }
func (d *macdDetector) Detect(s series) (Action, float64, string) {
	_, _, h, ok := indicators.MACDLast(s.close, d.fast, d.slow, d.signal)
	if !ok {
		return Hold, 0, "macd: undefined"
	}
	switch {
	case h > 0:
		return Buy, clamp01(math.Abs(h) * 2), fmt.Sprintf("macd hist %.4f>0", h)
	case h < 0:
		return Sell, clamp01(math.Abs(h) * 2), fmt.Sprintf("macd hist %.4f<0", h)
	}
	return Hold, 0, "macd flat"
}

// ----- Bollinger Bands (mean reversion) -----

type bollingerDetector struct {
	period int
	k      float64
}

func newBollingerDetector(c config.StrategyConfig) *bollingerDetector {
	return &bollingerDetector{period: c.BBPeriod, k: c.BBStdDev}
}
func (d *bollingerDetector) Name() string   { return "bollinger" }
func (d *bollingerDetector) WarmupBars() int { return d.period + 2 }
func (d *bollingerDetector) Detect(s series) (Action, float64, string) {
	_, up, lo, ok := indicators.BollingerLast(s.close, d.period, d.k)
	if !ok {
		return Hold, 0, "bbands: undefined"
	}
	price := s.close[len(s.close)-1]
	band := up - lo
	if band <= 0 {
		return Hold, 0, "bbands: flat"
	}
	switch {
	case price <= lo:
		return Buy, clamp01((lo - price) / band + 0.5), fmt.Sprintf("bbands: %.2f<=lower %.2f", price, lo)
	case price >= up:
		return Sell, clamp01((price - up) / band + 0.5), fmt.Sprintf("bbands: %.2f>=upper %.2f", price, up)
	}
	return Hold, 0, "bbands: inside"
}

// ----- VWAP (trend bias) -----

type vwapDetector struct{ period int }

func newVWAPDetector(c config.StrategyConfig) *vwapDetector { return &vwapDetector{period: c.VWAPPeriod} }
func (d *vwapDetector) Name() string   { return "vwap" }
func (d *vwapDetector) WarmupBars() int { return d.period + 2 }
func (d *vwapDetector) Detect(s series) (Action, float64, string) {
	v, ok := indicators.VWAPLast(s.high, s.low, s.close, s.volume, d.period)
	if !ok || v == 0 {
		return Hold, 0, "vwap: undefined"
	}
	price := s.close[len(s.close)-1]
	dev := (price - v) / v
	switch {
	case price > v:
		return Buy, clamp01(math.Abs(dev) * 25), fmt.Sprintf("vwap: %.2f>VWAP %.2f", price, v)
	case price < v:
		return Sell, clamp01(math.Abs(dev) * 25), fmt.Sprintf("vwap: %.2f<VWAP %.2f", price, v)
	}
	return Hold, 0, "vwap: at"
}

// ----- ATR (volatility breakout) -----

type atrDetector struct {
	period int
	mult   float64
}

func newATRDetector(c config.StrategyConfig) *atrDetector {
	return &atrDetector{period: c.ATRPeriod, mult: c.ATRMult}
}
func (d *atrDetector) Name() string   { return "atr" }
func (d *atrDetector) WarmupBars() int { return d.period + 2 }
func (d *atrDetector) Detect(s series) (Action, float64, string) {
	a, ok := indicators.ATRLast(s.high, s.low, s.close, d.period)
	n := len(s.close)
	if !ok || n < 2 || a <= 0 {
		return Hold, 0, "atr: undefined"
	}
	// Breakout: latest close moves more than mult*ATR from the prior close.
	prev := s.close[n-2]
	move := s.close[n-1] - prev
	thr := d.mult * a
	switch {
	case move >= thr:
		return Buy, clamp01(move / (thr * 2)), fmt.Sprintf("atr breakout +%.2f>=%.2f", move, thr)
	case move <= -thr:
		return Sell, clamp01(-move / (thr * 2)), fmt.Sprintf("atr breakdown %.2f<=-%.2f", move, thr)
	}
	return Hold, 0, fmt.Sprintf("atr: move %.2f within %.2f", move, thr)
}

// ----- RVOL (relative volume confirmation) -----

type rvolDetector struct {
	period    int
	threshold float64
}

func newRVOLDetector(c config.StrategyConfig) *rvolDetector {
	return &rvolDetector{period: c.RVOLPeriod, threshold: c.RVOLThreshold}
}
func (d *rvolDetector) Name() string   { return "rvol" }
func (d *rvolDetector) WarmupBars() int { return d.period + 2 }
func (d *rvolDetector) Detect(s series) (Action, float64, string) {
	rv, ok := indicators.RVOLLast(s.volume, d.period)
	n := len(s.close)
	if !ok || n < 2 {
		return Hold, 0, "rvol: undefined"
	}
	if rv < d.threshold {
		return Hold, 0, fmt.Sprintf("rvol %.2f<%.2f (quiet)", rv, d.threshold)
	}
	// High relative volume confirms the direction of the latest move.
	move := s.close[n-1] - s.close[n-2]
	str := clamp01((rv - d.threshold) / 3)
	switch {
	case move > 0:
		return Buy, str, fmt.Sprintf("rvol %.2f surge, price up", rv)
	case move < 0:
		return Sell, str, fmt.Sprintf("rvol %.2f surge, price down", rv)
	}
	return Hold, 0, fmt.Sprintf("rvol %.2f surge, flat", rv)
}

// ----- Volume Profile (value-area mean reversion) -----

type volumeProfileDetector struct {
	window       int
	buckets      int
	valueAreaPct float64
}

func newVolumeProfileDetector(c config.StrategyConfig) *volumeProfileDetector {
	return &volumeProfileDetector{window: c.VPWindow, buckets: c.VPBuckets, valueAreaPct: c.VPValueAreaPct}
}
func (d *volumeProfileDetector) Name() string   { return "volume_profile" }
func (d *volumeProfileDetector) WarmupBars() int { return d.window + 2 }
func (d *volumeProfileDetector) Detect(s series) (Action, float64, string) {
	vp, ok := indicators.BuildVolumeProfile(s.close, s.volume, d.window, d.buckets, d.valueAreaPct)
	if !ok {
		return Hold, 0, "vp: undefined"
	}
	price := s.close[len(s.close)-1]
	va := vp.ValueAreaHigh - vp.ValueAreaLow
	if va <= 0 {
		return Hold, 0, "vp: flat"
	}
	switch {
	case price < vp.ValueAreaLow:
		return Buy, clamp01((vp.ValueAreaLow-price)/va + 0.4), fmt.Sprintf("vp: %.2f<VAL %.2f (POC %.2f)", price, vp.ValueAreaLow, vp.POC)
	case price > vp.ValueAreaHigh:
		return Sell, clamp01((price-vp.ValueAreaHigh)/va + 0.4), fmt.Sprintf("vp: %.2f>VAH %.2f (POC %.2f)", price, vp.ValueAreaHigh, vp.POC)
	}
	return Hold, 0, fmt.Sprintf("vp: in value area (POC %.2f)", vp.POC)
}

// ----- ADX (directional-movement trend follow) -----

type adxDetector struct {
	period    int
	threshold float64
}

func newADXDetector(c config.StrategyConfig) *adxDetector {
	thr := c.ADXThreshold
	if thr <= 0 {
		thr = 25
	}
	return &adxDetector{period: c.ADXPeriod, threshold: thr}
}
func (d *adxDetector) Name() string    { return "adx" }
func (d *adxDetector) WarmupBars() int { return 2*d.period + 2 }
func (d *adxDetector) Detect(s series) (Action, float64, string) {
	adx, plus, minus, ok := indicators.ADXLast(s.high, s.low, s.close, d.period)
	if !ok {
		return Hold, 0, "adx: undefined"
	}
	if adx < d.threshold {
		return Hold, 0, fmt.Sprintf("adx %.1f<%.0f (no trend)", adx, d.threshold)
	}
	// Strength grows with ADX above the threshold (saturating ~50 over it).
	str := clamp01((adx - d.threshold) / 50)
	switch {
	case plus > minus:
		return Buy, str, fmt.Sprintf("adx %.1f, +DI %.1f>-DI %.1f", adx, plus, minus)
	case minus > plus:
		return Sell, str, fmt.Sprintf("adx %.1f, -DI %.1f>+DI %.1f", adx, minus, plus)
	}
	return Hold, 0, fmt.Sprintf("adx %.1f, DI tied", adx)
}

// ----- Stochastic oscillator (mean reversion) -----

type stochasticDetector struct {
	kPeriod, dPeriod      int
	overbought, oversold float64
}

func newStochasticDetector(c config.StrategyConfig) *stochasticDetector {
	ob, os := c.StochOverbought, c.StochOversold
	if ob <= 0 {
		ob = 80
	}
	if os <= 0 {
		os = 20
	}
	return &stochasticDetector{kPeriod: c.StochKPeriod, dPeriod: c.StochDPeriod, overbought: ob, oversold: os}
}
func (d *stochasticDetector) Name() string    { return "stochastic" }
func (d *stochasticDetector) WarmupBars() int { return d.kPeriod + d.dPeriod + 2 }
func (d *stochasticDetector) Detect(s series) (Action, float64, string) {
	k, dv, ok := indicators.StochasticLast(s.high, s.low, s.close, d.kPeriod, d.dPeriod)
	if !ok {
		return Hold, 0, "stoch: undefined"
	}
	switch {
	case k <= d.oversold:
		return Buy, clamp01((d.oversold-k)/d.oversold), fmt.Sprintf("stoch %%K %.1f<=%.0f oversold", k, d.oversold)
	case k >= d.overbought:
		return Sell, clamp01((k-d.overbought)/(100-d.overbought)), fmt.Sprintf("stoch %%K %.1f>=%.0f overbought", k, d.overbought)
	}
	return Hold, 0, fmt.Sprintf("stoch %%K %.1f %%D %.1f neutral", k, dv)
}

// ----- Supertrend (ATR-based trend follow) -----

type supertrendDetector struct {
	period int
	mult   float64
}

func newSupertrendDetector(c config.StrategyConfig) *supertrendDetector {
	return &supertrendDetector{period: c.SupertrendPeriod, mult: c.SupertrendMult}
}
func (d *supertrendDetector) Name() string    { return "supertrend" }
func (d *supertrendDetector) WarmupBars() int { return d.period + 2 }
func (d *supertrendDetector) Detect(s series) (Action, float64, string) {
	dir, level, ok := indicators.SupertrendLast(s.high, s.low, s.close, d.period, d.mult)
	if !ok || level <= 0 {
		return Hold, 0, "supertrend: undefined"
	}
	price := s.close[len(s.close)-1]
	// Strength scales with how far price has advanced past the flip band.
	str := clamp01(math.Abs(price-level) / level * 20)
	if dir > 0 {
		return Buy, str, fmt.Sprintf("supertrend up, %.2f>band %.2f", price, level)
	}
	return Sell, str, fmt.Sprintf("supertrend down, %.2f<band %.2f", price, level)
}

// ----- Donchian channel (breakout) -----

type donchianDetector struct{ period int }

func newDonchianDetector(c config.StrategyConfig) *donchianDetector {
	return &donchianDetector{period: c.DonchianPeriod}
}
func (d *donchianDetector) Name() string    { return "donchian" }
func (d *donchianDetector) WarmupBars() int { return d.period + 2 }
func (d *donchianDetector) Detect(s series) (Action, float64, string) {
	up, lo, ok := indicators.DonchianLast(s.high, s.low, d.period)
	if !ok {
		return Hold, 0, "donchian: undefined"
	}
	price := s.close[len(s.close)-1]
	band := up - lo
	if band <= 0 {
		return Hold, 0, "donchian: flat"
	}
	switch {
	case price > up:
		return Buy, clamp01((price-up)/band + 0.4), fmt.Sprintf("donchian breakout %.2f>%.2f", price, up)
	case price < lo:
		return Sell, clamp01((lo-price)/band + 0.4), fmt.Sprintf("donchian breakdown %.2f<%.2f", price, lo)
	}
	return Hold, 0, fmt.Sprintf("donchian: inside [%.2f,%.2f]", lo, up)
}
