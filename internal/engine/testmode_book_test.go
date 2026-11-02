package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/protect"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// EN3-1: simulated fills wrote the REAL open-position records. After test mode
// ended, pendingExposure read a simulated buy's record as a fill still
// settling, lifted the drawdown peak by its value, and when the 10-minute
// overlay expired equity fell back and the max-drawdown halt fired on an
// account that never moved. These are the TM-3 incident's numbers (650 cash
// + 260 stock, ~400 of simulated buys; the unexplained 09:19 halt read peak
// 1310.98 against 909.89).
func TestSimulatedBuysDoNotTripAFalseHaltAfterTestMode(t *testing.T) {
	real := &fakeRealBroker{
		cash:      650,
		positions: []broker.Position{{Symbol: "MSFT", Qty: 1, AvgPrice: 260, Current: 260}},
	}
	e, _ := testModeEngine(real)
	e.prices.Set("MSFT", 260)
	e.prices.Set("AAPL", 100)
	e.barsStore = newBarStore(10, time.Minute)
	e.SetProtections(protect.New(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.25}))

	e.syncPortfolio(context.Background())

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := e.ManualOrder(context.Background(), "AAPL", broker.Buy, 4); err != nil {
		t.Fatal(err)
	}

	e.mu.RLock()
	_, phantom := e.openPos["AAPL"]
	e.mu.RUnlock()
	if !phantom {
		t.Fatal("the simulated buy made no record; the scenario did not set up")
	}

	e.EndTestMode("done") // syncs the real broker → checkDrawdown

	restore := now
	defer func() { now = restore }()
	at := time.Now().Add(pendingFillGrace + time.Minute)
	now = func() time.Time { return at }

	e.syncPortfolio(context.Background())

	if halted, why := e.Protections().Halted(); halted {
		t.Errorf("real account never moved, yet: %s", why)
	}

	e.mu.RLock()
	_, still := e.openPos["AAPL"]
	e.mu.RUnlock()

	if still {
		t.Errorf("simulated AAPL record survived test mode as a real open position")
	}
}

// EN3-1's other direction: a simulated sale of a REAL holding deleted its
// record, losing its peak, scale-out progress and entry time; the holding was
// later re-adopted as new.
func TestASimulatedSaleKeepsTheRealRecord(t *testing.T) {
	real := &fakeRealBroker{
		cash:      650,
		positions: []broker.Position{{Symbol: "MSFT", Qty: 1, AvgPrice: 260, Current: 260}},
	}
	e, _ := testModeEngine(real)
	e.prices.Set("MSFT", 260)
	e.barsStore = newBarStore(10, time.Minute)
	e.openPos["MSFT"] = &OpenPosition{Symbol: "MSFT", Qty: 1, EntryPrice: 260,
		EntryTime: time.Now().Add(-72 * time.Hour), PartialsTaken: 1, Peak: 300}

	e.syncPortfolio(context.Background())

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := e.ManualOrder(context.Background(), "MSFT", broker.Sell, 1); err != nil {
		t.Fatal(err)
	}

	e.EndTestMode("done")

	e.mu.RLock()
	_, ok := e.openPos["MSFT"]
	e.mu.RUnlock()

	if !ok {
		t.Fatal("the real MSFT position (still held at the real broker) lost its record to a simulated sale")
	}

	if p := e.openPos["MSFT"]; p.PartialsTaken != 1 || p.Peak != 300 {
		t.Errorf("the real record came back altered: %+v", *p)
	}
}

type bookStore struct {
	countingStore
	mu     sync.Mutex
	day    string
	amount float64
	rows   map[string]OpenPosition
}

func (s *bookStore) SaveDailySpend(day string, amount float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.day, s.amount = day, amount
	return nil
}
func (s *bookStore) SaveOpenPosition(p OpenPosition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[p.Symbol] = p
	return nil
}
func (s *bookStore) DeleteOpenPosition(sym string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, sym)
	return nil
}

// SG3-1: TM-3 handed the real daily spend back in memory only. Every simulated
// buy had persisted the inflated total, so a restart later that UTC day
// restored it and refused real buys against a budget simulated orders spent.
func TestPersistedSpendExcludesSimulatedBuys(t *testing.T) {
	e, _ := testModeEngine(&fakeRealBroker{cash: 5000})
	st := &bookStore{rows: map[string]OpenPosition{}}
	e.store = st
	e.rm = risk.New(config.Limits{MaxDailySpend: 500}, config.CostsConfig{})

	e.rm.RecordBuy(60)
	e.persistSpend()

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	e.rm.RecordBuy(200)
	e.persistSpend()
	e.rm.RecordBuy(200)
	e.persistSpend()

	// A crash or restart DURING the window restores whatever was persisted.
	if st.amount != 60 {
		t.Errorf("mid-window the store holds spend %v, want the real 60", st.amount)
	}

	e.EndTestMode("done")

	// "restart"
	rm2 := risk.New(config.Limits{MaxDailySpend: 500}, config.CostsConfig{})
	rm2.Restore(st.day, st.amount)
	if got := rm2.SpentToday(); got != 60 {
		t.Errorf("after restart spentToday = %v, want 60 (simulated buys came back from the store)", got)
	}
}

// SG3-2: a simulated buy wrote a durable open_positions row for stock the real
// broker never bought; it outlived test mode and every restart.
func TestASimulatedOpenPositionDoesNotOutliveTestMode(t *testing.T) {
	e, _ := testModeEngine(&fakeRealBroker{cash: 5000})
	st := &bookStore{rows: map[string]OpenPosition{}}
	e.store = st

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	e.recordOpen("AAPL", 2, 150, "")

	if _, ok := st.rows["AAPL"]; ok {
		t.Error("a simulated buy was persisted mid-window; a crash now would restore it as real")
	}

	e.EndTestMode("done")

	if _, ok := st.rows["AAPL"]; ok {
		t.Errorf("persisted open position for a simulated-only buy survives test mode: %+v", st.rows["AAPL"])
	}
	if _, ok := e.openPos["AAPL"]; ok {
		t.Errorf("in-memory open position for a simulated-only buy survives test mode")
	}
}

// PR3-2: a real limit order still resting when test mode starts is filled by
// the REAL broker inside the window but booked as simulated — so the
// protections never saw a real loss — and the book restore would undo it.
// Test mode waits for real working orders instead.
func TestTestModeWaitsForWorkingRealOrders(t *testing.T) {
	e, _ := testModeEngine(&fakeRealBroker{cash: 5000})
	done := e.trackWatcher("SAP.DE")

	err := e.EnableTestMode(context.Background(), time.Hour)
	if err == nil || !strings.Contains(err.Error(), "still working") {
		t.Fatalf("test mode started with a real limit order resting: err = %v", err)
	}

	done()

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("test mode refused with nothing working: %v", err)
	}
}
