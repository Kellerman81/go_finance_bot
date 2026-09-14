package api

import (
	"strconv"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/autotune"
	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/gin-gonic/gin"
)

// The strategy tab's per-symbol optimizer: a manually-triggered, bounded
// sibling of `go run ./cmd/optimize`, scoped to one watchlist symbol's own
// cached candle history and answering in well under the length of an HTTP
// request rather than the minutes a full CLI grid search takes.
//
// The core search/scoring logic lives in internal/autotune, shared with the
// scheduled auto-optimizer (autotune.Schedule) so a manual click and a
// scheduled pass can never run concurrently (autotune.TryAcquire/Release is
// the single shared semaphore) and judge a symbol by exactly the same rule
// (autotune.RunSymbolOptimize's dual-baseline "must beat both" gate — see
// that package's doc comment). This handler stays propose-only: unlike
// Schedule, it never calls config.PatchSymbolOverride or
// Engine.SetSymbolStrategies — it only renders autotune.Result for an
// operator to read and, if they agree, apply by hand.

// fragSymbolOptimize renders the per-symbol optimizer panel: a symbol
// picker plus the same Objective/MinReturnPct controls the scheduled
// auto-optimizer's settings form has (see internal/api/formfields.go's
// "autotune" section) — the underlying search is the exact same
// autotune.RunSymbolOptimize call either way, so an operator exploring a
// symbol by hand can try the same objectives and loss floor a scheduled
// pass would use, rather than being stuck with the hardcoded defaults this
// page used before. And — once run — the proposed config and the reasoning
// behind it.
func (s *Server) fragSymbolOptimize(c *gin.Context) {
	symbols := s.eng.Watchlist()
	symbol := strings.ToUpper(strings.TrimSpace(c.Query("symbol")))
	objective := symbolOptimizeObjective(c)
	minReturnPct := symbolOptimizeMinReturnPct(c)

	t := translator(langOf(c))
	data := map[string]any{
		"Symbols": symbols, "Symbol": symbol,
		"Objectives": backtest.ScorerNames, "Objective": objective, "MinReturnPct": minReturnPct,
	}

	if symbol == "" {
		render(c, "symbolOptimize", data)

		return
	}

	if !contains(symbols, symbol) {
		data["Error"] = t("opt.unknownSymbol", "symbol", symbol)
		render(c, "symbolOptimize", data)

		return
	}

	candles := s.eng.Series(symbol)
	if len(candles) < autotune.MinBars {
		data["Error"] = t("opt.notEnough", "symbol", symbol)
		render(c, "symbolOptimize", data)

		return
	}

	if !autotune.TryAcquire() {
		data["Error"] = t("opt.busy")
		render(c, "symbolOptimize", data)

		return
	}
	defer autotune.Release()

	data["Result"] = autotune.RunSymbolOptimize(
		s.eng, s.rm, s.pf, symbol, candles, autotune.DefaultMinTrades, objective, minReturnPct)

	render(c, "symbolOptimize", data)
}

// symbolOptimizeObjective reads the form's objective selector, falling back
// to autotune.DefaultObjective ("blend") when absent or blank — the same
// fallback autotune.RunSymbolOptimize itself applies to an unrecognized
// name, so an unset/stale value never produces an error, just the default.
func symbolOptimizeObjective(c *gin.Context) string {
	if o := strings.TrimSpace(c.Query("objective")); o != "" {
		return o
	}

	return autotune.DefaultObjective
}

// symbolOptimizeMinReturnPct reads the form's minimum-return floor,
// tolerating a blank or unparsable value by falling back to
// autotune.DefaultMinReturnPct (0) — this is a manual exploration tool, not
// a validated settings form, so a stray typo should not surface as an HTTP
// error, just silently keep the safe default.
func symbolOptimizeMinReturnPct(c *gin.Context) float64 {
	raw := strings.TrimSpace(c.Query("min_return_pct"))
	if raw == "" {
		return autotune.DefaultMinReturnPct
	}

	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return autotune.DefaultMinReturnPct
	}

	return v
}
