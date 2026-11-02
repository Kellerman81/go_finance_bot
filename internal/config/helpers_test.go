package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBalancedProfileSitsBetweenTheOthers extends TestProfilesAreCoherent to
// the middle profile, which nothing else applied: "balanced" must sit between
// conservative and aggressive on every axis that orders them, and like the
// others it must leave the protections and exits on.
func TestBalancedProfileSitsBetweenTheOthers(t *testing.T) {
	t.Parallel()

	get := func(name string) Config {
		c, err := ApplyProfile(Default(), name)
		if err != nil {
			t.Fatalf("ApplyProfile(%s): %v", name, err)
		}

		return c
	}

	cons, bal, aggr := get("conservative"), get(" Balanced "), get("aggressive")

	between := func(what string, lo, mid, hi float64) {
		t.Helper()

		if (mid < lo && mid < hi) || (mid > lo && mid > hi) {
			t.Errorf("balanced %s = %v is outside conservative %v / aggressive %v", what, mid, lo, hi)
		}
	}

	between("min_strength", cons.Strategy.MinStrength, bal.Strategy.MinStrength, aggr.Strategy.MinStrength)
	between("confirm_buy", float64(cons.Engine.ConfirmBuy), float64(bal.Engine.ConfirmBuy),
		float64(aggr.Engine.ConfirmBuy))
	between("min_positions", float64(cons.Engine.MinPositions), float64(bal.Engine.MinPositions),
		float64(aggr.Engine.MinPositions))
	between("max_drawdown_pct", cons.Protections.MaxDrawdownPct, bal.Protections.MaxDrawdownPct,
		aggr.Protections.MaxDrawdownPct)
	between("stop_loss_pct", cons.Exits.StopLossPct, bal.Exits.StopLossPct, aggr.Exits.StopLossPct)

	if !bal.Protections.Enabled || !bal.Exits.Enabled {
		t.Error("balanced profile disabled protections or exits")
	}
}

// TestFindOnLineStaysOnItsLine guards the patcher's value locator: it must
// search ONLY the given line. A search that ran on into the next line would
// splice a new value over the first later occurrence of the same text — the
// "0" of the next key, say — and corrupt a different setting.
func TestFindOnLineStaysOnItsLine(t *testing.T) {
	t.Parallel()

	src := []byte("a: 0\nb: 1\nc: 0\n")
	lines := []int{0, 5, 10}

	if off, ok := findOnLine(src, lines, 0, "0"); !ok || off != 3 {
		t.Errorf("line 0: off=%d ok=%v, want 3 true", off, ok)
	}

	if _, ok := findOnLine(src, lines, 1, "0"); ok {
		t.Error("line 1 has no 0, but findOnLine found one (it read past its line)")
	}

	// The last line has no successor offset: the search runs to EOF.
	if off, ok := findOnLine(src, lines, 2, "0"); !ok || off != 13 {
		t.Errorf("last line: off=%d ok=%v, want 13 true", off, ok)
	}
}

// TestPatchCreatesAMissingNestedBlock guards planParent, the insert path
// for a leaf whose parent block the file does not have yet: saving the
// on_loss settings into an autotune: block written before on_loss existed
// must create "on_loss:" under autotune, not refuse the save or write the
// leaf at the wrong depth — and the result must still load.
func TestPatchCreatesAMissingNestedBlock(t *testing.T) {
	t.Parallel()

	src := "autotune:\n  enabled: false\n  interval: 24h\n"

	want := Default().Autotune
	want.OnLoss.Enabled = true
	want.OnLoss.AfterLosses = 3

	out := patched(t, src, "autotune", want)

	if !strings.Contains(out, "\n  on_loss:") {
		t.Fatalf("no on_loss block created under autotune:\n%s", out)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("patched file does not load: %v\n%s", err, out)
	}

	if !got.Autotune.OnLoss.Enabled || got.Autotune.OnLoss.AfterLosses != 3 {
		t.Errorf("on_loss read back as %+v", got.Autotune.OnLoss)
	}
}

// TestCostHelpers guards the two cost formulas both live sizing and the
// backtester charge: commission has a minimum and is symmetric in side, and
// the configured spread is a FLOOR a measurement can raise but never lower —
// letting a quiet stretch of history estimate the cost below budget would
// flatter every backtest.
func TestCostHelpers(t *testing.T) {
	t.Parallel()

	c := CostsConfig{CommissionFlat: 1, CommissionPct: 0.001, CommissionMin: 2, SpreadPct: 0.001}

	for _, tc := range []struct{ notional, want float64 }{
		{0, 2},        // 1 flat, under the minimum
		{500, 2},      // 1.5, under the minimum
		{5000, 6},     // 1 + 5
		{-5000, 6},    // a sell costs the same
		{100000, 101}, // 1 + 100
	} {
		if got := c.Commission(tc.notional); got != tc.want {
			t.Errorf("Commission(%v) = %v, want %v", tc.notional, got, tc.want)
		}
	}

	if got := c.SpreadFor(0.01); got != 0.001 {
		t.Errorf("SpreadFor with measure_spread off = %v, want the configured 0.001", got)
	}

	c.MeasureSpread = true

	if got := c.SpreadFor(0.01); got != 0.01 {
		t.Errorf("SpreadFor(0.01) = %v, want the wider measurement", got)
	}

	if got := c.SpreadFor(0.0001); got != 0.001 {
		t.Errorf("SpreadFor(0.0001) = %v, want the 0.001 floor", got)
	}

	if got := c.SpreadFor(0); got != 0.001 {
		t.Errorf("SpreadFor(unknown) = %v, want the 0.001 floor", got)
	}
}

// TestTightestPerOrderCap guards the optimizer's search-space ceiling: it is
// the smaller of max_order_value and max_per_position, with percentages
// resolved against the equity the search runs on. A percentage-only config
// that resolved to "no cap" would make the search fall back to its default
// fraction and test order sizes the live limits would never allow.
func TestTightestPerOrderCap(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		l      Limits
		equity float64
		want   float64
	}{
		{"none", Limits{}, 10000, 0},
		{"order only", Limits{MaxOrderValue: 500}, 10000, 500},
		{"position only", Limits{MaxPerPosition: 800}, 10000, 800},
		{"smaller wins", Limits{MaxOrderValue: 500, MaxPerPosition: 300}, 10000, 300},
		{"pct resolves", Limits{MaxPerPositionPct: 0.05}, 10000, 500},
		{"pct and absolute", Limits{MaxOrderValue: 400, MaxPerPositionPct: 0.05}, 10000, 400},
		{"book caps ignored", Limits{MaxTotalInvested: 100, CashReserve: 50}, 10000, 0},
	} {
		if got := tc.l.TightestPerOrderCap(tc.equity); got != tc.want {
			t.Errorf("%s: TightestPerOrderCap = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSaxoAuthURLFollowsTheGateway guards Saxo's OAuth host: a sim gateway
// paired with the live logon host (or the reverse) fails every token
// exchange, so the derived host must follow the gateway and an explicit
// auth_url must win.
func TestSaxoAuthURLFollowsTheGateway(t *testing.T) {
	t.Parallel()

	sim := SaxoConfig{BaseURL: "https://gateway.saxobank.com/sim/openapi"}
	if got := sim.SaxoAuthURL(); got != "https://sim.logonvalidation.net" {
		t.Errorf("sim gateway -> %q", got)
	}

	live := SaxoConfig{BaseURL: "https://gateway.saxobank.com/openapi"}
	if got := live.SaxoAuthURL(); got != "https://live.logonvalidation.net" {
		t.Errorf("live gateway -> %q", got)
	}

	explicit := SaxoConfig{BaseURL: sim.BaseURL, AuthURL: "https://example.test"}
	if got := explicit.SaxoAuthURL(); got != "https://example.test" {
		t.Errorf("explicit auth_url -> %q", got)
	}
}

// TestBrokerProviderResolution guards which broker the bot trades through:
// an explicit broker: wins (spelling-insensitive, "t212" accepted), and only
// a legacy config with Alpaca keys and use_sim off may fall back to Alpaca —
// anything else must land on the paper Sim, never on a real account by
// accident.
func TestBrokerProviderResolution(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		c    Config
		want string
	}{
		{"saxo", Config{Broker: " Saxo "}, "saxo"},
		{"alpaca", Config{Broker: "alpaca"}, "alpaca"},
		{"t212 alias", Config{Broker: "T212"}, "trading212"},
		{"trading212", Config{Broker: "trading212"}, "trading212"},
		{"sim", Config{Broker: "sim", Alpaca: AlpacaConfig{KeyID: "k"}}, "sim"},
		{"legacy alpaca keys", Config{Alpaca: AlpacaConfig{KeyID: "k"}}, "alpaca"},
		{"legacy alpaca use_sim", Config{Alpaca: AlpacaConfig{KeyID: "k", UseSim: true}}, "sim"},
		{"nothing", Config{}, "sim"},
		{"unknown name", Config{Broker: "ibkr"}, "sim"},
	} {
		if got := tc.c.BrokerProvider(); got != tc.want {
			t.Errorf("%s: BrokerProvider = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestResolveOverridesFileReLayersOntoANewBase guards AT3-2: after a shared
// strategy save, each override must inherit the NEW base for every field it
// does not name, exactly as a restart would — and a missing file is an
// error, not an empty map that would silently drop every override.
func TestResolveOverridesFileReLayersOntoANewBase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
strategy:
  detectors: ["ema_cross", "rsi"]
  min_strength: 0.30
strategy_overrides:
  MSFT:
    min_strength: 0.50
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	base := Default().Strategy
	base.Detectors = []string{"macd", "trend"}
	base.MinStrength = 0.10

	got, err := ResolveOverridesFile(path, base)
	if err != nil {
		t.Fatalf("ResolveOverridesFile: %v", err)
	}

	msft, ok := got["MSFT"]
	if !ok {
		t.Fatalf("no MSFT override in %v", got)
	}

	if msft.MinStrength != 0.50 {
		t.Errorf("MSFT min_strength = %v, want its own 0.50", msft.MinStrength)
	}

	if strings.Join(msft.Detectors, ",") != "macd,trend" {
		t.Errorf("MSFT detectors = %v, want the NEW base's [macd trend]", msft.Detectors)
	}

	if _, err := ResolveOverridesFile(filepath.Join(t.TempDir(), "missing.yaml"), base); err == nil {
		t.Error("a missing file resolved without error")
	}
}
