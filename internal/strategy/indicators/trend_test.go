package indicators

import (
	"math"
	"testing"
)

func TestLinRegPerfectUptrend(t *testing.T) {
	// y = 100 + 2x is a perfect line: slope 2, intercept 100, R² 1.
	v := make([]float64, 10)
	for i := range v {
		v[i] = 100 + 2*float64(i)
	}
	slope, intercept, r2, ok := LinReg(v, 0)
	if !ok {
		t.Fatal("LinReg not ok on a clean line")
	}
	if math.Abs(slope-2) > 1e-9 || math.Abs(intercept-100) > 1e-9 {
		t.Errorf("slope/intercept = %.4f/%.4f, want 2/100", slope, intercept)
	}
	if math.Abs(r2-1) > 1e-9 {
		t.Errorf("R² = %.6f, want 1", r2)
	}
}

func TestLinRegDowntrend(t *testing.T) {
	v := []float64{50, 49, 48, 47, 46, 45}
	slope, _, r2, ok := LinReg(v, 0)
	if !ok || slope >= 0 {
		t.Fatalf("expected negative slope, got %.4f (ok=%v)", slope, ok)
	}
	if r2 < 0.99 {
		t.Errorf("clean downtrend R² = %.4f, want ~1", r2)
	}
}

func TestLinRegFlatAndNoise(t *testing.T) {
	flat := []float64{10, 10, 10, 10, 10}
	slope, _, r2, ok := LinReg(flat, 0)
	if !ok || math.Abs(slope) > 1e-9 {
		t.Errorf("flat slope = %.6f (ok=%v), want 0", slope, ok)
	}
	if r2 != 1 {
		t.Errorf("flat R² = %.4f, want 1", r2)
	}

	// A symmetric zig-zag has ~zero slope and a poor fit (low R²).
	noise := []float64{10, 11, 10, 11, 10, 11}
	_, _, r2n, _ := LinReg(noise, 0)
	if r2n > 0.5 {
		t.Errorf("noisy series R² = %.4f, want low", r2n)
	}
}

func TestLinRegUsesLastPeriod(t *testing.T) {
	// First half flat, second half rising: a short period sees only the rise.
	v := []float64{5, 5, 5, 5, 5, 6, 7, 8, 9, 10}
	slope, _, _, ok := LinReg(v, 5)
	if !ok || slope <= 0.9 {
		t.Errorf("period-limited slope = %.4f (ok=%v), want ~1", slope, ok)
	}
}

func TestLinRegTooFewPoints(t *testing.T) {
	if _, _, _, ok := LinReg([]float64{42}, 0); ok {
		t.Error("LinReg should not be ok with a single point")
	}
}
