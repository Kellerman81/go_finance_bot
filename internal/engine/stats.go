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
	// ExpectancyPct is the mean realized P&L per closed trade, as a percentage
	// of entry cost — the same definition backtest.Result.ExpectancyPct uses,
	// deliberately: it is the one metric that is genuinely comparable between
	// a real (capital-constrained) track record and an isolated backtest,
	// without needing to normalize by how much capital was actually deployed
	// on this symbol versus tied up elsewhere. Win rate is the other.
	// TotalPnL/TotalReturnPct are NOT comparable this way — one is an
	// absolute dollar amount from whatever capital this symbol actually got,
	// the other is a return on an isolated backtest's full starting cash.
	ExpectancyPct float64 `json:"expectancy_pct"`
	// ExitBreakdown counts closed trades by ExitKind* (stop-loss, trailing-
	// stop, take-profit, roi, strategy). A symbol whose losses cluster in
	// ExitStopLoss is a different problem (the entry itself was wrong) from
	// one whose losses cluster in ExitStrategy (the exit gave back a
	// winner) — the aggregate win rate above can't distinguish the two.
	ExitBreakdown map[string]int `json:"exit_breakdown,omitempty"`
}

// Exit kinds a closed trade's Reason classifies into. Mirrors
// internal/backtest/roundtrip.go's identically-named constants and
// classifyExit — kept as a separate copy (rather than an import) so this
// package, which backtest already depends on for TradeRecord, never depends
// on backtest in return. Keep the two in sync if the reason-string
// convention ("EXIT <kind> ...", set in exits.go) ever changes.
const (
	ExitKindStopLoss     = "stop-loss"
	ExitKindTrailingStop = "trailing-stop"
	ExitKindTakeProfit   = "take-profit"
	ExitKindROI          = "roi"
	ExitKindStrategy     = "strategy"
)

// ClassifyExitKind maps a closed sell's Reason onto one of the ExitKind*
// constants. Anything that isn't a protective exit's "EXIT <kind> ..." tag
// (see exits.go) came from a strategy signal.
func ClassifyExitKind(reason string) string {
	switch {
	case strings.Contains(reason, ExitKindTrailingStop):
		return ExitKindTrailingStop
	case strings.Contains(reason, ExitKindStopLoss):
		return ExitKindStopLoss
	case strings.Contains(reason, ExitKindROI):
		return ExitKindROI
	case strings.Contains(reason, ExitKindTakeProfit):
		return ExitKindTakeProfit
	}

	return ExitKindStrategy
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

// StatsSince returns realized win/loss performance from since to now, for a
// caller that needs a specific window rather than one of Stats' canned
// periods — e.g. matching a backtest's own candle range, so a "real vs.
// backtest" comparison is actually over the same span of time instead of
// "all of history" against "the last N cached bars".
func (e *Engine) StatsSince(since time.Time) (TradeStats, error) {
	stats, err := e.store.TradeStats(since)
	if err != nil {
		return TradeStats{}, err
	}

	stats.Period = "custom"
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
