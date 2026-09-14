package engine

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// buyOrder is a market buy of qty at the given reference price.
func buyOrder(qty float64) broker.Order {
	return broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: qty, Type: broker.Market}
}

// TestMarketIsTheDefault checks an unconfigured engine still sends one plain
// market order — the new machinery must not change existing behaviour.
func TestMarketIsTheDefault(t *testing.T) {
	t.Parallel()

	plan := planExecution(config.ExecutionConfig{}, buyOrder(10), 100, false)

	if len(plan.orders) != 1 {
		t.Fatalf("plan has %d orders, want 1", len(plan.orders))
	}

	if plan.orders[0].Type != broker.Market || plan.note != "" {
		t.Errorf("plan = %+v, want a single unannotated market order", plan)
	}
}

// TestLimitIsPassive checks the limit price sits on the side that earns the
// spread rather than paying it: below the market for a buy, above for a sell.
func TestLimitIsPassive(t *testing.T) {
	t.Parallel()

	cfg := config.ExecutionConfig{Type: "limit", LimitOffsetPct: 0.001} // 10 bps

	buy := planExecution(cfg, buyOrder(10), 100, false)
	if buy.orders[0].Type != broker.Limit {
		t.Fatalf("order type = %v, want limit", buy.orders[0].Type)
	}

	if math.Abs(buy.orders[0].LimitPx-99.9) > 1e-9 {
		t.Errorf("buy limit = %v, want 99.90 (below the market)", buy.orders[0].LimitPx)
	}

	sell := planExecution(cfg,
		broker.Order{Symbol: "AAPL", Side: broker.Sell, Qty: 10, Type: broker.Market},
		100, false)

	if math.Abs(sell.orders[0].LimitPx-100.1) > 1e-9 {
		t.Errorf("sell limit = %v, want 100.10 (above the market)", sell.orders[0].LimitPx)
	}

	if !strings.Contains(buy.note, "limit") {
		t.Errorf("note = %q, want it to record the limit price", buy.note)
	}
}

// TestUrgentOrdersGoToMarket checks protective exits ignore limit execution. A
// stop resting unfilled while the market runs away is the exact outcome the stop
// existed to prevent.
func TestUrgentOrdersGoToMarket(t *testing.T) {
	t.Parallel()

	cfg := config.ExecutionConfig{Type: "limit", LimitOffsetPct: 0.005, SliceNotional: 100}

	plan := planExecution(cfg,
		broker.Order{Symbol: "AAPL", Side: broker.Sell, Qty: 100, Type: broker.Market},
		100, true)

	if len(plan.orders) != 1 {
		t.Errorf("urgent plan has %d orders, want 1 unsliced clip", len(plan.orders))
	}

	if plan.orders[0].Type != broker.Market {
		t.Errorf("urgent order type = %v, want market", plan.orders[0].Type)
	}
}

// TestSlicingSplitsAndPreservesQuantity checks a large order becomes several
// clips whose quantities still add up exactly to the parent's.
func TestSlicingSplitsAndPreservesQuantity(t *testing.T) {
	t.Parallel()

	cfg := config.ExecutionConfig{SliceNotional: 1000, MaxSlices: 5}

	// 37 shares at 100 = $3700 notional, so 4 clips of <= $1000.
	plan := planExecution(cfg, buyOrder(37), 100, false)

	if len(plan.orders) < 2 {
		t.Fatalf("plan has %d orders, want it sliced", len(plan.orders))
	}

	var total float64

	for _, o := range plan.orders {
		if o.Qty <= 0 {
			t.Errorf("slice with non-positive qty %v", o.Qty)
		}

		total += o.Qty
	}

	if math.Abs(total-37) > 1e-9 {
		t.Errorf("slices total %v, want exactly the parent's 37", total)
	}

	if !strings.Contains(plan.note, "slices") {
		t.Errorf("note = %q, want it to record the slicing", plan.note)
	}
}

// TestSlicingRespectsMaxSlices checks a huge order cannot generate an unbounded
// stream of child orders.
func TestSlicingRespectsMaxSlices(t *testing.T) {
	t.Parallel()

	cfg := config.ExecutionConfig{SliceNotional: 10, MaxSlices: 3}

	plan := planExecution(cfg, buyOrder(1000), 100, false) // $100k in $10 clips
	if len(plan.orders) != 3 {
		t.Errorf("plan has %d orders, want the max_slices cap of 3", len(plan.orders))
	}
}

// TestSmallOrdersAreNotSliced checks an order already under the clip size stays
// whole.
func TestSmallOrdersAreNotSliced(t *testing.T) {
	t.Parallel()

	cfg := config.ExecutionConfig{SliceNotional: 5000}

	if plan := planExecution(cfg, buyOrder(10), 100, false); len(plan.orders) != 1 {
		t.Errorf("plan has %d orders, want 1 for a $1000 order under a $5000 clip",
			len(plan.orders))
	}
}

// TestSlicedLimitOrdersShareAPrice checks the two features compose: every clip
// of a sliced limit order carries the same passive price.
func TestSlicedLimitOrdersShareAPrice(t *testing.T) {
	t.Parallel()

	cfg := config.ExecutionConfig{
		Type: "limit", LimitOffsetPct: 0.002, SliceNotional: 1000, MaxSlices: 4,
	}

	plan := planExecution(cfg, buyOrder(30), 100, false)
	if len(plan.orders) < 2 {
		t.Fatal("expected slicing")
	}

	for i, o := range plan.orders {
		if o.Type != broker.Limit {
			t.Errorf("slice %d type = %v, want limit", i, o.Type)
		}

		if math.Abs(o.LimitPx-99.8) > 1e-9 {
			t.Errorf("slice %d limit = %v, want 99.80", i, o.LimitPx)
		}
	}
}

// TestLimitTimeoutDefault checks the working-time default and override.
func TestLimitTimeoutDefault(t *testing.T) {
	t.Parallel()

	if got := limitTimeout(config.ExecutionConfig{}); got != defaultLimitTimeout {
		t.Errorf("limitTimeout = %v, want %v", got, defaultLimitTimeout)
	}

	if got := limitTimeout(
		config.ExecutionConfig{LimitTimeout: 30 * time.Second},
	); got != 30*time.Second {
		t.Errorf("limitTimeout = %v, want the configured 30s", got)
	}
}

// TestZeroPriceFallsBackToMarket checks a missing reference price cannot produce
// a limit order at 0 (which would either never fill or fill catastrophically).
func TestZeroPriceFallsBackToMarket(t *testing.T) {
	t.Parallel()

	cfg := config.ExecutionConfig{Type: "limit", LimitOffsetPct: 0.01}

	plan := planExecution(cfg, buyOrder(10), 0, false)
	if len(plan.orders) != 1 || plan.orders[0].Type != broker.Market {
		t.Errorf("plan = %+v with no price, want a single market order", plan)
	}
}
