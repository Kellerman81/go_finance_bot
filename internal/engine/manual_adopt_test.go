package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// AP3-2: a partly filled resting limit buy shows as "held, not tracked" (bot 0,
// broker 60), and the panel offers Adopt. Adopting put the 60 in openPos, so
// at the timeout filledWhileResting measured nothing filled and the market
// fallback re-bought the whole clip — 160 shares for a 100-share order. While
// a buy of the symbol is working, adoption waits.
func TestManualAdoptWaitsForARestingBuy(t *testing.T) {
	e := watchLimitEngine(cancellingBroker(new([]broker.Order), []broker.Position{{Symbol: "X", Qty: 60, Current: 100}}))
	e.syncPortfolio(context.Background())

	done := e.trackWatcher("X") // the limit buy is still resting

	if err := e.AdoptHeldPosition("X"); !errors.Is(err, ErrOrderInFlight) {
		t.Errorf("adopt of a resting partial: err = %v, want ErrOrderInFlight", err)
	}

	done()

	if err := e.AdoptHeldPosition("X"); err != nil {
		t.Errorf("adopt refused once nothing was working: %v", err)
	}
}

// PF3-4: AdoptHeldPosition tested Qty <= 0, so clicking Adopt on a broker's
// dust remainder created a permanent dust record.
func TestManualAdoptRefusesDust(t *testing.T) {
	e := watchLimitEngine(cancellingBroker(new([]broker.Order), nil))
	e.pf.SyncPositions([]portfolio.Holding{{Symbol: "X", Qty: 5e-7, AvgPrice: 100, LastPrice: 100}})

	if err := e.AdoptHeldPosition("X"); !errors.Is(err, ErrNotAdoptable) {
		t.Errorf("adopting a 5e-7 dust row: err = %v, want ErrNotAdoptable", err)
	}
}
