package indicators

import (
	"math"
	"testing"
)

// RVOL's baseline is the bars that traded, not every interval in the window.
//
// These guard a regression that filling quiet intervals with flat zero-volume
// bars introduced. Empty intervals in the baseline drag the average toward zero,
// so on a thinly-traded instrument every bar that traded reads as a surge — the
// detector stops filtering and starts firing on everything.

// thinWindow builds `period` baseline bars of which only `traded` carry volume,
// plus a current bar. RVOL is computed on the whole slice.
func thinWindow(period, traded int, vol, current float64) []float64 {
	out := make([]float64, period+1)

	// The traded bars sit at the end of the baseline, where a real feed would
	// have put them after a quiet stretch.
	for i := period - traded; i < period; i++ {
		out[i] = vol
	}

	out[period] = current

	return out
}

// A thin instrument must read as ordinary when it trades its usual amount.
func TestRVOLIgnoresEmptyIntervals(t *testing.T) {
	t.Parallel()

	// 3 of 20 baseline bars traded 10 ticks each; the current bar also traded 10.
	got, ok := RVOLLast(thinWindow(20, 3, 10, 10), 20)
	if !ok {
		t.Fatal("RVOLLast reported no value for a window with traded bars")
	}

	if math.Abs(got-1) > 1e-9 {
		t.Errorf("RVOL = %.2f for a typical bar on a thin instrument, want 1.00.\n"+
			"Averaging the empty intervals into the baseline gives ~6.7, which is "+
			"above the 1.5 default threshold — the detector would fire on every "+
			"traded minute.", got)
	}
}

// A genuine surge must still register.
func TestRVOLStillDetectsASurge(t *testing.T) {
	t.Parallel()

	got, ok := RVOLLast(thinWindow(20, 3, 10, 40), 20)
	if !ok {
		t.Fatal("RVOLLast reported no value")
	}

	if math.Abs(got-4) > 1e-9 {
		t.Errorf("RVOL = %.2f for a bar trading 4x the usual amount, want 4.00", got)
	}
}

// A liquid instrument, where every bar trades, is unaffected by the change.
func TestRVOLUnchangedWhenEveryBarTrades(t *testing.T) {
	t.Parallel()

	vol := make([]float64, 21)
	for i := range vol {
		vol[i] = 100
	}

	vol[20] = 250

	got, ok := RVOLLast(vol, 20)
	if !ok {
		t.Fatal("RVOLLast reported no value")
	}

	if math.Abs(got-2.5) > 1e-9 {
		t.Errorf("RVOL = %.2f, want 2.50", got)
	}
}

// With no baseline there is no answer, and the caller must hold rather than act
// on a fabricated number.
func TestRVOLUndefinedWhenNothingTraded(t *testing.T) {
	t.Parallel()

	quiet := make([]float64, 21)
	quiet[20] = 10 // only the current bar traded

	if _, ok := RVOLLast(quiet, 20); ok {
		t.Error("RVOLLast produced a value with an entirely empty baseline")
	}
}

// A bar in which nothing traded is not a surge.
func TestRVOLOfAnEmptyBarIsZero(t *testing.T) {
	t.Parallel()

	got, ok := RVOLLast(thinWindow(20, 5, 10, 0), 20)
	if !ok {
		t.Fatal("RVOLLast reported no value")
	}

	if got != 0 {
		t.Errorf("RVOL = %.2f for a bar with no trading, want 0", got)
	}
}

// The full-series and last-value forms must agree, or a backtest and the live
// engine read different numbers from the same data.
func TestRVOLSeriesMatchesLast(t *testing.T) {
	t.Parallel()

	window := thinWindow(20, 6, 12, 30)

	series := RVOL(window, 20)
	last, ok := RVOLLast(window, 20)

	if !ok {
		t.Fatal("RVOLLast reported no value")
	}

	if math.Abs(series[len(series)-1]-last) > 1e-9 {
		t.Errorf("RVOL series ends at %.4f but RVOLLast is %.4f",
			series[len(series)-1], last)
	}
}

// VWAP weights by volume, so an empty interval contributes nothing and needs no
// special case — but an entirely empty window must not divide by zero.
func TestVWAPHandlesEmptyIntervals(t *testing.T) {
	t.Parallel()

	high := []float64{10, 10, 10, 10}
	low := []float64{10, 10, 10, 10}
	closes := []float64{10, 10, 10, 10}

	if _, ok := VWAPLast(high, low, closes, []float64{0, 0, 0, 0}, 4); ok {
		t.Error("VWAPLast produced a value from a window in which nothing traded")
	}

	got, ok := VWAPLast(high, low, closes, []float64{0, 0, 5, 0}, 4)
	if !ok {
		t.Fatal("VWAPLast reported no value despite one traded bar")
	}

	if math.Abs(got-10) > 1e-9 {
		t.Errorf("VWAP = %.4f, want 10 (the only traded bar's price)", got)
	}
}
