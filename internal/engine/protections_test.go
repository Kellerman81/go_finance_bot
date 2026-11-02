package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/protect"
)

// protectionStateStore is a minimal in-memory Store that actually persists
// protection state (countingStore's embedded nopStore discards it), so tests
// can verify a value written by one engine is readable by another —
// standing in for a real restart.
type protectionStateStore struct {
	nopStore

	saved protect.State
	has   bool
}

func (s *protectionStateStore) SaveProtectionState(st protect.State) error {
	s.saved, s.has = st, true

	return nil
}

func (s *protectionStateStore) LoadProtectionState() (protect.State, bool, error) {
	return s.saved, s.has, nil
}

// TestSetProtectionsConfigReusesManagerAcrossDisableEnable reproduces the
// disable-destroys-halt finding at the engine wiring level:
// SetProtectionsConfig used to replace e.prot with nil on disable and build a
// brand new *protect.Manager on re-enable, discarding any active halt. It
// must now reuse the same instance throughout a disable/enable round-trip.
func TestSetProtectionsConfigReusesManagerAcrossDisableEnable(t *testing.T) {
	e, _ := forgetEngine()

	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.20})

	first := e.Protections()
	if first == nil {
		t.Fatal("enabling protections built no manager")
	}

	// Trip a halt directly on the manager, as ObserveEquity would.
	first.ObserveEquity(100000)

	if reason := first.ObserveEquity(70000); reason == "" {
		t.Fatal("want a halt at 30% drawdown")
	}

	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: false})

	if e.Protections() != first {
		t.Fatal("disabling protections replaced the manager instance — any halt state is now lost")
	}

	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.20})

	if e.Protections() != first {
		t.Fatal("re-enabling protections built a new manager instead of reusing the existing one")
	}

	if ok, _ := e.Protections().AllowEntry("AAPL"); ok {
		t.Error("entry allowed after a disable/enable round-trip — the halt from before disabling was silently cleared")
	}
}

// TestProtectionHaltSurvivesARestart reproduces the halt-not-persisted
// finding: an active halt must be readable by a fresh Engine instance
// backed by the same store — standing in for a process restart — instead of
// silently clearing because a fresh Manager's peak resets to whatever
// equity is first observed after startup.
func TestProtectionHaltSurvivesARestart(t *testing.T) {
	store := &protectionStateStore{}

	before, _ := forgetEngine()
	before.store = store
	before.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.20})

	p := before.Protections()
	p.ObserveEquity(100000)

	if reason := p.ObserveEquity(70000); reason == "" {
		t.Fatal("want a halt at 30% drawdown")
	}

	before.PersistProtectionState()

	if !store.has {
		t.Fatal("PersistProtectionState did not save anything")
	}

	// A fresh engine, as if the process had just restarted, sharing only the
	// store.
	after, _ := forgetEngine()
	after.store = store
	after.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.20})
	after.RestoreProtectionState()

	if ok, _ := after.Protections().AllowEntry("AAPL"); ok {
		t.Error("entry allowed on a freshly-restarted engine — the halt from before the restart was lost")
	}

	if halted, _ := after.Protections().Halted(); !halted {
		t.Error("Halted() says no after restoring a persisted halt")
	}
}

// TestFeedProtectionsTripsOnRealizedDrawdown reproduces the "count only
// losses" gate end-to-end: a closed trade's realized P&L can trip
// MaxRealizedDrawdownPct on its own — nothing here ever calls ObserveEquity —
// and the resulting halt disables trading exactly like the mark-to-market
// drawdown halt does, via the same haltFromProtections path.
func TestFeedProtectionsTripsOnRealizedDrawdown(t *testing.T) {
	e, _ := forgetEngine()
	e.tradingEnabled = true
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxRealizedDrawdownPct: 0.10})

	p := e.Protections()
	p.ObserveEquity(100000) // seeds peakEquity, the % denominator

	loss := -11000.0
	e.record(TradeRecord{
		Time: time.Now(), Symbol: "AAPL", Side: broker.Sell, Qty: 1, Price: 100,
		Status: "filled", PnL: &loss,
	})

	if e.tradingEnabled {
		t.Error("trading still enabled after a realized-drawdown halt")
	}

	if halted, reason := p.Halted(); !halted || !strings.Contains(reason, "realized drawdown") {
		t.Errorf("Halted() = (%v, %q), want an active realized-drawdown halt", halted, reason)
	}
}

// TestFeedProtectionsIgnoresASimulatedRealizedLoss checks a trade filled
// while test mode was active — TradeRecord.Simulated is stamped true by
// record() — never locks a symbol out or halts trading, however large the
// simulated loss: protections protect the REAL account, and a simulated
// close never touched it.
func TestFeedProtectionsIgnoresASimulatedRealizedLoss(t *testing.T) {
	e, _ := forgetEngine()
	e.tradingEnabled = true
	e.SetProtectionsConfig(config.ProtectionsConfig{
		Enabled: true, MaxRealizedDrawdownPct: 0.10, CooldownAfterLoss: time.Hour,
	})

	p := e.Protections()
	p.ObserveEquity(100000)

	// record() derives Simulated from e.testModeActive itself (overwriting
	// whatever the caller passes), so this — not a Simulated field literal
	// — is what actually puts the trade in the "test mode was active" case.
	e.testModeActive = true

	loss := -50000.0 // far past the 10% realized-drawdown threshold
	e.record(TradeRecord{
		Time: time.Now(), Symbol: "AAPL", Side: broker.Sell, Qty: 1, Price: 100,
		Status: "filled", PnL: &loss,
	})

	if !e.tradingEnabled {
		t.Error("a simulated realized loss disabled trading")
	}

	if halted, reason := p.Halted(); halted {
		t.Errorf("a simulated realized loss halted protections: %q", reason)
	}

	if ok, reason := p.AllowEntry("AAPL"); !ok {
		t.Errorf("a simulated loss locked AAPL out of new entries: %q", reason)
	}
}

// TestFeedProtectionsIgnoresAWinningClose checks a profitable close never
// trips the realized gate and does not spuriously touch tradingEnabled.
func TestFeedProtectionsIgnoresAWinningClose(t *testing.T) {
	e, _ := forgetEngine()
	e.tradingEnabled = true
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxRealizedDrawdownPct: 0.10})
	e.Protections().ObserveEquity(100000)

	win := 500.0
	e.record(TradeRecord{
		Time: time.Now(), Symbol: "AAPL", Side: broker.Sell, Qty: 1, Price: 100,
		Status: "filled", PnL: &win,
	})

	if !e.tradingEnabled {
		t.Error("a winning close disabled trading")
	}
}

// TestCheckDrawdownSkipsWhileTestModeIsActive checks a simulated equity crash
// during test mode neither halts trading nor pollutes the real peakEquity
// baseline that trading resumes against once test mode ends — checkDrawdown
// must not observe equity at all while e.testModeActive is true.
func TestCheckDrawdownSkipsWhileTestModeIsActive(t *testing.T) {
	e, _ := forgetEngine()
	e.tradingEnabled = true
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.10})

	p := e.Protections()
	p.ObserveEquity(100000) // a real peak, established before test mode starts

	e.testModeActive = true
	e.pf.SetCash(1) // a huge simulated equity crash

	e.checkDrawdown()

	if halted, reason := p.Halted(); halted {
		t.Errorf("a simulated equity crash halted protections: %q", reason)
	}

	if !e.tradingEnabled {
		t.Error("a simulated equity crash disabled trading")
	}

	if got := p.Status().PeakEquity; got != 100000 {
		t.Errorf("peakEquity = %v, want the real 100000 preserved — checkDrawdown must not "+
			"observe the simulated equity at all while test mode is active", got)
	}
}

// drawdownEngine is a forgetEngine over store with the drawdown halt at 20%
// and the account's equity set to cash (no positions).
func drawdownEngine(store Store, enabled bool) *Engine {
	e, _ := forgetEngine()
	e.store = store
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: enabled, MaxDrawdownPct: 0.20})

	return e
}

// TestDrawdownInProgressSurvivesARestart is the regression for PR2-1. The
// protection state was saved only when a halt tripped or on Resume, so a
// restart in the MIDDLE of a drawdown reset the peak to the current, already
// lower equity. With a 20% limit, a 100k peak and 85k equity, a restart and a
// further fall to 75k — 25% below the real peak — halted nothing.
func TestDrawdownInProgressSurvivesARestart(t *testing.T) {
	store := &protectionStateStore{}

	before := drawdownEngine(store, true)
	before.pf.SetCash(100000)
	before.checkDrawdown()
	before.pf.SetCash(85000) // -15%: no halt yet
	before.checkDrawdown()

	if halted, _ := before.Protections().Halted(); halted {
		t.Fatal("halted at -15% under a 20% limit; the scenario did not set up")
	}

	after := drawdownEngine(store, true) // the restart, sharing only the store
	after.pf.SetCash(75000)              // -25% from the real 100k peak
	after.checkDrawdown()

	if halted, _ := after.Protections().Halted(); !halted {
		t.Error("equity 25% below the pre-restart peak did not halt — the restart reset the drawdown baseline")
	}
}

// TestHaltSurvivesARestartWhileProtectionsAreOff is the regression for PR2-2.
// A disabled manager is nil from protect.New, so a restart with protections
// off had nothing to restore into, and enabling them later built a clean
// manager: the halt was gone without anyone resuming it, and the next save
// overwrote the stored halt for good.
func TestHaltSurvivesARestartWhileProtectionsAreOff(t *testing.T) {
	store := &protectionStateStore{}

	before := drawdownEngine(store, true)
	before.pf.SetCash(100000)
	before.checkDrawdown()
	before.pf.SetCash(70000) // -30%: halts and persists
	before.checkDrawdown()

	after := drawdownEngine(store, false) // restarted with protections switched off
	after.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.20})

	if halted, _ := after.Protections().Halted(); !halted {
		t.Error("re-enabling protections after a restart dropped the stored halt nobody resumed")
	}
}

// countingProtStore is a protectionStateStore that counts its saves.
type countingProtStore struct {
	protectionStateStore

	saves int
}

func (s *countingProtStore) SaveProtectionState(st protect.State) error {
	s.saves++

	return s.protectionStateStore.SaveProtectionState(st)
}

// TestRisingPeakSavesAreThrottled pins the cost side of the PR2-1 fix:
// checkDrawdown runs every sync, and a climbing account raises the peak on
// most of them. Only a material change (new day, halt, realized counters, a
// lowered peak) saves at once; a peak that merely rose waits for the throttle,
// so the evaluation path does not write the database every 30 s.
func TestRisingPeakSavesAreThrottled(t *testing.T) {
	store := &countingProtStore{}

	e := drawdownEngine(store, true)
	for i := range 20 {
		e.pf.SetCash(100000 + float64(i)*100) // a new peak every sync
		e.checkDrawdown()
	}

	if store.saves != 1 {
		t.Errorf("20 syncs of a rising peak saved %d times, want 1 (the first sight of the day)", store.saves)
	}
}

// TestLoweredPeakIsSavedAtOnce covers the forced PR2-1 save: ForgetOpenPosition
// lowers the peak to undo a phantom holding's inflation, and a restart that
// restored the old peak would trip exactly the false halt LowerPeak prevented.
func TestLoweredPeakIsSavedAtOnce(t *testing.T) {
	store := &countingProtStore{}

	e := drawdownEngine(store, true)
	e.pf.SetCash(100000)
	e.checkDrawdown()

	e.openPos["X"] = &OpenPosition{Symbol: "X", Qty: 10, EntryPrice: 100}
	e.prices.Set("X", 100)
	e.checkDrawdown() // the phantom 1000 is overlaid: peak 101000

	before := store.saves

	e.ForgetOpenPosition("X")

	if store.saves == before {
		t.Fatal("lowering the peak on a forget was not saved")
	}

	if store.saved.PeakEquity >= 101000 {
		t.Errorf("saved peak %v still carries the forgotten phantom", store.saved.PeakEquity)
	}
}

// TestProtectedEquityCountsPendingFills is the regression for PR2-8: Resume
// re-based the protections on the raw broker snapshot, while checkDrawdown
// measures with pending fills overlaid. With a buy still settling at resume
// time the raw figure was low by the cash that buy committed, so the day's
// starting equity was set too low for the rest of the UTC day. Resume now
// uses this, the figure checkDrawdown measures.
func TestProtectedEquityCountsPendingFills(t *testing.T) {
	e, _ := forgetEngine()
	e.pf.SetCash(9000) // 1000 already spent on a fill the broker has not reported

	e.openPos["X"] = &OpenPosition{Symbol: "X", Qty: 10, EntryPrice: 100, EntryTime: time.Now()}
	e.prices.Set("X", 100)

	if got := e.ProtectedEquity(); got != 10000 {
		t.Errorf("ProtectedEquity = %v, want 10000 (9000 cash + the 1000 pending fill)", got)
	}
}
