package engine

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// countingStore records which open-position rows were deleted, so the test can
// prove the durable row goes with the in-memory one.
type countingStore struct {
	nopStore

	deleted []string
	saved   []OpenPosition
}

// DeleteOpenPosition records the symbol and reports success.
func (s *countingStore) DeleteOpenPosition(symbol string) error {
	s.deleted = append(s.deleted, symbol)

	return nil
}

// SaveOpenPosition records the written row, so tests can prove that state which
// must survive a restart (ladder progress, the trailing peak) was persisted.
func (s *countingStore) SaveOpenPosition(p OpenPosition) error {
	s.saved = append(s.saved, p)

	return nil
}

// forgetEngine builds the minimum Engine that ForgetOpenPosition touches.
func forgetEngine() (*Engine, *countingStore) {
	st := &countingStore{}
	e := &Engine{
		openPos:     make(map[string]*OpenPosition),
		peaks:       make(map[string]float64),
		pendingSell: make(map[string]float64),
		unconfirmed: make(map[string]UnconfirmedOrder),
		driftSince:  make(map[string]time.Time),
		pf:          portfolio.New(),
		prices:      NewPriceCache(),
		store:       st,
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	return e, st
}

// A position closed by hand in the broker's own app leaves a record describing
// stock that is not there. Forgetting it must clear every trace that would keep
// the engine acting on the phantom holding.
func TestForgetOpenPositionClearsEveryTrace(t *testing.T) {
	e, st := forgetEngine()

	e.openPos["PRAJL.XC"] = &OpenPosition{
		Symbol: "PRAJL.XC", Qty: 3.0926, EntryPrice: 32.33, EntryTime: time.Now(),
	}
	e.peaks["PRAJL.XC"] = 36.84
	e.pendingSell["PRAJL.XC"] = 3.0926

	if !e.ForgetOpenPosition("PRAJL.XC") {
		t.Fatal("ForgetOpenPosition reported no record for a symbol that has one")
	}

	if _, ok := e.openPos["PRAJL.XC"]; ok {
		t.Error("the open-position record survived")
	}

	if _, ok := e.peaks["PRAJL.XC"]; ok {
		t.Error("the trailing-stop peak survived; it would arm exits on a phantom holding")
	}

	if _, ok := e.pendingSell["PRAJL.XC"]; ok {
		t.Error("the pending-sell guard survived; it would suppress a later legitimate exit")
	}

	if len(st.deleted) != 1 || st.deleted[0] != "PRAJL.XC" {
		t.Errorf("durable row not deleted: %v", st.deleted)
	}
}

// No trade is booked: the bot does not know what the manual sale filled at, and
// a made-up exit price would corrupt realized P&L and the win/loss stats.
func TestForgetOpenPositionBooksNoTrade(t *testing.T) {
	e, _ := forgetEngine()

	e.openPos["X"] = &OpenPosition{Symbol: "X", Qty: 1, EntryPrice: 10}
	e.ForgetOpenPosition("X")

	if len(e.trades) != 0 {
		t.Fatalf("forgetting a position booked %d trade(s); it must book none", len(e.trades))
	}
}

// An unknown symbol reports false so the API can 404 instead of claiming a
// success that did nothing.
func TestForgetUnknownOpenPositionReportsFalse(t *testing.T) {
	e, st := forgetEngine()

	if e.ForgetOpenPosition("NOPE") {
		t.Error("ForgetOpenPosition reported success for a symbol it has no record of")
	}

	if len(st.deleted) != 0 {
		t.Errorf("a missing record still hit the store: %v", st.deleted)
	}
}
