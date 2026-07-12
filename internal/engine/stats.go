package engine

import (
	"strings"
	"time"
)

// TradeStats aggregates realized (closed) trade outcomes over a period, for
// win/loss performance tracking.
type TradeStats struct {
	Period     string        `json:"period"` // "week" | "month" | "all"
	Since      time.Time     `json:"since"`
	Trades     int           `json:"trades"` // total realized (closed) trades in the period
	Wins       int           `json:"wins"`
	Losses     int           `json:"losses"`
	WinRatePct float64       `json:"win_rate_pct"` // 0 if Trades==0
	TotalPnL   float64       `json:"total_pnl"`
	AvgWin     float64       `json:"avg_win"`  // 0 if Wins==0
	AvgLoss    float64       `json:"avg_loss"` // <=0; 0 if Losses==0
	BySymbol   []SymbolStats `json:"by_symbol"`
}

// SymbolStats is one symbol's slice of TradeStats.
type SymbolStats struct {
	Symbol     string  `json:"symbol"`
	Trades     int     `json:"trades"`
	Wins       int     `json:"wins"`
	Losses     int     `json:"losses"`
	WinRatePct float64 `json:"win_rate_pct"`
	TotalPnL   float64 `json:"total_pnl"`
}

// Stats returns realized win/loss performance for period ("week", "month" or
// "all"; anything else — including "" — defaults to "week").
func (e *Engine) Stats(period string) (TradeStats, error) {
	label, since := periodSince(period)
	stats, err := e.store.TradeStats(since)
	if err != nil {
		return TradeStats{}, err
	}
	stats.Period = label
	stats.Since = since
	return stats, nil
}

// periodSince resolves a period keyword to its canonical label and
// since-timestamp cutoff.
func periodSince(period string) (label string, since time.Time) {
	n := time.Now()
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "month":
		return "month", n.AddDate(0, 0, -30)
	case "all":
		return "all", time.Time{} // zero time sorts before every real ts
	default:
		return "week", n.AddDate(0, 0, -7)
	}
}
