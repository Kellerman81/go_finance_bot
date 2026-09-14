package backtest

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"
)

// RoundTrip is one completed entry-to-exit cycle. The aggregate metrics answer
// "did this strategy make money"; these records answer the follow-up questions
// that actually change a configuration — which symbols carried it, which exits
// paid for themselves, and how long capital was tied up.
type RoundTrip struct {
	Symbol    string        `json:"symbol"`
	Entry     time.Time     `json:"entry"`
	Exit      time.Time     `json:"exit"`
	Held      time.Duration `json:"held"`
	EntryPx   float64       `json:"entry_price"`
	ExitPx    float64       `json:"exit_price"`
	Qty       float64       `json:"qty"`
	PnL       float64       `json:"pnl"`     // net of round-trip commission
	PnLPct    float64       `json:"pnl_pct"` // as % of the position's cost basis
	ExitKind  string        `json:"exit_kind"`
	ExitLabel string        `json:"exit_label"` // the raw reason, for context
}

// Won reports whether the round trip made money net of costs.
func (r RoundTrip) Won() bool { return r.PnL >= 0 }

// Exit kinds. Grouping raw reason strings into a handful of kinds is what makes
// the breakdown useful: "stop-loss 92.40<=100.00" is unique per trade and
// aggregates into nothing.
const (
	ExitStopLoss     = "stop-loss"
	ExitTrailingStop = "trailing-stop"
	ExitTakeProfit   = "take-profit"
	ExitROI          = "roi"
	ExitStrategy     = "strategy"
)

// classifyExit maps a sell's reason string onto an exit kind. Protective exits
// are prefixed "EXIT " by the caller and name their rule; anything else came
// from a strategy signal.
func classifyExit(reason string) string {
	switch {
	case strings.Contains(reason, ExitTrailingStop):
		return ExitTrailingStop
	case strings.Contains(reason, ExitStopLoss):
		return ExitStopLoss
	case strings.Contains(reason, ExitROI):
		return ExitROI
	case strings.Contains(reason, ExitTakeProfit):
		return ExitTakeProfit
	}

	return ExitStrategy
}

// SymbolResult is one symbol's contribution to the run.
type SymbolResult struct {
	Symbol     string        `json:"symbol"`
	Trades     int           `json:"trades"`
	Wins       int           `json:"wins"`
	Losses     int           `json:"losses"`
	WinRatePct float64       `json:"win_rate_pct"`
	PnL        float64       `json:"pnl"`
	AvgPnLPct  float64       `json:"avg_pnl_pct"`
	AvgHeld    time.Duration `json:"avg_held"`
}

// ExitResult is one exit kind's contribution. This is the single most
// decision-changing table in the report: an exit rule whose trades are net
// negative is costing money, and no aggregate number will tell you which one.
type ExitResult struct {
	Kind       string        `json:"kind"`
	Trades     int           `json:"trades"`
	Wins       int           `json:"wins"`
	WinRatePct float64       `json:"win_rate_pct"`
	PnL        float64       `json:"pnl"`
	AvgPnLPct  float64       `json:"avg_pnl_pct"`
	AvgHeld    time.Duration `json:"avg_held"`
}

// PeriodReturn is the equity change over one calendar month.
type PeriodReturn struct {
	Month     string  `json:"month"` // YYYY-MM
	ReturnPct float64 `json:"return_pct"`
	StartEq   float64 `json:"start_equity"`
	EndEq     float64 `json:"end_equity"`
}

// HoldStats describes how long positions were held. A strategy whose median
// hold is two bars is a different (and far more cost-sensitive) animal than one
// holding for weeks, however similar their returns look.
type HoldStats struct {
	Trades int           `json:"trades"`
	Min    time.Duration `json:"min"`
	Median time.Duration `json:"median"`
	Mean   time.Duration `json:"mean"`
	Max    time.Duration `json:"max"`
	// AvgWinHeld/AvgLossHeld expose the classic failure mode: cutting winners
	// early and sitting on losers shows up as a much longer average loss.
	AvgWinHeld  time.Duration `json:"avg_win_held"`
	AvgLossHeld time.Duration `json:"avg_loss_held"`
}

// bySymbol aggregates round trips per symbol, worst P&L last.
func bySymbol(trips []RoundTrip) []SymbolResult {
	if len(trips) == 0 {
		return nil
	}

	acc := make(map[string]*SymbolResult, len(trips))
	pct := make(map[string]float64, len(trips))
	held := make(map[string]time.Duration, len(trips))

	for _, t := range trips {
		s, ok := acc[t.Symbol]
		if !ok {
			s = &SymbolResult{Symbol: t.Symbol}
			acc[t.Symbol] = s
		}

		s.Trades++

		s.PnL += t.PnL

		if t.Won() {
			s.Wins++
		} else {
			s.Losses++
		}

		pct[t.Symbol] += t.PnLPct
		held[t.Symbol] += t.Held
	}

	out := make([]SymbolResult, 0, len(acc))

	for sym, s := range acc {
		if s.Trades > 0 {
			s.WinRatePct = float64(s.Wins) / float64(s.Trades) * 100
			s.AvgPnLPct = pct[sym] / float64(s.Trades)
			s.AvgHeld = held[sym] / time.Duration(s.Trades)
		}

		out = append(out, *s)
	}

	slices.SortFunc(out, func(a, b SymbolResult) int { return cmp.Compare(b.PnL, a.PnL) })

	return out
}

// byExitKind aggregates round trips per exit rule, most profitable first.
func byExitKind(trips []RoundTrip) []ExitResult {
	if len(trips) == 0 {
		return nil
	}

	acc := make(map[string]*ExitResult, len(trips))
	pct := make(map[string]float64, len(trips))
	held := make(map[string]time.Duration, len(trips))

	for _, t := range trips {
		e, ok := acc[t.ExitKind]
		if !ok {
			e = &ExitResult{Kind: t.ExitKind}
			acc[t.ExitKind] = e
		}

		e.Trades++

		e.PnL += t.PnL

		if t.Won() {
			e.Wins++
		}

		pct[t.ExitKind] += t.PnLPct
		held[t.ExitKind] += t.Held
	}

	out := make([]ExitResult, 0, len(acc))

	for kind, e := range acc {
		if e.Trades > 0 {
			e.WinRatePct = float64(e.Wins) / float64(e.Trades) * 100
			e.AvgPnLPct = pct[kind] / float64(e.Trades)
			e.AvgHeld = held[kind] / time.Duration(e.Trades)
		}

		out = append(out, *e)
	}

	slices.SortFunc(out, func(a, b ExitResult) int { return cmp.Compare(b.PnL, a.PnL) })

	return out
}

// monthlyReturns splits the equity curve by calendar month. A single total
// return hides whether it came from one lucky month or twelve steady ones.
//
// Each month is measured from the PREVIOUS month's closing equity, not from its
// own first bar, so compounding the reported months reproduces the whole run.
// Anchoring on a month's own first bar instead left the move across every month
// boundary belonging to no month at all: measured on daily bars at +1%/day over
// three months, compounding the reported months gave +137.66% against an actual
// +142.44%, and the shortfall grows with the number of boundaries. startCash
// anchors the first month for the same reason — the equity curve's first point
// is already after bar 0's fills, so without it that bar's move would be the
// one remaining orphan. A non-positive startCash falls back to the first
// point's own equity. See CODE_REVIEW_FINDINGS.md's BT-3 finding.
func monthlyReturns(equity []EquityPoint, startCash float64) []PeriodReturn {
	if len(equity) < 2 {
		return nil
	}

	var (
		out     []PeriodReturn
		month   string
		startEq float64
		lastEq  float64
	)

	// The baseline the NEXT month opens from: the run's starting cash until a
	// month has actually closed, then that month's closing equity.
	prevClose := startCash

	flush := func() {
		if month == "" || startEq <= 0 {
			return
		}

		out = append(out, PeriodReturn{
			Month: month, StartEq: startEq, EndEq: lastEq,
			ReturnPct: (lastEq/startEq - 1) * 100,
		})

		prevClose = lastEq
	}

	for _, p := range equity {
		if p.Time.IsZero() {
			continue
		}

		m := p.Time.UTC().Format("2006-01")
		if m != month {
			flush()

			startEq = prevClose
			if startEq <= 0 {
				startEq = p.Equity // no usable baseline; fall back to this month's open
			}

			month = m
		}

		lastEq = p.Equity
	}

	flush()

	return out
}

// holdStats summarises holding periods across the round trips.
func holdStats(trips []RoundTrip) HoldStats {
	if len(trips) == 0 {
		return HoldStats{}
	}

	durations := make([]time.Duration, 0, len(trips))

	var (
		total, winTotal, lossTotal time.Duration
		wins, losses               int
	)

	for _, t := range trips {
		durations = append(durations, t.Held)

		total += t.Held

		if t.Won() {
			winTotal += t.Held
			wins++
		} else {
			lossTotal += t.Held
			losses++
		}
	}

	slices.Sort(durations)

	s := HoldStats{
		Trades: len(trips),
		Min:    durations[0],
		Median: durations[len(durations)/2],
		Mean:   total / time.Duration(len(trips)),
		Max:    durations[len(durations)-1],
	}

	if wins > 0 {
		s.AvgWinHeld = winTotal / time.Duration(wins)
	}

	if losses > 0 {
		s.AvgLossHeld = lossTotal / time.Duration(losses)
	}

	return s
}

// pnlSeries extracts the cash P&L of each round trip, in order — the input the
// Monte Carlo bootstrap resamples.
func pnlSeries(trips []RoundTrip) []float64 {
	out := make([]float64, len(trips))
	for i, t := range trips {
		out[i] = t.PnL
	}

	return out
}

// pctSeries extracts each round trip's return as a percentage of its cost basis.
func pctSeries(trips []RoundTrip) []float64 {
	out := make([]float64, 0, len(trips))

	for _, t := range trips {
		if !math.IsNaN(t.PnLPct) && !math.IsInf(t.PnLPct, 0) {
			out = append(out, t.PnLPct)
		}
	}

	return out
}
