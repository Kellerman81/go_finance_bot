package risk

import (
	"math"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// fakeReturns serves canned return series per symbol.
type fakeReturns map[string][]float64

// Returns gives up to n recent returns for symbol.
func (f fakeReturns) Returns(symbol string, n int) ([]float64, bool) {
	r, ok := f[strings.ToUpper(symbol)]
	if !ok || len(r) == 0 {
		return nil, false
	}

	if len(r) > n {
		r = r[len(r)-n:]
	}

	return r, true
}

// wiggle builds a return series. sign flips it, so two series built with
// opposite signs correlate at -1 and identical signs at +1.
func wiggle(n int, sign float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = sign * math.Sin(float64(i)) * 0.01
	}

	return out
}

// snapshotOf builds a portfolio snapshot holding the given symbol→value pairs.
func snapshotOf(holdings map[string]float64) portfolio.Snapshot {
	snap := portfolio.Snapshot{Holdings: map[string]portfolio.Holding{}}
	for sym, val := range holdings {
		snap.Holdings[sym] = portfolio.Holding{Symbol: sym, Qty: 1, MarketValue: val}
		snap.Invested += val
		snap.OpenPositions++
	}

	return snap
}

// TestSectorValueCap checks the per-sector market-value cap.
func TestSectorValueCap(t *testing.T) {
	t.Parallel()

	sectors := map[string]string{"AAPL": "tech", "MSFT": "tech", "XOM": "energy"}
	g := NewPortfolioGuard(
		config.PortfolioConfig{Enabled: true, MaxPerSector: 500},
		sectors, nil, nil)

	if g == nil {
		t.Fatal("guard not built for an enabled sector cap")
	}

	snap := snapshotOf(map[string]float64{"AAPL": 400})

	// 400 held + 200 more = 600 > 500.
	ok, reason := g.AllowEntry("MSFT", snap, 200)
	if ok {
		t.Error("entry allowed past the sector value cap")
	}

	if !strings.Contains(reason, "tech") || !strings.Contains(reason, "max_per_sector") {
		t.Errorf("reason = %q, want it to name the sector and the cap", reason)
	}

	// A different sector is unaffected.
	if ok, reason := g.AllowEntry("XOM", snap, 400); !ok {
		t.Errorf("energy entry blocked by a tech cap: %s", reason)
	}

	// And a smaller order that fits is allowed.
	if ok, reason := g.AllowEntry("MSFT", snap, 50); !ok {
		t.Errorf("a fitting order was blocked: %s", reason)
	}
}

// TestSectorPositionCountCap checks the per-sector position-count cap.
func TestSectorPositionCountCap(t *testing.T) {
	t.Parallel()

	sectors := map[string]string{"A": "tech", "B": "tech", "C": "tech", "D": "energy"}
	g := NewPortfolioGuard(
		config.PortfolioConfig{Enabled: true, MaxPositionsPerSector: 2},
		sectors, nil, nil)

	snap := snapshotOf(map[string]float64{"A": 100, "B": 100})

	if ok, reason := g.AllowEntry("C", snap, 100); ok {
		t.Error("a third tech position was allowed past a cap of 2")
	} else if !strings.Contains(reason, "max_positions_per_sector") {
		t.Errorf("reason = %q, want it to name the cap", reason)
	}

	if ok, _ := g.AllowEntry("D", snap, 100); !ok {
		t.Error("an energy position was blocked by the tech count")
	}
}

// TestUnmappedSymbolSkipsSectorCaps checks a symbol with no sector label isn't
// blocked by a cap it cannot be measured against.
func TestUnmappedSymbolSkipsSectorCaps(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(
		config.PortfolioConfig{Enabled: true, MaxPerSector: 100},
		map[string]string{"AAPL": "tech"}, nil, nil)

	snap := snapshotOf(map[string]float64{"AAPL": 100})

	if ok, reason := g.AllowEntry("UNKNOWN", snap, 5000); !ok {
		t.Errorf("an unmapped symbol was blocked by a sector cap: %s", reason)
	}
}

// TestCorrelationCapBlocksTheSameBet is the point of the feature: holding three
// names that move as one is one position, not three.
func TestCorrelationCapBlocksTheSameBet(t *testing.T) {
	t.Parallel()

	// A, B and CAND all move identically; D moves inversely.
	rets := fakeReturns{
		"A": wiggle(200, 1), "B": wiggle(200, 1),
		"CAND": wiggle(200, 1), "D": wiggle(200, -1),
	}

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, CorrelationThreshold: 0.8, MaxCorrelatedPositions: 2,
		CorrelationLookback: 200,
	}, nil, nil, rets)

	if g == nil {
		t.Fatal("guard not built for an enabled correlation cap")
	}

	// Two perfectly correlated holdings already => the cap is reached.
	ok, reason := g.AllowEntry("CAND", snapshotOf(map[string]float64{"A": 100, "B": 100}), 100)
	if ok {
		t.Error("a third perfectly-correlated position was allowed")
	}

	if !strings.Contains(reason, "correlates") {
		t.Errorf("reason = %q, want it to explain the correlation", reason)
	}

	// One correlated holding is under the cap.
	if ok, reason := g.AllowEntry("CAND", snapshotOf(map[string]float64{"A": 100}), 100); !ok {
		t.Errorf("a second correlated position was blocked by a cap of 2: %s", reason)
	}

	// An inversely-correlated book is not concentrated at all.
	if ok, reason := g.AllowEntry(
		"CAND", snapshotOf(map[string]float64{"D": 100, "DD": 100}), 100,
	); !ok {
		t.Errorf("an uncorrelated book blocked the entry: %s", reason)
	}
}

// TestUnknownCorrelationAllows checks a symbol with no history is not refused
// forever — blocking every new symbol would be a worse failure than occasionally
// allowing a correlated one.
func TestUnknownCorrelationAllows(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, CorrelationThreshold: 0.5, MaxCorrelatedPositions: 1,
	}, nil, nil, fakeReturns{"A": wiggle(200, 1)})

	if ok, reason := g.AllowEntry("BRAND-NEW", snapshotOf(map[string]float64{"A": 100}), 100); !ok {
		t.Errorf("a symbol with no history was blocked: %s", reason)
	}
}

// TestTopUpNeverBlocked checks adding to a position already held is allowed: the
// concentration decision was made when it was opened.
func TestTopUpNeverBlocked(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPerSector: 100, MaxPositionsPerSector: 1,
	}, map[string]string{"AAPL": "tech"}, nil, nil)

	snap := snapshotOf(map[string]float64{"AAPL": 5000})

	if ok, reason := g.AllowEntry("AAPL", snap, 5000); !ok {
		t.Errorf("topping up an existing position was blocked: %s", reason)
	}
}

// TestGuardDisabledAndNil checks both "off" paths need no special case.
func TestGuardDisabledAndNil(t *testing.T) {
	t.Parallel()

	if g := NewPortfolioGuard(
		config.PortfolioConfig{Enabled: false, MaxPerSector: 1},
		nil,
		nil,
		nil,
	); g != nil {
		t.Error("a disabled config built a guard")
	}

	// Enabled but with nothing to enforce: don't pay the cost on every order.
	if g := NewPortfolioGuard(config.PortfolioConfig{Enabled: true}, nil, nil, nil); g != nil {
		t.Error("an enabled config with no caps built a guard")
	}

	var g *PortfolioGuard

	if ok, _ := g.AllowEntry("AAPL", snapshotOf(nil), 100); !ok {
		t.Error("a nil guard blocked an entry")
	}
}

// TestCorrelation checks the maths on known series, including the degenerate
// cases that would otherwise produce a confident NaN.
func TestCorrelation(t *testing.T) {
	t.Parallel()

	up, down := wiggle(100, 1), wiggle(100, -1)

	if c, ok := correlation(up, up); !ok || math.Abs(c-1) > 1e-9 {
		t.Errorf("correlation(x,x) = %v (ok=%v), want 1", c, ok)
	}

	if c, ok := correlation(up, down); !ok || math.Abs(c+1) > 1e-9 {
		t.Errorf("correlation(x,-x) = %v (ok=%v), want -1", c, ok)
	}

	// Too little overlap to trust.
	if _, ok := correlation(up[:5], down[:5]); ok {
		t.Error("correlated a 5-sample overlap; too few to mean anything")
	}

	// A flat series has no deviation to correlate against.
	flat := make([]float64, 100)
	if _, ok := correlation(up, flat); ok {
		t.Error("correlated against a constant series")
	}
}

// TestBarReturns checks the close-to-return conversion.
func TestBarReturns(t *testing.T) {
	t.Parallel()

	got := BarReturns([]float64{100, 110, 99})
	if len(got) != 2 {
		t.Fatalf("got %d returns from 3 closes, want 2", len(got))
	}

	if math.Abs(got[0]-0.10) > 1e-9 || math.Abs(got[1]+0.10) > 1e-9 {
		t.Errorf("returns = %v, want [+0.10, -0.10]", got)
	}

	if BarReturns([]float64{100}) != nil {
		t.Error("a single close should yield no returns")
	}
}
