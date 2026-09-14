package engine

import (
	"context"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/notify"
	"github.com/Kellerman81/go_finance_bot/internal/protect"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/screener"
)

// Live-editable settings.
//
// Every setter here rebuilds whatever component owns the setting, so a change
// takes effect on the next evaluation without a restart. Two of them are more
// subtle than they look and are handled explicitly rather than by rebuilding
// blindly:
//
//   - Protections keep their state across an edit. Rebuilding would clear an
//     active halt as a side effect of adjusting an unrelated threshold, turning
//     a settings tweak into the deliberate act that Resume is supposed to be.
//   - The AI and ML detectors are captured when the strategy is built, so
//     changing their configuration also has to rebuild the strategy. Without
//     that, enabling the advisor appears to do nothing until a restart.

// CostsConfig returns the active commission and friction model.
func (e *Engine) CostsConfig() config.CostsConfig { return e.rm.Costs() }

// SetCostsConfig replaces the commission and friction model. It feeds order
// sizing and the cash-headroom checks immediately, and the simulated broker's
// fill friction when one is in use.
func (e *Engine) SetCostsConfig(c config.CostsConfig) {
	e.rm.SetCosts(c)

	// The simulated broker prices its own fills; a live broker charges what it
	// charges and ignores this. The closure resolves the symbol's own measured
	// spread at fill time rather than at install time, so a later screening pass
	// updates the friction without re-installing anything.
	if sim, ok := e.brk.(*broker.Sim); ok {
		sim.SetSlippage(func(symbol string, buy bool, price float64) float64 {
			return e.CostsFor(symbol).FillPrice(buy, price)
		})
	}

	e.log.Info("costs updated",
		"commission_pct", c.CommissionPct, "spread_pct", c.SpreadPct,
		"slippage_pct", c.SlippagePct)
}

// CostsFor returns the cost model to charge for one symbol: the shared
// commission and slippage, with the spread replaced by that instrument's own
// measured value when costs.measure_spread is on and a measurement exists.
//
// The configured spread_pct stays a floor. An estimate says the real cost is at
// least this much; it is not licence to assume a symbol is cheaper to trade than
// you budgeted for.
func (e *Engine) CostsFor(symbol string) config.CostsConfig {
	c := e.rm.Costs()
	if !c.MeasureSpread {
		return c
	}

	c.SpreadPct = c.SpreadFor(e.SpreadFor(symbol))

	return c
}

// SpreadFor returns the last measured bid-ask spread for symbol as a fraction of
// price, or 0 when it has never been screened.
func (e *Engine) SpreadFor(symbol string) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.spreads[symbol]
}

// ScaleInConfig returns the active averaging-down ladder settings.
func (e *Engine) ScaleInConfig() config.ScaleInConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.cfg.ScaleIn
}

// EntryPolicy groups the settings that decide when a NEW position (or an
// addition to one) may be opened, as opposed to how one is sized or exited.
type EntryPolicy struct {
	ScaleIn         config.ScaleInConfig `json:"scale_in"`
	SkipFullSlots   bool                 `json:"skip_full_slots"`
	AllowPyramiding bool                 `json:"allow_pyramiding"`
}

// EngineConfig returns a copy of the engine's own configuration block.
//
// It exists so the API can write the live `engine:` settings back to the YAML
// file. Returning a copy matters: the caller must not be able to reach in and
// change evaluation cadence without going through the guarded setters.
func (e *Engine) EngineConfig() config.EngineConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.cfg
}

// EntryPolicy returns the active entry rules.
func (e *Engine) EntryPolicy() EntryPolicy {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return EntryPolicy{
		ScaleIn:         e.cfg.ScaleIn,
		SkipFullSlots:   e.cfg.SkipFullSlots,
		AllowPyramiding: e.cfg.AllowPyramiding,
	}
}

// SetEntryPolicy replaces the entry rules. They are read per evaluation, so the
// change takes effect on the next cycle with nothing to rebuild.
func (e *Engine) SetEntryPolicy(p EntryPolicy) {
	e.mu.Lock()

	e.cfg.ScaleIn = p.ScaleIn
	e.cfg.SkipFullSlots = p.SkipFullSlots
	e.cfg.AllowPyramiding = p.AllowPyramiding
	e.mu.Unlock()

	e.log.Info("entry policy updated",
		"scale_in", p.ScaleIn.Enabled, "max_adds", p.ScaleIn.MaxAdds,
		"skip_full_slots", p.SkipFullSlots, "allow_pyramiding", p.AllowPyramiding)
}

// ProtectionsConfig returns the active protection thresholds.
func (e *Engine) ProtectionsConfig() config.ProtectionsConfig {
	if p := e.Protections(); p != nil {
		return p.Config()
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.protCfg
}

// SetProtectionsConfig applies new protection thresholds, preserving any active
// halt and symbol lockouts. An existing manager is never destroyed by turning
// protections off — that would silently clear an active halt, which is
// supposed to require an explicit Resume — it is left in place with
// SetConfig, and protect.Manager's gating methods treat cfg.Enabled=false as
// "allow everything" while keeping halted/lockedUntil/peakEquity intact for a
// later re-enable. A manager is only created when turning protections on for
// the first time (none exists yet); turning them on-then-off-then-on again
// reuses the same instance throughout.
func (e *Engine) SetProtectionsConfig(cfg config.ProtectionsConfig) {
	e.mu.Lock()

	e.protCfg = cfg

	current := e.prot
	e.mu.Unlock()

	switch {
	case current != nil:
		current.SetConfig(cfg) // keeps halt state and lockouts regardless of cfg.Enabled
	case cfg.Enabled:
		e.SetProtections(protect.New(cfg))
	}
	// current == nil && !cfg.Enabled: nothing to create, nothing to preserve.

	e.log.Info("protections updated", "enabled", cfg.Enabled,
		"max_drawdown_pct", cfg.MaxDrawdownPct, "cooldown_after_loss", cfg.CooldownAfterLoss)
}

// PortfolioConfig returns the active concentration caps, the sector map and the
// asset-class map.
func (e *Engine) PortfolioConfig() (config.PortfolioConfig, map[string]string, map[string]string) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.portCfg, e.sectors, e.assetClasses
}

// SetPortfolioConfig replaces the concentration caps. The guard is stateless, so
// a plain rebuild is correct here.
//
// The caps, the sector map and the asset-class map are replaced together
// because the guard reads all three: swapping them one at a time would leave a
// window in which a cap is enforced against a map that does not yet describe
// the symbols it applies to.
func (e *Engine) SetPortfolioConfig(
	cfg config.PortfolioConfig,
	sectors, assetClasses map[string]string,
) {
	e.mu.Lock()

	e.portCfg, e.sectors, e.assetClasses = cfg, sectors, assetClasses
	e.mu.Unlock()

	e.SetPortfolioGuard(risk.NewPortfolioGuard(cfg, sectors, assetClasses, e))

	// The screener enforces the same sector limit at selection time and is
	// told how to count it at construction, so a change to
	// SectorCapsPerAssetClass has to reach it too — otherwise the two would
	// disagree until something else happened to rebuild the screener.
	if screen := e.ScreenerConfig(); screen.Enabled {
		e.SetScreenerConfig(screen)
	}

	e.log.Info("portfolio limits updated", "enabled", cfg.Enabled,
		"max_per_sector", cfg.MaxPerSector,
		"max_correlated_positions", cfg.MaxCorrelatedPositions,
		"max_pct_per_asset_class", cfg.MaxPctPerAssetClass)
}

// ScreenerConfig returns the active universe-selection settings.
func (e *Engine) ScreenerConfig() config.ScreenerConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.screenCfg
}

// SetScreenerConfig replaces the universe-selection settings.
//
// The filters take effect on the next screening pass. The *interval* does not:
// the ticker was started when Run began, so changing it needs a restart — which
// the API reports rather than pretending otherwise.
func (e *Engine) SetScreenerConfig(cfg config.ScreenerConfig) {
	e.mu.Lock()

	e.screenCfg = cfg

	hist := e.history
	e.mu.Unlock()

	if !cfg.Enabled || hist == nil {
		e.SetScreener(nil)
		e.log.Info("screener disabled")

		return
	}

	port, _, _ := e.PortfolioConfig()

	e.SetScreener(screener.New(cfg, hist, market.Res1Min, screener.Options{
		Classify:                e,
		Sectors:                 e,
		Prober:                  e.screenProber(),
		Costs:                   e.CostsConfig(),
		SectorCapsPerAssetClass: port.SectorCapsPerAssetClass,
	}))
	e.log.Info("screener updated",
		"max_symbols", cfg.MaxSymbols, "rank", cfg.Rank,
		"max_symbols_per_sector", cfg.MaxSymbolsPerSector,
		"classes", len(cfg.ByAssetClass), "probe", cfg.Probe.Enabled)
}

// AutotuneConfig returns the active scheduled auto-optimizer settings.
func (e *Engine) AutotuneConfig() config.AutotuneConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.autotuneCfg
}

// SetAutotuneConfig replaces the scheduled auto-optimizer settings.
//
// Bars/MinTrades/RunOnlyForSymbols/ExcludeSymbols — and Enabled itself, to
// turn the scheduler off without a restart — take effect on the next
// scheduled pass: internal/autotune's Schedule re-reads this on every tick.
// Interval does not: like ScreenerConfig's own Interval, Schedule's ticker
// is built once at startup, so a changed interval (or enabling the job for
// the first time when it started disabled, and so never built a ticker at
// all) needs a restart — which the API reports rather than pretending
// otherwise.
func (e *Engine) SetAutotuneConfig(cfg config.AutotuneConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.autotuneCfg = cfg
}

// RescreenNow runs a screening pass immediately instead of waiting for the
// interval, so an edit can be checked at once.
func (e *Engine) RescreenNow(ctx context.Context) { e.rescreen(ctx) }

// NotifyConfig returns the active alert settings.
func (e *Engine) NotifyConfig() config.NotifyConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.notifyCfg
}

// SetNotifyConfig rebuilds the alert sinks. The previous notifier is closed so
// its delivery worker does not outlive it.
func (e *Engine) SetNotifyConfig(cfg config.NotifyConfig) {
	e.mu.Lock()

	e.notifyCfg = cfg

	old := e.notifier
	e.mu.Unlock()

	e.SetNotifier(notify.New(cfg))

	old.Close()

	e.log.Info("notifications updated", "enabled", cfg.Enabled, "sinks", e.NotifyStats().Sinks)
}
