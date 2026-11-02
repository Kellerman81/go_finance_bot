package strategy

import (
	"math"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// closesToCandles builds candles whose Close follows the given series (OHLC all
// set to the close), enough for the trend detector's warmup.
func closesToCandles(closes []float64) []market.Candle {
	out := make([]market.Candle, len(closes))
	for i, c := range closes {
		out[i] = market.Candle{Open: c, High: c, Low: c, Close: c, Volume: 1}
	}
	return out
}

func risingCloses(n int, start, step float64) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = start + step*float64(i)
	}
	return v
}

func TestTrendDetectorDirection(t *testing.T) {
	cfg := config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01}
	d := newTrendDetector(cfg)

	// A clear ~10% rise over 30 bars => BUY.
	up := toSeries(closesToCandles(risingCloses(30, 100, 0.34)))
	if a, str, _ := d.Detect(up); a != Buy || str <= 0 {
		t.Errorf("uptrend => %s str %.2f, want BUY with positive strength", a, str)
	}

	// Mirror image => SELL.
	down := toSeries(closesToCandles(risingCloses(30, 110, -0.34)))
	if a, _, _ := d.Detect(down); a != Sell {
		t.Errorf("downtrend => %s, want SELL", a)
	}

	// Flat below the threshold => HOLD.
	flat := toSeries(closesToCandles(risingCloses(30, 100, 0.0001)))
	if a, _, _ := d.Detect(flat); a != Hold {
		t.Errorf("flat trend => %s, want HOLD", a)
	}
}

// TestTrendDetectorInterceptNearZeroDoesNotBlowUp reproduces the
// trend-formula-singularity finding: the fitted regression intercept is the
// line's value at the START of the window, and a small POSITIVE intercept
// (not caught by the old base <= 0 guard, which only handled the negative
// side) produces the same denominator blowup as a small negative one. A
// window whose fitted line starts near zero and rises to a normal price is
// a real, if extreme, pattern — but the reported percentage must stay
// bounded (anchored to the current price once the intercept is too close to
// the singularity to use), not blow up into the tens of thousands of
// percent the way it did before this fix.
func TestTrendDetectorInterceptNearZeroDoesNotBlowUp(t *testing.T) {
	cfg := config.StrategyConfig{TrendPeriod: 30, TrendThreshold: 0.01}
	d := newTrendDetector(cfg)

	// A perfectly linear series from 0.01 up to 100 over 30 bars: LinReg
	// fits this exactly, so intercept ≈ 0.01 (tiny, positive — the case the
	// old "base <= 0" guard did not catch) and r2 ≈ 1.
	closes := risingCloses(30, 0.01, (100.0-0.01)/29.0)

	action, str, reason := d.Detect(toSeries(closesToCandles(closes)))
	if action != Buy {
		t.Fatalf("a clean rise from 0.01 to 100 should read as an uptrend, got %s", action)
	}

	if math.IsNaN(str) || math.IsInf(str, 0) {
		t.Fatalf("strength = %v, want a finite value", str)
	}

	// The OLD code (intercept used directly as the denominator since 0.01 is
	// not <= 0) computes chg = slope*29/0.01 ≈ 10005, i.e. "+1000500.00%".
	// The fix anchors to the current price instead, giving chg ≈ slope*29/100
	// ≈ 1.0, i.e. "+100.xx%" — three orders of magnitude smaller. Assert the
	// reason does not contain the blown-up magnitude.
	if strings.Contains(reason, "+1000") || strings.Contains(reason, "+10005") {
		t.Errorf("reason contains an unbounded percentage (the pre-fix blowup): %q", reason)
	}
}

func TestTrendDetectorWarmup(t *testing.T) {
	d := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})
	short := toSeries(closesToCandles(risingCloses(trendMinBars-1, 100, 1)))
	if a, _, reason := d.Detect(short); a != Hold || !strings.Contains(reason, "warming up") {
		t.Errorf("below trendMinBars => %s/%q, want HOLD warming up", a, reason)
	}
}

func TestTrendGateBlocksCounterTrend(t *testing.T) {
	upSeries := closesToCandles(risingCloses(40, 100, 0.5)) // strong uptrend
	filter := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})

	// A SELL signal against an up-trend is vetoed.
	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Sell, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: filter,
	}
	sig := c.Evaluate("X", upSeries)
	if sig.Action != Hold {
		t.Fatalf("SELL into an up-trend => %s, want HOLD", sig.Action)
	}
	if !strings.Contains(sig.Reason, "blocked by") {
		t.Errorf("reason should explain the veto, got %q", sig.Reason)
	}

	// A BUY with the up-trend is allowed.
	cb := &Combined{
		detectors: []Detector{fakeDetector{"a", Buy, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: filter,
	}
	if got := cb.Evaluate("X", upSeries).Action; got != Buy {
		t.Errorf("BUY with the up-trend => %s, want BUY", got)
	}
}

func TestTrendGateFlatDoesNotBlock(t *testing.T) {
	flat := closesToCandles(risingCloses(40, 100, 0.0001)) // below threshold
	filter := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})
	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Sell, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: filter,
	}
	if got := c.Evaluate("X", flat).Action; got != Sell {
		t.Errorf("SELL with a flat trend => %s, want SELL (no gating)", got)
	}
}

func TestCombinedMaxLookback(t *testing.T) {
	cfg := config.StrategyConfig{
		Detectors: []string{"rsi", "trend"}, Combine: "majority", MinStrength: 0.1,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 26, RSIPeriod: 14,
		TrendPeriod: 1200, TrendThreshold: 0.01,
	}
	cmb := New(cfg).(*Combined)
	lb, ok := interface{}(cmb).(interface{ MaxLookback() int })
	if !ok {
		t.Fatal("Combined should expose MaxLookback")
	}
	if got := lb.MaxLookback(); got < 1200 {
		t.Errorf(
			"MaxLookback = %d, want >= trend period 1200 so backtests size the window correctly",
			got,
		)
	}
}

// TestEMACrossMaxLookbackExceedsWarmupBars reproduces "Combined.MaxLookback()
// doesn't scale with EMA period": WarmupBars is only the minimum bars for a
// non-NaN EMA value, not enough for the SMA seed's influence to have decayed
// — a windowed backtest fed only WarmupBars-worth of history produces a
// meaningfully different EMA than the same detector sees live, with a full
// history behind it. MaxLookback must report the larger, convergence-aware
// figure.
func TestEMACrossMaxLookbackExceedsWarmupBars(t *testing.T) {
	d := newEMACrossDetector(config.StrategyConfig{FastMA: 12, SlowMA: 200})

	warmup := d.WarmupBars()
	lookback := d.MaxLookback()

	if lookback <= warmup {
		t.Fatalf("MaxLookback = %d, WarmupBars = %d — MaxLookback must be larger, "+
			"accounting for the EMA seed's decay, not just the minimum to produce a value",
			lookback, warmup)
	}

	if lookback < 3*200 {
		t.Errorf("MaxLookback = %d, want >= 3x the slow period (600) for the seed's "+
			"residual influence to have decayed to a negligible level", lookback)
	}
}

// TestCombinedMaxLookbackPicksUpTheEMAPeriod confirms Combined.MaxLookback
// actually surfaces ema_cross's own MaxLookback (not just its WarmupBars) —
// the aggregation Combined already does correctly once a detector implements
// the optional MaxLookback() interface; this is what breaks if
// emaCrossDetector stops implementing it.
func TestCombinedMaxLookbackPicksUpTheEMAPeriod(t *testing.T) {
	cfg := config.StrategyConfig{
		Detectors: []string{"ema_cross"}, Combine: "majority", MinStrength: 0.1,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 200,
	}
	cmb := New(cfg).(*Combined)

	lb, ok := interface{}(cmb).(interface{ MaxLookback() int })
	if !ok {
		t.Fatal("Combined should expose MaxLookback")
	}

	if got := lb.MaxLookback(); got < 3*200 {
		t.Errorf("Combined.MaxLookback() = %d, want >= 3x the ema_cross slow period "+
			"(600) — it must surface ema_cross's own MaxLookback, not just its "+
			"WarmupBars (which strategy.warmup would already cover on its own)", got)
	}
}

// TestMACDMaxLookbackExceedsWarmupBars is the MACD equivalent of
// TestEMACrossMaxLookbackExceedsWarmupBars: MACD is built from the same
// EMA machinery (fast/slow EMA, then a signal-line EMA of their
// difference), so it has the identical seed-convergence gap.
func TestMACDMaxLookbackExceedsWarmupBars(t *testing.T) {
	d := newMACDDetector(config.StrategyConfig{MACDFast: 12, MACDSlow: 200, MACDSignal: 9})

	warmup := d.WarmupBars()
	lookback := d.MaxLookback()

	if lookback <= warmup {
		t.Fatalf("MaxLookback = %d, WarmupBars = %d — MaxLookback must be larger",
			lookback, warmup)
	}

	if lookback < 3*(200+9) {
		t.Errorf("MaxLookback = %d, want >= 3x(slow+signal) (627) for both EMA "+
			"stages to have converged", lookback)
	}
}

// risingClosesPct builds a series that grows by the same FRACTIONAL step each
// bar (unlike risingCloses' absolute step), so two series built at different
// price levels with the same pctStep represent the same relative move.
func risingClosesPct(n int, start, pctStep float64) []float64 {
	v := make([]float64, n)
	p := start

	for i := range v {
		v[i] = p
		p *= 1 + pctStep
	}

	return v
}

// TestMACDStrengthIsPriceScaleInvariant reproduces the AT-7 fix: macdDetector
// used to vote clamp01(|histogram|*2) — the histogram is a PRICE difference,
// so the same relative move voted with wildly different conviction depending
// on the symbol's price level (mute on a low-priced instrument, saturated on
// a high-priced one), which matters for autotune specifically since
// min_strength is searched per symbol. Detect must now normalize by price,
// so the same relative move votes with comparable strength at any price
// level.
func TestMACDStrengthIsPriceScaleInvariant(t *testing.T) {
	d := newMACDDetector(config.StrategyConfig{MACDFast: 12, MACDSlow: 26, MACDSignal: 9})

	const bars = 60
	const pctStep = 0.02 // ~2% per bar, a clear sustained move

	low := toSeries(closesToCandles(risingClosesPct(bars, 5, pctStep)))
	high := toSeries(closesToCandles(risingClosesPct(bars, 5000, pctStep)))

	aLow, strLow, _ := d.Detect(low)
	aHigh, strHigh, _ := d.Detect(high)

	if aLow != Buy || aHigh != Buy {
		t.Fatalf("action = %s / %s, want BUY on both a low-priced and a high-priced instrument "+
			"with the identical relative uptrend", aLow, aHigh)
	}

	// Old (unnormalized) behavior gave these strengths in completely
	// different regimes: ~0 at price ~5 (mute — histogram is a fraction of
	// a cent), clamped to 1.0 at price ~5000 (saturated). Normalized, they
	// must be identical (same relative move, same ratio to price).
	if diff := math.Abs(strLow - strHigh); diff > 1e-9 {
		t.Errorf("strength at low price = %.4f, at high price = %.4f (diff %.4g) — "+
			"want them equal: the SAME relative move must vote with the SAME "+
			"conviction regardless of price level", strLow, strHigh, diff)
	}

	if strLow < 0.08 {
		t.Errorf("strength at low price = %.3f, want meaningfully non-zero for a clear 2%%/bar move", strLow)
	}
}

func TestNewBuildsTrendGate(t *testing.T) {
	cfg := config.StrategyConfig{
		Detectors: []string{"trend"}, Combine: "majority", MinStrength: 0.1,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 26,
		TrendPeriod: 100, TrendThreshold: 0.01, TrendGate: true,
	}
	s := New(cfg)
	cmb, ok := s.(*Combined)
	if !ok {
		t.Fatalf("New returned %T, want *Combined", s)
	}
	if !cmb.trendGate || cmb.trendFilter == nil {
		t.Error("trend gate not wired from config")
	}
}

// TestNewBuildsSellTrendGate covers TrendPeriodSell/TrendThresholdSell: no
// override leaves trendFilterSell nil (the gate falls back to the shared
// trendFilter for both sides, exactly as before this feature); a full
// override builds trendFilterSell with its own parameters; a partial
// override (only one of the two fields set) inherits the buy-side value for
// the other.
func TestNewBuildsSellTrendGate(t *testing.T) {
	base := config.StrategyConfig{
		Detectors: []string{"ema_cross"}, Combine: "majority", MinStrength: 0.1,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 26,
		TrendPeriod: 100, TrendThreshold: 0.01, TrendGate: true,
	}

	if plain := New(base).(*Combined); plain.trendFilterSell != nil {
		t.Error("trendFilterSell should be nil when no sell override is configured")
	}

	full := base
	full.TrendPeriodSell = 50
	full.TrendThresholdSell = 0.5

	cmb := New(full).(*Combined)
	if cmb.trendFilterSell == nil {
		t.Fatal("trendFilterSell should be built when TrendPeriodSell/TrendThresholdSell are set")
	}

	if cmb.trendFilterSell.period != 50 || cmb.trendFilterSell.threshold != 0.5 {
		t.Errorf("trendFilterSell = {period:%d threshold:%v}, want {50 0.5}",
			cmb.trendFilterSell.period, cmb.trendFilterSell.threshold)
	}

	partial := base
	partial.TrendThresholdSell = 0.4 // TrendPeriodSell left at 0 => inherit TrendPeriod

	cmb2 := New(partial).(*Combined)
	if cmb2.trendFilterSell == nil {
		t.Fatal("trendFilterSell should be built when only TrendThresholdSell is set")
	}

	if cmb2.trendFilterSell.period != 100 {
		t.Errorf("period = %d, want 100 (inherited from TrendPeriod)", cmb2.trendFilterSell.period)
	}

	if cmb2.trendFilterSell.threshold != 0.4 {
		t.Errorf("threshold = %v, want the override 0.4", cmb2.trendFilterSell.threshold)
	}
}

// TestTrendGateSellUsesItsOwnThreshold proves the sell-side override is
// actually consulted, not just built: the same ~19.5% rise that blocks a
// SELL under the default 0.01 threshold (per TestTrendGateBlocksCounterTrend)
// must NOT block it once a stricter 0.5 sell-side threshold reads that same
// move as flat.
func TestTrendGateSellUsesItsOwnThreshold(t *testing.T) {
	upSeries := closesToCandles(risingCloses(40, 100, 0.5)) // ~19.5% end-to-end rise

	buyFilter := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})

	// Sanity: without a sell override, this move blocks (existing behaviour).
	plain := &Combined{
		detectors: []Detector{fakeDetector{"a", Sell, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: buyFilter,
	}
	if got := plain.Evaluate("X", upSeries).Action; got != Hold {
		t.Fatalf("sanity check failed: without a sell override this move should still block, got %s", got)
	}

	strictSellFilter := newTrendDetectorParams(100, 0.5) // this move reads as flat here
	withOverride := &Combined{
		detectors: []Detector{fakeDetector{"a", Sell, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: buyFilter, trendFilterSell: strictSellFilter,
	}

	sig := withOverride.Evaluate("X", upSeries)
	if sig.Action != Sell {
		t.Fatalf("SELL into a move the sell-side filter reads as flat => %s, want SELL (not blocked)",
			sig.Action)
	}
}

// TestTrendGateSellIgnoresStaleTrendVoteWhenOverridden guards the reuse
// optimization: trendVote normally reuses an already-tallied "trend" vote
// instead of recomputing, but that vote (when one exists in the sell
// detector set) is always built from the BUY-side TrendPeriod/TrendThreshold
// via the registry — not from TrendPeriodSell/TrendThresholdSell. Reusing it
// for a configured trendFilterSell would silently apply the wrong
// parameters. Here the tallied "trend" vote says BUY (which would veto the
// SELL if reused), but the series is flat, so a fresh read from
// trendFilterSell must say HOLD and let the SELL through.
func TestTrendGateSellIgnoresStaleTrendVoteWhenOverridden(t *testing.T) {
	flat := closesToCandles(risingCloses(40, 100, 0.0001))

	buyFilter := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})
	sellFilter := newTrendDetectorParams(100, 0.01)

	c := &Combined{
		detectors: []Detector{fakeDetector{"x", Hold, 0}},
		sellDetectors: []Detector{
			fakeDetector{"b1", Sell, 0.9},
			fakeDetector{"b2", Sell, 0.9},
			fakeDetector{"trend", Buy, 0.9}, // stale opinion if wrongly reused
		},
		mode: Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: buyFilter, trendFilterSell: sellFilter,
	}

	sig := c.Evaluate("X", flat)
	if sig.Action != Sell {
		t.Fatalf("got %s, want SELL — the sell-side filter freshly reads the flat series as "+
			"no-trend, so the veto must not fire even though a stale 'trend' vote of BUY "+
			"exists in the tally", sig.Action)
	}
}

// TestTrendGateBuySideUnaffectedBySellOverride confirms the BUY-veto side
// always uses the shared trendFilter, never trendFilterSell, regardless of
// how loose the sell-side override is.
func TestTrendGateBuySideUnaffectedBySellOverride(t *testing.T) {
	downSeries := closesToCandles(risingCloses(40, 140, -0.5)) // clear downtrend

	buyFilter := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})
	looseSellFilter := newTrendDetectorParams(100, 0.9) // would call this move "flat"

	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Buy, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: buyFilter, trendFilterSell: looseSellFilter,
	}

	if got := c.Evaluate("X", downSeries).Action; got != Hold {
		t.Errorf("BUY into a down-trend => %s, want HOLD — the BUY veto must keep using "+
			"trendFilter regardless of a configured sell-side override", got)
	}
}

// TestTrendDetectorReadsAConvexRallyAsUp is the regression for ST2-2. The
// singularity fix compared |intercept| with 1% of the price, which lets a
// fitted intercept far BELOW zero through as the denominator — and dividing by
// a negative base flips the sign of the move. A long flat stretch followed by
// a steep ramp (the shape of a penny, biotech or meme-stock breakout) fits a
// line starting well below zero: 80 bars at 1, then a ramp to 21, read
// "SELL -471%", and the default trend gate blocked the BUY.
func TestTrendDetectorReadsAConvexRallyAsUp(t *testing.T) {
	t.Parallel()

	closes := make([]float64, 0, 100)
	for range 80 {
		closes = append(closes, 1)
	}

	for i := 1; i <= 20; i++ {
		closes = append(closes, 1+float64(i))
	}

	d := newTrendDetectorParams(len(closes), 0.01)

	action, _, reason := d.Detect(toSeries(closesToCandles(closes)))
	if action != Buy {
		t.Errorf("a rally from 1 to 21 read %v (%s), want BUY", action, reason)
	}
}
