package portfolio

import (
	"math"
	"testing"
)

// PF2-2: Saxo reports "SAP:xetr" and "SAP:xnys" as two rows that both
// normalise to "SAP". Last-row-wins kept one of them at random — a full exit
// then sold 5 and stranded 10, and equity understated by 1000 on every sync,
// which a drawdown check reads as a loss. Colliding rows must merge.
func TestSyncPositionsMergesRowsThatNormaliseAlike(t *testing.T) {
	t.Parallel()

	p := New()
	p.SetCash(0)

	collided := p.SyncPositions([]Holding{
		{Symbol: "SAP", Qty: 10, AvgPrice: 100, LastPrice: 100, MarketValue: 1000},
		{Symbol: "sap ", Qty: 5, AvgPrice: 110, LastPrice: 110, MarketValue: 550},
	})

	if got := p.PositionQty("SAP"); got != 15 {
		t.Errorf("PositionQty = %v, want 15 (both rows)", got)
	}

	if got := p.Equity(); got != 1550 {
		t.Errorf("Equity = %v, want 1550", got)
	}

	h, _ := p.Holding("SAP")
	if want := (10*100 + 5*110) / 15.0; math.Abs(h.AvgPrice-want) > 1e-9 {
		t.Errorf("AvgPrice = %v, want the quantity-weighted %v", h.AvgPrice, want)
	}

	if len(collided) != 1 || collided[0] != "SAP" {
		t.Errorf("collided = %v, want [SAP] so the caller can surface the ambiguous mapping", collided)
	}
}

// PF2-4: the engine treats a residual at or below DustQty as closed — never
// sold, never adopted. Counting it as an open position let a 5e-7 rounding
// remainder hold a max_open_positions slot forever.
func TestDustRowIsNotAnOpenPosition(t *testing.T) {
	t.Parallel()

	p := New()
	p.SyncPositions([]Holding{{Symbol: "X", Qty: 5e-7}})

	if got := p.Snapshot().OpenPositions; got != 0 {
		t.Errorf("OpenPositions = %d for a dust row, want 0", got)
	}
}

// PF2-5: a zero tick (Finnhub does not filter them) valued the holding at 0,
// and a drawdown check reading that equity could trip a sticky halt over a
// bad print.
func TestMarkPriceIgnoresNonPositiveAndNonFinitePrices(t *testing.T) {
	t.Parallel()

	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		p := New()
		p.SyncPositions([]Holding{{Symbol: "X", Qty: 10, AvgPrice: 100, LastPrice: 100, MarketValue: 1000}})

		p.MarkPrice("X", bad)

		if got := p.Equity(); got != 1000 {
			t.Errorf("MarkPrice(%v): equity = %v, want the last good 1000", bad, got)
		}
	}
}
