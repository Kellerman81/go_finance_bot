package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// fxStalledBroker is stalledFeedBroker whose position carries an FX rate.
type fxStalledBroker struct {
	stalledFeedBroker

	rate float64
}

func (b *fxStalledBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return []broker.Position{{Symbol: "X", Qty: 2, AvgPrice: 100, Current: 80, FXRate: b.rate}}, nil
}

// PF3-1: setFXRate rescaled the cached tick through PriceCache.Set, which
// stamps it NOW — so after each rate change (about hourly) a tick the feed
// stopped updating 30 minutes ago read as fresh, and syncPrice and checkExits
// trusted it over the broker. With the feed frozen at 120, the broker at 80
// and the stop at 90, the stop sent nothing: PF2-1 undone.
func TestAnFXRateChangeDoesNotMakeAStaleTickFresh(t *testing.T) {
	e := ledgerEngine(t)
	b := &fxStalledBroker{rate: 1.01}
	e.brk = b

	if e.fxRates == nil {
		e.fxRates = map[string]float64{}
	}

	e.fxRates["X"] = 1.0

	e.prices.mu.Lock()
	e.prices.m["X"] = priceEntry{price: 120, at: time.Now().Add(-30 * time.Minute)}
	e.prices.mu.Unlock()

	e.SetExitsConfig(config.ExitsConfig{Enabled: true, StopLossPct: 0.10}) // stop at 90

	e.syncPortfolio(context.Background())

	if px, ok := e.prices.GetFresh("X", maxExitPriceAge); ok {
		t.Errorf("a 30-minute-old tick reads as fresh after the rate change: %v", px)
	}

	e.checkExits(context.Background())

	if b.sells != 1 {
		t.Errorf("the stop at 90 with the broker at 80 sent %d sells, want 1", b.sells)
	}
}

// EN3-4: setFXRate rescaled the live trailing peak but not the record's Peak
// or TrailFloor. A rate that lowers prices left the floor on the old scale,
// above the market — a spurious full stop-out — and a restart restored the
// stale Peak.
func TestAnFXRescaleReachesTheDurableRecord(t *testing.T) {
	e, _ := forgetEngine()
	e.fxRates = make(map[string]float64)
	e.barsStore = newBarStore(10, time.Minute)
	e.openPos["X"] = &OpenPosition{Symbol: "X", Qty: 1, EntryPrice: 92, Peak: 110, TrailFloor: 104}
	e.peaks["X"] = 110

	e.setFXRate("X", 0.92) // the rate arrives late: ticks had been unconverted

	e.mu.RLock()
	p := *e.openPos["X"]
	pk := e.peaks["X"]
	e.mu.RUnlock()

	if p.Peak != pk {
		t.Errorf("record Peak %.2f left on the old scale (live peak %.2f)", p.Peak, pk)
	}

	if p.TrailFloor > 104*0.92+1e-9 {
		t.Errorf("TrailFloor %.2f left on the old scale: above a market near %.2f", p.TrailFloor, 110*0.92)
	}
}

// countingBuyBroker never confirms and counts buys.
type countingBuyBroker struct {
	asyncBroker

	buys int
}

func (b *countingBuyBroker) SubmitOrder(ctx context.Context, o broker.Order) (broker.OrderResult, error) {
	if o.Side == broker.Buy {
		b.buys++
	}

	return b.asyncBroker.SubmitOrder(ctx, o)
}

// buysWith runs one BUY signal for X against the given broker holdings and an
// optional in-flight reservation, and returns how many buys reached the broker.
func buysWith(t *testing.T, hs []portfolio.Holding, inFlight bool) int {
	t.Helper()

	e := ledgerEngine(t)
	b := &countingBuyBroker{}
	e.brk = b
	delete(e.openPos, "X")
	e.pf.SyncPositions(hs)
	e.prices.Set("X", 120)

	if inFlight {
		e.rm.ReserveBuy("X", 1, 120) // a resting limit buy of X
	}

	e.act(context.Background(), strategy.Signal{Symbol: "X", Action: strategy.Buy, Price: 120}, nil, true)

	return b.buys
}

// PF3-2: the pyramid guard tested Qty > 0, so with allow_pyramiding false a
// 5e-7 broker remainder after a full exit blocked every later buy of that
// symbol — no record, no log.
func TestADustRowDoesNotBlockReentry(t *testing.T) {
	if buysWith(t, nil, false) == 0 {
		t.Fatal("control: an unheld symbol did not buy; the fixture gates entries elsewhere")
	}

	dust := []portfolio.Holding{{Symbol: "X", Qty: 5e-7, AvgPrice: 100, LastPrice: 120, MarketValue: 6e-5}}
	if buysWith(t, dust, false) == 0 {
		t.Error("a dust row blocked the re-entry")
	}
}

// RK3-1, engine side: a resting limit buy is not in the snapshot or openPos, so
// with allow_pyramiding false a standing BUY signal sent another full clip
// every evaluation while the first rested.
func TestABuyInFlightBlocksASecondBuyWithoutPyramiding(t *testing.T) {
	if buysWith(t, nil, true) != 0 {
		t.Error("a second buy of X went out while one was in flight, with pyramiding off")
	}
}
