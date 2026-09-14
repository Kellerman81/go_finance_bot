package api

import (
	"math"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/gin-gonic/gin"
)

// The strategy tab's field optimizer: an interactive, single-field sibling of
// `go run ./cmd/optimize -sensitivity`. That CLI command sweeps ~28 parameter
// axes against a CSV or synthetic series, which is the right tool for a
// thorough offline search but far too slow to sit behind a page load. This
// runs backtest.SensitivityAxis for exactly the one field the operator picked,
// against the engine's own cached candle history for one watchlist symbol —
// cheap enough to answer synchronously.

// optimizeAxis maps a strategy field's name (as declared in strategyFields,
// which is also the name attribute the "strategy" form's inputs use) to the
// backtest.Sensitivity axis that sweeps it. Only fields with a swept axis are
// listed: a few strategy fields (rsi_overbought, rsi_mode, the detector
// selections, per-symbol weights) have nothing to sweep against and are
// silently absent from the optimizer's field list rather than erroring.
//
// The two sides happen to be spelled identically for every strategyFields
// entry below — strategyFields uses config.StrategyConfig's own json tags,
// and backtest's Sensitivity axis names were independently given the same
// snake_case convention — but they are two different namespaces (a strategy
// field vs. a Sensitivity axis identifier) that could diverge, so the
// mapping stays explicit rather than assuming the key doubles as the value.
var optimizeAxis = map[string]string{
	"combine":           "combine",
	"combine_sell":      "combine_sell",
	"min_strength":      "min_strength",
	"min_strength_sell": "min_strength_sell",
	"fast_ma":           "fast_ma",
	"slow_ma":           "slow_ma",
	"rsi_period":        "rsi_period",
	"rsi_mode":          "rsi_mode",
	"rsi_overbought":    "rsi_overbought",
	"rsi_oversold":      "rsi_oversold",
	"macd_fast":         "macd_fast",
	"macd_slow":         "macd_slow",
	"macd_signal":       "macd_signal",
	"bb_period":         "bb_period",
	"bb_stddev":         "bb_stddev",
	"vwap_period":       "vwap_period",
	"atr_period":        "atr_period",
	"atr_mult":          "atr_mult",
	"rvol_period":       "rvol_period",
	"rvol_threshold":    "rvol_threshold",
	"vp_window":         "vp_window",
	"vp_buckets":        "vp_buckets",
	"vp_value_area_pct": "vp_value_area_pct",
	"trend_period":      "trend_period",
	"trend_threshold":   "trend_threshold",
	"trend_gate":        "trend_gate",
	"adx_period":        "adx_period",
	"adx_threshold":     "adx_threshold",
	"stoch_k_period":    "stoch_k_period",
	"stoch_d_period":    "stoch_d_period",
	"stoch_overbought":  "stoch_overbought",
	"stoch_oversold":    "stoch_oversold",
	"supertrend_period": "supertrend_period",
	"supertrend_mult":   "supertrend_mult",
	"donchian_period":   "donchian_period",

	// These vary an exit rule or the friction model rather than a strategy
	// field, so they have no entry in strategyFields — see nonStrategyAxes
	// and pickerField.
	"settings.exits.stop_loss_pct":     "stop_loss",
	"settings.exits.trailing_stop_pct": "trailing_stop",
	"settings.exits.take_profit_pct":   "take_profit",

	"settings.costs.commission_pct":  "commission_pct",
	"settings.costs.commission_flat": "commission_flat",
	"settings.costs.commission_min":  "commission_min",
	"settings.costs.spread_pct":      "spread_pct",
	"settings.costs.slippage_pct":    "slippage_pct",
}

// nonStrategyAxes are sensitivity axes the sweep engine already computes that
// don't correspond to a strategyFields entry, because the setting they vary
// lives on the Settings tab (in the exits or costs block), not the Strategy
// tab where the optimizer panel is rendered. They're still worth comparing
// here — the same one-field-at-a-time backtest, against the same cached
// history — even though there's no in-page form input for the sweep table's
// "Apply" button to fill, so pickerField leaves those rows' ApplyPrefix
// empty and the template omits the button for them.
var nonStrategyAxes = []struct{ Section, Name string }{
	{"exits", "stop_loss_pct"},
	{"exits", "trailing_stop_pct"},
	{"exits", "take_profit_pct"},
	{"costs", "commission_pct"},
	{"costs", "commission_flat"},
	{"costs", "commission_min"},
	{"costs", "spread_pct"},
	{"costs", "slippage_pct"},
}

// pickerField is one entry in the field optimizer's dropdown.
type pickerField struct {
	// Key is sent as ?field= and looked up in optimizeAxis.
	Key string
	// LabelPrefix/LabelName resolve the option's text via tkey, reusing the
	// field's own settings-form translation entry rather than a duplicate.
	LabelPrefix, LabelName string
	// ApplyPrefix/ApplyName are the target form's "f-<prefix>-<name>" id for
	// the sweep table's Apply button. Empty ApplyPrefix means that id isn't
	// on this page (see nonStrategyAxes), so the template renders no button.
	ApplyPrefix, ApplyName string
}

// optimizableFields returns every field the sweep can test: the strategy
// form's fields with a swept axis, in the order they appear on that form,
// followed by the exits fields the sweep also covers (see nonStrategyAxes).
func optimizableFields() []pickerField {
	out := make([]pickerField, 0, len(optimizeAxis))

	for _, fld := range strategyFields {
		if _, ok := optimizeAxis[fld.Name]; ok {
			out = append(out, pickerField{
				Key: fld.Name, LabelPrefix: "strategy", LabelName: fld.Name,
				ApplyPrefix: "strategy", ApplyName: fld.Name,
			})
		}
	}

	for _, ax := range nonStrategyAxes {
		prefix := "settings." + ax.Section
		key := prefix + "." + ax.Name

		if _, ok := optimizeAxis[key]; !ok {
			continue
		}

		out = append(out, pickerField{Key: key, LabelPrefix: prefix, LabelName: ax.Name})
	}

	return out
}

// findPickerField looks up a picker field by its Key.
func findPickerField(fields []pickerField, key string) (pickerField, bool) {
	for _, fld := range fields {
		if fld.Key == key {
			return fld, true
		}
	}

	return pickerField{}, false
}

// optimizeMinBars is the fewest candles worth backtesting a single field
// against. Below this, indicator warm-up eats most of the series and the
// result is noise dressed as a number.
const optimizeMinBars = 150

// sweepSem bounds how many field-optimizer sweeps run concurrently,
// process-wide. Each sweep runs a full backtest per candidate value
// synchronously on the request-handling goroutine — cheap for one request
// against one cached symbol, as the package doc above assumes, but nothing
// bounded how many could run at once: a refreshing tab, more than one
// operator, or a client retrying a slow response could each start their own
// full sweep concurrently and stack up CPU work with no limit. A single
// slot matches what this panel actually is — one operator exploring one
// field interactively — not a batch job; a request that can't get the slot
// fails fast with a clear message instead of queuing (an unbounded queue
// wait would just move the problem, the same reasoning
// internal/ai.Advisor.refresh's own concurrency-slot fix used). See
// CODE_REVIEW_FINDINGS.md's "synchronous sensitivity-sweep backtest runs
// inline on a GET request with no bound on repetition or concurrency".
var sweepSem = make(chan struct{}, 1)

// sweepRowView adds what the template needs to render and act on one tested
// value: the exact string to write into the strategy form's input (which,
// for trend_gate, is not the same spelling Sensitivity reports — the sweep
// labels it "on"/"off" for readability, the form's select uses "true"/"false"),
// and whether this row is the best-scoring one.
type sweepRowView struct {
	backtest.SweepRow

	ApplyValue string
	Best       bool
}

// fragOptimizeField renders the field optimizer panel on the strategy tab: a
// symbol + field picker, and — once both are chosen — the ranked candidate
// values for that one field, holding everything else at its current setting.
func (s *Server) fragOptimizeField(c *gin.Context) {
	symbols := s.eng.Watchlist()

	symbol := strings.ToUpper(strings.TrimSpace(c.Query("symbol")))
	if symbol == "" && len(symbols) > 0 {
		symbol = symbols[0]
	}

	fieldName := c.Query("field")
	fields := optimizableFields()

	data := map[string]any{
		"Symbols": symbols, "Symbol": symbol,
		"Fields": fields, "Field": fieldName,
	}

	if pf, ok := findPickerField(fields, fieldName); ok {
		data["ApplyPrefix"] = pf.ApplyPrefix
		data["ApplyName"] = pf.ApplyName
	}

	if symbol == "" || fieldName == "" {
		render(c, "optimizeField", data)

		return
	}

	t := translator(langOf(c))

	if !contains(symbols, symbol) {
		data["Error"] = t("opt.unknownSymbol", "symbol", symbol)
		render(c, "optimizeField", data)

		return
	}

	axis, ok := optimizeAxis[fieldName]
	if !ok {
		data["Error"] = t("opt.unknownField")
		render(c, "optimizeField", data)

		return
	}

	candles := s.eng.SeriesResampled(symbol, market.Resolution("1"))
	if len(candles) < optimizeMinBars {
		data["Error"] = t("opt.notEnough", "symbol", symbol)
		render(c, "optimizeField", data)

		return
	}

	select {
	case sweepSem <- struct{}{}:
		defer func() { <-sweepSem }()
	default:
		data["Error"] = t("opt.busy")
		render(c, "optimizeField", data)

		return
	}

	baseline, sweep := s.runFieldSweep(symbol, axis, candles)
	if sweep == nil {
		data["Error"] = t("opt.unknownField")
		render(c, "optimizeField", data)

		return
	}

	rows, bestIdx := scoredRows(*sweep, fieldName)

	data["Baseline"] = baseline
	data["Rows"] = rows
	data["BestIndex"] = bestIdx
	data["Bars"] = len(candles)

	render(c, "optimizeField", data)
}

// runFieldSweep builds a backtest.Config from the live strategy/exits/costs/
// limits settings and runs the one-axis sweep against candles.
//
// respectLimits is off, same as the CLI's default: this is exploring what a
// field's best value looks like, not what today's money limits would let a
// live order size into. cash and the baseline capital fraction come from the
// account's own current equity and order size, so "25%" in the result means
// the same thing it would live.
func (s *Server) runFieldSweep(
	symbol, axis string, candles []market.Candle,
) (backtest.Result, *backtest.AxisSweep) {
	port, sectors, assetClasses := s.eng.PortfolioConfig()

	cfg := config.Config{
		Strategy:     s.eng.StrategyConfig(),
		Exits:        s.eng.ExitsConfig(),
		Costs:        s.eng.CostsConfig(),
		Engine:       s.eng.EngineConfig(),
		Limits:       s.rm.Limits(),
		Portfolio:    port,
		Sectors:      sectors,
		AssetClasses: assetClasses,
	}

	cash := s.pf.Snapshot().Equity
	if cash <= 0 {
		cash = 10000
	}

	// The per-trade deployment the search centres on. There is no
	// order_size_usd any more — sizing starts at the money-limit headroom —
	// so the natural basis is the tightest per-order ceiling the limits
	// impose. 0.25 when neither is set, matching the old default.
	baseFrac := 0.25

	if perTrade := cfg.Limits.TightestPerOrderCap(cash); perTrade > 0 && cash > 0 {
		baseFrac = perTrade / cash
	}

	data := map[string][]market.Candle{symbol: candles}

	return backtest.SensitivityAxis(cfg, data, cash, baseFrac, false, axis)
}

// scoredRows ranks a sweep's rows by the same blended score the optimizer
// defaults to, and translates each row's label into the exact value the
// strategy form's input for field expects.
func scoredRows(sweep backtest.AxisSweep, fieldName string) ([]sweepRowView, int) {
	score, _ := backtest.ScorerByName("blend") // "blend" is always a recognized name

	rows := make([]sweepRowView, len(sweep.Rows))
	best, bestScore := -1, math.Inf(-1)

	for i, row := range sweep.Rows {
		rows[i] = sweepRowView{SweepRow: row, ApplyValue: formApplyValue(fieldName, row.Label)}

		if sc := score(row.Result); sc > bestScore {
			bestScore, best = sc, i
		}
	}

	if best >= 0 {
		rows[best].Best = true
	}

	return rows, best
}

// formApplyValue converts a sweep row's display label into the value the
// strategy form's own input expects. Every axis but one prints the same
// spelling the form uses (a bare number, or one of the combine/rsi_mode-style
// select options); trend_gate is the exception, since the sweep labels it
// "on"/"off" for readability while the form's boolean select uses "true"/
// "false".
func formApplyValue(fieldName, label string) string {
	if fieldName != "trend_gate" {
		return label
	}

	if label == "on" {
		return "true"
	}

	return "false"
}
