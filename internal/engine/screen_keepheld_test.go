package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/screener"
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

// TestAProviderOutageDoesNotEmptyTheWatchlist is the regression for SC2-3. A
// failed (or empty) history fetch got the same verdict as a measured
// rejection, so in a pass where Yahoo was down or rate-limiting every symbol
// not held or pinned was dropped — with a notification — and nothing came
// back until the next pass, six hours later by default. A symbol the
// screener could not measure must stay where it is.
func TestAProviderOutageDoesNotEmptyTheWatchlist(t *testing.T) {
	current := []string{"AAPL", "MSFT"}
	verdicts := []screener.Verdict{
		{Symbol: "AAPL", Reason: "history unavailable: 429", Unmeasured: true},
		{Symbol: "MSFT", Reason: "history unavailable: 429", Unmeasured: true},
	}

	_, drop := diffUniverse(current, nil, protectedFromDrop(nil, verdicts))

	if len(drop) != 0 {
		t.Errorf("an outage pass dropped %v, want nothing dropped", drop)
	}
}

// failingHistory is a history source in an outage: every fetch fails.
type failingHistory struct{}

func (failingHistory) Candles(
	context.Context, string, market.Resolution, time.Time, time.Time,
) ([]market.Candle, error) {
	return nil, errors.New("HTTP 429")
}

// TestRescreenDuringAnOutageKeepsTheWatchlist is the end-to-end half of SC2-3:
// through rescreen itself, so reverting the wiring (protectedFromDrop not
// passed to diffUniverse) fails here even though the helper test above passes.
func TestRescreenDuringAnOutageKeepsTheWatchlist(t *testing.T) {
	e := pinEngine(&pinStore{})
	e.pf = portfolio.New()
	e.prices = NewPriceCache()
	e.rm = risk.New(config.Limits{}, config.CostsConfig{})

	if err := e.AddScreened("AAPL", "MSFT"); err != nil {
		t.Fatal(err)
	}

	e.SetHistorySource(failingHistory{})
	e.SetScreenerConfig(config.ScreenerConfig{Enabled: true, Filters: []string{"history"}, MinHistoryBars: 10})

	e.rescreen(context.Background())

	if got := e.Watchlist(); len(got) != 2 {
		t.Errorf("watchlist after a pass where every fetch failed = %v, want both symbols kept", got)
	}
}
