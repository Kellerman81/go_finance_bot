package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// advancedBroker is a Sim that also implements every optional capability the
// order helpers type-assert for, recording what reached it.
type advancedBroker struct {
	*broker.Sim

	mu        sync.Mutex
	brackets  []broker.BracketOrder
	bracketEr error
	cancelled []string
	open      []broker.OpenOrder
	openErr   error
	listCalls int
	resolve   map[string]string // symbol -> ticker; missing = refused
	classes   map[string]string
}

func (b *advancedBroker) PlaceBracket(_ context.Context, o broker.BracketOrder) (broker.BracketResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.bracketEr != nil {
		return broker.BracketResult{}, b.bracketEr
	}

	b.brackets = append(b.brackets, o)

	return broker.BracketResult{EntryID: "E1", StopID: "S1", TakeProfitID: "T1"}, nil
}

func (b *advancedBroker) CancelOrders(_ context.Context, ids ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.cancelled = append(b.cancelled, ids...)

	return nil
}

func (b *advancedBroker) OpenOrders(context.Context) ([]broker.OpenOrder, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.listCalls++

	return b.open, b.openErr
}

func (b *advancedBroker) ModifyOrder(_ context.Context, m broker.OrderModification) (broker.OrderResult, error) {
	return broker.OrderResult{ID: m.OrderID}, nil
}

func (b *advancedBroker) ResolveSymbol(ctx context.Context, symbol string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if t, ok := b.resolve[symbol]; ok {
		return t, nil
	}

	return "", errors.New("instrument not found")
}

func (b *advancedBroker) AssetClass(_ context.Context, symbol string) (string, error) {
	if c, ok := b.classes[symbol]; ok {
		return c, nil
	}

	return "", errors.New("unknown")
}

// orderEngine is a fully constructed engine over brk with generous limits
// except where a test narrows them.
func orderEngine(t *testing.T, brk broker.Broker, limits config.Limits) *Engine {
	t.Helper()

	provider := market.NewMock(time.Hour)
	t.Cleanup(func() { _ = provider.Close() })

	pf := portfolio.New()
	pf.SetCash(10000)

	e := New(
		config.EngineConfig{EvalInterval: time.Hour}, config.ExitsConfig{},
		config.SafetyConfig{}, config.StrategyConfig{Detectors: []string{"ema_cross"}},
		provider, brk, risk.New(limits, config.CostsConfig{}), pf, NewPriceCache(), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	e.tradingEnabled = true

	return e
}

func generous() config.Limits {
	return config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10,
	}
}

func lastRecord(e *Engine) TradeRecord {
	if tr := e.Trades(); len(tr) > 0 {
		return tr[0]
	}

	return TradeRecord{}
}

// TestPlaceBracketPassesThroughRisk guards the manual bracket entry, the
// one order path the strategy never takes: its entry leg must clear
// risk.Authorize (and be CLAMPED by it, like every other buy), price itself
// from the live cache when no entry price is given, count against the daily
// spend once accepted, and — when the broker refuses it — release the
// reservation it held, or a refused bracket would eat buy headroom until
// restart. A broker without brackets, or no price, is refused up front.
func TestPlaceBracketPassesThroughRisk(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	plain := orderEngine(t, broker.NewSim(10000, func(string) (float64, bool) { return 100, true }, nil), generous())
	if _, err := plain.PlaceBracket(ctx, broker.BracketOrder{Symbol: "AAA", Side: broker.Buy, Qty: 1}); err == nil ||
		!strings.Contains(err.Error(), "does not support bracket") {
		t.Errorf("Sim bracket: err = %v", err)
	}

	if plain.AdvancedSupported() {
		t.Error("a plain Sim reports advanced order support")
	}

	limits := generous()
	limits.MaxOrderValue = 500

	brk := &advancedBroker{Sim: broker.NewSim(10000, func(string) (float64, bool) { return 100, true }, nil)}
	e := orderEngine(t, brk, limits)

	if !e.AdvancedSupported() {
		t.Error("a broker with every capability reports no advanced support")
	}

	if _, err := e.PlaceBracket(ctx, broker.BracketOrder{Symbol: "AAA", Side: broker.Buy, Qty: 1}); err == nil ||
		!strings.Contains(err.Error(), "no price") {
		t.Errorf("no cached price: err = %v", err)
	}

	e.prices.Set("AAA", 100)

	res, err := e.PlaceBracket(ctx, broker.BracketOrder{
		Symbol: "AAA", Side: broker.Buy, Qty: 20, StopLoss: 90, TakeProfit: 120,
	})
	if err != nil {
		t.Fatalf("PlaceBracket: %v", err)
	}

	if res.EntryID != "E1" || len(brk.brackets) != 1 {
		t.Fatalf("result %+v, %d brackets at the broker", res, len(brk.brackets))
	}

	if got := brk.brackets[0].Qty; got != 5 {
		t.Errorf("bracket qty at the broker = %v, want 5 (clamped by max_order_value 500 at 100)", got)
	}

	if got := e.rm.SpentToday(); got != 500 {
		t.Errorf("daily spend after the bracket = %v, want 500", got)
	}

	if r := lastRecord(e); r.Status != "bracket-submitted" || r.OrderID != "E1" {
		t.Errorf("record = %+v", r)
	}

	// 500 of a 1000 daily budget is spent: exactly one more 500 order fits,
	// so a reservation the refused bracket left behind would leave none.
	limits.MaxDailySpend = 1000
	e.rm.SetLimits(limits)

	brk.bracketEr = errors.New("venue closed")

	if _, err := e.PlaceBracket(ctx, broker.BracketOrder{Symbol: "AAA", Side: broker.Buy, Qty: 5}); err == nil {
		t.Fatal("a refused bracket reported success")
	}

	if r := lastRecord(e); r.Status != "error" || !strings.Contains(r.Err, "venue closed") {
		t.Errorf("refused bracket record = %+v", r)
	}

	if got := e.rm.SpentToday(); got != 500 {
		t.Errorf("a refused bracket counted as spend: %v, want 500", got)
	}

	if o, d := e.rm.Authorize(broker.Order{Symbol: "BBB", Side: broker.Buy, Qty: 5, Type: broker.Market},
		100, e.pf.Snapshot()); !d.Approved || o.Qty != 5 {
		t.Errorf("a refused bracket left a reservation behind: approved=%v qty=%v %s", d.Approved, o.Qty, d.Reason)
	}

	// A risk refusal is recorded as blocked and never reaches the broker.
	e.rm.SetLimits(config.Limits{MaxOpenPositions: 0, MaxDailySpend: 1})
	brk.bracketEr = nil
	before := len(brk.brackets)

	if _, err := e.PlaceBracket(ctx, broker.BracketOrder{Symbol: "AAA", Side: broker.Buy, Qty: 5}); err == nil ||
		!strings.Contains(err.Error(), "blocked by risk") {
		t.Errorf("risk refusal: err = %v", err)
	}

	if len(brk.brackets) != before {
		t.Error("a bracket blocked by risk reached the broker")
	}

	if r := lastRecord(e); r.Status != "blocked" || !strings.HasPrefix(r.Reason, "bracket: ") {
		t.Errorf("blocked record = %+v", r)
	}
}

// TestOrderCapabilityHelpers guards the capability-gated order helpers the
// API exposes: each must report an unsupported broker as an error naming
// it (the UI shows it) rather than panicking on a failed type assertion,
// pass a supported call through, and record a cancellation.
// CheckOrderable must keep a refused symbol in the list with the broker's
// reason instead of dropping it.
func TestOrderCapabilityHelpers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	plain := orderEngine(t, broker.NewSim(1000, nil, nil), generous())

	if err := plain.CancelOrders(ctx, "1"); err == nil || !strings.Contains(err.Error(), `"sim"`) {
		t.Errorf("Sim cancel: err = %v", err)
	}

	if _, err := plain.OpenOrders(ctx); err == nil {
		t.Error("Sim listed open orders")
	}

	if _, err := plain.CheckOrderable(ctx); err == nil {
		t.Error("Sim resolved symbols")
	}

	brk := &advancedBroker{
		Sim:     broker.NewSim(1000, nil, nil),
		open:    []broker.OpenOrder{{OrderID: "O1", Symbol: "AAA"}},
		resolve: map[string]string{"AAA": "AAA_US_EQ"},
	}
	e := orderEngine(t, brk, generous())
	e.watchlist["AAA"] = struct{}{}
	e.watchlist["ZZZ"] = struct{}{}

	if err := e.CancelOrders(ctx, "O1", "O2"); err != nil {
		t.Fatalf("CancelOrders: %v", err)
	}

	if strings.Join(brk.cancelled, ",") != "O1,O2" {
		t.Errorf("cancelled = %v", brk.cancelled)
	}

	if r := lastRecord(e); r.Status != "cancelled" || r.Reason != "cancel O1,O2" {
		t.Errorf("cancel record = %+v", r)
	}

	if oo, err := e.OpenOrders(ctx); err != nil || len(oo) != 1 || oo[0].OrderID != "O1" {
		t.Errorf("OpenOrders = %v, %v", oo, err)
	}

	got, err := e.CheckOrderable(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("CheckOrderable = %v, %v", got, err)
	}

	if !got[0].Orderable || got[0].Ticker != "AAA_US_EQ" {
		t.Errorf("AAA = %+v", got[0])
	}

	if got[1].Symbol != "ZZZ" || got[1].Orderable || got[1].Note == "" {
		t.Errorf("ZZZ = %+v, want listed as not orderable with the broker's reason", got[1])
	}
}

// TestTradeableAtBrokerSeparatesVerdictFromFailure guards the symbol
// picker's "can my broker trade this?" column: a refusal is a KNOWN "no",
// but a broker that cannot answer — no resolver, or our own context ending —
// is "unknown", never rendered as "your broker cannot trade this".
func TestTradeableAtBrokerSeparatesVerdictFromFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	if tr, known := orderEngine(t, broker.NewSim(1000, nil, nil), generous()).
		TradeableAtBroker(ctx, "AAA"); tr || known {
		t.Errorf("no resolver: tradeable=%v known=%v, want unknown", tr, known)
	}

	brk := &advancedBroker{Sim: broker.NewSim(1000, nil, nil), resolve: map[string]string{"AAA": "A"}}
	e := orderEngine(t, brk, generous())

	if tr, known := e.TradeableAtBroker(ctx, "AAA"); !tr || !known {
		t.Errorf("resolvable: %v %v", tr, known)
	}

	if tr, known := e.TradeableAtBroker(ctx, "ZZZ"); tr || !known {
		t.Errorf("refused: %v %v, want a known no", tr, known)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	if tr, known := e.TradeableAtBroker(cctx, "ZZZ"); tr || known {
		t.Errorf("cancelled context: %v %v, want unknown", tr, known)
	}
}

// TestBrokerAssetClassNormalises guards the broker fallback classifier: a
// broker's own spelling ("Etf", "STOCK") must fold into the canonical class
// the allocation caps are keyed by, and a broker that does not know the
// symbol leaves it unclassified rather than guessing.
func TestBrokerAssetClassNormalises(t *testing.T) {
	t.Parallel()

	brk := &advancedBroker{Sim: broker.NewSim(1000, nil, nil), classes: map[string]string{"VUSA": "Etf", "AAA": "STOCK"}}
	e := orderEngine(t, brk, generous())
	ctx := context.Background()

	if got := e.brokerAssetClass(ctx, brk, "VUSA"); got != "etf" {
		t.Errorf("VUSA -> %q, want etf", got)
	}

	if got := e.brokerAssetClass(ctx, brk, "AAA"); got != "stock" {
		t.Errorf("AAA -> %q, want stock", got)
	}

	if got := e.brokerAssetClass(ctx, brk, "NOPE"); got != "" {
		t.Errorf("unknown -> %q, want empty", got)
	}
}

// TestReconcileClearsSettledSellGuards guards the double-sell guard's
// release: a pendingSell marker must survive while its sell is still working
// at the broker and be cleared once it is not (or every later exit of that
// symbol stays blocked). The poll is throttled to reconcileTTL, and a failed
// listing changes nothing — a broker outage must not release a guard.
func TestReconcileClearsSettledSellGuards(t *testing.T) {
	t.Parallel()

	brk := &advancedBroker{
		Sim:  broker.NewSim(1000, nil, nil),
		open: []broker.OpenOrder{{Symbol: "AAA", Side: broker.Sell}, {Symbol: "BBB", Side: broker.Buy}},
	}
	e := orderEngine(t, brk, generous())
	e.pendingSell["AAA"] = 5
	e.pendingSell["BBB"] = 3

	ctx := context.Background()

	brk.openErr = errors.New("timeout")
	e.reconcile(ctx)

	if len(e.pendingSell) != 2 {
		t.Fatalf("a failed listing cleared guards: %v", e.pendingSell)
	}

	brk.openErr = nil
	e.reconcile(ctx)

	if _, ok := e.pendingSell["AAA"]; !ok {
		t.Error("the guard of a still-working sell was cleared")
	}

	if _, ok := e.pendingSell["BBB"]; ok {
		t.Error("the guard of a symbol with no working sell survived")
	}

	if got := e.Metrics().OpenOrders; got != 2 {
		t.Errorf("open-order metric = %d, want 2", got)
	}

	// Inside the TTL a second call does not poll.
	brk.open = nil
	calls := brk.listCalls
	e.reconcile(ctx)

	if brk.listCalls != calls {
		t.Error("reconcile polled again inside its TTL")
	}

	if e.reconcileTTL() != defaultReconcileTTL {
		t.Errorf("reconcileTTL with no sync_interval = %v", e.reconcileTTL())
	}

	e.cfg.SyncInterval = time.Minute
	if e.reconcileTTL() != time.Minute {
		t.Errorf("reconcileTTL = %v, want the configured minute", e.reconcileTTL())
	}

	// A broker that cannot list orders is left alone.
	plain := orderEngine(t, broker.NewSim(1000, nil, nil), generous())
	plain.pendingSell["AAA"] = 1
	plain.reconcile(ctx)

	if len(plain.pendingSell) != 1 {
		t.Error("reconcile on a non-listing broker cleared a guard")
	}
}

// TestRefreshFXRatesIsThrottled guards the FX cache's refresh: it runs on
// the sync path, so it must re-resolve only once per fxRefreshInterval — a
// rate fetch per symbol per sync would hammer a paced broker — and skip a
// broker with no currency resolver entirely.
func TestRefreshFXRatesIsThrottled(t *testing.T) {
	t.Parallel()

	brk := &currencyBroker{rates: map[string]float64{"NVDA": 0.9}}
	e := fxTestEngine(brk)
	e.watchlist = map[string]struct{}{"NVDA": {}}

	e.refreshFXRates()

	if got := e.fxRateFor("NVDA"); got != 0.9 {
		t.Fatalf("first refresh: rate %v, want 0.9", got)
	}

	brk.rates["NVDA"] = 0.5
	e.refreshFXRates()

	if got := e.fxRateFor("NVDA"); got != 0.9 {
		t.Errorf("a refresh inside the interval re-resolved: rate %v", got)
	}

	e.fxSyncAt = time.Now().Add(-fxRefreshInterval - time.Second)
	e.refreshFXRates()

	if got := e.fxRateFor("NVDA"); got != 0.5 {
		t.Errorf("a stale cache was not refreshed: rate %v, want 0.5", got)
	}

	plain := fxTestEngine(broker.NewSim(1000, nil, nil))
	plain.refreshFXRates()

	if !plain.fxSyncAt.IsZero() {
		t.Error("a broker with no currency resolver started the refresh clock")
	}
}

// statsStore answers TradeStats with a fixed result and records the cutoff.
type statsStore struct {
	nopStore

	since time.Time
	err   error
}

func (s *statsStore) TradeStats(since time.Time) (TradeStats, error) {
	s.since = since

	return TradeStats{Wins: 3, Losses: 1}, s.err
}

// TestStatsPeriods guards the dashboard's performance window: the period
// keyword picks the cutoff passed to the store ("all" is the zero time, an
// unknown or empty one is "week"), the result is labelled with the canonical
// period, and a store error is passed up rather than reported as zero
// trades.
func TestStatsPeriods(t *testing.T) {
	t.Parallel()

	st := &statsStore{}
	e := &Engine{store: st}

	for _, tc := range []struct {
		in, label string
		days      int
	}{
		{"week", "week", 7}, {" Month ", "month", 30}, {"", "week", 7}, {"bogus", "week", 7},
	} {
		s, err := e.Stats(tc.in)
		if err != nil || s.Period != tc.label || s.Wins != 3 {
			t.Errorf("Stats(%q) = %+v, %v", tc.in, s, err)
		}

		// ±2h: the cutoff is a calendar date, so a DST change inside the
		// window moves it by an hour.
		if age := time.Since(st.since); age < time.Duration(tc.days)*24*time.Hour-2*time.Hour ||
			age > time.Duration(tc.days)*24*time.Hour+2*time.Hour {
			t.Errorf("Stats(%q) cutoff %v ago, want %d days", tc.in, age, tc.days)
		}
	}

	if s, _ := e.Stats("all"); s.Period != "all" || !st.since.IsZero() {
		t.Errorf("Stats(all) = %+v, cutoff %v; want the zero time", s, st.since)
	}

	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if s, err := e.StatsSince(since); err != nil || s.Period != "custom" || !s.Since.Equal(since) ||
		!st.since.Equal(since) {
		t.Errorf("StatsSince = %+v, %v", s, err)
	}

	st.err = errors.New("db locked")

	if _, err := e.Stats("week"); err == nil {
		t.Error("a store error was swallowed by Stats")
	}

	if _, err := e.StatsSince(since); err == nil {
		t.Error("a store error was swallowed by StatsSince")
	}
}

// TestClassifyExitKind guards the exit breakdown on the stats panel: every
// reason exits.Compute writes must land in its own bucket — a reason that
// fell through to "strategy" would credit a stop-out to the signal — and a
// partial take-profit rung is still a take-profit.
func TestClassifyExitKind(t *testing.T) {
	t.Parallel()

	for reason, want := range map[string]string{
		"trailing-stop 95.00<=96.00":                          ExitKindTrailingStop,
		"stop-loss 90.00<=90.50":                              ExitKindStopLoss,
		"roi 1h +2.0% 105.00>=104.00":                         ExitKindROI,
		"take-profit +20.0% 120.00>=120.00":                   ExitKindTakeProfit,
		"partial take-profit rung 1 (+5.0%, 50% of position)": ExitKindTakeProfit,
		"max hold 720h0m0s reached":                           ExitKindMaxHold,
		"strategy SELL 0.62":                                  ExitKindStrategy,
		"":                                                    ExitKindStrategy,
	} {
		if got := ClassifyExitKind(reason); got != want {
			t.Errorf("ClassifyExitKind(%q) = %q, want %q", reason, got, want)
		}
	}
}

// TestSettingsAccessorsRoundTrip guards the runtime settings the API writes
// and reads back: a save that wrote a copy would show the new value in the
// form while the engine kept trading on the old one. SetBehavior must also
// clamp confirmations below 1 and ignore a negative keep_interval rather
// than apply them.
func TestSettingsAccessorsRoundTrip(t *testing.T) {
	t.Parallel()

	e := orderEngine(t, broker.NewSim(1000, nil, nil), generous())

	e.SetBehavior(2*time.Hour, 3, 4)

	if k, b, s := e.Behavior(); k != 2*time.Hour || b != 3 || s != 4 {
		t.Errorf("Behavior = %v %d %d", k, b, s)
	}

	e.SetBehavior(-time.Minute, 0, -2)

	if k, b, s := e.Behavior(); k != 2*time.Hour || b != 3 || s != 4 {
		t.Errorf("invalid SetBehavior changed settings: %v %d %d", k, b, s)
	}

	p := EntryPolicy{ScaleIn: config.ScaleInConfig{Enabled: true, MaxAdds: 2}, SkipFullSlots: true}
	e.SetEntryPolicy(p)

	if got := e.EntryPolicy(); got.ScaleIn.MaxAdds != 2 || !got.SkipFullSlots || got.AllowPyramiding {
		t.Errorf("EntryPolicy = %+v", got)
	}

	e.SetAutotuneConfig(config.AutotuneConfig{Enabled: true, MinTrades: 7})

	if got := e.AutotuneConfig(); !got.Enabled || got.MinTrades != 7 {
		t.Errorf("AutotuneConfig = %+v", got)
	}

	e.SetNotifyConfig(config.NotifyConfig{Enabled: true, MinNotional: 50})

	if got := e.NotifyConfig(); !got.Enabled || got.MinNotional != 50 {
		t.Errorf("NotifyConfig = %+v", got)
	}

	if s := e.NotifyStats(); len(s.Sinks) != 0 {
		t.Errorf("NotifyStats with no sink configured = %+v", s)
	}

	e.SetConfigPath("/tmp/x.yaml")

	if e.configFile() != "/tmp/x.yaml" {
		t.Errorf("configFile = %q", e.configFile())
	}

	if e.LossTuneRequests() == nil {
		t.Error("LossTuneRequests is nil: autotune.Schedule would block forever")
	}

	if res := e.LastScreen(); len(res.Verdicts) != 0 {
		t.Errorf("LastScreen before any pass = %+v", res)
	}

	// With no screener configured a pass is a no-op that still "ran".
	if !e.RescreenNow(context.Background()) {
		t.Error("RescreenNow with no screener reported a pass already running")
	}

	if got := e.StrategyConfig(); !slices.Equal(got.Detectors, []string{"ema_cross"}) {
		t.Errorf("StrategyConfig = %+v", got)
	}
}

// TestStrategyViews guards the API's strategy views: OverriddenSymbols
// lists exactly the symbols with their own parameters, StrategyWarmup is
// the LONGEST warmup across the shared strategy and every override (the
// warmup report reads it to say when trading can start), Signals is sorted,
// signalFor answers HOLD for a symbol never evaluated, and DetectorSignals
// covers every watched symbol.
func TestStrategyViews(t *testing.T) {
	t.Parallel()

	e := orderEngine(t, broker.NewSim(1000, nil, nil), generous())

	long := config.StrategyConfig{Detectors: []string{"ema_cross"}, FastMA: 50, SlowMA: 400}
	e.SetSymbolStrategies(map[string]config.StrategyConfig{"AAA": long})

	if got := e.OverriddenSymbols(); !slices.Equal(got, []string{"AAA"}) {
		t.Errorf("OverriddenSymbols = %v", got)
	}

	shared, override := e.start.WarmupBars(), e.symStrategies["AAA"].WarmupBars()
	if override <= shared {
		t.Fatalf("setup: override warmup %d is not longer than the shared %d", override, shared)
	}

	if w := e.StrategyWarmup(); w != override {
		t.Errorf("StrategyWarmup = %d, want the override's %d (shared %d)", w, override, shared)
	}

	e.signals["ZZZ"] = strategy.Signal{Symbol: "ZZZ", Action: strategy.Sell}
	e.signals["AAA"] = strategy.Signal{Symbol: "AAA", Action: strategy.Buy}

	if sig := e.Signals(); len(sig) != 2 || sig[0].Symbol != "AAA" {
		t.Errorf("Signals = %+v, want sorted", sig)
	}

	if e.signalFor("ZZZ") != strategy.Sell || e.signalFor("NEW") != strategy.Hold {
		t.Error("signalFor did not return the recorded action / HOLD")
	}

	e.watchlist["AAA"] = struct{}{}
	e.watchlist["BBB"] = struct{}{}

	ds := e.DetectorSignals()
	if len(ds) != 2 || len(ds["AAA"]) != len(strategy.AvailableDetectors()) {
		t.Errorf("DetectorSignals covers %d symbols, AAA has %d detectors", len(ds), len(ds["AAA"]))
	}
}

// TestEngineReturnsUsesItsOwnBars guards the correlation cap's input in
// live trading: returns come from the engine's own bars, at most n of them,
// and a symbol with fewer than two bars is "unknown" rather than an empty
// series the guard would read as "uncorrelated".
func TestEngineReturnsUsesItsOwnBars(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(nil)

	if _, ok := e.Returns("AAA", 5); ok {
		t.Error("a symbol with no bars reported returns")
	}

	base := time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)
	e.barsStore.seed("AAA", []market.Candle{
		{Time: base, Close: 100}, {Time: base.Add(time.Minute), Close: 110},
		{Time: base.Add(2 * time.Minute), Close: 99}, {Time: base.Add(3 * time.Minute), Close: 120},
	})

	rets, ok := e.Returns("AAA", 2)
	if !ok || len(rets) != 2 {
		t.Fatalf("Returns(AAA, 2) = %v, %v; want the last two", rets, ok)
	}

	if d := rets[1] - (120.0/99 - 1); d > 1e-12 || d < -1e-12 {
		t.Errorf("last return = %v, want 120/99-1", rets[1])
	}
}

// TestSmallHelpers covers the leaf helpers the alerts and order paths format
// with: orNone, errNoPrice, PriceCache.Snapshot (a COPY — the API must not be
// able to edit the engine's live prices) and nopStore, the no-persistence
// store, which must answer every call with "nothing", never an error that
// would stop a persistence-free bot.
func TestSmallHelpers(t *testing.T) {
	t.Parallel()

	if orNone(nil) != "none" || orNone([]string{"A", "B"}) != "A, B" {
		t.Error("orNone")
	}

	if err := errNoPrice("AAA"); err == nil || !strings.Contains(err.Error(), "AAA") {
		t.Errorf("errNoPrice = %v", err)
	}

	pc := NewPriceCache()
	pc.Set("AAA", 10)

	snap := pc.Snapshot()
	snap["AAA"] = 99

	if p, _ := pc.Get("AAA"); p != 10 {
		t.Error("editing a Snapshot changed the live price")
	}

	var s Store = nopStore{}

	errs := []error{
		s.SaveTrade(TradeRecord{}), s.SaveWatchlist(nil), s.SavePinned(nil), s.SaveNames(nil),
		s.SaveSectors(nil), s.SaveAssetClasses(nil), s.SaveLossStreaks(nil), s.SaveAutotuneApplies(nil),
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("nopStore save %d = %v", i, err)
		}
	}

	if st, err := s.TradeStats(time.Time{}); err != nil || st.Wins != 0 {
		t.Errorf("nopStore TradeStats = %+v, %v", st, err)
	}
}
