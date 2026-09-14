package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
)

// maxRows bounds each breakdown table so a 300-symbol run stays readable. The
// tables are sorted by P&L, so the truncated tail is the least interesting part.
const maxRows = 15

// printBreakdowns prints the per-symbol, per-exit-reason, monthly and holding
// tables. These answer the questions the aggregate metrics raise: which symbols
// carried the result, which exit rules paid for themselves, whether the return
// was steady or one lucky month, and how long capital was committed.
func printBreakdowns(r backtest.Result) {
	if len(r.RoundTrips) == 0 {
		return
	}

	printExitTable(r.ByExit)
	printSymbolTable(r.BySymbol)
	printMonthTable(r.ByMonth)
	printHoldStats(r.Hold)
}

// printExitTable prints P&L per exit rule.
//
// This is usually the most actionable table in the report. An exit kind whose
// total P&L is negative is not "the cost of doing business" — it is a rule
// losing money on every trade it closes, and no aggregate number reveals which
// one. A `stop-loss` row is expected to be negative (that is its job); a
// negative `take-profit` or `roi` row means the targets are set wrong.
func printExitTable(rows []backtest.ExitResult) {
	if len(rows) == 0 {
		return
	}

	fmt.Println("\n---- P&L by exit rule ----")
	fmt.Printf("%-16s %7s %7s %8s %11s %10s  %s\n",
		"exit", "trades", "wins", "win%", "P&L", "avg%", "avg held")
	fmt.Println(strings.Repeat("-", 78))

	for _, e := range rows {
		fmt.Printf("%-16s %7d %7d %7.1f%% %11.2f %9.2f%%  %s\n",
			e.Kind, e.Trades, e.Wins, e.WinRatePct, e.PnL, e.AvgPnLPct, short(e.AvgHeld))
	}
}

// printSymbolTable prints P&L per symbol, best first.
func printSymbolTable(rows []backtest.SymbolResult) {
	if len(rows) == 0 {
		return
	}

	fmt.Println("\n---- P&L by symbol ----")
	fmt.Printf("%-12s %7s %7s %7s %8s %11s %10s  %s\n",
		"symbol", "trades", "wins", "losses", "win%", "P&L", "avg%", "avg held")
	fmt.Println(strings.Repeat("-", 82))

	shown := min(len(rows), maxRows)

	for _, s := range rows[:shown] {
		fmt.Printf("%-12s %7d %7d %7d %7.1f%% %11.2f %9.2f%%  %s\n",
			s.Symbol, s.Trades, s.Wins, s.Losses, s.WinRatePct, s.PnL, s.AvgPnLPct,
			short(s.AvgHeld))
	}

	if len(rows) > shown {
		fmt.Printf("... and %d more symbols (use -json for all)\n", len(rows)-shown)
	}
}

// printMonthTable prints the equity change per calendar month, with a crude bar
// so the shape of the run is visible at a glance.
func printMonthTable(rows []backtest.PeriodReturn) {
	if len(rows) < 2 {
		return // a single (partial) month says nothing about consistency
	}

	fmt.Println("\n---- monthly returns ----")

	worst := 0.0

	for _, m := range rows {
		if a := abs(m.ReturnPct); a > worst {
			worst = a
		}
	}

	for _, m := range rows {
		fmt.Printf("%-9s %8.2f%%  %s\n", m.Month, m.ReturnPct, bar(m.ReturnPct, worst))
	}
}

// printHoldStats prints the holding-period distribution.
func printHoldStats(h backtest.HoldStats) {
	if h.Trades == 0 {
		return
	}

	fmt.Println("\n---- holding periods ----")
	fmt.Printf("min %s   median %s   mean %s   max %s\n",
		short(h.Min), short(h.Median), short(h.Mean), short(h.Max))
	fmt.Printf("avg winner held %s   avg loser held %s\n",
		short(h.AvgWinHeld), short(h.AvgLossHeld))

	// The classic failure mode, worth naming rather than leaving to be spotted.
	if h.AvgLossHeld > 2*h.AvgWinHeld && h.AvgWinHeld > 0 {
		fmt.Println("NOTE: losers are held far longer than winners — cutting winners early " +
			"and sitting on losers. Check the take-profit against the stop distance.")
	}
}

// printMonteCarlo prints the bootstrap distribution.
func printMonteCarlo(mc backtest.MonteCarlo) {
	fmt.Printf("\n---- Monte Carlo (%d resamplings of %d round trips) ----\n", mc.Runs, mc.Trades)
	fmt.Printf("Return   p5 %7.2f%%   p25 %7.2f%%   median %7.2f%%   p75 %7.2f%%   p95 %7.2f%%\n",
		mc.ReturnP5, mc.ReturnP25, mc.ReturnP50, mc.ReturnP75, mc.ReturnP95)
	fmt.Printf("Max DD   median %6.2f%%   p95 %6.2f%%   p99 %6.2f%%   worst %6.2f%%\n",
		mc.MaxDDP50, mc.MaxDDP95, mc.MaxDDP99, mc.MaxDDWorst)
	fmt.Printf("Probability of profit: %5.1f%%      probability of a %.0f%% drawdown: %5.1f%%\n",
		mc.ProbProfit*100, mc.RuinPct, mc.ProbRuin*100)

	// The headline number is one sample from this distribution. Say so, because
	// the temptation is to read the single backtest return as the expectation.
	if mc.ReturnP5 < 0 && mc.ReturnP50 > 0 {
		fmt.Println("The 5th percentile is a loss: this strategy's edge is not robust to the " +
			"order its trades arrive in. Size for the p95 drawdown, not the median.")
	}

	fmt.Println("Caveat: the bootstrap resamples trades independently, so it cannot see losing " +
		"streaks that cluster in one regime. Treat the spread as a floor on uncertainty.")
}

// annualised formats a figure that only means something over a decent span. On
// a short run it prints "n/a" rather than a number: compounding a two-day result
// up to a year produces values that are arithmetically correct and actively
// misleading, and a reader who sees 7368653951852423168 learns nothing except
// that something is wrong.
func annualised(v float64, spanTooShort bool, unit string) string {
	if spanTooShort {
		return fmt.Sprintf("%12s", "n/a")
	}

	return fmt.Sprintf("%11.2f%s", v, unit)
}

// short renders a duration compactly: 90m rather than 1h30m0s.
func short(d time.Duration) string {
	switch {
	case d == 0:
		return "—"
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	}

	return fmt.Sprintf("%.1fd", d.Hours()/24)
}

// bar renders a value as a crude horizontal bar scaled to the largest magnitude.
func bar(v, scale float64) string {
	const width = 24

	if scale <= 0 {
		return ""
	}

	n := int(abs(v) / scale * width)
	if n < 1 && v != 0 {
		n = 1
	}

	ch := "+"
	if v < 0 {
		ch = "-"
	}

	return strings.Repeat(ch, n)
}

// abs returns the absolute value of v.
func abs(v float64) float64 {
	if v < 0 {
		return -v
	}

	return v
}
