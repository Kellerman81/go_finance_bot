package portfolio

import "testing"

// TestPositionValueReadsTheMarkedValue guards the per-symbol value the risk
// manager's max_per_position check reads: it must follow MarkPrice, normalise
// the symbol the same way SyncPositions does, and answer 0 — not panic or
// carry a stale value — for a symbol that is not held.
func TestPositionValueReadsTheMarkedValue(t *testing.T) {
	t.Parallel()

	p := New()
	p.SyncPositions([]Holding{{Symbol: "AAPL", Qty: 10, MarketValue: 1000}})

	if got := p.PositionValue("AAPL"); got != 1000 {
		t.Errorf("PositionValue(AAPL) = %v, want 1000", got)
	}

	p.MarkPrice("AAPL", 120)

	if got := p.PositionValue(" aapl "); got != 1200 {
		t.Errorf("PositionValue after mark, unnormalised key = %v, want 1200", got)
	}

	if got := p.PositionValue("MSFT"); got != 0 {
		t.Errorf("PositionValue(not held) = %v, want 0", got)
	}
}
