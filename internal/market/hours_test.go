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
	if ok, _ := OrderingAllowed(
		"SAP.DE",
		time.Date(2026, 6, 22, 10, 0, 0, 0, berlin),
		5*time.Minute,
	); !ok {
		t.Error("SAP.DE at 10:00 Berlin should be allowed")
	}
	if ok, _ := OrderingAllowed(
		"SAP.DE",
		time.Date(2026, 6, 22, 18, 0, 0, 0, berlin),
		5*time.Minute,
	); ok {
		t.Error("SAP.DE at 18:00 Berlin (after close) should be blocked")
	}
	// A US symbol during US-morning is still before Xetra... but evaluated on its
	// own (US) calendar it's fine — checked above. Here confirm the DE symbol is
	// gated by Berlin time, not the caller's clock: 10:00 Berlin is allowed even
	// though it's the middle of the night in New York.
}

// An unmodelled suffix must not be silently blocked — including at the weekend,
// because this same answer is what gates an EXIT, and a holding nobody can put
// on a calendar must never become unsellable.
func TestOrderingAllowedUnknownVenueFailsOpen(t *testing.T) {
	sat := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) // a Saturday
	wed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	for _, at := range []time.Time{sat, wed} {
		if ok, why := OrderingAllowed("FOO.ZZ", at, 5*time.Minute); !ok {
			t.Errorf("unknown venue on %s should fail open (allowed): %s",
				at.Weekday(), why)
		}
	}
}

// The weekend needs no venue calendar, which is what lets the ENTRY path be
// stricter than OrderingAllowed without teaching it a session it does not know.
func TestIsWeekendUTC(t *testing.T) {
	cases := map[string]struct {
		at   time.Time
		want bool
	}{
		"saturday":     {time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC), true},
		"sunday":       {time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC), true},
		"friday":       {time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC), false},
		"monday":       {time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC), false},
		"friday in NY": {time.Date(2026, 9, 11, 20, 0, 0, 0, mustLoad(t, "America/New_York")), true},
	}

	for name, c := range cases {
		if got := IsWeekendUTC(c.at); got != c.want {
			t.Errorf("%s: IsWeekendUTC = %v, want %v", name, got, c.want)
		}
	}
}

// PRAJL.XC is a Cboe UK listing. Unmapped, it was the one watchlist symbol the
// hours gate waved through all weekend — and every evaluation behind it
// recorded a blocked trade for a venue price that cannot exist on a Saturday.
func TestCboeUKIsMapped(t *testing.T) {
	london := mustLoad(t, "Europe/London")

	ex, ok := ExchangeFor("PRAJL.XC")
	if !ok || ex.TZ != "Europe/London" {
		t.Fatalf("PRAJL.XC => %+v ok=%v, want a London venue", ex, ok)
	}

	if ok, _ := OrderingAllowed(
		"PRAJL.XC", time.Date(2026, 9, 12, 12, 0, 0, 0, london), 5*time.Minute,
	); ok {
		t.Error("PRAJL.XC on a Saturday should be closed")
	}

	if ok, why := OrderingAllowed(
		"PRAJL.XC", time.Date(2026, 9, 14, 10, 0, 0, 0, london), 5*time.Minute,
	); !ok {
		t.Errorf("PRAJL.XC at 10:00 London on a Monday should be allowed: %s", why)
	}
}

func TestExchangeFor(t *testing.T) {
	if ex, ok := ExchangeFor("AAPL"); !ok || ex.TZ != "America/New_York" {
		t.Errorf("AAPL => %+v ok=%v, want US", ex, ok)
	}
	if ex, ok := ExchangeFor("LXS.DE"); !ok || ex.TZ != "Europe/Berlin" {
		t.Errorf("LXS.DE => %+v ok=%v, want Xetra", ex, ok)
	}
	if _, ok := ExchangeFor("FOO.ZZ"); ok {
		t.Error("FOO.ZZ should be an unknown venue")
	}
}

// TestOrderingAllowedCrypto reproduces the crypto-trading-hours finding:
// Binance pairs like "BTCUSDT" have no Yahoo-style dot suffix, so before
// this fix ExchangeFor fell through to the "" (no-suffix) case and applied
// NYSE hours to a market that trades continuously — blocking entries ~76%
// of the time and deferring every protective exit over the weekend.
func TestOrderingAllowedCrypto(t *testing.T) {
	ny := mustLoad(t, "America/New_York")

	cases := []struct {
		symbol string
		t      time.Time
	}{
		// A Saturday at 3am New York time — about as "closed" as a US-hours
		// gate could read it.
		{"BTCUSDT", time.Date(2026, 6, 20, 3, 0, 0, 0, ny)},
		{"ETHEUR", time.Date(2026, 6, 20, 3, 0, 0, 0, ny)},
		{"BNBUSD", time.Date(2026, 6, 20, 3, 0, 0, 0, ny)},
	}

	for _, c := range cases {
		if ok, reason := OrderingAllowed(c.symbol, c.t, 5*time.Minute); !ok {
			t.Errorf("%s on a weekend should always be allowed (24/7 venue), got blocked: %q", c.symbol, reason)
		}
	}

	ex, ok := ExchangeFor("BTCUSDT")
	if !ok || ex.Name != "Crypto" {
		t.Errorf("BTCUSDT => %+v ok=%v, want the 24/7 Crypto venue", ex, ok)
	}

	// A genuine no-suffix equity ticker must not be misclassified just
	// because ExchangeFor now also checks for crypto pair patterns.
	if ex, ok := ExchangeFor("AAPL"); !ok || ex.Name != "US" {
		t.Errorf("AAPL => %+v ok=%v, want US (not misclassified as crypto)", ex, ok)
	}
}

// TestUSCryptoETFsKeepTheirExchangeHours is the regression for MK2-5. The
// crypto-pair heuristic needed one character before a quote currency, so GBTC,
// FBTC, FETH, CETH, EETH and SETH — US-listed funds — came back as known 24/7
// venues: OrderingAllowed returned true on a Saturday and the engine's weekend
// guard for unknown venues was skipped, so a weekend buy was queued at the
// broker for Monday's open.
func TestUSCryptoETFsKeepTheirExchangeHours(t *testing.T) {
	t.Parallel()

	for _, sym := range []string{"GBTC", "FBTC", "FETH", "CETH", "EETH", "SETH"} {
		if ex, _ := ExchangeFor(sym); ex.Name == exCrypto.Name {
			t.Errorf("%s is a US-listed fund, read as a 24/7 crypto pair", sym)
		}
	}

	for _, pair := range []string{"BTCUSDT", "OPUSDT", "ETHBTC", "WUSDT", "SUSDT"} {
		if ex, _ := ExchangeFor(pair); ex.Name != exCrypto.Name {
			t.Errorf("%s is a crypto pair, read as %q", pair, ex.Name)
		}
	}
}

// MK2-8: the German regional suffixes were gated to Xetra's 09:00-17:30, so
// a stop on a Stuttgart holding that triggered at 18:00 waited for the next
// morning while Stuttgart was open — on a broker with no schedule the hours
// gate holds exits too.
func TestGermanRegionalVenuesKeepTheirOwnSessions(t *testing.T) {
	t.Parallel()

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata")
	}

	wed := func(h, m int) time.Time { return time.Date(2026, 10, 14, h, m, 0, 0, berlin) }

	cases := []struct {
		symbol string
		at     time.Time
		open   bool
	}{
		{"LX9B.SG", wed(18, 0), true},
		{"LX9B.SG", wed(21, 30), true},
		{"LX9B.SG", wed(22, 30), false},
		{"2HP.F", wed(19, 0), true},
		{"2HP.F", wed(20, 30), true}, // MK3-6: Frankfurt trades shares until 22:00
		{"2HP.F", wed(22, 30), false},
		{"SAP.DE", wed(18, 0), false}, // Xetra itself is unchanged
	}

	for _, c := range cases {
		if got, why := OrderingAllowed(c.symbol, c.at, 0); got != c.open {
			t.Errorf("%s at %s: allowed = %v (%s), want %v", c.symbol, c.at.Format("15:04"), got, why, c.open)
		}
	}
}
