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
