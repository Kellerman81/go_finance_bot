package market

import (
	"testing"
	"time"
	_ "time/tzdata" // make tz lookups deterministic regardless of OS zoneinfo
)

func mustLoad(t *testing.T, tz string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %s: %v", tz, err)
	}
	return loc
}

func TestOrderingAllowedUSHours(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	pre := 5 * time.Minute
	// 2026-06-22 is a Monday.
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"midday open", time.Date(2026, 6, 22, 12, 0, 0, 0, ny), true},
		{"before open", time.Date(2026, 6, 22, 8, 0, 0, 0, ny), false},
		{"after close", time.Date(2026, 6, 22, 16, 30, 0, 0, ny), false},
		{"within pre-open window", time.Date(2026, 6, 22, 9, 26, 0, 0, ny), true},
		{"just before pre-open", time.Date(2026, 6, 22, 9, 20, 0, 0, ny), false},
		{"weekend", time.Date(2026, 6, 20, 12, 0, 0, 0, ny), false}, // Saturday
	}
	for _, c := range cases {
		got, reason := OrderingAllowed("AAPL", c.t, pre)
		if got != c.want {
			t.Errorf("%s: OrderingAllowed=%v (%q), want %v", c.name, got, reason, c.want)
		}
	}
}

func TestOrderingAllowedXetra(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	// Xetra 09:00–17:30 local. 2026-06-22 Monday.
	if ok, _ := OrderingAllowed("SAP.DE", time.Date(2026, 6, 22, 10, 0, 0, 0, berlin), 5*time.Minute); !ok {
		t.Error("SAP.DE at 10:00 Berlin should be allowed")
	}
	if ok, _ := OrderingAllowed("SAP.DE", time.Date(2026, 6, 22, 18, 0, 0, 0, berlin), 5*time.Minute); ok {
		t.Error("SAP.DE at 18:00 Berlin (after close) should be blocked")
	}
	// A US symbol during US-morning is still before Xetra... but evaluated on its
	// own (US) calendar it's fine — checked above. Here confirm the DE symbol is
	// gated by Berlin time, not the caller's clock: 10:00 Berlin is allowed even
	// though it's the middle of the night in New York.
}

func TestOrderingAllowedUnknownVenueFailsOpen(t *testing.T) {
	// An unmodelled suffix must not be silently blocked.
	if ok, _ := OrderingAllowed("PRAJL.XC", time.Now(), 5*time.Minute); !ok {
		t.Error("unknown venue should fail open (allowed)")
	}
}

func TestExchangeFor(t *testing.T) {
	if ex, ok := ExchangeFor("AAPL"); !ok || ex.TZ != "America/New_York" {
		t.Errorf("AAPL => %+v ok=%v, want US", ex, ok)
	}
	if ex, ok := ExchangeFor("LXS.DE"); !ok || ex.TZ != "Europe/Berlin" {
		t.Errorf("LXS.DE => %+v ok=%v, want Xetra", ex, ok)
	}
	if _, ok := ExchangeFor("FOO.XC"); ok {
		t.Error("FOO.XC should be an unknown venue")
	}
}
