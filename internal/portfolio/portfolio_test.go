package portfolio

import "testing"

// TestOpenPositionsExcludesZeroQuantityRows reproduces the
// "max_open_positions bypassed by a zero-quantity holdings row" finding's
// other half: OpenPositions used to be len(p.holdings), counting a
// zero-quantity row (a broker reporting a just-closed position, or a
// rounding remainder — SyncPositions carries whatever the broker reports
// straight through) as an open position, inflating the count risk.Manager
// compares against max_open_positions without any real exposure behind it.
func TestOpenPositionsExcludesZeroQuantityRows(t *testing.T) {
	p := New()

	p.SyncPositions([]Holding{
		{Symbol: "AAPL", Qty: 10},
		{Symbol: "CLOSED", Qty: 0},
		{Symbol: "MSFT", Qty: 5},
	})

	snap := p.Snapshot()

	if snap.OpenPositions != 2 {
		t.Errorf("OpenPositions = %d, want 2 (the zero-qty row must not count)", snap.OpenPositions)
	}

	// The zero-qty row is still visible in Holdings (callers that key off
	// presence for other reasons, e.g. reading a stale AvgPrice, are
	// unaffected) — only the count changed.
	if _, ok := snap.Holdings["CLOSED"]; !ok {
		t.Error("the zero-qty row was dropped from Holdings; it should only be excluded from the count")
	}
}

func TestSyncPositionsReplacesTheWholeBook(t *testing.T) {
	p := New()

	p.SyncPositions([]Holding{{Symbol: "AAPL", Qty: 10}})
	p.SyncPositions([]Holding{{Symbol: "MSFT", Qty: 5}})

	snap := p.Snapshot()

	if _, ok := snap.Holdings["AAPL"]; ok {
		t.Error("a symbol from a prior sync survived a later, authoritative sync")
	}

	if snap.Holdings["MSFT"].Qty != 5 {
		t.Errorf("MSFT qty = %v, want 5", snap.Holdings["MSFT"].Qty)
	}
}

func TestMarkPriceUpdatesValueAndUnrealizedPL(t *testing.T) {
	p := New()
	p.SyncPositions([]Holding{{Symbol: "AAPL", Qty: 10, AvgPrice: 100}})

	p.MarkPrice("AAPL", 110)

	h, ok := p.Holding("AAPL")
	if !ok {
		t.Fatal("AAPL not held after sync")
	}

	if h.MarketValue != 1100 {
		t.Errorf("MarketValue = %v, want 1100", h.MarketValue)
	}

	if h.UnrealizedPL != 100 {
		t.Errorf("UnrealizedPL = %v, want 100", h.UnrealizedPL)
	}
}

// MarkPrice on a symbol not held must be a no-op, not a panic or a phantom
// holding.
func TestMarkPriceOnUnknownSymbolIsANoOp(t *testing.T) {
	p := New()

	p.MarkPrice("GHOST", 100)

	if _, ok := p.Holding("GHOST"); ok {
		t.Error("marking an unknown symbol created a holding for it")
	}
}

func TestEquityIsCashPlusMarkedValue(t *testing.T) {
	p := New()
	p.SetCash(1000)
	p.SyncPositions([]Holding{{Symbol: "AAPL", Qty: 10, AvgPrice: 100}})
	p.MarkPrice("AAPL", 100)

	if got := p.Equity(); got != 2000 {
		t.Errorf("Equity = %v, want 2000 (1000 cash + 1000 market value)", got)
	}
}

// TestHoldingsAreMatchedCaseInsensitively reproduces the EN-6 finding. This
// package used to key holdings by whatever string the broker reported, while
// its consumers disagreed about the form: risk.PortfolioGuard.AllowEntry
// upper-cased before looking up, risk.Authorize and MarkPrice did not. A broker
// reporting "sap.de" against a "SAP.DE" watchlist therefore made MarkPrice a
// silent no-op (equity stopped tracking price) and made every SELL fail
// Authorize with "no position to sell" — an exit that can never fill.
func TestHoldingsAreMatchedCaseInsensitively(t *testing.T) {
	p := New()
	p.SetCash(1000)
	p.SyncPositions([]Holding{{Symbol: "sap.de", Qty: 10, AvgPrice: 100, MarketValue: 1000}})

	if got := p.PositionQty("SAP.DE"); got != 10 {
		t.Errorf("PositionQty(\"SAP.DE\") = %v, want 10 for a holding synced as \"sap.de\"", got)
	}

	if got := p.PositionQty(" sap.de "); got != 10 {
		t.Errorf("PositionQty with surrounding space = %v, want 10", got)
	}

	// MarkPrice must find it, or the holding never marks to market.
	p.MarkPrice("SAP.DE", 150)

	h, ok := p.Holding("sap.de")
	if !ok {
		t.Fatal("Holding(\"sap.de\") missing")
	}

	if h.LastPrice != 150 || h.MarketValue != 1500 {
		t.Errorf("after MarkPrice: LastPrice=%v MarketValue=%v, want 150 / 1500", h.LastPrice, h.MarketValue)
	}

	snap := p.Snapshot()
	if _, ok := snap.Holdings["SAP.DE"]; !ok {
		t.Errorf("snapshot keys = %v, want the canonical SAP.DE", snap.Holdings)
	}

	if got, ok := snap.Holding("sap.de"); !ok || got.Qty != 10 {
		t.Errorf("Snapshot.Holding(\"sap.de\") = %+v, %v; want the holding", got, ok)
	}

	if snap.Equity != 2500 {
		t.Errorf("Equity = %v, want 2500 (1000 cash + 1500 marked) — a missed mark "+
			"silently freezes equity at the last sync's value", snap.Equity)
	}
}
