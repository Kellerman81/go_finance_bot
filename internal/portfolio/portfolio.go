// Package portfolio tracks cash, positions and their market value. It is the
// source of truth the risk manager consults before authorising an order. State
// is synced from the broker and marked-to-market from the live price feed.
package portfolio

import (
	"strings"
	"sync"
)

// Normalize is the canonical form every holding is keyed by: upper-cased and
// trimmed, matching config.PerSymbolStrategy, risk.PortfolioGuard.AllowEntry
// and every symbol the API/screener admit.
//
// Without it this package keyed holdings by whatever string the broker
// happened to report while its consumers disagreed about the form — the
// guard upper-cased before looking up, risk.Authorize and MarkPrice did not.
// A broker reporting "sap.de" against a watchlist of "SAP.DE" therefore made
// MarkPrice a silent no-op (the holding never marked to market, so equity
// stopped tracking price) and made every SELL fail Authorize with "no
// position to sell" — an exit that can never fill. See
// CODE_REVIEW_FINDINGS.md's EN-6 finding.
func Normalize(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

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

// Holding returns symbol's holding from the snapshot, matching it the same
// normalized way Portfolio keys its own map.
//
// Prefer this over indexing Holdings directly: a raw map lookup silently
// misses on a case/whitespace difference and reports "not held", which on the
// sell path means "no position to sell" — see Normalize.
func (s Snapshot) Holding(symbol string) (Holding, bool) {
	h, ok := s.Holdings[Normalize(symbol)]

	return h, ok
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
		h.Symbol = Normalize(h.Symbol)

		p.holdings[h.Symbol] = &h
	}
}

// MarkPrice updates the last price for a symbol and recomputes its value.
func (p *Portfolio) MarkPrice(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	h, ok := p.holdings[Normalize(symbol)]
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

	if h, ok := p.holdings[Normalize(symbol)]; ok {
		return h.Qty
	}

	return 0
}

// PositionValue returns the current market value held in a symbol.
func (p *Portfolio) PositionValue(symbol string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if h, ok := p.holdings[Normalize(symbol)]; ok {
		return h.MarketValue
	}

	return 0
}

// Holding returns one symbol's position by value, without building a Snapshot.
//
// Reading a single holding through Snapshot allocates and copies a map of the
// whole book. That is fine once per decision and wasteful once per symbol per
// bar, which is how the backtester's exit sweep used it.
func (p *Portfolio) Holding(symbol string) (Holding, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if h, ok := p.holdings[Normalize(symbol)]; ok {
		return *h, true
	}

	return Holding{}, false
}

// Equity returns cash plus the marked value of every holding, without building
// a Snapshot.
func (p *Portfolio) Equity() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	equity := p.cash
	for _, h := range p.holdings {
		equity += h.MarketValue
	}

	return equity
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

		// A broker can report a zero-quantity row for a symbol right after a
		// position closes (eventual consistency, or a rounding remainder),
		// and SyncPositions carries that row straight through. It is not an
		// open position — counting it inflates max_open_positions against
		// nothing actually held. See CODE_REVIEW_FINDINGS.md's
		// "max_open_positions bypassed by a zero-quantity holdings row".
		if h.Qty > 0 {
			out.OpenPositions++
		}
	}

	out.Equity = out.Cash + out.Invested

	return out
}
