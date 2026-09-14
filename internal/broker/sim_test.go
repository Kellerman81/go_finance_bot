package broker

import (
	"context"
	"testing"
)

// TestSeedPositionsReplacesWholesale checks the shape internal/engine's test
// mode depends on: a fresh Sim with existing holdings, seeded with a
// DIFFERENT cash/position snapshot, ends up reporting exactly the seeded
// state — not a merge of old and new — so a symbol from before the seed
// never lingers.
func TestSeedPositionsReplacesWholesale(t *testing.T) {
	prices := map[string]float64{"AAPL": 150, "MSFT": 300, "NVDA": 900}
	priceOf := func(sym string) (float64, bool) { p, ok := prices[sym]; return p, ok }

	s := NewSim(10000, priceOf, nil)

	ctx := context.Background()

	if _, err := s.SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Buy, Qty: 10, Type: Market}); err != nil {
		t.Fatalf("seed buy: %v", err)
	}

	s.SeedPositions(5000, []Position{
		{Symbol: "MSFT", Qty: 3, AvgPrice: 280},
		{Symbol: "NVDA", Qty: 1, AvgPrice: 850},
	})

	acct, err := s.GetAccount(ctx)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}

	if acct.Cash != 5000 {
		t.Errorf("Cash = %v, want the seeded 5000, not the pre-seed value", acct.Cash)
	}

	positions, err := s.GetPositions(ctx)
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}

	if len(positions) != 2 {
		t.Fatalf("positions = %+v, want exactly the 2 seeded ones (AAPL must be gone)", positions)
	}

	bySymbol := make(map[string]Position, len(positions))
	for _, p := range positions {
		bySymbol[p.Symbol] = p
	}

	if _, ok := bySymbol["AAPL"]; ok {
		t.Error("AAPL (bought before the seed) is still present — SeedPositions must replace, not merge")
	}

	msft, ok := bySymbol["MSFT"]
	if !ok || msft.Qty != 3 || msft.AvgPrice != 280 {
		t.Errorf("MSFT = %+v (present=%v), want {Qty:3 AvgPrice:280}", msft, ok)
	}

	nvda, ok := bySymbol["NVDA"]
	if !ok || nvda.Qty != 1 || nvda.AvgPrice != 850 {
		t.Errorf("NVDA = %+v (present=%v), want {Qty:1 AvgPrice:850}", nvda, ok)
	}
}

// TestSeedPositionsValuesAreLiveNotFrozen checks that Current/MarketValue/
// UnrealizedPL on a seeded position come from priceOf at read time, not
// whatever (zero) values a caller happened to pass in — SeedPositions only
// needs Symbol/Qty/AvgPrice to be meaningful.
func TestSeedPositionsValuesAreLiveNotFrozen(t *testing.T) {
	price := 200.0
	priceOf := func(string) (float64, bool) { return price, true }

	s := NewSim(0, priceOf, nil)
	s.SeedPositions(1000, []Position{{Symbol: "AAPL", Qty: 2, AvgPrice: 150}})

	positions, err := s.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}

	if len(positions) != 1 {
		t.Fatalf("positions = %+v, want exactly 1", positions)
	}

	got := positions[0]
	if got.Current != 200 || got.MarketValue != 400 || got.UnrealizedPL != 100 {
		t.Errorf("got %+v, want Current=200 MarketValue=400 UnrealizedPL=100 (computed from priceOf, not frozen at seed time)", got)
	}
}
