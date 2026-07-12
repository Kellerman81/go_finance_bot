package broker

import "testing"

func TestBuildBracketBody_LongThreeWay(t *testing.T) {
	inst := instrument{Uic: 211, AssetType: "Stock"}
	b := BracketOrder{
		Symbol: "AAPL", Side: Buy, Qty: 10,
		EntryType: Limit, EntryPrice: 150,
		StopLoss: 140, TakeProfit: 170,
	}
	body := buildBracketBody("ACC", inst, b, 10)

	if body["BuySell"] != "Buy" || body["OrderType"] != "Limit" || body["OrderPrice"] != 150.0 {
		t.Fatalf("entry leg wrong: %#v", body)
	}
	if body["Uic"].(int) != 211 || body["AssetType"] != "Stock" || body["Amount"].(float64) != 10 {
		t.Fatalf("entry instrument/amount wrong: %#v", body)
	}
	if dur := body["OrderDuration"].(map[string]any)["DurationType"]; dur != "GoodTillCancel" {
		t.Fatalf("entry duration = %v, want GoodTillCancel", dur)
	}

	legs, ok := body["Orders"].([]map[string]any)
	if !ok || len(legs) != 2 {
		t.Fatalf("want 2 related legs, got %#v", body["Orders"])
	}
	// Stop-loss leg: opposite side (Sell), StopIfTraded at 140.
	if legs[0]["BuySell"] != "Sell" || legs[0]["OrderType"] != "StopIfTraded" || legs[0]["OrderPrice"] != 140.0 {
		t.Errorf("stop leg wrong: %#v", legs[0])
	}
	// Take-profit leg: opposite side (Sell), Limit at 170.
	if legs[1]["BuySell"] != "Sell" || legs[1]["OrderType"] != "Limit" || legs[1]["OrderPrice"] != 170.0 {
		t.Errorf("take-profit leg wrong: %#v", legs[1])
	}
}

func TestBuildBracketBody_MarketEntryStopOnly(t *testing.T) {
	inst := instrument{Uic: 5, AssetType: "Etf"}
	b := BracketOrder{Symbol: "SPY", Side: Buy, Qty: 3, EntryType: Market, StopLoss: 400}
	body := buildBracketBody("ACC", inst, b, 3)

	if _, has := body["OrderPrice"]; has {
		t.Error("market entry must not carry OrderPrice")
	}
	legs := body["Orders"].([]map[string]any)
	if len(legs) != 1 || legs[0]["OrderType"] != "StopIfTraded" {
		t.Fatalf("want a single stop leg, got %#v", legs)
	}
}

func TestBuildBracketBody_ShortBracketSidesFlip(t *testing.T) {
	inst := instrument{Uic: 7, AssetType: "Stock"}
	b := BracketOrder{Symbol: "X", Side: Sell, Qty: 2, EntryType: Market, StopLoss: 110, TakeProfit: 90}
	body := buildBracketBody("ACC", inst, b, 2)
	legs := body["Orders"].([]map[string]any)
	for _, l := range legs {
		if l["BuySell"] != "Buy" {
			t.Errorf("short bracket legs must Buy to cover, got %v", l["BuySell"])
		}
	}
}

func TestPlaceBracket_Validation(t *testing.T) {
	s := &Saxo{accountKey: "ACC"}
	// Long bracket with stop above take-profit is invalid.
	if _, err := s.PlaceBracket(nil, BracketOrder{Symbol: "X", Side: Buy, Qty: 1, StopLoss: 170, TakeProfit: 150}); err == nil {
		t.Error("expected error for long bracket with stop >= take-profit")
	}
	// Negative quantity is invalid.
	if _, err := s.PlaceBracket(nil, BracketOrder{Symbol: "X", Side: Buy, Qty: -1}); err == nil {
		t.Error("expected error for negative quantity")
	}
	// Bad side is invalid.
	if _, err := s.PlaceBracket(nil, BracketOrder{Symbol: "X", Side: "sideways", Qty: 1}); err == nil {
		t.Error("expected error for invalid side")
	}
}

func TestBuildModifyBody(t *testing.T) {
	inst := instrument{Uic: 211, AssetType: "Stock"}
	m := OrderModification{OrderID: "ord-1", Symbol: "AAPL", Side: Sell, Type: Limit, Qty: 5, Price: 160}
	body := buildModifyBody("ACC", inst, m, 5)
	if body["OrderId"] != "ord-1" || body["OrderPrice"] != 160.0 || body["Amount"].(float64) != 5 ||
		body["BuySell"] != "Sell" || body["OrderType"] != "Limit" {
		t.Fatalf("modify body wrong: %#v", body)
	}
}
