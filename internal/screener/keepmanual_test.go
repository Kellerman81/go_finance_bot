package screener

import (
	"slices"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// keep_manual makes the screener additive: it may extend the watchlist but not
// prune what the operator chose. These tests cover the three ways a hand-picked
// symbol used to disappear — failing a filter, losing the ranking, and being
// pushed past the cap — plus the cases where it must still be dropped.

// pickHistory has one strong candidate, one ordinary one, and one that fails
// every quality bar.
func pickHistory() fakeHistory {
	return fakeHistory{series: map[string][]market.Candle{
		"BIG":  synth(200, 100, 0.02, 900000),
		"MID":  synth(200, 100, 0.02, 500000),
		"MINE": synth(200, 100, 0.02, 10), // far too thin to pass on merit
	}}
}

// manualCfg is the screener a user would configure to borrow the best few names
// while keeping their own.
func manualCfg(maxSymbols int) config.ScreenerConfig {
	return config.ScreenerConfig{
		Universe:     []string{"BIG", "MID", "MINE"},
		MaxSymbols:   maxSymbols,
		Rank:         "volume",
		MinAvgVolume: 100000, // MINE cannot clear this
		KeepManual:   true,
	}
}

// The whole point: a symbol the operator picked stays even when it fails the
// filters outright.
func TestPinnedSymbolSurvivesAFailedFilter(t *testing.T) {
	t.Parallel()

	s := newScreener(t, manualCfg(0), pickHistory())
	res := s.Screen(t.Context(), Inputs{Pinned: []string{"MINE"}})

	if !slices.Contains(res.Selected, "MINE") {
		t.Errorf("selected = %v, want the hand-picked MINE kept despite failing "+
			"min_avg_volume", res.Selected)
	}
}

// A pinned symbol must not consume one of the max_symbols slots, or turning
// keep_manual on would quietly shrink how many names the screener may find.
// max_symbols becomes "how many to borrow", not "how many in total".
//
// The pinned symbol here is the *highest*-ranked one on purpose. A pinned
// symbol that would lose the ranking anyway proves nothing — it gets appended
// after the cap either way — so only pinning the leader can show whether it
// spent a slot.
func TestPinnedSymbolsDoNotConsumeTheCap(t *testing.T) {
	t.Parallel()

	h := pickHistory()
	h.series["SMALL"] = synth(200, 100, 0.02, 200000) // passes, ranks last

	cfg := manualCfg(2)
	cfg.Universe = []string{"BIG", "MID", "SMALL"}

	res := newScreener(t, cfg, h).Screen(t.Context(), Inputs{Pinned: []string{"BIG"}})

	// BIG is the operator's. The two slots must go to MID and SMALL.
	for _, want := range []string{"BIG", "MID", "SMALL"} {
		if !slices.Contains(res.Selected, want) {
			t.Errorf("selected = %v, want %s.\nBIG is pinned and ranks highest; "+
				"if it spends one of the 2 slots then SMALL is squeezed out and "+
				"the cap has silently become 1 borrowed name, not 2.",
				res.Selected, want)
		}
	}
}

// The verdict still records why the symbol would otherwise have gone, so the
// screener panel does not look like it silently approved a thin instrument.
func TestPinnedSymbolKeepsAnHonestVerdict(t *testing.T) {
	t.Parallel()

	s := newScreener(t, manualCfg(0), pickHistory())
	res := s.Screen(t.Context(), Inputs{Pinned: []string{"MINE"}})

	for _, v := range res.Verdicts {
		if v.Symbol != "MINE" {
			continue
		}

		if !v.Passed {
			t.Fatalf("MINE was not passed: %+v", v)
		}

		if !strings.Contains(v.Reason, "kept") ||
			!strings.Contains(v.Reason, "watchlist") {
			t.Errorf("reason = %q, want it to say the symbol was kept because it "+
				"is on the operator's watchlist, and why it would otherwise have "+
				"been dropped", v.Reason)
		}
	}
}

// With the switch off, nothing changes: the screener still owns the watchlist.
// This is the default, so it is the behaviour most installations get.
func TestPinnedSymbolIsDroppedWhenKeepManualIsOff(t *testing.T) {
	t.Parallel()

	cfg := manualCfg(0)
	cfg.KeepManual = false

	s := newScreener(t, cfg, pickHistory())
	res := s.Screen(t.Context(), Inputs{Pinned: []string{"MINE"}})

	if slices.Contains(res.Selected, "MINE") {
		t.Errorf("selected = %v, want MINE dropped: keep_manual is off, so the "+
			"screener owns the whole watchlist", res.Selected)
	}
}

// A pinned symbol that passes on merit must appear once, not twice — it is
// appended after the cap, and the pre-cap ranking must have skipped it.
func TestPinnedSymbolIsNotSelectedTwice(t *testing.T) {
	t.Parallel()

	s := newScreener(t, manualCfg(0), pickHistory())
	res := s.Screen(t.Context(), Inputs{Pinned: []string{"BIG"}})

	if n := count(res.Selected, "BIG"); n != 1 {
		t.Errorf("BIG appears %d times in %v, want exactly 1", n, res.Selected)
	}
}

// Held and pinned overlap constantly — you tend to hold what you picked. The
// position is the one with money at stake, so it names the reason.
func TestHeldOutranksPinnedInTheReason(t *testing.T) {
	t.Parallel()

	cfg := manualCfg(0)
	cfg.KeepHeld = true

	s := newScreener(t, cfg, pickHistory())
	res := s.Screen(t.Context(), Inputs{
		Held: []string{"MINE"}, Pinned: []string{"MINE"},
	})

	if n := count(res.Selected, "MINE"); n != 1 {
		t.Fatalf("MINE appears %d times in %v, want exactly 1", n, res.Selected)
	}

	for _, v := range res.Verdicts {
		if v.Symbol == "MINE" && !strings.Contains(v.Reason, "position open") {
			t.Errorf("reason = %q, want the open position named: it is the "+
				"reason with money behind it", v.Reason)
		}
	}
}

// count returns how many times sym appears.
func count(syms []string, sym string) int {
	n := 0

	for _, s := range syms {
		if s == sym {
			n++
		}
	}

	return n
}
