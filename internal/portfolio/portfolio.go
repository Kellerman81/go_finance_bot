// Package portfolio tracks cash, positions and their market value. It is the
// source of truth the risk manager consults before authorising an order. State
// is synced from the broker and marked-to-market from the live price feed.
package portfolio

import "sync"

// Holding is a single tracked position.
type Holding struct {
	Symbol       string  `json:"symbol"`
	Qty          float64 `json:"qty"`
	AvgPrice     float64 `json:"avg_price"`
	LastPrice    float64 `json:"last_price"`
	MarketValue  float64 `json:"market_value"`
	UnrealizedPL float64 `json:"unrealized_pl"`
}

// Snapshot is an immutable view of the portfolio at a point in time.
type Snapshot struct {
	Cash          float64            `json:"cash"`
	Equity        float64            `json:"equity"`
	Invested      float64            `json:"invested"`
	OpenPositions int                `json:"open_positions"`
	Holdings      map[string]Holding `json:"holdings"`
}

// Portfolio holds the mutable, concurrency-safe portfolio state.
type Portfolio struct {
	mu       sync.RWMutex
	cash     float64
	holdings map[string]*Holding
}

// New creates an empty portfolio.
func New() *Portfolio {
	return &Portfolio{holdings: make(map[string]*Holding)}
}

// SetCash updates the cash balance.
func (p *Portfolio) SetCash(cash float64) {
	p.mu.Lock()

	p.cash = cash
	p.mu.Unlock()
}

// SyncPositions replaces tracked holdings from an authoritative broker list.
func (p *Portfolio) SyncPositions(hs []Holding) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.holdings = make(map[string]*Holding, len(hs))
	for i := range hs {
		h := hs[i]

		p.holdings[h.Symbol] = &h
	}
}

// MarkPrice updates the last price for a symbol and recomputes its value.
func (p *Portfolio) MarkPrice(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	h, ok := p.holdings[symbol]
	if !ok {
		return
	}

	h.LastPrice = price
	h.MarketValue = price * h.Qty
	h.UnrealizedPL = (price - h.AvgPrice) * h.Qty
}

// PositionQty returns the quantity held in a symbol without allocating a full
// snapshot (cheap path for hot loops).
func (p *Portfolio) PositionQty(symbol string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if h, ok := p.holdings[symbol]; ok {
		return h.Qty
	}

	return 0
}

// PositionValue returns the current market value held in a symbol.
func (p *Portfolio) PositionValue(symbol string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if h, ok := p.holdings[symbol]; ok {
		return h.MarketValue
	}

	return 0
}

// Snapshot returns an immutable copy of the current state.
func (p *Portfolio) Snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := Snapshot{
		Cash:     p.cash,
		Holdings: make(map[string]Holding, len(p.holdings)),
	}
	for sym, h := range p.holdings {
		out.Holdings[sym] = *h

		out.Invested += h.MarketValue
	}

	out.OpenPositions = len(p.holdings)
	out.Equity = out.Cash + out.Invested

	return out
}
