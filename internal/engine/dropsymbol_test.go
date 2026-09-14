package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// dropEngine builds an engine that counts losses and can drop a symbol, with a
// real config file on disk so the comment-out path is exercised end to end.
func dropEngine(t *testing.T, dropAfter int, onLoss config.AutotuneConfig) (*Engine, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path,
		[]byte("watchlist:\n  - AAPL\n  - MRVL\n\nlimits:\n  max_order_value: 300\n"),
		0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	e := lossEngine(&lossStore{}, onLoss)
	e.pf = portfolio.New()
	e.protCfg = config.ProtectionsConfig{DropAfterLosses: dropAfter}
	e.configPath = path
	e.watchlist = map[string]struct{}{"AAPL": {}, "MRVL": {}}
	e.pinned = map[string]struct{}{"AAPL": {}, "MRVL": {}}

	return e, path
}

// The headline behaviour: a symbol that keeps losing leaves the watchlist and
// the config file, so the drop survives a restart.
func TestDropAfterLossesRemovesAndCommentsOut(t *testing.T) {
	t.Parallel()

	e, path := dropEngine(t, 3, config.AutotuneConfig{})

	for range 2 {
		e.noteRealizedClose(closed("AAPL", -10))
	}

	if !contains(e.Watchlist(), "AAPL") {
		t.Fatal("dropped before the threshold")
	}

	e.noteRealizedClose(closed("AAPL", -10))

	if contains(e.Watchlist(), "AAPL") {
		t.Fatalf("AAPL still on the watchlist: %v", e.Watchlist())
	}

	if contains(e.Watchlist(), "MRVL") == false {
		t.Error("dropping one symbol removed another")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	got := string(raw)
	if !strings.Contains(got, "# - AAPL") {
		t.Errorf("config entry was not commented out:\n%s", got)
	}

	// The reason and the count stay in the file so the drop is reversible and
	// explicable months later.
	if !strings.Contains(got, "drop_after_losses") || !strings.Contains(got, "3 consecutive") {
		t.Errorf("commented entry lost its reason:\n%s", got)
	}

	if !strings.Contains(got, "  - MRVL") {
		t.Errorf("an unrelated entry was disturbed:\n%s", got)
	}

	// The streak is cleared: a re-added symbol starts fresh.
	if e.LossStreaks()["AAPL"] != 0 {
		t.Error("the streak survived the drop")
	}
}

// Dropping a held symbol would strand the position — no exits, no trailing
// stop, no way for the strategy to sell it. It waits for the close instead.
func TestDropAfterLossesNeverStrandsAPosition(t *testing.T) {
	t.Parallel()

	e, path := dropEngine(t, 2, config.AutotuneConfig{})
	e.pf.SyncPositions([]portfolio.Holding{
		{Symbol: "AAPL", Qty: 3, AvgPrice: 100, MarketValue: 300},
	})

	for range 5 {
		e.noteRealizedClose(closed("AAPL", -10))
	}

	if !contains(e.Watchlist(), "AAPL") {
		t.Fatal("dropped a symbol with an open position")
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "# - AAPL") {
		t.Error("commented out a held symbol")
	}

	// The streak keeps climbing, so the drop happens on the close after the
	// position is finally flat rather than being forgotten.
	if got := e.LossStreaks()["AAPL"]; got != 5 {
		t.Errorf("streak = %d, want it to keep counting while held", got)
	}

	e.pf.SyncPositions(nil)
	e.noteRealizedClose(closed("AAPL", -10))

	if contains(e.Watchlist(), "AAPL") {
		t.Error("did not drop once the position was closed")
	}
}

// A win resets the count, so a symbol that loses twice and recovers is never
// dropped however many times that happens.
func TestDropAfterLossesResetsOnAWin(t *testing.T) {
	t.Parallel()

	e, _ := dropEngine(t, 3, config.AutotuneConfig{})

	for range 10 {
		e.noteRealizedClose(closed("AAPL", -10))
		e.noteRealizedClose(closed("AAPL", -10))
		e.noteRealizedClose(closed("AAPL", 5))
	}

	if !contains(e.Watchlist(), "AAPL") {
		t.Error("dropped a symbol that keeps recovering")
	}
}

// 0 means never, and must not accumulate a streak either — enabling the
// protection later must not fire instantly off history recorded while it was
// off.
func TestDropAfterLossesOffByDefault(t *testing.T) {
	t.Parallel()

	e, _ := dropEngine(t, 0, config.AutotuneConfig{})

	for range 20 {
		e.noteRealizedClose(closed("AAPL", -10))
	}

	if !contains(e.Watchlist(), "AAPL") {
		t.Error("dropped with the protection off")
	}

	if got := e.LossStreaks()["AAPL"]; got != 0 {
		t.Errorf("streak = %d, want none accumulated while off", got)
	}
}

// The drop works with autotune off entirely: it is a protection, not a tuning
// feature, and must not depend on one.
func TestDropAfterLossesWorksWithoutAutotune(t *testing.T) {
	t.Parallel()

	e, _ := dropEngine(t, 2, config.AutotuneConfig{Enabled: false})

	e.noteRealizedClose(closed("AAPL", -1))
	e.noteRealizedClose(closed("AAPL", -1))

	if contains(e.Watchlist(), "AAPL") {
		t.Error("the drop did not fire with autotune disabled")
	}
}

// Both triggers share one honest consecutive-loss counter: on_loss re-tunes
// every N, and the count keeps climbing to the drop threshold rather than
// being reset by the re-tune.
func TestOnLossRetunesRepeatedlyThenDrops(t *testing.T) {
	t.Parallel()

	e, _ := dropEngine(t, 6, onLossCfg(2))

	tunes := 0

	for i := 1; i <= 6; i++ {
		e.noteRealizedClose(closed("AAPL", -10))

		if _, ok := requested(e); ok {
			tunes++
		}
	}

	// Losses 2 and 4 each ask for a re-tune. Loss 6 hits the drop threshold,
	// and giving up wins: once the symbol is off the watchlist there is
	// nothing left to tune, so no search is requested on the way out.
	if tunes != 2 {
		t.Errorf("re-tunes = %d over six losses with after_losses 2 and a drop at 6, want 2", tunes)
	}

	if contains(e.Watchlist(), "AAPL") {
		t.Error("the streak never reached the drop threshold")
	}

	if sym, ok := requested(e); ok {
		t.Errorf("a re-tune was requested for %s on the loss that dropped it", sym)
	}
}

// With no config path the engine must still drop from the running watchlist —
// it simply cannot make it durable, and must not crash trying.
func TestDropAfterLossesWithoutAConfigPath(t *testing.T) {
	t.Parallel()

	e, _ := dropEngine(t, 2, config.AutotuneConfig{})
	e.configPath = ""

	e.noteRealizedClose(closed("AAPL", -1))
	e.noteRealizedClose(closed("AAPL", -1))

	if contains(e.Watchlist(), "AAPL") {
		t.Error("did not drop from the running watchlist")
	}
}

// A missing or unreadable config file is logged, not fatal: the symbol is
// already off the watchlist and the useful part of the drop has happened.
func TestDropAfterLossesToleratesABadConfigPath(t *testing.T) {
	t.Parallel()

	e, _ := dropEngine(t, 2, config.AutotuneConfig{})
	e.configPath = filepath.Join(t.TempDir(), "nope", "config.yaml")

	e.noteRealizedClose(closed("AAPL", -1))
	e.noteRealizedClose(closed("AAPL", -1))

	if contains(e.Watchlist(), "AAPL") {
		t.Error("a bad config path prevented the drop")
	}
}

// droppedReason is what both the file and the trade log show.
func TestDroppedReasonFormat(t *testing.T) {
	t.Parallel()

	got := droppedReason(8, 6, time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"drop_after_losses", "8 consecutive", "max 6", "2026-09-12"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason %q is missing %q", got, want)
		}
	}
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}

	return false
}
