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
	slipOf  SlippageFunc

	mu        sync.Mutex
	cash      float64
	positions map[string]*Position
	seq       int
}

// SlippageFunc moves a reference price adversely for the given side, modelling
// the bid-ask spread and slippage a market order really pays. buy is true for a
// buy fill.
//
// It takes the symbol because friction is not one number across a book: a
// megacap trades inside a basis point where a thin ETF listing costs twenty
// times that, and charging both the same flatters one and libels the other.
// Callers with a single global spread can ignore the argument.
type SlippageFunc func(symbol string, buy bool, price float64) float64

// SetPriceOracle replaces the price source used to fill orders.
//
// It exists so a caller that builds the Sim before it has the richer oracle
// available can install it afterwards — cmd/bot constructs the paper broker
// before the engine that knows how to price a symbol from its bar history. A
// nil oracle is ignored rather than leaving the Sim unable to fill anything.
func (s *Sim) SetPriceOracle(f func(symbol string) (float64, bool)) {
	if f == nil {
		return
	}

	s.mu.Lock()

	s.priceOf = f
	s.mu.Unlock()
}

// SetSlippage installs the adverse fill-price model applied to every simulated
// market fill. Without it, fills happen at the exact reference price — free
// liquidity that does not exist in any real market. Passing nil restores that
// frictionless behaviour.
func (s *Sim) SetSlippage(f SlippageFunc) {
	s.mu.Lock()

	s.slipOf = f
	s.mu.Unlock()
}

// SetCommission installs the commission model charged on every simulated
// fill. NewSim takes it as a method value, which captures a COPY of the
// costs config, so without this a costs save reached sizing and headroom but
// never the paper broker: a flat 5 commission raised in the UI was never
// debited, and every paper round trip read better than the cost model the
// engine books (AP3-8). nil means zero-cost fills.
func (s *Sim) SetCommission(f func(notional float64) float64) {
	if f == nil {
		f = func(float64) float64 { return 0 }
	}

	s.mu.Lock()

	s.costOf = f
	s.mu.Unlock()
}

// fillPrice applies the configured slippage model to a reference price.
func (s *Sim) fillPrice(symbol string, side Side, px float64) float64 {
	if s.slipOf == nil {
		return px
	}

	if adj := s.slipOf(symbol, side == Buy, px); adj > 0 {
		return adj
	}

	return px
}

// NewSim creates a simulated broker seeded with starting cash. priceOf returns
// the latest known price for a symbol; costOf returns the commission for a
// trade of the given notional (may be nil for zero-cost fills).
func NewSim(
	startingCash float64,
	priceOf func(string) (float64, bool),
	costOf func(float64) float64,
) *Sim {
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

// SeedPositions replaces the simulated book's cash and holdings wholesale —
// used by internal/engine's test mode when it starts, so the simulation
// continues realistically from wherever the real account currently stands
// rather than a flat book. Only Symbol/Qty/AvgPrice matter: Current/
// MarketValue/UnrealizedPL are recomputed live from priceOf on every
// GetPositions call regardless of what's passed in here, and there is no
// real fill history to reconstruct realized P&L from — a position closed
// during test mode prices only against this seeded average cost.
func (s *Sim) SeedPositions(cash float64, positions []Position) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cash = cash
	s.positions = make(map[string]*Position, len(positions))

	for _, p := range positions {
		cp := p
		s.positions[p.Symbol] = &cp
	}
}

// Name identifies the broker adapter.
func (*Sim) Name() string { return "sim" }

// SubmitOrder fills market (and marketable limit) orders instantly at the
// current price, debiting commission and updating cash and positions.
//
//nolint:cyclop // validate → price → settle per side is one fill simulation
func (s *Sim) SubmitOrder(_ context.Context, o Order) (OrderResult, error) {
	px, ok := s.priceOf(o.Symbol)
	if !ok || px <= 0 {
		return OrderResult{}, fmt.Errorf("sim: no price for %s", o.Symbol)
	}

	limit := o.Type == Limit && o.LimitPx > 0
	if limit {
		// Only fill a limit order if marketable against the current price.
		if (o.Side == Buy && px > o.LimitPx) || (o.Side == Sell && px < o.LimitPx) {
			return OrderResult{
				Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
				Status: "pending", SubmittedAt: time.Now(),
			}, nil
		}

		px = o.LimitPx
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Market orders cross the spread and suffer slippage; a limit order fills at
	// its own price (that is the point of one), so it is left alone.
	if !limit {
		px = s.fillPrice(o.Symbol, o.Side, px)
	}

	notional := px * o.Qty
	commission := s.costOf(notional)
	pos := s.positions[o.Symbol]

	switch o.Side {
	case Buy:
		total := notional + commission // commission increases cash outlay
		if total > s.cash {
			return OrderResult{}, fmt.Errorf(
				"sim: insufficient cash (need %.2f incl. fee, have %.2f)",
				total,
				s.cash,
			)
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

			return OrderResult{}, fmt.Errorf(
				"sim: insufficient shares of %s (need %.4f, have %.4f)",
				o.Symbol,
				o.Qty,
				held,
			)
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
		Terminal:    true, // Sim fills instantly and deterministically
	}, nil
}

// GetAccount returns the simulated cash and mark-to-market equity.
func (s *Sim) GetAccount(_ context.Context) (Account, error) {
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

// GetPositions returns the simulated holdings.
func (s *Sim) GetPositions(_ context.Context) ([]Position, error) {
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
