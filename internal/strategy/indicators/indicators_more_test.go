package indicators

import (
	"math"
	"testing"
)

func lastVal(t *testing.T, v []float64) float64 {
	t.Helper()
	x, ok := Last(v)
	if !ok {
		t.Fatal("series has no defined value")
	}
	return x
}

func TestATRConstantRange(t *testing.T) {
	n := 30
	high := make([]float64, n)
	low := make([]float64, n)
	cl := make([]float64, n)
	for i := 0; i < n; i++ {
		cl[i] = 100
		high[i] = 100.5
		low[i] = 99.5 // true range = 1.0 each bar
	}
	atr := lastVal(t, ATR(high, low, cl, 14))
	if math.Abs(atr-1.0) > 1e-9 {
		t.Errorf("ATR = %f, want 1.0", atr)
	}
}

func TestBollingerConstant(t *testing.T) {
	vals := make([]float64, 25)
	for i := range vals {
		vals[i] = 50
	}
	mid, up, lo := BollingerBands(vals, 20, 2)
	m := lastVal(t, mid)
	if math.Abs(m-50) > 1e-9 {
		t.Errorf("mid = %f, want 50", m)
	}
	// Zero variance => bands collapse onto the mean.
	if math.Abs(lastVal(t, up)-50) > 1e-9 || math.Abs(lastVal(t, lo)-50) > 1e-9 {
		t.Error("bands should equal mean when variance is zero")
	}
}

func TestVWAPEqualVolume(t *testing.T) {
	n := 10
	h := make([]float64, n)
	l := make([]float64, n)
	c := make([]float64, n)
	v := make([]float64, n)
	for i := 0; i < n; i++ {
		h[i], l[i], c[i] = 100+float64(i), 100+float64(i), 100+float64(i) // typical price = 100+i
		v[i] = 1
	}
	// Over the last 5 bars (i=5..9), typical prices 105..109, equal volume => mean 107.
	got := lastVal(t, VWAP(h, l, c, v, 5))
	if math.Abs(got-107) > 1e-9 {
		t.Errorf("VWAP = %f, want 107", got)
	}
}

func TestRVOLConstantVolume(t *testing.T) {
	v := make([]float64, 30)
	for i := range v {
		v[i] = 1000
	}
	if got := lastVal(t, RVOL(v, 20)); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("RVOL = %f, want 1.0", got)
	}
}

func TestVolumeProfilePOC(t *testing.T) {
	// Most volume concentrated at price 100 => POC bucket should contain 100.
	close := []float64{90, 100, 100, 100, 110, 100, 95, 100, 105, 100}
	vol := []float64{1, 50, 50, 50, 1, 50, 1, 50, 1, 50}
	vp, ok := BuildVolumeProfile(close, vol, len(close), 10, 0.7)
	if !ok {
		t.Fatal("profile not built")
	}
	if math.Abs(vp.POC-100) > 3 { // within one bucket width of 100
		t.Errorf("POC = %f, want ~100", vp.POC)
	}
	if vp.ValueAreaLow > vp.POC || vp.ValueAreaHigh < vp.POC {
		t.Errorf(
			"value area [%f,%f] must contain POC %f",
			vp.ValueAreaLow,
			vp.ValueAreaHigh,
			vp.POC,
		)
	}
}
