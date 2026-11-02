package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	aipkg "github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	mlpkg "github.com/Kellerman81/go_finance_bot/internal/ml"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
	"github.com/gin-gonic/gin"
)

// The v2 fragment handlers: one per screen, each rendering a template from the
// same values the JSON API returns.
//
// These are the "views" the browser used to build. A fragment is plain HTML with
// no wrapper — the client swaps it into a container — so there is no envelope
// format to agree on and nothing to parse.

// v2Tabs is the navigation, in order.
var v2Tabs = []string{
	"dashboard", "chart", "positions", "trades",
	"watchlist", "strategy", "settings", "diagnostics",
}

// serveV2Shell renders the page skeleton. Everything inside it arrives later as
// a fragment, so this is the only full document the server sends.
func (*Server) serveV2Shell(c *gin.Context) {
	render(c, "layout", map[string]any{"Tabs": v2Tabs})
}

// serveV2Login renders the login fragment. It is registered unauthenticated
// (see registerV2) so a client with no token can fetch the very screen that
// lets it enter one.
func (*Server) serveV2Login(c *gin.Context) {
	render(c, "login", map[string]any{"Rejected": c.Query("rejected") == "1"})
}

// serveV2Fragment dispatches to one screen's renderer.
//
//nolint:cyclop // one case per screen; a flat dispatch
func (s *Server) serveV2Fragment(c *gin.Context) {
	switch c.Param("name") {
	case "pills":
		s.fragPills(c)
	case "dashboard":
		s.fragDashboard(c)
	case "activity":
		s.fragActivity(c)
	case "chart":
		s.fragChart(c)
	case "positions":
		s.fragPositions(c)
	case "trades":
		s.fragTrades(c)
	case "watchlist":
		s.fragWatchlist(c)
	case "search":
		s.fragSearch(c)
	case "strategy":
		s.fragStrategy(c)
	case "optimizeField":
		s.fragOptimizeField(c)
	case "symbolOptimize":
		s.fragSymbolOptimize(c)
	case "settings":
		s.fragSettings(c)
	case "diagnostics":
		s.fragDiagnostics(c)
	default:
		c.String(http.StatusNotFound, "no such view")
	}
}

func (s *Server) fragPills(c *gin.Context) {
	render(c, "pills", map[string]any{
		"Status":      s.eng.Status(),
		"Positions":   s.pf.Snapshot(),
		"Protections": s.eng.Protections().Status(),
	})
}

func (s *Server) fragDashboard(c *gin.Context) {
	snap := s.pf.Snapshot()

	render(c, "dashboard", map[string]any{
		"Status":    s.eng.Status(),
		"Positions": snap,
		"Metrics":   s.eng.Metrics(),
		"Open":      s.eng.OpenPositions(),
		"Signals":   s.eng.Signals(),
		"Trades":    recent(s.eng.Trades(), 40),
	})
}

func (s *Server) fragActivity(c *gin.Context) {
	render(c, "activity", map[string]any{"Trades": recent(s.eng.Trades(), 40)})
}

func (s *Server) fragPositions(c *gin.Context) {
	snap := s.pf.Snapshot()
	tracked := s.eng.OpenPositions()

	// AssetClasses(), not Engine.AssetClass(ctx, sym): the latter falls back to
	// a live provider lookup for an unknown symbol, which has no business
	// running once per holding inside a page render.
	port, _, classes := s.eng.PortfolioConfig()

	// No caps while portfolio limits are off: the guard is nil then and
	// enforces nothing, and a panel showing ceilings and "At cap" there
	// described limits that do not bind (AP2-7).
	var caps map[string]float64

	if port.Enabled {
		caps = port.NormalizedAssetClassCaps()
	}

	render(c, "positions", map[string]any{
		"Holdings":   sortedHoldings(snap),
		"Tracked":    tracked,
		"Drift":      reconcile(snap, tracked),
		"Allocation": allocation(snap, classes, caps),
	})
}

// lossStreakTarget is the consecutive-loss count that triggers a re-tune, or 0
// when the trigger is off — so a row only advertises a streak the bot is
// actually counting toward something.
func lossStreakTarget(cfg config.AutotuneConfig) int {
	if !cfg.Enabled || !cfg.OnLoss.Enabled {
		return 0
	}

	if cfg.OnLoss.AfterLosses > 0 {
		return cfg.OnLoss.AfterLosses
	}

	return defaultOnLossAfterLosses
}

// defaultOnLossAfterLosses mirrors config.AutotuneOnLossConfig.AfterLosses'
// documented zero-value fallback, so the UI counts toward the same number the
// engine does.
const defaultOnLossAfterLosses = 3

// driftRow is one disagreement between the bot's records and the broker's.
type driftRow struct {
	Symbol      string
	Bot, Broker float64
}

// driftTolerance is the relative quantity difference below which the two sides
// count as agreeing.
//
// An absolute epsilon does not work here. Brokers that sell fractional shares
// round the filled quantity, so a position the bot recorded as 3.0926 comes back
// as 3.0920 — a 0.02% difference that is settlement, not a tracking failure.
// Flagging it every time teaches the operator to ignore this panel, which is the
// one place a genuinely stranded position would show up.
//
// One percent is far above any rounding and far below any difference that would
// matter: on a 3-share position it is 0.03 shares.
const driftTolerance = 0.01

// reconcile lists the symbols the two sides disagree about.
//
// A record with no matching holding is the one that matters: the bot keeps exits
// armed and a slot occupied for stock it no longer owns, usually because it was
// sold by hand. The mirror case — held but not tracked — means the bot is not
// managing that position at all: no stop, no ladder, no time barrier.
func reconcile(snap portfolio.Snapshot, tracked []engine.OpenPosition) []driftRow {
	held := make(map[string]float64, len(snap.Holdings))

	for sym, h := range snap.Holdings {
		// Dust is not a position: shown as "held, not tracked" it invited an
		// Adopt click that recorded a permanent dust position (PF3-4).
		if h.Held() {
			held[sym] = h.Qty
		}
	}

	var out []driftRow

	for _, p := range tracked {
		// held is keyed the way the snapshot is — normalised — while an
		// openPos key is whatever the order carried; a raw lookup reported a
		// differently-cased record as both "not held" and "not tracked" (PF2-6).
		key := portfolio.Normalize(p.Symbol)

		broker := held[key]
		if disagrees(p.Qty, broker) {
			out = append(out, driftRow{Symbol: p.Symbol, Bot: p.Qty, Broker: broker})
		}

		delete(held, key)
	}

	for sym, qty := range held {
		out = append(out, driftRow{Symbol: sym, Bot: 0, Broker: qty})
	}

	slices.SortFunc(out, func(a, b driftRow) int { return cmp.Compare(a.Symbol, b.Symbol) })

	return out
}

func (s *Server) fragTrades(c *gin.Context) {
	period := c.DefaultQuery("period", "week")
	symbol := strings.ToUpper(strings.TrimSpace(c.Query("symbol")))
	status := c.Query("status")

	stats, err := s.eng.Stats(period)
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)

		return
	}

	// Filtering happens here rather than in the browser. It costs a round-trip
	// where the client-side version cost none, and it removes the filter state
	// machine the browser used to hold.
	var rows []engine.TradeRecord

	for _, t := range s.eng.Trades() {
		if symbol != "" && !strings.Contains(t.Symbol, symbol) {
			continue
		}

		if status != "" && t.Status != status {
			continue
		}

		rows = append(rows, t)
	}

	// No reversal: engine.Trades already returns newest-first and the filter
	// loop preserves that order. The decision log leads with the newest entry.

	render(c, "trades", map[string]any{
		"Stats": map[string]any{
			"Trades": stats.Trades, "Wins": stats.Wins, "Losses": stats.Losses,
			"WinRate": stats.WinRatePct / 100, "TotalPnL": stats.TotalPnL,
			"AvgPnL": avgPnL(stats.TotalPnL, stats.Trades),
		},
		"Trades":       rows,
		"Period":       period,
		"Periods":      []string{"week", "month", "all"},
		"Symbol":       symbol,
		"StatusFilter": status,
		"Statuses":     []string{"", "filled", "dry-run", "blocked", "error"},
	})
}

// watchRow is one watchlist entry and where it came from. Auto marks a symbol
// the screener found rather than one the operator chose, which matters because
// a later screening pass may drop it again — the operator should be able to see
// which of the names on the list are only visiting.
type watchRow struct {
	Symbol string
	// Name is the company or fund name, blank when not resolved yet. A row of
	// LXS.DE tells an operator nothing; "LANXESS Aktiengesellschaft" does.
	Name string
	Auto bool
	// Refused is the broker's reason for rejecting this instrument outright.
	// Non-empty means the bot has stopped trying to enter it — the operator has
	// to decide whether to remove it, since the bot deliberately will not.
	Refused string
	// AutotuneExcluded reports whether this symbol is currently named in
	// AutotuneConfig.ExcludeSymbols — kept out of the scheduled
	// auto-optimizer's reach entirely, whatever the search would otherwise
	// find for it. Drives both the row's tag and which of the
	// exclude/include button labels is shown.
	AutotuneExcluded bool
	// AutotuneRunOnly reports whether this symbol is currently named in
	// AutotuneConfig.RunOnlyForSymbols — an ALLOW-list, not a per-symbol
	// flag on its own: as soon as this is non-empty ANYWHERE on the
	// watchlist, every symbol NOT named in it becomes ineligible too. See
	// the page-level RunOnlyActive/RunOnlySymbols for that wider effect;
	// this field only drives THIS row's own button label/tag.
	AutotuneRunOnly bool
	// AssetClass and Sector are the buckets the screener's per-class profiles
	// and the portfolio caps put this symbol in. Shown because a cap that
	// refuses an order is only legible once you can see which bucket the
	// symbol sits in — and because a misclassification is otherwise invisible
	// until it changes a decision.
	AssetClass string
	Sector     string
	// LossStreak is how many realized closes in a row this symbol has lost,
	// and LossStreakOf the count that triggers an autotune re-tune. 0 for the
	// latter means the trigger is off, so the streak is not advertised at all.
	LossStreak   int
	LossStreakOf int
}

func (s *Server) fragWatchlist(c *gin.Context) {
	last := s.eng.LastScreen()

	pinned := make(map[string]bool)
	for _, sym := range s.eng.Pinned() {
		pinned[sym] = true
	}

	refused := s.eng.Untradeable()
	names := s.eng.SymbolNames()
	symbols := s.eng.Watchlist()
	rows := make([]watchRow, 0, len(symbols))

	autotune := s.eng.AutotuneConfig()

	// Cached maps only — the live-lookup forms of these would fire a provider
	// call per row on every page render.
	_, sectors, classes := s.eng.PortfolioConfig()
	streaks := s.eng.LossStreaks()

	excluded := make(map[string]bool, len(autotune.ExcludeSymbols))
	for _, sym := range autotune.ExcludeSymbols {
		excluded[strings.ToUpper(strings.TrimSpace(sym))] = true
	}

	runOnly := make(map[string]bool, len(autotune.RunOnlyForSymbols))
	for _, sym := range autotune.RunOnlyForSymbols {
		runOnly[strings.ToUpper(strings.TrimSpace(sym))] = true
	}

	for _, sym := range symbols {
		key := strings.ToUpper(strings.TrimSpace(sym))
		rows = append(rows, watchRow{
			Symbol:           sym,
			Name:             names[sym],
			Auto:             !pinned[sym],
			Refused:          refused[sym],
			AutotuneExcluded: excluded[key],
			AutotuneRunOnly:  runOnly[key],
			AssetClass:       classes[key],
			Sector:           sectors[key],
			LossStreak:       streaks[key],
			LossStreakOf:     lossStreakTarget(autotune),
		})
	}

	render(c, "watchlist", map[string]any{
		"Symbols":        rows,
		"RunOnlyActive":  len(autotune.RunOnlyForSymbols) > 0,
		"RunOnlySymbols": autotune.RunOnlyForSymbols,
		"CanSearch":      s.eng.CanSearch(),
		"Quotes":         sortedQuotes(s.prices.Snapshot()),
		"Verdicts":       last.Verdicts,
		"ScreenedAt":     last.At,
	})
}

// fragSearch renders the symbol picker's results.
//
// Search is server-side for the same reason filtering is: the browser would
// otherwise need the query state, a debounce, a renderer and an error path, and
// this way it needs a form.
func (s *Server) fragSearch(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))

	data := map[string]any{"Query": query, "Searched": query != ""}

	if query == "" {
		render(c, "search", data)

		return
	}

	found, err := s.eng.SearchInstruments(c.Request.Context(), query, searchResults)
	if err != nil {
		// "Search is unavailable" and "no matches" mean very different things
		// to someone who just typed a company name, so they are not merged.
		data["Error"] = err.Error()
		render(c, "search", data)

		return
	}

	data["Results"] = s.annotateTradeable(c, found)
	render(c, "search", data)
}

// searchRow is one match plus what the broker makes of it.
type searchRow struct {
	market.Instrument

	// Tradeable is meaningful only when Known. A broker that cannot answer must
	// not have its silence rendered as a refusal.
	Tradeable bool
	Known     bool
}

// annotateTradeable asks the broker about each match and floats the ones it can
// actually trade to the top.
//
// A search for one company returns every venue it lists on — Lanxess comes back
// as XETRA, Milan, Frankfurt, Vienna, Dusseldorf, Stuttgart and Munich. Most
// accounts can trade one of them. Offering seven identical-looking rows with no
// hint which is which is how an operator ends up with an instrument the broker
// refuses outright, which is a failure they only discover at the first buy.
func (s *Server) annotateTradeable(c *gin.Context, found []market.Instrument) []searchRow {
	rows := make([]searchRow, 0, len(found))

	for _, inst := range found {
		ok, known := s.eng.TradeableAtBroker(c.Request.Context(), inst.Symbol)

		rows = append(rows, searchRow{Instrument: inst, Tradeable: ok, Known: known})
	}

	// Stable, so within each group the provider's own relevance order survives.
	// Unknown sorts with tradeable: when the broker cannot answer, the honest
	// presentation is the provider's ranking, not a guess.
	slices.SortStableFunc(rows, func(a, b searchRow) int {
		return cmp.Compare(rank(b), rank(a))
	})

	return rows
}

// rank scores a row for ordering: tradeable first, unknown next, refused last.
func rank(r searchRow) int {
	switch {
	case !r.Known:
		return 1
	case r.Tradeable:
		return 2
	default:
		return 0
	}
}

// searchResults is how many matches the picker offers. It is scanned by eye, so
// a longer list is a worse one.
const searchResults = 12

func (s *Server) fragStrategy(c *gin.Context) {
	cfg := s.eng.StrategyConfig()

	names, matrix := detectorMatrix(s.eng.DetectorSignals())

	render(c, "strategy", map[string]any{
		"Available": strings.Join(strategy.AvailableDetectors(), ", "),
		"Fields":    strategyFields,
		"Values":    asMap(cfg),
		"Names":     names,
		"Matrix":    matrix,
		"Overrides": symbolOverrides(s.eng, cfg),
		"Health":    symbolHealth(s.eng),
	})
}

// symbolHealthRow is one traded symbol's realized track record plus its
// current protection-lock state, side by side — the two things that decide
// whether a symbol is worth a strategy_overrides entry (see
// symbolOptimizeResult): is it actually losing, and specifically how (a
// stop-loss-heavy pattern is a different problem from a strategy-exit-heavy
// one).
type symbolHealthRow struct {
	Symbol     string
	Trades     int
	WinRatePct float64
	TotalPnL   float64
	StopLosses int
	Locked     bool
	LockReason string
	LockUntil  string
}

// symbolHealth lists every symbol with at least one realized trade, all
// time, joined against the protections' current lock state.
func symbolHealth(eng *engine.Engine) []symbolHealthRow {
	stats, err := eng.Stats("all")
	if err != nil {
		return nil
	}

	locked := eng.Protections().Status().Locked

	out := make([]symbolHealthRow, 0, len(stats.BySymbol))

	for _, sym := range stats.BySymbol {
		row := symbolHealthRow{
			Symbol: sym.Symbol, Trades: sym.Trades,
			WinRatePct: sym.WinRatePct, TotalPnL: sym.TotalPnL,
			StopLosses: sym.ExitBreakdown[engine.ExitKindStopLoss],
		}

		if info, ok := locked[sym.Symbol]; ok {
			row.Locked, row.LockReason, row.LockUntil = true, info.Reason, info.Until
		}

		out = append(out, row)
	}

	slices.SortFunc(out, func(a, b symbolHealthRow) int { return cmp.Compare(a.Symbol, b.Symbol) })

	return out
}

// symbolOverrideRow is one symbol's strategy_overrides entry, reduced to just
// the fields that actually differ from the shared config — the override
// struct inherits everything it doesn't name, so showing all of it would bury
// the handful of fields someone actually changed under dozens that just match
// the base. Read-only: there is no write path for strategy_overrides yet (it
// can only be edited by hand in config.yaml — see config.Patch's doc comment
// on why a dynamic per-symbol section isn't a simple addition).
type symbolOverrideRow struct {
	Symbol string
	Diff   []string // "field: value" pairs, in strategyFields' declared order
}

// symbolOverrides lists every symbol with its own strategy parameters
// (engine.OverriddenSymbols), each reduced to a diff against base via
// engine.StrategyConfigFor.
func symbolOverrides(eng *engine.Engine, base config.StrategyConfig) []symbolOverrideRow {
	syms := eng.OverriddenSymbols()
	slices.Sort(syms)

	baseMap := asMap(base)
	out := make([]symbolOverrideRow, 0, len(syms))

	for _, sym := range syms {
		m := asMap(eng.StrategyConfigFor(sym))

		var diff []string

		for _, fld := range strategyFields {
			if bv, mv := baseMap[fld.Name], m[fld.Name]; fmt.Sprint(bv) != fmt.Sprint(mv) {
				diff = append(diff, fmt.Sprintf("%s: %v", fld.Name, mv))
			}
		}

		out = append(out, symbolOverrideRow{Symbol: sym, Diff: diff})
	}

	return out
}

// matrixCell is one detector's reading for one symbol.
type matrixCell struct {
	Action  string
	Reason  string
	Enabled bool
}

// matrixRow is a symbol's readings, in the shared column order.
type matrixRow struct {
	Symbol string
	Cells  []matrixCell
}

// detectorMatrix pivots the per-symbol detector results into a table, so every
// row has a cell in every column even when a detector reported nothing.
func detectorMatrix(bySymbol map[string][]strategy.DetectorResult) ([]string, []matrixRow) {
	seen := make(map[string]bool)

	var names []string

	for _, results := range bySymbol {
		for _, r := range results {
			if !seen[r.Name] {
				seen[r.Name] = true

				names = append(names, r.Name)
			}
		}
	}

	slices.Sort(names)

	symbols := make([]string, 0, len(bySymbol))
	for sym := range bySymbol {
		symbols = append(symbols, sym)
	}

	slices.Sort(symbols)

	rows := make([]matrixRow, 0, len(symbols))

	for _, sym := range symbols {
		byName := make(map[string]strategy.DetectorResult, len(bySymbol[sym]))
		for _, r := range bySymbol[sym] {
			byName[r.Name] = r
		}

		row := matrixRow{Symbol: sym, Cells: make([]matrixCell, 0, len(names))}

		for _, n := range names {
			r, ok := byName[n]
			if !ok {
				row.Cells = append(row.Cells, matrixCell{Action: "—"})

				continue
			}

			row.Cells = append(row.Cells, matrixCell{
				Action: string(r.Action), Reason: r.Reason, Enabled: r.Enabled,
			})
		}

		rows = append(rows, row)
	}

	return names, rows
}

// settingsBlock is one collapsible settings section, ready to render.
type settingsBlock struct {
	Name   string
	Action string
	Danger bool
	Fields []field
	Values map[string]any
}

func (s *Server) fragSettings(c *gin.Context) {
	port, _, _ := s.eng.PortfolioConfig()

	// The form edits the HAND-WRITTEN maps: filled from the merged view, a
	// save posted every learned label back as a hand-written one (AP3-3,
	// SG2-2 again through the form round trip).
	sectors, assetClasses := s.eng.PortfolioOverrides()

	values := map[string]map[string]any{
		"exits":   asMap(wrapExits(s.eng.ExitsConfig())),
		"entries": asMap(s.eng.EntryPolicy()),
		"costs":   asMap(s.eng.CostsConfig()),
		// Portfolio's sector and asset-class maps live beside the block in
		// the API; the form edits them together, so they are merged here.
		"portfolio":   withSymbolMaps(asMap(port), sectors, assetClasses),
		"protections": asMap(wrapProtections(s.eng.ProtectionsConfig())),
		"screener":    asMap(wrapScreener(s.eng.ScreenerConfig())),
		"autotune":    asMap(wrapAutotune(s.eng.AutotuneConfig())),
		"notify":      asMap(wrapNotify(redactNotify(s.eng.NotifyConfig()))),
		"ai":          asMap(wrapAI(redactAI(s.aiConfig()))),
		"ml":          asMap(s.mlConfig()),
	}

	blocks := make([]settingsBlock, 0, len(settingsSections)+3)

	for _, sec := range settingsSections {
		blocks = append(blocks, settingsBlock{
			Name: sec.Name, Action: "settings/" + sec.Name, Danger: sec.Danger,
			Fields: sec.Fields, Values: values[sec.Name],
		})
	}

	minPos, maxRisk, minOrderSize := s.eng.Sizing()
	keep, confirmBuy, confirmSell := s.eng.Behavior()

	blocks = append(blocks,
		settingsBlock{
			Name: "limits", Action: "limits", Fields: limitFields,
			Values: asMap(s.rm.Limits()),
		},
		settingsBlock{
			Name: "sizing", Action: "sizing", Fields: sizingFields,
			Values: map[string]any{
				"min_positions":      minPos,
				"max_risk_size_usd":  maxRisk,
				"min_order_size_usd": minOrderSize,
			},
		},
		settingsBlock{
			Name: "behavior", Action: "behavior", Fields: behaviorFields,
			Values: map[string]any{
				"keep_interval": keep.String(),
				"confirm_buy":   confirmBuy,
				"confirm_sell":  confirmSell,
			},
		})

	render(c, "settings", map[string]any{"Blocks": blocks})
}

// withSymbolMaps folds the per-symbol sector and asset-class maps into the
// portfolio block's values, which are otherwise flat scalars.
func withSymbolMaps(
	values map[string]any,
	sectors, assetClasses map[string]string,
) map[string]any {
	values["sectors"] = symbolMap(sectors)
	values["asset_classes"] = symbolMap(assetClasses)

	return values
}

// symbolMap widens a symbol -> label map into the any-valued form the form
// renderer expects.
func symbolMap(labels map[string]string) map[string]any {
	m := make(map[string]any, len(labels))
	for k, v := range labels {
		m[k] = v
	}

	return m
}

// lockRow is one symbol's protection lockout.
type lockRow struct{ Symbol, Until, Reason string }

func (s *Server) fragDiagnostics(c *gin.Context) {
	prot := s.eng.Protections().Status()

	locks := make([]lockRow, 0, len(prot.Locked))
	for sym, info := range prot.Locked {
		locks = append(locks, lockRow{Symbol: sym, Until: info.Until, Reason: info.Reason})
	}

	slices.SortFunc(locks, func(a, b lockRow) int { return cmp.Compare(a.Symbol, b.Symbol) })

	aiStats := aipkg.Default().Stats()
	mlStats := mlpkg.Default().Stats()

	render(c, "diagnostics", map[string]any{
		"Protections": prot,
		"Locked":      locks,
		"LockedCount": len(locks),
		"Errors":      recentErrors(),
		"AI": map[string]any{
			"Enabled": aiStats.Enabled, "Provider": aiStats.Provider, "Model": aiStats.Model,
			"Calls": aiStats.Calls, "Errors": aiStats.Errors, "Cached": aiStats.Cached,
			"LastError": aiStats.LastError,
		},
		"ML": map[string]any{
			"Enabled": mlStats.Enabled, "Models": mlStats.Symbols, "MeanSkill": mlStats.AvgSkill,
		},
		"Notify": s.eng.NotifyStats(),
	})
}

// recentErrors returns the error log newest-first.
// recentErrors returns the API error log, newest first — which is the order
// errlog.Recent already produces, so this only exists to say so at the call
// site rather than leaving a bare errlog.Recent() looking un-ordered.
func recentErrors() []errlog.Entry {
	return errlog.Recent()
}

// ----- small shared helpers -----

// quoteRow pairs a symbol with its last price, for stable table ordering.
type quoteRow struct {
	Symbol string
	Price  float64
}

func sortedQuotes(m map[string]float64) []quoteRow {
	out := make([]quoteRow, 0, len(m))
	for sym, px := range m {
		out = append(out, quoteRow{sym, px})
	}

	slices.SortFunc(out, func(a, b quoteRow) int { return cmp.Compare(a.Symbol, b.Symbol) })

	return out
}

func sortedHoldings(snap portfolio.Snapshot) []portfolio.Holding {
	out := make([]portfolio.Holding, 0, len(snap.Holdings))

	for _, h := range snap.Holdings {
		if h.Qty > 0 {
			out = append(out, h)
		}
	}

	slices.SortFunc(
		out,
		func(a, b portfolio.Holding) int { return cmp.Compare(a.Symbol, b.Symbol) },
	)

	return out
}

// recent returns the last n entries, newest first.
// recent returns the newest n entries, newest first.
//
// It takes its input ALREADY newest-first — both sources hand it over that way
// (engine.Trades and errlog.Recent each reverse their append-order buffer before
// returning). So the newest n are at the head, not the tail, and no reversal is
// wanted. Getting this backwards is quiet and nasty: the view still renders a
// full table, it is just the oldest records, upside down.
func recent[T any](all []T, n int) []T {
	if len(all) > n {
		all = all[:n]
	}

	return slices.Clone(all)
}

func avgPnL(total float64, trades int) float64 {
	if trades == 0 {
		return 0
	}

	return total / float64(trades)
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}

	return v
}

// disagrees reports whether two quantities differ by more than broker rounding.
// One side being zero always counts: that is a position one side does not know
// about, which is the case this panel exists for.
func disagrees(bot, broker float64) bool {
	if bot <= 0 || broker <= 0 {
		return bot > 0 || broker > 0
	}

	return abs(bot-broker)/max(bot, broker) > driftTolerance
}

// parseIntDefault reads a positive integer query parameter.
func parseIntDefault(raw string, fallback int) int {
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return n
	}

	return fallback
}

// resolutionChoice is one entry in the chart's timeframe menu.
type resolutionChoice struct{ Value, Label string }

var chartResolutions = []resolutionChoice{
	{"1", "1m"}, {"5", "5m"}, {"15", "15m"}, {"60", "1h"}, {"D", "1d"},
}

var chartBarChoices = []int{120, 240, 480, 1000}
