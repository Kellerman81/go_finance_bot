package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// newID returns a short random hex identifier for simulated fills.
func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "sim-" + hex.EncodeToString(b[:])
}

// Sim is an in-memory simulated broker. It fills market orders immediately at a
// price supplied by a price oracle, tracks cash and positions, and is the
// default when no Alpaca credentials are configured. Fully offline.
type Sim struct {
	priceOf func(symbol string) (float64, bool)
	costOf  func(notional float64) float64

	mu        sync.Mutex
	cash      float64
	positions map[string]*Position
	seq       int
}

// NewSim creates a simulated broker seeded with starting cash. priceOf returns
// the latest known price for a symbol; costOf returns the commission for a
// trade of the given notional (may be nil for zero-cost fills).
func NewSim(startingCash float64, priceOf func(string) (float64, bool), costOf func(float64) float64) *Sim {
	if costOf == nil {
		costOf = func(float64) float64 { return 0 }
	}
	return &Sim{
		priceOf:   priceOf,
		costOf:    costOf,
		cash:      startingCash,
		positions: make(map[string]*Position),
	}
}

func (s *Sim) Name() string { return "sim" }

func (s *Sim) SubmitOrder(ctx context.Context, o Order) (OrderResult, error) {
	px, ok := s.priceOf(o.Symbol)
	if !ok || px <= 0 {
		return OrderResult{}, fmt.Errorf("sim: no price for %s", o.Symbol)
	}
	if o.Type == Limit && o.LimitPx > 0 {
		// Only fill a limit order if marketable against the current price.
		if (o.Side == Buy && px > o.LimitPx) || (o.Side == Sell && px < o.LimitPx) {
			return OrderResult{Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
				Status: "pending", SubmittedAt: time.Now()}, nil
		}
		px = o.LimitPx
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	notional := px * o.Qty
	commission := s.costOf(notional)
	pos := s.positions[o.Symbol]

	switch o.Side {
	case Buy:
		total := notional + commission // commission increases cash outlay
		if total > s.cash {
			return OrderResult{}, fmt.Errorf("sim: insufficient cash (need %.2f incl. fee, have %.2f)", total, s.cash)
		}
		s.cash -= total
		if pos == nil {
			pos = &Position{Symbol: o.Symbol}
			s.positions[o.Symbol] = pos
		}
		newQty := pos.Qty + o.Qty
		pos.AvgPrice = (pos.AvgPrice*pos.Qty + notional) / newQty
		pos.Qty = newQty
	case Sell:
		if pos == nil || pos.Qty < o.Qty {
			held := 0.0
			if pos != nil {
				held = pos.Qty
			}
			return OrderResult{}, fmt.Errorf("sim: insufficient shares of %s (need %.4f, have %.4f)", o.Symbol, o.Qty, held)
		}
		s.cash += notional - commission // commission reduces proceeds
		pos.Qty -= o.Qty
		if pos.Qty == 0 {
			delete(s.positions, o.Symbol)
		}
	}

	s.seq++
	return OrderResult{
		ID:          newID(),
		Symbol:      o.Symbol,
		Side:        o.Side,
		Qty:         o.Qty,
		FilledQty:   o.Qty,
		FilledPx:    px,
		Status:      "filled",
		SubmittedAt: time.Now(),
	}, nil
}

func (s *Sim) GetAccount(ctx context.Context) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	equity := s.cash
	for sym, p := range s.positions {
		px, ok := s.priceOf(sym)
		if !ok {
			px = p.AvgPrice
		}
		equity += px * p.Qty
	}
	return Account{Cash: s.cash, Equity: equity, BuyingPower: s.cash, Currency: "USD"}, nil
}

func (s *Sim) GetPositions(ctx context.Context) ([]Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Position, 0, len(s.positions))
	for sym, p := range s.positions {
		px, ok := s.priceOf(sym)
		if !ok {
			px = p.AvgPrice
		}
		out = append(out, Position{
			Symbol:       sym,
			Qty:          p.Qty,
			AvgPrice:     p.AvgPrice,
			Current:      px,
			MarketValue:  px * p.Qty,
			UnrealizedPL: (px - p.AvgPrice) * p.Qty,
		})
	}
	return out, nil
}
