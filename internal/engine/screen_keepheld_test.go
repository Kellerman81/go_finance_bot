package engine

import (
	"slices"
	"testing"
)

// TestDiffUniverseNeverDropsAHeldSymbolRegardlessOfSelected reproduces (as a
// behavioral lock, not just a documentation fix) the "keep_held: false is
// inert in the live engine" finding: diffUniverse must exempt every held
// symbol from drop unconditionally — whether or not it appears in selected.
// screener.KeepHeld only changes whether a failing held symbol appears in
// selected (see config.ScreenerConfig.KeepHeld's doc comment); it must NOT
// change whether the engine can drop it, which stays a hard invariant
// independent of that flag. This test simulates both settings by varying
// selected directly, without needing a real Screener.
func TestDiffUniverseNeverDropsAHeldSymbolRegardlessOfSelected(t *testing.T) {
	current := []string{"AAPL", "MSFT"}
	held := []string{"MSFT"} // holding a position, currently failing the filters

	t.Run("keep_held=true equivalent: held symbol present in selected", func(t *testing.T) {
		_, drop := diffUniverse(current, []string{"AAPL", "MSFT"}, held)

		if slices.Contains(drop, "MSFT") {
			t.Errorf("drop = %v, want MSFT never dropped while held", drop)
		}
	})

	t.Run("keep_held=false equivalent: held symbol absent from selected", func(t *testing.T) {
		_, drop := diffUniverse(current, []string{"AAPL"}, held)

		if slices.Contains(drop, "MSFT") {
			t.Errorf("drop = %v, want MSFT still never dropped while held, even though "+
				"it failed the filters and keep_held would report it that way", drop)
		}
	})
}

// TestDiffUniverseStillDropsAnUnheldSymbolThatFailedFilters confirms the fix
// (and the invariant test above) doesn't overcorrect into never dropping
// anything: an unheld symbol missing from selected must still be dropped.
func TestDiffUniverseStillDropsAnUnheldSymbolThatFailedFilters(t *testing.T) {
	current := []string{"AAPL", "JUNK"}

	_, drop := diffUniverse(current, []string{"AAPL"}, nil)

	if !slices.Contains(drop, "JUNK") {
		t.Errorf("drop = %v, want JUNK dropped — it is not held and no longer selected", drop)
	}
}
