package backtest

import (
	"math"
	"path/filepath"
	"testing"
)

func TestSaveCSVRoundTrip(t *testing.T) {
	orig := Generate([]string{"AAPL", "MSFT"}, 300)
	path := filepath.Join(t.TempDir(), "ds.csv")
	if err := SaveCSV(path, orig); err != nil {
		t.Fatalf("SaveCSV: %v", err)
	}
	loaded, err := LoadCSV(path)
	if err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	if len(loaded) != len(orig) {
		t.Fatalf("symbol count: got %d want %d", len(loaded), len(orig))
	}
	for sym, oc := range orig {
		lc := loaded[sym]
		if len(lc) != len(oc) {
			t.Fatalf("%s: bar count %d != %d", sym, len(lc), len(oc))
		}
		// Spot-check first and last bars survive the round-trip exactly.
		for _, i := range []int{0, len(oc) - 1} {
			if math.Abs(lc[i].Close-oc[i].Close) > 1e-9 || !lc[i].Time.Equal(oc[i].Time) {
				t.Errorf("%s[%d]: mismatch (%v %.6f) vs (%v %.6f)",
					sym, i, lc[i].Time, lc[i].Close, oc[i].Time, oc[i].Close)
			}
		}
	}
}
