package backtest

import (
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// barReturnsAt supplies per-bar returns up to a given bar index, so the
// concentration guard sees exactly the history that existed at decision time.
//
// This is the same look-ahead discipline the rest of the backtester follows: a
// correlation computed over the whole dataset would let a decision on bar 100 be
// influenced by bar 5000. It implements risk.Returns against a moving cursor
// that the simulation loop advances.
type barReturnsAt struct {
	data map[string][]market.Candle
	bar  int // the index currently being simulated
}

// Returns gives up to n per-bar returns for symbol, ending at the current bar.
func (b *barReturnsAt) Returns(symbol string, n int) ([]float64, bool) {
	candles := b.data[symbol]
	if len(candles) == 0 {
		return nil, false
	}

	end := min(b.bar+1, len(candles))
	if end < 2 {
		return nil, false
	}

	start := max(end-(n+1), 0) // n returns need n+1 closes

	window := candles[start:end]
	closes := make([]float64, len(window))

	for i, c := range window {
		closes[i] = c.Close
	}

	rets := risk.BarReturns(closes)

	return rets, len(rets) > 0
}
