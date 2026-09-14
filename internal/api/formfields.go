package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/gin-gonic/gin"
)

// Form descriptors, and the decoding of what comes back.
//
// This is what the browser's fields.js and forms.js used to be. Moving it here
// removed the larger of the two: forms.js spent 267 lines parsing durations,
// rung ladders and key:value maps and range-checking them — all of which this
// process already did on the way in, in config.Validate and the section
// handlers. Now the form posts and the existing validation answers.
//
// A field is declared once. The label and help text come from the translation
// catalog under `<prefix>.<name>.label` / `.help`, exactly as before, so adding
// a setting is still one line here and two strings per language.

// fieldKind decides how a field renders and how its value is decoded.
type fieldKind string

const (
	kindText   fieldKind = "text"   // a string
	kindNum    fieldKind = "num"    // a float
	kindInt    fieldKind = "int"    // an integer
	kindBool   fieldKind = "bool"   // true/false
	kindDur    fieldKind = "dur"    // a Go duration string ("30m", "0s")
	kindCSV    fieldKind = "csv"    // comma-separated list → []string
	kindKV     fieldKind = "kv"     // "a:x, b:y" → map[string]string
	kindKVF    fieldKind = "kvf"    // "a:1, b:2" → map[string]float64
	kindKVI    fieldKind = "kvi"    // "a:2, b:3" → map[string]int
	kindRungs  fieldKind = "rungs"  // "0.05:0.33" → [{profit_pct, sell_pct}]
	kindSelect fieldKind = "select" // one of Options
	kindSecret fieldKind = "secret" // rendered as a password input
)

// field is one editable setting.
type field struct {
	// Name is the JSON name the endpoint uses. A dotted name addresses a nested
	// block (scale_in.max_adds) and is rebuilt on the way back.
	Name string
	Kind fieldKind
	// Options are the choices for a select. An empty first option means "unset".
	Options []string
}

// f is shorthand for declaring a field.
func f(name string, kind fieldKind, options ...string) field {
	return field{Name: name, Kind: kind, Options: options}
}

// settingsSections are the /api/settings blocks, most consequential first.
var settingsSections = []struct {
	Name   string
	Danger bool // renders the block's `.danger` warning above the form
	Fields []field
}{
	{Name: "exits", Fields: []field{
		f("enabled", kindBool),
		f("bypass_trading_halt", kindBool),
		f("stop_loss_pct", kindNum),
		f("trailing_stop_pct", kindNum),
		f("take_profit_pct", kindNum),
		f("atr_period", kindInt),
		f("atr_stop_mult", kindNum),
		f("atr_trail_mult", kindNum),
		f("trailing_offset_pct", kindNum),
		f("trailing_tight_pct", kindNum),
		f("trailing_only_after_offset", kindBool),
		f("max_hold", kindDur),
		f("roi", kindKVF),
		f("partial_take_profit", kindRungs),
	}},
	{Name: "entries", Danger: true, Fields: []field{
		f("allow_pyramiding", kindBool),
		f("skip_full_slots", kindBool),
		f("scale_in.enabled", kindBool),
		f("scale_in.step_pct", kindNum),
		f("scale_in.max_adds", kindInt),
		f("scale_in.size_pct", kindNum),
		f("scale_in.require_signal", kindBool),
	}},
	{Name: "protections", Fields: []field{
		f("enabled", kindBool),
		f("cooldown_after_loss", kindDur),
		f("stop_guard_trades", kindInt),
		f("stop_guard_lookback", kindDur),
		f("stop_guard_lock", kindDur),
		f("max_drawdown_pct", kindNum),
		f("max_daily_loss_pct", kindNum),
		f("max_realized_drawdown_pct", kindNum),
		f("low_profit_trades", kindInt),
		f("low_profit_lookback", kindDur),
		f("low_profit_lock", kindDur),
		f("low_profit_min_pnl", kindNum),
		f("drop_after_losses", kindInt),
	}},
	{Name: "costs", Fields: []field{
		f("commission_pct", kindNum),
		f("commission_flat", kindNum),
		f("commission_min", kindNum),
		f("spread_pct", kindNum),
		f("slippage_pct", kindNum),
		f("measure_spread", kindBool),
	}},
	{Name: "portfolio", Fields: []field{
		f("enabled", kindBool),
		f("max_per_sector", kindNum),
		f("max_positions_per_sector", kindInt),
		f("sector_caps_per_asset_class", kindBool),
		f("correlation_threshold", kindNum),
		f("max_correlated_positions", kindInt),
		f("correlation_lookback", kindInt),
		f("max_pct_per_asset_class", kindKVF),
		f("min_positions_per_asset_class", kindKVI),
		f("sectors", kindKV),
		f("asset_classes", kindKV),
	}},
	{Name: "screener", Fields: []field{
		f("enabled", kindBool),
		f("universe", kindCSV),
		f("interval", kindDur),
		f("max_symbols", kindInt),
		f("max_symbols_per_sector", kindInt),
		f("rank", kindSelect, "volume", "volatility", "momentum", "trend", "cost"),
		f("min_price", kindNum),
		f("max_price", kindNum),
		f("min_avg_volume", kindNum),
		f("min_volatility_pct", kindNum),
		f("max_volatility_pct", kindNum),
		f("min_history_bars", kindInt),
		f("max_spread_pct", kindNum),
		f("filters", kindCSV),
		f("resolution", kindSelect, "", "1", "5", "15", "60", "D"),
		f("lookback_days", kindInt),
		f("min_trend_r2", kindNum),
		f("min_trend_slope_pct", kindNum),
		f("max_drawdown_pct", kindNum),
		f("min_cost_multiple", kindNum),
		f("suggest_universe", kindBool),
		f("probe.enabled", kindBool),
		f("probe.backtest", kindBool),
		f("probe.autotune", kindBool),
		f("probe.max_candidates", kindInt),
		f("probe.min_trades", kindInt),
		f("probe.min_expectancy_pct", kindNum),
		f("keep_held", kindBool),
		f("keep_manual", kindBool),
	}},
	{Name: "autotune", Danger: true, Fields: []field{
		f("enabled", kindBool),
		f("interval", kindDur),
		f("bars", kindInt),
		f("min_trades", kindInt),
		f("objective", kindSelect, backtest.ScorerNames...),
		f("min_return_pct", kindNum),
		f("run_only_for_symbols", kindCSV),
		f("exclude_symbols", kindCSV),
		f("on_loss.enabled", kindBool),
		f("on_loss.after_losses", kindInt),
		f("on_loss.cooldown", kindDur),
	}},
	{Name: "test_mode", Danger: true, Fields: []field{
		f("enabled", kindBool),
		f("duration", kindDur),
	}},
	{Name: "notify", Fields: []field{
		f("enabled", kindBool),
		f("events", kindCSV),
		f("min_notional", kindNum),
		f("timeout", kindDur),
		f("webhook", kindSecret),
		f("discord_webhook", kindSecret),
		f("telegram_token", kindSecret),
		f("telegram_chat_id", kindText),
		f("pushover_token", kindSecret),
		f("pushover_user", kindSecret),
		f("pushover_device", kindText),
		f("pushover_priority", kindInt),
		f("apprise_url", kindText),
		f("apprise_services", kindSecret),
		f("apprise_tag", kindText),
	}},
	{Name: "ai", Fields: []field{
		f("enabled", kindBool),
		f("provider", kindSelect, "ollama", "openai"),
		f("base_url", kindText),
		f("model", kindText),
		f("api_key", kindSecret),
		f("timeout", kindDur),
		f("refresh_interval", kindDur),
		f("max_age", kindDur),
		f("blocking", kindBool),
		f("temperature", kindNum),
		f("max_concurrent", kindInt),
		f("min_confidence", kindNum),
		f("max_strength", kindNum),
		f("system_prompt", kindText),
	}},
	{Name: "ml", Fields: []field{
		f("enabled", kindBool),
		f("horizon", kindInt),
		f("threshold", kindNum),
		f("train_bars", kindInt),
		f("retrain_every", kindInt),
		f("min_samples", kindInt),
		f("buy_prob", kindNum),
		f("sell_prob", kindNum),
		f("min_skill", kindNum),
		f("max_strength", kindNum),
		f("epochs", kindInt),
		f("learning_rate", kindNum),
		f("l2", kindNum),
	}},
}

// strategyFields drive /api/strategy. Names must match
// config.StrategyConfig's own `json` tags (snake_case) exactly: the form's
// posted body is decoded straight into that struct via ShouldBindJSON, and
// the displayed values come from marshalling that same struct — a name that
// doesn't match the tag silently fails both directions (an empty field on
// load, and a save that quietly resets everything to its zero value instead
// of applying what was submitted). See CODE_REVIEW_FINDINGS.md's "/v2
// strategy page fields render empty" entry.
var strategyFields = []field{
	f("detectors", kindCSV),
	f("detectors_sell", kindCSV),
	f("weights", kindKVF),
	f("combine", kindSelect, "majority", "unanimous", "weighted"),
	f("combine_sell", kindSelect, "", "majority", "unanimous", "weighted"),
	f("min_strength", kindNum),
	f("min_strength_sell", kindNum),
	f("fast_ma", kindInt), f("slow_ma", kindInt),
	f("rsi_period", kindInt),
	f("rsi_mode", kindSelect, "midline", "extremes"),
	f("rsi_overbought", kindNum), f("rsi_oversold", kindNum),
	f("macd_fast", kindInt), f("macd_slow", kindInt), f("macd_signal", kindInt),
	f("bb_period", kindInt), f("bb_stddev", kindNum),
	f("vwap_period", kindInt),
	f("atr_period", kindInt), f("atr_mult", kindNum),
	f("rvol_period", kindInt), f("rvol_threshold", kindNum),
	f("vp_window", kindInt), f("vp_buckets", kindInt), f("vp_value_area_pct", kindNum),
	f("trend_period", kindInt), f("trend_threshold", kindNum), f("trend_gate", kindBool),
	f("trend_period_sell", kindInt), f("trend_threshold_sell", kindNum),
	f("deny_gate", kindBool),
	f("deny_breakout_period", kindInt), f("deny_breakout_rvol_period", kindInt),
	f("deny_breakout_rvol_threshold", kindNum),
	f("deny_reversal_stoch_period", kindInt), f("deny_reversal_overbought", kindNum),
	f("adx_period", kindInt), f("adx_threshold", kindNum),
	f("stoch_k_period", kindInt), f("stoch_d_period", kindInt),
	f("stoch_overbought", kindNum), f("stoch_oversold", kindNum),
	f("supertrend_period", kindInt), f("supertrend_mult", kindNum),
	f("donchian_period", kindInt),
}

// limitFields, sizingFields and behaviorFields drive the blocks that keep their
// own endpoints.
//
// limitFields' names must match config.Limits' own `json` tags (snake_case)
// exactly, for the same reason spelled out on strategyFields above — see
// CODE_REVIEW_FINDINGS.md's "/v2 money-limits fields render empty" entry.
var (
	limitFields = []field{
		f("max_total_invested", kindNum), f("max_total_invested_pct", kindNum),
		f("max_per_position", kindNum), f("max_per_position_pct", kindNum),
		f("max_order_value", kindNum), f("max_order_value_pct", kindNum),
		f("max_daily_spend", kindNum), f("max_daily_spend_pct", kindNum),
		f("cash_reserve", kindNum), f("cash_reserve_pct", kindNum),
		f("max_open_positions", kindInt),
	}
	sizingFields = []field{
		f("min_positions", kindInt), f("max_risk_size_usd", kindNum),
		f("min_order_size_usd", kindNum),
	}
	behaviorFields = []field{
		f("keep_interval", kindDur), f("confirm_buy", kindInt), f("confirm_sell", kindInt),
	}
)

// ----- rendering -----

// formValue returns the text to show in a field's input, given the block's
// current values decoded from JSON.
//
//nolint:cyclop // one case per field kind; a flat dispatch
func formValue(values map[string]any, fl field) string {
	v := lookupPath(values, fl.Name)
	if v == nil {
		return ""
	}

	switch fl.Kind {
	case kindBool:
		if b, ok := v.(bool); ok && b {
			return "true"
		}

		return "false"

	case kindCSV:
		items, _ := v.([]any)
		parts := make([]string, 0, len(items))

		for _, it := range items {
			parts = append(parts, fmt.Sprint(it))
		}

		return strings.Join(parts, ", ")

	case kindKV, kindKVF, kindKVI:
		m, _ := v.(map[string]any)

		return joinPairs(m)

	case kindRungs:
		items, _ := v.([]any)
		parts := make([]string, 0, len(items))

		for _, it := range items {
			r, _ := it.(map[string]any)

			parts = append(parts, trimNum(r["profit_pct"])+":"+trimNum(r["sell_pct"]))
		}

		return strings.Join(parts, ", ")

	case kindNum, kindInt:
		return trimNum(v)

	case kindText, kindDur, kindSelect, kindSecret:
		return fmt.Sprint(v)
	}

	return fmt.Sprint(v)
}

// lookupPath resolves a dotted name through nested maps.
func lookupPath(values map[string]any, name string) any {
	var cur any = values

	for _, part := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}

		cur, ok = m[part]
		if !ok {
			return nil
		}
	}

	return cur
}

// joinPairs renders a map as "a:x, b:y" with stable ordering.
func joinPairs(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sortStrings(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + trimNum(m[k])
	}

	return strings.Join(parts, ", ")
}

// trimNum renders a JSON number without a trailing run of zeros, so 0.330000
// reads as 0.33 in the input the operator edits.
func trimNum(v any) string {
	switch n := v.(type) {
	case float64:
		s := strconv.FormatFloat(n, 'f', -1, 64)

		return s

	case string:
		return n
	case nil:
		return ""
	default:
		return fmt.Sprint(n)
	}
}

// sortStrings sorts in place (a tiny local helper so this file needs no import
// juggling with the render helpers).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ----- decoding -----

// decodeForm turns a posted form into the JSON body the API section handler
// expects, using the field declarations to decide each value's type.
//
// It does not validate. The handlers behind /api/settings already do, and
// duplicating their rules here is how the two drift apart.
func decodeForm(c *gin.Context, fields []field) (map[string]any, error) {
	out := make(map[string]any)

	for _, fl := range fields {
		raw := strings.TrimSpace(c.PostForm(fl.Name))

		value, err := decodeField(fl, raw)
		if err != nil {
			return nil, err
		}

		setPath(out, fl.Name, value)
	}

	return out, nil
}

// decodeField converts one submitted string to its JSON shape.
//
//nolint:cyclop // one case per field kind; a flat dispatch
func decodeField(fl field, raw string) (any, error) {
	switch fl.Kind {
	case kindBool:
		return raw == "true", nil

	case kindInt:
		if raw == "" {
			return 0, nil
		}

		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a whole number", fl.Name, raw)
		}

		return n, nil

	case kindNum:
		if raw == "" {
			return 0.0, nil
		}

		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number", fl.Name, raw)
		}

		return n, nil

	case kindDur:
		// Empty means zero, and zero must still be a duration string: the API
		// parses these with time.ParseDuration, which rejects a bare 0.
		if raw == "" {
			return "0s", nil
		}

		if _, err := time.ParseDuration(raw); err != nil {
			return nil, fmt.Errorf(
				"%s: %q is not a duration — use 30m, 2h or 0s (there is no day unit; write 72h, not 3d)",
				fl.Name,
				raw,
			)
		}

		return raw, nil

	case kindCSV:
		return splitList(raw), nil

	case kindKV:
		return decodeStringMap(raw), nil

	case kindKVF:
		return decodeFloatMap(fl, raw)

	case kindKVI:
		return decodeIntMap(fl, raw)

	case kindRungs:
		return decodeRungs(fl, raw)

	case kindText, kindSelect, kindSecret:
		return raw, nil
	}

	return raw, nil
}

// decodeStringMap reads "a:x, b:y".
func decodeStringMap(raw string) map[string]any {
	out := make(map[string]any)

	for _, pair := range splitStrings(raw) {
		if k, v, ok := strings.Cut(pair, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	return out
}

// decodeFloatMap reads "a:1, b:2" — detector weights and the ROI ladder.
func decodeFloatMap(fl field, raw string) (map[string]any, error) {
	out := make(map[string]any)

	for _, pair := range splitStrings(raw) {
		k, v, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}

		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number in %q", fl.Name, v, pair)
		}

		out[strings.TrimSpace(k)] = n
	}

	return out, nil
}

// decodeIntMap reads "a:2, b:3" — counts, not fractions. It refuses "2.5"
// here rather than letting the JSON decoder reject it one layer down, where
// the message names a Go type instead of the field the operator typed in.
func decodeIntMap(fl field, raw string) (map[string]any, error) {
	out := make(map[string]any)

	for _, pair := range splitStrings(raw) {
		k, v, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}

		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a whole number in %q", fl.Name, v, pair)
		}

		out[strings.TrimSpace(k)] = n
	}

	return out, nil
}

// decodeRungs reads the scale-out ladder, "0.05:0.33, 0.12:0.33".
func decodeRungs(fl field, raw string) ([]any, error) {
	var out []any

	for _, pair := range splitStrings(raw) {
		profit, sell, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, fmt.Errorf(
				"%s: write rungs as profit:fraction pairs, e.g. 0.05:0.33", fl.Name)
		}

		p, err1 := strconv.ParseFloat(strings.TrimSpace(profit), 64)
		s, err2 := strconv.ParseFloat(strings.TrimSpace(sell), 64)

		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("%s: %q is not a profit:fraction pair", fl.Name, pair)
		}

		out = append(out, map[string]any{"profit_pct": p, "sell_pct": s})
	}

	return out, nil
}

// setPath writes a dotted name into a nested map.
func setPath(out map[string]any, name string, value any) {
	parts := strings.Split(name, ".")
	cur := out

	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = make(map[string]any)
			cur[p] = next
		}

		cur = next
	}

	cur[parts[len(parts)-1]] = value
}

// splitList splits a comma-separated field into the []any a JSON array needs.
func splitList(raw string) []any {
	parts := splitStrings(raw)
	out := make([]any, len(parts))

	for i, p := range parts {
		out[i] = p
	}

	return out
}

// splitStrings splits a comma-separated field, dropping blanks.
func splitStrings(raw string) []string {
	var out []string

	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}

	return out
}

// asMap round-trips a value through JSON so templates can address it by the
// same names the API uses, including through the wire types that rewrite
// durations as strings.
func asMap(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		return make(map[string]any)
	}

	out := make(map[string]any)

	_ = json.Unmarshal(raw, &out)

	return out
}
