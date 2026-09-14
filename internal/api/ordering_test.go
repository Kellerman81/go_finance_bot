package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// Both sources of log rows already hand back newest-first: Engine.Trades and
// errlog.Recent each reverse their internal append-order buffer before
// returning. Reversing again in the view puts the oldest row on top — and in
// recent()'s case also picks the wrong n, because taking the tail of a
// newest-first slice takes the OLDEST entries.

// recent must keep the newest n, in newest-first order.
func TestRecentKeepsTheNewestEntriesNewestFirst(t *testing.T) {
	t.Parallel()

	// As Engine.Trades hands them over: newest first.
	all := []int{100, 99, 98, 97, 96, 95}

	got := recent(all, 3)
	want := []int{100, 99, 98}

	if !slices.Equal(got, want) {
		t.Errorf("recent(newest-first, 3) = %v, want %v.\nTaking the tail of a "+
			"newest-first slice returns the OLDEST entries, so the dashboard "+
			"showed the oldest decisions it still had, bottom-up.", got, want)
	}
}

// Fewer entries than the limit must still come back newest-first.
func TestRecentBelowTheLimitKeepsItsOrder(t *testing.T) {
	t.Parallel()

	all := []int{3, 2, 1}

	if got := recent(all, 10); !slices.Equal(got, all) {
		t.Errorf("recent = %v, want %v unchanged", got, all)
	}
}

// recent must not alias or reorder the caller's slice: Engine.Trades hands back
// a fresh copy, but errlog and future callers may not.
func TestRecentDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()

	all := []int{5, 4, 3, 2, 1}
	before := slices.Clone(all)

	recent(all, 2)

	if !slices.Equal(all, before) {
		t.Errorf("input became %v, was %v", all, before)
	}
}

// The API error log is newest-first at the source too.
func TestErrorLogIsNewestFirst(t *testing.T) {
	errlog.Clear()
	t.Cleanup(errlog.Clear)

	errlog.Record("finnhub", "first")
	errlog.Record("saxo", "second")
	errlog.Record("yahoo", "third")

	got := recentErrors()
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}

	if got[0].Message != "third" {
		t.Errorf("top entry is %q, want the newest (%q)", got[0].Message, "third")
	}
}

// End-to-end: the decision log on the trades screen must lead with the newest
// row. This is the one the operator actually reads.
func TestDecisionLogLeadsWithTheNewestTrade(t *testing.T) {
	s := newTestServer(t)

	for _, sym := range []string{"OLDEST", "MIDDLE", "NEWEST"} {
		s.prices.Set(sym, 100)

		if _, err := s.eng.ManualOrder(context.Background(), sym, broker.Buy, 1); err != nil {
			t.Fatalf("seed %s: %v", sym, err)
		}
	}

	rec := fetch(t, s, "/v2/f/trades")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	body := rec.Body.String()

	first := strings.Index(body, "NEWEST")
	last := strings.Index(body, "OLDEST")

	if first < 0 || last < 0 {
		t.Fatalf("seeded symbols missing from the log (NEWEST=%d OLDEST=%d)", first, last)
	}

	if first > last {
		t.Error("the decision log leads with the oldest entry; newest must be on top")
	}
}
