// Package config loads bot configuration from a YAML file with environment
// variable overrides. Money limits are first-class citizens here: every value
// in Limits is enforced by the risk manager before any order reaches a broker.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
)

// Config is the full runtime configuration for the bot.
type Config struct {
	// Profile seeds a coherent set of strategy/exit/protection settings before
	// the rest of this file is applied ("conservative", "balanced",
	// "aggressive"). Anything set explicitly below overrides it. Empty = no
	// profile, just the built-in defaults.
	Profile string `yaml:"profile"`

	// Server is the gin HTTP API listen address, e.g. ":8080".
	Server ServerConfig `yaml:"server"`

	// Feed selects the market-data source: "" (auto — Finnhub when a key is
	// set, otherwise the offline mock) or "binance" for public crypto data.
	//
	// The feed is independent of the broker: binance data is read-only, so
	// crypto can be tracked and paper-traded against broker: sim, while real
	// orders continue to go only to the audited equity brokers.
	Feed string `yaml:"feed"`

	// BinanceBaseURL overrides the public API host (api.binance.com is
	// geo-blocked in some places; api.binance.us serves the same paths).
	BinanceBaseURL string `yaml:"binance_base_url"`

	// Finnhub holds market-data feed credentials/settings.
	Finnhub FinnhubConfig `yaml:"finnhub"`

	// Broker selects the execution venue: "sim", "alpaca" or "saxo". When
	// empty it is derived for backward compatibility (sim unless Alpaca
	// credentials are present).
	Broker string `yaml:"broker"`

	// Alpaca holds broker credentials/settings (paper by default).
	Alpaca AlpacaConfig `yaml:"alpaca"`

	// Saxo holds Saxo Bank OpenAPI credentials/settings (EU broker).
	Saxo SaxoConfig `yaml:"saxo"`

	// Trading212 holds Trading 212 Public API settings (EU, self-service keys).
	Trading212 Trading212Config `yaml:"trading212"`

	// Watchlist is the set of symbols the bot tracks and may trade.
	Watchlist []string `yaml:"watchlist"`

	// Limits are the hard money limits enforced on every order.
	Limits Limits `yaml:"limits"`

	// Strategy holds the shared technical-indicator parameters.
	Strategy StrategyConfig `yaml:"strategy"`

	// SymbolStrategy holds fully-resolved per-symbol parameter overrides, built
	// from the `strategy_overrides` block at load time. Each entry inherits every
	// field the override did not name, so writing one setting cannot silently
	// zero the rest. Read it through PerSymbolStrategy.
	SymbolStrategy map[string]StrategyConfig `yaml:"-"`

	// Engine controls evaluation cadence and trade enablement.
	Engine EngineConfig `yaml:"engine"`

	// Exits configures automatic protective exits (stop-loss / trailing / take-profit).
	Exits ExitsConfig `yaml:"exits"`

	// Storage configures persistence of trades and state.
	Storage StorageConfig `yaml:"storage"`

	// Safety holds automated-trading safety controls (rate limits / breaker).
	Safety SafetyConfig `yaml:"safety"`

	// Costs models trading commissions so order sizing, money limits and
	// backtests reflect the true cash cost of a trade.
	Costs CostsConfig `yaml:"costs"`

	// AI configures the optional LLM-backed "ai" detector (Ollama by default).
	AI AIConfig `yaml:"ai"`

	// ML configures the optional learned "ml" detector: a logistic-regression
	// classifier retrained on a rolling window.
	ML MLConfig `yaml:"ml"`

	// Protections are the outcome-driven circuit breakers (cooldowns, stop
	// guard, drawdown halt) applied on top of the per-order money limits.
	Protections ProtectionsConfig `yaml:"protections"`

	// Screener optionally maintains the watchlist automatically instead of
	// trading a fixed list.
	Screener ScreenerConfig `yaml:"screener"`

	// Autotune optionally runs the per-symbol optimizer on a schedule and
	// writes winning configs into strategy_overrides unsupervised. Off by
	// default.
	Autotune AutotuneConfig `yaml:"autotune"`

	// TestMode runs a default-on simulated "training phase" at startup: the
	// engine reads real market data and runs its normal loop, but every
	// order is routed to an internal simulated broker instead of the real
	// one, so simulated fills feed the real trade-stats machinery (and
	// Autotune's own gating) without the real broker ever being touched.
	// On by default.
	TestMode TestModeConfig `yaml:"test_mode"`

	// Notify configures outbound alerts (webhook / Telegram / Discord).
	Notify NotifyConfig `yaml:"notify"`

	// Portfolio caps concentration across positions — correlation and sector
	// exposure — which the per-order money limits cannot see.
	Portfolio PortfolioConfig `yaml:"portfolio"`

	// Sectors maps symbols to a sector label for Portfolio.MaxPerSector.
	Sectors map[string]string `yaml:"sectors"`

	// AssetClasses maps symbols to an asset class ("stock", "etf", "bond",
	// "crypto", ...) for Portfolio.MaxPctPerAssetClass. It is an OVERRIDE
	// layer, not the only source: the engine learns a class for every
	// tracked symbol from the data provider's instrument search (and from
	// the broker's own instrument metadata where it has any), caches it,
	// and only falls back to whatever is stated here. Write an entry when
	// you disagree with what was learned, or for a symbol nothing can
	// classify.
	AssetClasses map[string]string `yaml:"asset_classes"`
}

// PortfolioConfig caps concentration across the whole book.
//
// The money limits in Limits are per-order and per-symbol: they will happily let
// the bot hold twenty positions that are really one bet. Ten US megacap tech
// names at 300 each is not a 3000 diversified book, it is 3000 of Nasdaq with
// extra commission — and it draws down like it. These limits look at what is
// already held before allowing another entry.
type PortfolioConfig struct {
	// Enabled is the master switch. Off by default: the caps need either a
	// sector map or enough history to correlate, and silently doing nothing
	// would be worse than being explicit.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// MaxPerSector caps the combined market value of positions sharing a sector
	// label (see Config.Sectors). 0 = no cap.
	MaxPerSector float64 `json:"max_per_sector" yaml:"max_per_sector"`
	// MaxPositionsPerSector caps how many distinct symbols may be held in one
	// sector. 0 = no cap.
	MaxPositionsPerSector int `json:"max_positions_per_sector" yaml:"max_positions_per_sector"`

	// SectorCapsPerAssetClass counts the sector caps above WITHIN each asset
	// class instead of across the whole book, so a tech ETF and a tech stock
	// no longer compete for the same slot.
	//
	// Off by default, which is the stricter reading: a sector cap exists
	// because correlated positions draw down together, and a tech ETF falls
	// with tech stocks whatever wrapper it comes in.
	//
	// On is defensible for a different reason: a sector ETF holds dozens of
	// companies, so one unit of it is not one unit of single-name risk, and
	// MaxPctPerAssetClass already bounds how much of the book each wrapper may
	// take. Counting them separately says "two single-stock bets AND two
	// diversified bets per sector" rather than treating the two as
	// interchangeable.
	//
	// It applies to MaxPerSector and MaxPositionsPerSector together, and
	// internal/screener's own MaxSymbolsPerSector follows it — the screener
	// diversifying on a different rule than the guard enforces would just move
	// the disagreement rather than fix it.
	SectorCapsPerAssetClass bool `json:"sector_caps_per_asset_class" yaml:"sector_caps_per_asset_class"`

	// CorrelationThreshold is the return correlation above which two symbols
	// count as the same bet (0.8 is a reasonable start; 1.0 disables by being
	// unreachable). Correlation is computed on the same bars the strategy runs.
	CorrelationThreshold float64 `json:"correlation_threshold" yaml:"correlation_threshold"`
	// MaxCorrelatedPositions caps how many held symbols may exceed that
	// threshold against a candidate. 0 = no cap.
	MaxCorrelatedPositions int `json:"max_correlated_positions" yaml:"max_correlated_positions"`
	// CorrelationLookback is how many recent bars to correlate over. 0 defaults
	// to 200. Too short and the estimate is noise; too long and it describes a
	// regime that has passed.
	CorrelationLookback int `json:"correlation_lookback" yaml:"correlation_lookback"`

	// MaxPctPerAssetClass caps how much of the book may sit in one asset
	// class, as a FRACTION of account equity (0.30 = 30%), keyed by the
	// canonical class names in internal/market (stock, etf, fund, bond,
	// crypto, option, future, cfd, currency, index) plus the reserved
	// UnclassifiedAssetClass key. An absent or zero entry means "no cap on
	// that class".
	//
	// This is the allocation question the other limits cannot answer. The
	// per-order limits ask "is this order too big?", the sector cap asks
	// "is this sector too big?", and both will happily let a book that was
	// meant to be mostly trackers with a little single-stock risk drift
	// into the opposite. Once a class is at its cap, further BUYS in it
	// are refused — including topping up a position already held, because
	// a top-up spends the same budget — while every SELL and every exit
	// stays unaffected.
	MaxPctPerAssetClass map[string]float64 `json:"max_pct_per_asset_class" yaml:"max_pct_per_asset_class"`

	// MinPositionsPerAssetClass divides a class's budget into at least this
	// many positions: with max_pct_per_asset_class[stock] = 0.50 and 2 here,
	// no single stock position may be worth more than 25% of equity. It is
	// the per-class twin of engine.min_positions, which splits
	// max_total_invested the same way.
	//
	// It answers the failure the percentage cap cannot: a class budget is
	// spent first-come-first-served, so one early conviction buy can take
	// the whole 50% and the class is then shut for everything else — the cap
	// held, and the diversification it was written for did not happen. This
	// reserves the room instead of hoping the sizing leaves some.
	//
	// A ceiling, never a floor: it does not make the bot buy N positions,
	// and it is not a target. It clamps each buy (including a scale-in's
	// add) to what is left of that symbol's slot, so it behaves like
	// limits.max_order_value, and the smaller of the two wins.
	//
	// Keyed like MaxPctPerAssetClass, UnclassifiedAssetClass included. 0 or 1
	// means "no split" — one position may use the whole class budget, which
	// is the behaviour when the key is absent. A class named here needs a
	// MaxPctPerAssetClass entry, since without a budget there is nothing to
	// divide; once Enabled, Validate rejects that pairing rather than let the
	// setting sit there doing nothing. While the block is off both are inert
	// anyway, so staging the two together is allowed.
	MinPositionsPerAssetClass map[string]int `json:"min_positions_per_asset_class" yaml:"min_positions_per_asset_class"`
}

// UnclassifiedAssetClass is the reserved MaxPctPerAssetClass key for every
// holding nothing could name: no instrument type from the data provider, none
// from the broker, and no asset_classes: override. It is not a class any
// provider reports — it is the bucket they all fall out of.
//
// It exists because the alternative is worse than it looks. Without it the one
// position the bot understands least — an exotic wrapper, a delisted line, a
// ticker the provider has never heard of — is the only one no allocation limit
// can bind, and the only way to cap it is to notice it in the UI and hand-write
// an asset_classes: entry, which nobody does in time. Name this key and the
// unknown bucket is budgeted like any other class, including the symbols that
// have not been bought yet.
const UnclassifiedAssetClass = "unclassified"

// Validate rejects an out-of-range correlation threshold and any negative
// cap. PUT /api/settings/portfolio already checks CorrelationThreshold; this
// closes the gap for config.yaml, and adds the caps the API path didn't
// check either. See CODE_REVIEW_FINDINGS.md's "numerous sections have no
// range validation at load".
func (c PortfolioConfig) Validate() error {
	if c.CorrelationThreshold < 0 || c.CorrelationThreshold > 1 {
		return fmt.Errorf("portfolio.correlation_threshold must be in 0..1, got %v",
			c.CorrelationThreshold)
	}

	for name, v := range map[string]float64{
		"max_per_sector": c.MaxPerSector,
	} {
		if v < 0 {
			return fmt.Errorf("portfolio.%s must be >= 0, got %v", name, v)
		}
	}

	for name, v := range map[string]int{
		"max_positions_per_sector": c.MaxPositionsPerSector,
		"max_correlated_positions": c.MaxCorrelatedPositions,
		"correlation_lookback":     c.CorrelationLookback,
	} {
		if v < 0 {
			return fmt.Errorf("portfolio.%s must be >= 0, got %d", name, v)
		}
	}

	for class, pct := range c.MaxPctPerAssetClass {
		if strings.TrimSpace(class) == "" {
			return errors.New("portfolio.max_pct_per_asset_class has an empty class name")
		}

		// A fraction, like every other _pct in this file. Catching >1 here
		// is the point: "30" meaning 30% would otherwise be read as 3000%
		// of equity, i.e. a cap that can never bind, and the operator
		// would believe they were capped when they were not.
		if pct < 0 || pct > 1 {
			return fmt.Errorf(
				"portfolio.max_pct_per_asset_class[%s] must be a fraction in 0..1 "+
					"(0.30 = 30%%), got %v", class, pct)
		}
	}

	caps := c.NormalizedAssetClassCaps()

	for class, n := range c.MinPositionsPerAssetClass {
		name := strings.ToLower(strings.TrimSpace(class))
		if name == "" {
			return errors.New("portfolio.min_positions_per_asset_class has an empty class name")
		}

		if n < 0 {
			return fmt.Errorf(
				"portfolio.min_positions_per_asset_class[%s] must be >= 0, got %d", class, n)
		}

		// A class with slots but no budget of its own used to be refused here
		// as a no-op. It is no longer one: such a class shares whatever
		// fraction the named budgets left unclaimed (see
		// risk.PortfolioGuard.MaxPositionValue), which is now the supported
		// way to say "split the rest of the book evenly".
	}

	// The budgets are fractions of ONE book, so together they cannot claim
	// more than all of it. Over 100% the caps stop describing an allocation:
	// every class's slot is computed from its own share, so oversubscribed
	// budgets hand out slots whose total exceeds the money available and the
	// cash limits then refuse whichever order happens to arrive last —
	// enforcement by arrival order rather than by the allocation written down.
	//
	// Only once the block is ENABLED. config.example.yaml stages every
	// setting at its off value, and a cross-field rule that fires while the
	// block is off would break that fixture.
	if c.Enabled {
		var total float64
		for _, pct := range caps {
			total += pct
		}

		// Tolerance for the usual decimal representation error: 0.7 + 0.3
		// is not exactly 1 in binary floating point, and refusing the most
		// natural way to write a full allocation would be absurd.
		if total > 1+1e-9 {
			return fmt.Errorf(
				"portfolio.max_pct_per_asset_class sums to %.4f, more than the whole book "+
					"(1.0): the class budgets must not claim more than 100%% between them",
				total)
		}
	}

	return nil
}

// TightestPerOrderCap is the smallest ceiling these limits place on a single
// order's notional, or 0 when neither is set.
//
// It exists for the optimisers, which need a per-trade deployment figure to
// centre their capital-fraction search on. That used to be engine.order_size_usd;
// with sizing starting from the money-limit headroom instead, the honest
// equivalent is the tightest ceiling an order can actually run into. Only the
// two true per-ORDER caps count: max_total_invested and cash_reserve bound the
// book rather than any one order, and the position slot depends on equity and
// asset class, neither of which a search space should bake in.
//
// equity resolves any percentage half of those limits (see Resolve); pass the
// cash the search is run against. Without it a percentage-only configuration
// would report no cap at all and the search would silently fall back to its
// default fraction.
func (l Limits) TightestPerOrderCap(equity float64) float64 {
	r := l.Resolve(equity)

	capUSD := r.MaxOrderValue
	if r.MaxPerPosition > 0 && (capUSD <= 0 || r.MaxPerPosition < capUSD) {
		capUSD = r.MaxPerPosition
	}

	return max(capUSD, 0)
}

// NormalizedAssetClassCaps returns MaxPctPerAssetClass keyed by lower-cased,
// trimmed class names with the no-op entries (zero or negative) dropped, so the
// guard can look a class up without re-normalising on every order.
//
// Config files are written by hand: "ETF", " etf" and "etf" must all cap the
// same bucket the classifier fills.
func (c PortfolioConfig) NormalizedAssetClassCaps() map[string]float64 {
	if len(c.MaxPctPerAssetClass) == 0 {
		return nil
	}

	out := make(map[string]float64, len(c.MaxPctPerAssetClass))

	for class, pct := range c.MaxPctPerAssetClass {
		if name := strings.ToLower(strings.TrimSpace(class)); name != "" && pct > 0 {
			out[name] = pct
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// NormalizedAssetClassSlots returns MinPositionsPerAssetClass keyed the same
// way NormalizedAssetClassCaps is, with the no-op entries dropped: 0 (absent)
// and 1 (the whole budget in one position) both mean "do not split", and a
// class whose slot count cannot bind is better absent than present.
func (c PortfolioConfig) NormalizedAssetClassSlots() map[string]int {
	if len(c.MinPositionsPerAssetClass) == 0 {
		return nil
	}

	out := make(map[string]int, len(c.MinPositionsPerAssetClass))

	for class, n := range c.MinPositionsPerAssetClass {
		if name := strings.ToLower(strings.TrimSpace(class)); name != "" && n > 1 {
			out[name] = n
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// MLConfig configures the learned "ml" detector in internal/ml: a logistic
// regression over the same normalised indicator readings the other detectors
// vote on, retrained on a rolling window and predicting the probability that the
// next Horizon bars are up.
//
// It is off by default and, like every detector, does nothing unless "ml" appears
// in strategy.detectors. Two things are worth understanding before enabling it:
//
//   - Every prediction is genuinely out-of-sample. The model trains on rows whose
//     forward window has already closed and predicts the current bar, so a
//     backtest of it is a continuous walk-forward rather than a fit to the data
//     it is scored on.
//   - It will usually find very little. Bar-to-bar direction on liquid
//     instruments is close to unpredictable from indicators alone; MinSkill
//     exists to make a model that learned nothing say so instead of emitting
//     confident probabilities from noise.
type MLConfig struct {
	// Enabled builds the predictor. Off ⇒ the "ml" detector always votes HOLD.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Horizon is how many bars ahead the label looks. 0 defaults to 10.
	Horizon int `json:"horizon" yaml:"horizon"`
	// Threshold is the forward return that counts as "up" (0.005 = 0.5%). A
	// threshold of 0 asks "did price tick up", which after costs is not a
	// tradeable question and yields a near-coin-flip target. 0 is allowed but
	// rarely what you want.
	Threshold float64 `json:"threshold" yaml:"threshold"`

	// TrainBars is the rolling training window in bars. 0 defaults to 1500.
	TrainBars int `json:"train_bars" yaml:"train_bars"`
	// RetrainEvery is how many new bars trigger a refit. 0 defaults to 250.
	// Measured in bars, not time, so a backtest and a live run retrain on the
	// same schedule.
	RetrainEvery int `json:"retrain_every" yaml:"retrain_every"`
	// MinSamples is the fewest labelled rows required to fit at all. 0 defaults
	// to 200.
	MinSamples int `json:"min_samples" yaml:"min_samples"`

	// BuyProb / SellProb are the probabilities at which the model votes. 0
	// defaults to 0.58 / 0.42 — deliberately not 0.5, since a model hovering at
	// the coin-flip line should not be trading.
	BuyProb  float64 `json:"buy_prob"  yaml:"buy_prob"`
	SellProb float64 `json:"sell_prob" yaml:"sell_prob"`

	// MinSkill is the minimum training accuracy *above the majority-class base
	// rate* before the model is allowed to vote (0.02 = two percentage points).
	// This is the guard against acting on a model that learned nothing. 0
	// disables the check, which is not recommended.
	MinSkill float64 `json:"min_skill" yaml:"min_skill"`
	// MaxStrength caps the strength the detector contributes (0..1).
	MaxStrength float64 `json:"max_strength" yaml:"max_strength"`

	// Fitting hyperparameters. The defaults converge on this problem; L2 is the
	// one worth touching, since it is the main brake on overfitting the window.
	Epochs       int     `json:"epochs"        yaml:"epochs"`
	LearningRate float64 `json:"learning_rate" yaml:"learning_rate"`
	L2           float64 `json:"l2"            yaml:"l2"`
}

// Validate rejects an out-of-range probability and a crossed buy/sell pair.
// Mirrors PUT /api/settings/ml's existing check exactly (see api/settings.go
// applyML) so the two paths can no longer disagree — a config.yaml with
// swapped buy_prob/sell_prob used to load clean and silently invert the
// detector's BUY/SELL logic, since only the API path checked this. See
// CODE_REVIEW_FINDINGS.md's "numerous sections have no range validation at
// load".
func (c MLConfig) Validate() error {
	if c.BuyProb < 0 || c.BuyProb > 1 || c.SellProb < 0 || c.SellProb > 1 {
		return errors.New("ml.buy_prob and ml.sell_prob must be probabilities in 0..1")
	}

	if c.Enabled && c.SellProb >= c.BuyProb {
		return fmt.Errorf("ml.sell_prob (%.2f) must be below ml.buy_prob (%.2f)",
			c.SellProb, c.BuyProb)
	}

	return nil
}

// ProtectionsConfig configures the trading protections in internal/protect.
// Money limits cap what is at stake per order; these watch outcomes over time
// and stop the bot when the pattern is bad. Every zero value disables that
// protection.
type ProtectionsConfig struct {
	// Enabled is the master switch. With it off nothing here applies.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// CooldownAfterLoss blocks new entries in a symbol for this long after a
	// losing exit, so the bot doesn't immediately re-buy what it just lost on.
	CooldownAfterLoss time.Duration `json:"-" yaml:"cooldown_after_loss"`
	// StopGuardTrades is how many protective stop-outs of one symbol within
	// StopGuardLookback lock that symbol for StopGuardLock. Repeated stop-outs
	// mean the symbol is not behaving the way the strategy assumes.
	StopGuardTrades   int           `json:"stop_guard_trades" yaml:"stop_guard_trades"`
	StopGuardLookback time.Duration `json:"-"                 yaml:"stop_guard_lookback"`
	StopGuardLock     time.Duration `json:"-"                 yaml:"stop_guard_lock"`
	// MaxDrawdownPct halts all automated trading when account equity falls this
	// far below its peak (0.15 = 15%). Resuming is a deliberate API call.
	MaxDrawdownPct float64 `json:"max_drawdown_pct" yaml:"max_drawdown_pct"`
	// MaxDailyLossPct halts automated trading when equity falls this far below
	// where it started the (UTC) day.
	MaxDailyLossPct float64 `json:"max_daily_loss_pct" yaml:"max_daily_loss_pct"`
	// MaxRealizedDrawdownPct halts automated trading when cumulative REALIZED
	// P&L (closed trades only) has given back this much of the account's peak
	// equity since its own high-water mark — unlike MaxDrawdownPct, this
	// ignores unrealized mark-to-market swings in open positions entirely, so
	// a heavy-exposure period with paper losses on positions still held does
	// not by itself trip it; only money actually locked in by closed trades
	// counts. Independent of MaxDrawdownPct — set either, both, or neither.
	// 0 disables it.
	MaxRealizedDrawdownPct float64 `json:"max_realized_drawdown_pct" yaml:"max_realized_drawdown_pct"`

	// LowProfitTrades is how many closed trades in one symbol within
	// LowProfitLookback are examined for the low-profit guard. It locks a symbol
	// whose realized P&L over that window is at or below LowProfitMinPnL.
	//
	// This catches a different failure from the stop guard. The stop guard counts
	// protective stop-outs — a symbol behaving unlike the backtest. This counts
	// money: a symbol the strategy keeps trading to a small net loss never trips
	// a stop guard and never trips a drawdown halt, it just bleeds commission and
	// spread. 0 disables it.
	LowProfitTrades   int           `json:"low_profit_trades" yaml:"low_profit_trades"`
	LowProfitLookback time.Duration `json:"-"                 yaml:"low_profit_lookback"`
	LowProfitLock     time.Duration `json:"-"                 yaml:"low_profit_lock"`
	// LowProfitMinPnL is the realized profit the window must clear, in account
	// currency. 0 means "must at least break even"; a small positive value
	// demands the symbol actually pay for itself.
	LowProfitMinPnL float64 `json:"low_profit_min_pnl" yaml:"low_profit_min_pnl"`

	// DropAfterLosses removes a symbol from the watchlist entirely after this
	// many CONSECUTIVE losing closes, and comments its entry out of this
	// file's watchlist: block so the removal survives a restart. 0 = never.
	//
	// The other guards here lock a symbol out for a while and then let it back
	// in. This is the one that gives up: some instruments simply do not suit
	// the strategy, and a lockout that expires keeps handing capital back to
	// them every time it does. A winning or breakeven close resets the count,
	// so this only fires on a symbol that is losing and not recovering.
	//
	// Set it well above autotune.on_loss.after_losses if you use both — the
	// intent is "re-tune a few times, and only give up if that did not help".
	// Two rules make it safe to leave on: a symbol with an OPEN POSITION is
	// never dropped (that would strand the position with no exits), and the
	// config entry is commented rather than deleted, so the symbol and the
	// reason stay in the file for you to reverse.
	//
	// Enforced by the engine rather than by protect.Manager: dropping a symbol
	// means editing the watchlist and the config file, neither of which that
	// package can reach.
	DropAfterLosses int `json:"drop_after_losses" yaml:"drop_after_losses"`
}

// ScreenerConfig configures the automatic universe selection in
// internal/screener. A fixed watchlist trades whatever you picked months ago,
// including instruments that have since gone illiquid or stopped moving; the
// screener re-derives the tradeable set from recent history on a schedule.
type ScreenerConfig struct {
	// Enabled turns automatic watchlist maintenance on. Off by default: the
	// watchlist stays exactly what config and the API say it is.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Universe is the candidate pool to screen. Empty means "screen the current
	// watchlist" (filtering only, never adding).
	Universe []string `json:"universe" yaml:"universe"`
	// Interval is how often the universe is re-screened. 0 defaults to 6h.
	Interval time.Duration `json:"-" yaml:"interval"`
	// MaxSymbols caps how many survivors are tracked (the best by Rank). 0 = all.
	MaxSymbols int `json:"max_symbols" yaml:"max_symbols"`

	// MaxSymbolsPerSector caps how many SELECTED symbols may share a sector
	// label (see Config.Sectors). 0 = no cap.
	//
	// This is the screener's half of a limit the portfolio guard already
	// enforces at order time. Without it the two disagree: the screener ranks
	// on trend or volume, which correlate hard within a sector, so it happily
	// fills every slot with one industry — and then
	// Portfolio.MaxPositionsPerSector refuses most of those buys. The
	// difference costs a feed subscription, a warmup history and a stream of
	// blocked trades for symbols that were never buyable.
	//
	// Set it to Portfolio.MaxPositionsPerSector (or a little above, so a
	// closing position has a replacement ready) and the watchlist stops
	// filling with names the guard will refuse.
	//
	// Counted across ALL asset classes, matching the guard, which does not
	// care which class a position is in either. Held and pinned symbols are
	// exempt, exactly as they are from MaxSymbols.
	MaxSymbolsPerSector int `json:"max_symbols_per_sector" yaml:"max_symbols_per_sector"`
	// Rank orders the survivors: "volume" (most traded), "volatility" (widest
	// ATR%), or "momentum" (strongest recent return). Default "volume".
	Rank string `json:"rank" yaml:"rank"`
	// MinPrice/MaxPrice exclude penny stocks and instruments too expensive to
	// size sensibly. 0 = no bound.
	MinPrice float64 `json:"min_price" yaml:"min_price"`
	MaxPrice float64 `json:"max_price" yaml:"max_price"`
	// MinAvgVolume drops thin instruments, where the spread eats the edge.
	MinAvgVolume float64 `json:"min_avg_volume" yaml:"min_avg_volume"`
	// MinVolatilityPct/MaxVolatilityPct bound recent ATR as a percentage of
	// price: too quiet is untradeable, too wild is unsurvivable. 0 = no bound.
	MinVolatilityPct float64 `json:"min_volatility_pct" yaml:"min_volatility_pct"`
	MaxVolatilityPct float64 `json:"max_volatility_pct" yaml:"max_volatility_pct"`
	// MinHistoryBars requires this much history before a symbol is eligible, so
	// the strategy is never asked to trade something it cannot warm up on.
	MinHistoryBars int `json:"min_history_bars" yaml:"min_history_bars"`
	// MaxSpreadPct drops instruments whose estimated bid-ask spread exceeds this
	// fraction of price (0.005 = 50 bps). The spread is paid twice per round
	// trip and is invisible in a candle chart, so a symbol can look perfectly
	// tradeable and still be unable to clear its own cost of entry. 0 = no bound.
	MaxSpreadPct float64 `json:"max_spread_pct" yaml:"max_spread_pct"`
	// KeepHeld controls only whether a symbol with an open position that no
	// longer passes the filters shows up in Screener.Result as "kept:
	// position open" (true) or "would have failed" (false) — a cosmetic
	// difference in the screener's own diagnostic output. It does NOT
	// control whether the live engine can actually drop a held symbol from
	// the watchlist: engine.rescreen protects every held symbol from being
	// dropped unconditionally, independent of this flag, on purpose —
	// dropping one would strand the position with no exits, no trailing
	// stop and no way to be sold, and that guarantee should not depend on
	// a screener config value. Setting this to false does not make that
	// happen; it only changes the wording of the (otherwise inert)
	// verdict. Default true, matching what actually happens either way in
	// the live engine. See CODE_REVIEW_FINDINGS.md's keep_held-inert
	// finding.
	KeepHeld bool `json:"keep_held" yaml:"keep_held"`

	// KeepManual makes the screener purely additive: symbols you chose by hand,
	// in the config watchlist or through the API, are never dropped and never
	// count against MaxSymbols. The screener's own discoveries stay temporary —
	// they come and go as they qualify — so MaxSymbols becomes "how many
	// borrowed names to run alongside mine" rather than "how many names in
	// total".
	//
	// Off by default, which keeps the original behaviour: the screener owns the
	// whole watchlist and a name that stops ranking is dropped no matter who
	// put it there.
	KeepManual bool `json:"keep_manual" yaml:"keep_manual"`

	// Filters names the checks to run, in order, e.g.
	// [history, price, volume, volatility, spread]. Evaluation stops at the
	// first failure, so a verdict's reason is the binding constraint rather than
	// a list of everything wrong with the candidate. Empty = every filter, in
	// the default order.
	//
	// Naming them buys two things a pile of magic zeroes does not: a filter can
	// be removed outright rather than disabled by finding the value that means
	// "no bound", and a costly check can be ordered behind a cheap one that
	// rejects most candidates first.
	Filters []string `json:"filters" yaml:"filters"`

	// Resolution is the bar size candidates are judged on ("1", "5", "15",
	// "60", "D"). Empty defaults to "1".
	//
	// This is the single most class-dependent setting there is. A broad
	// accumulating ETF judged on one-minute bars is indistinguishable from
	// noise — it has no intraday character to measure, and its ATR%, spread
	// estimate and momentum all describe the tick, not the instrument. The
	// same bars are exactly right for a liquid megacap. Set the base to
	// whatever most of your universe is and override the rest per class.
	Resolution string `json:"resolution" yaml:"resolution"`

	// LookbackDays is how much history to pull per candidate. 0 defaults to
	// 30. It must be long enough that Resolution yields more than
	// MinHistoryBars — daily bars over 30 days is 20-odd bars, which is not
	// enough to measure anything, so a daily profile wants a year.
	LookbackDays int `json:"lookback_days" yaml:"lookback_days"`

	// MinTrendR2 requires a candidate's price to fit a straight line at least
	// this well (0..1) over the window — the "trend" filter. R² separates a
	// clean advance from a sideways chop that happens to end higher than it
	// started, which momentum alone cannot. 0 = no bound.
	MinTrendR2 float64 `json:"min_trend_r2" yaml:"min_trend_r2"`
	// MinTrendSlopePct requires that regression line to rise at least this
	// fraction of price per bar. Pairs with MinTrendR2: R² says the trend is
	// clean, slope says it is worth having. 0 = no bound.
	MinTrendSlopePct float64 `json:"min_trend_slope_pct" yaml:"min_trend_slope_pct"`

	// MaxDrawdownPct drops a candidate whose own peak-to-trough decline over
	// the window exceeds this fraction — the "drawdown" filter. A falling
	// knife passes a volatility band and can pass momentum on a dead-cat
	// bounce; this is what refuses it. 0 = no bound.
	MaxDrawdownPct float64 `json:"max_drawdown_pct" yaml:"max_drawdown_pct"`

	// MinCostMultiple requires a candidate's typical bar range to be at least
	// this many times its own round-trip cost (two spreads plus commission) —
	// the "cost" filter. It is the question that decides whether an
	// instrument can pay for being traded at all, and the one a small account
	// gets wrong most expensively: an instrument that moves 0.1% a bar
	// against a 0.15% round trip cannot be profitable at any win rate. 3 is a
	// sensible floor; 0 = no bound.
	MinCostMultiple float64 `json:"min_cost_multiple" yaml:"min_cost_multiple"`

	// UniverseByAssetClass is the candidate pool per asset class, for the
	// common case where the pools have nothing to do with each other. A class
	// with no entry here falls back to that class's profile Universe, then to
	// the top-level Universe.
	UniverseByAssetClass map[string][]string `json:"universe_by_asset_class" yaml:"universe_by_asset_class,omitempty"`

	// SuggestUniverse seeds any asset class that has no configured universe
	// with a built-in starter pool of liquid, widely-available instruments of
	// that class (see screener.SuggestedUniverse). Overridable per class in
	// ByAssetClass, so one class can be seeded while another is left to the
	// pool you curated.
	//
	// It exists because "screen a universe" is useless advice if you do not
	// already know what belongs in one, and because an empty universe makes
	// the screener filter-only: it can drop symbols but never find any. The
	// starter pools are a place to begin, not a recommendation — they are
	// deliberately boring, and anything they surface still has to survive
	// every filter and probe like any other candidate.
	SuggestUniverse bool `json:"suggest_universe" yaml:"suggest_universe"`

	// ByAssetClass overrides any of the fields above per asset class ("etf",
	// "stock", "crypto", ...). Each profile INHERITS every field it does not
	// name, exactly like strategy_overrides, so setting one value cannot
	// silently zero the rest.
	ByAssetClass map[string]ScreenerProfile `json:"by_asset_class" yaml:"by_asset_class,omitempty"`

	// Probe optionally backtests the survivors before they are adopted — the
	// difference between "this instrument is tradeable" and "this instrument
	// is worth trading". See ScreenerProbeConfig.
	Probe ScreenerProbeConfig `json:"probe" yaml:"probe"`
}

// ScreenerProfile overrides ScreenerConfig per asset class. Every field is
// optional: nil (or an empty slice) inherits the base value, so a profile that
// names one setting cannot silently reset the others.
//
// The point is that "good" means different things per class. A megacap is
// judged on intraday bars, liquidity and momentum; a broad ETF on daily bars
// over a year, trend quality and whether it clears its own spread — measuring
// either one the other's way produces confident nonsense.
type ScreenerProfile struct {
	// Universe is this class's candidate pool. Empty inherits
	// UniverseByAssetClass[class], then the top-level Universe.
	Universe []string `json:"universe,omitempty" yaml:"universe,omitempty,flow"`

	Resolution          *string `json:"resolution,omitempty" yaml:"resolution,omitempty"`
	LookbackDays        *int    `json:"lookback_days,omitempty" yaml:"lookback_days,omitempty"`
	MaxSymbols          *int    `json:"max_symbols,omitempty" yaml:"max_symbols,omitempty"`
	MaxSymbolsPerSector *int    `json:"max_symbols_per_sector,omitempty" yaml:"max_symbols_per_sector,omitempty"`
	Rank                *string `json:"rank,omitempty" yaml:"rank,omitempty"`

	MinPrice         *float64 `json:"min_price,omitempty" yaml:"min_price,omitempty"`
	MaxPrice         *float64 `json:"max_price,omitempty" yaml:"max_price,omitempty"`
	MinAvgVolume     *float64 `json:"min_avg_volume,omitempty" yaml:"min_avg_volume,omitempty"`
	MinVolatilityPct *float64 `json:"min_volatility_pct,omitempty" yaml:"min_volatility_pct,omitempty"`
	MaxVolatilityPct *float64 `json:"max_volatility_pct,omitempty" yaml:"max_volatility_pct,omitempty"`
	MinHistoryBars   *int     `json:"min_history_bars,omitempty" yaml:"min_history_bars,omitempty"`
	MaxSpreadPct     *float64 `json:"max_spread_pct,omitempty" yaml:"max_spread_pct,omitempty"`
	MinTrendR2       *float64 `json:"min_trend_r2,omitempty" yaml:"min_trend_r2,omitempty"`
	MinTrendSlopePct *float64 `json:"min_trend_slope_pct,omitempty" yaml:"min_trend_slope_pct,omitempty"`
	MaxDrawdownPct   *float64 `json:"max_drawdown_pct,omitempty" yaml:"max_drawdown_pct,omitempty"`
	MinCostMultiple  *float64 `json:"min_cost_multiple,omitempty" yaml:"min_cost_multiple,omitempty"`

	// SuggestUniverse seeds THIS class from its built-in starter pool when it
	// has no universe of its own — the per-class form of
	// ScreenerConfig.SuggestUniverse, which it overrides.
	//
	// Per class rather than only global because the two halves of a book are
	// rarely at the same stage: you may know exactly which five stocks you
	// want to screen and have no idea which trackers are worth looking at.
	// A global switch forces the same answer on both.
	//
	// Seeding only applies to a class with no pool of its own. A curated list
	// is a decision, and quietly adding a dozen suggestions to it would
	// dilute that decision rather than help.
	SuggestUniverse *bool `json:"suggest_universe,omitempty" yaml:"suggest_universe,omitempty"`

	// Filters replaces the chain for this class. Empty inherits the base.
	Filters []string `json:"filters,omitempty" yaml:"filters,omitempty,flow"`
}

// ScreenerProbeConfig backtests shortlisted candidates before they are adopted.
//
// Every indicator filter answers "is this instrument tradeable?" — enough
// history, enough liquidity, moves enough to clear its costs. None of them
// answers the question you actually care about: would MY strategy have made
// money on it? A probe answers that by running the real backtester over the
// candidate's own bars.
//
// It is deliberately the last stage and deliberately bounded. A search per
// candidate over a large universe is minutes of CPU, so only the top
// MaxCandidates per class (already ranked by the cheap filters) are ever
// probed.
type ScreenerProbeConfig struct {
	// Enabled turns the probe stage on. Off by default: it costs real time
	// on every screening pass.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Backtest runs the LIVE strategy over each shortlisted candidate and
	// scores it on the result. This asks "would what I am running now have
	// worked here?" — the honest question for adopting a symbol into a book
	// that trades one configuration.
	Backtest bool `json:"backtest" yaml:"backtest"`

	// Autotune additionally runs the bounded per-symbol search to find the
	// BEST configuration for the candidate, and scores it on that.
	//
	// This asks a different question: "could this symbol work if I tuned for
	// it?" A symbol your current settings happen to suit badly still scores
	// well here, which is exactly what makes it worth knowing — and exactly
	// why its score must not be read as what you would actually earn. Pair it
	// with autotune.on_loss or a per-symbol override, or the achievable
	// number stays hypothetical. Much slower than Backtest.
	Autotune bool `json:"autotune" yaml:"autotune"`

	// MaxCandidates caps how many survivors per asset class are probed, taken
	// best-first from the cheap ranking. 0 defaults to 10.
	MaxCandidates int `json:"max_candidates" yaml:"max_candidates"`

	// Bars is the candle window per probe. 0 inherits the profile's own
	// screening window.
	Bars int `json:"bars" yaml:"bars"`

	// MinExpectancyPct rejects a candidate whose probe expectancy per trade
	// falls below this fraction. 0 rejects anything that does not at least
	// break even; negative tolerates a small loss; set it below -1 to score
	// without ever rejecting.
	MinExpectancyPct float64 `json:"min_expectancy_pct" yaml:"min_expectancy_pct"`

	// MinTrades ignores a probe result built on too few trades to mean
	// anything — the candidate keeps its cheap-filter ranking instead of
	// being judged on three lucky fills. 0 defaults to 5.
	MinTrades int `json:"min_trades" yaml:"min_trades"`
}

// Validate rejects a negative bound and a crossed min/max pair. Neither the
// YAML load path nor the API's PUT /api/settings/screener checked any of
// this before. See CODE_REVIEW_FINDINGS.md's "numerous sections have no
// range validation at load".
func (c ScreenerConfig) Validate() error {
	if err := c.validateBounds(); err != nil {
		return err
	}

	return c.validateProfiles()
}

// validateBounds checks one resolved set of screening bounds. Split out from
// Validate so each per-class profile can be checked the same way after
// inheritance — see validateProfiles.
func (c ScreenerConfig) validateBounds() error {
	for name, v := range map[string]float64{
		"min_price":          c.MinPrice,
		"max_price":          c.MaxPrice,
		"min_avg_volume":     c.MinAvgVolume,
		"min_volatility_pct": c.MinVolatilityPct,
		"max_volatility_pct": c.MaxVolatilityPct,
		"max_spread_pct":     c.MaxSpreadPct,
	} {
		if v < 0 {
			return fmt.Errorf("screener.%s must be >= 0, got %v", name, v)
		}
	}

	for name, v := range map[string]int{
		"max_symbols":            c.MaxSymbols,
		"max_symbols_per_sector": c.MaxSymbolsPerSector,
		"min_history_bars":       c.MinHistoryBars,
	} {
		if v < 0 {
			return fmt.Errorf("screener.%s must be >= 0, got %d", name, v)
		}
	}

	if c.MinPrice > 0 && c.MaxPrice > 0 && c.MinPrice > c.MaxPrice {
		return fmt.Errorf("screener.min_price (%v) must be <= max_price (%v)",
			c.MinPrice, c.MaxPrice)
	}

	if c.MinVolatilityPct > 0 && c.MaxVolatilityPct > 0 &&
		c.MinVolatilityPct > c.MaxVolatilityPct {
		return fmt.Errorf("screener.min_volatility_pct (%v) must be <= max_volatility_pct (%v)",
			c.MinVolatilityPct, c.MaxVolatilityPct)
	}

	for name, v := range map[string]float64{
		"min_trend_slope_pct": c.MinTrendSlopePct,
		"max_drawdown_pct":    c.MaxDrawdownPct,
		"min_cost_multiple":   c.MinCostMultiple,
	} {
		if v < 0 {
			return fmt.Errorf("screener.%s must be >= 0, got %v", name, v)
		}
	}

	// R² is a fraction of variance explained; anything outside 0..1 is a unit
	// mistake (50 meaning 50%) that would silently reject every candidate.
	if c.MinTrendR2 < 0 || c.MinTrendR2 > 1 {
		return fmt.Errorf("screener.min_trend_r2 must be in 0..1, got %v", c.MinTrendR2)
	}

	if c.LookbackDays < 0 {
		return fmt.Errorf("screener.lookback_days must be >= 0, got %d", c.LookbackDays)
	}

	return nil
}

// AutotuneConfig configures the scheduled per-symbol auto-optimizer: an
// optional background job that periodically runs the same bounded search
// the Strategy tab's manual "optimize this symbol" button does, against
// every eligible watchlist symbol's own cached history, and — unlike that
// button, which only ever proposes — writes a winning config straight into
// strategy_overrides (via config.PatchSymbolOverride) and the live engine,
// fully unsupervised. There is no confirmation step; MinTrades and the
// underlying search's own dual-baseline gate (a proposal must beat both the
// symbol's real, capital-constrained track record AND an isolated
// full-capital backtest — see internal/autotune) are the only safeguards
// against curve-fitting a live config to noise. Off by default.
type AutotuneConfig struct {
	// Enabled turns the scheduled job on. Off by default: nothing runs, and
	// nothing is ever written to strategy_overrides automatically.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Interval is how often a pass runs. Restart-only to change — like
	// Screener.Interval, the ticker is built once when the background
	// goroutine starts; there is no live-reconfigurable ticker anywhere in
	// this codebase. 0 defaults to 24h.
	Interval time.Duration `json:"-" yaml:"interval"`
	// Bars is the candle window analyzed per symbol per run, tail-sliced
	// from whatever is cached (SeriesResampled) — not a calendar-day
	// conversion (bar size depends on engine.bar_interval, and this
	// codebase has no exchange-calendar-aware helper to convert one into
	// the other). At the default 1-minute bars, ~390 covers one trading
	// day, ~1440 one calendar day. A symbol with fewer cached bars than the
	// manual button's own floor (see internal/autotune's
	// symbolOptimizeMinBars) is skipped for that run, same as the button.
	// 0 defaults to 1440.
	Bars int `json:"bars" yaml:"bars"`
	// MinTrades rejects a candidate config that only "worked" because it
	// barely traded. Deliberately independent of (and, by default,
	// stricter than) the manual button's own floor: a thin sample is a
	// smaller risk on a human-reviewed click than on an unsupervised,
	// every-symbol, real-money scheduled write. 0 defaults to 10.
	MinTrades int `json:"min_trades" yaml:"min_trades"`
	// Objective selects which backtest.Scorer ranks candidate configs
	// during a search — see backtest.ScorerNames for the full list:
	// "blend" (the default) rewards return and win rate while penalising
	// drawdown, i.e. "both"; "return" and "winrate" rank on exactly one of
	// those alone; "sharpe"/"sortino"/"calmar" rank risk-adjusted; and
	// "profitfactor"/"expectancy" rank trade quality independent of
	// position size. Not validated here (like Rank above) — internal/api's
	// settings form only ever offers backtest.ScorerNames as a dropdown, so
	// a typo cannot reach this field that way, and an unrecognized value
	// from a hand-edited file falls back to "blend" (see
	// backtest.ScorerByName). This only decides which CANDIDATE wins the
	// search — the separate gate against a symbol's real track record
	// always compares win rate and expectancy specifically, whatever
	// Objective is, since those are the only two metrics genuinely
	// comparable between an isolated backtest and a capital-constrained
	// real history (see engine.SymbolStats' own doc comment) — a proposal
	// must still hold up on both of those against reality regardless.
	// Empty defaults to "blend".
	Objective string `json:"objective" yaml:"objective"`
	// MinReturnPct is an absolute floor on the winning candidate's OWN
	// backtested total return: a proposal scoring higher than a weak or
	// empty baseline is not the same thing as a proposal that actually
	// makes money, and a symbol with no what-if trades and no real trades
	// in the window (baseline score 0, nothing to beat) would otherwise
	// let a literally lossmaking config through as "improved" just for
	// beating nothing. Independent of, and applied in addition to,
	// Objective and the win-rate/expectancy-vs-real gate — whatever those
	// decide is "best" must still clear this floor before it can be
	// Improved. 0 (the default) rejects any negative return; raise it for
	// a stricter bar, or lower it (negative) to tolerate a small loss
	// deliberately.
	MinReturnPct float64 `json:"min_return_pct" yaml:"min_return_pct"`
	// RunOnlyForSymbols, when non-empty, restricts every pass to exactly
	// these watchlist symbols — an allow-list. Empty means every watchlist
	// symbol is eligible (subject to ExcludeSymbols below).
	// The ",flow" yaml tag renders this as a single-line `[A, B]` list (as
	// every hand-written symbol list in this codebase already does) rather
	// than goccy's default one-item-per-line block style — without it,
	// config.Patch's single-line leaf-splice machinery corrupts the file
	// the first time this field is written through the API (see
	// SymbolOverrideFields.Detectors's identical tag, and
	// internal/config/patch.go's doc comment on why this matters).
	RunOnlyForSymbols []string `json:"run_only_for_symbols" yaml:"run_only_for_symbols,flow"`
	// ExcludeSymbols removes specific symbols from consideration regardless
	// of RunOnlyForSymbols — e.g. keep a hand-tuned override's own reasoning
	// out of the scheduler's reach entirely without maintaining an
	// allow-list of everything else. Applied after RunOnlyForSymbols, so a
	// symbol named in both lists is excluded.
	ExcludeSymbols []string `json:"exclude_symbols" yaml:"exclude_symbols,flow"`

	// OnLoss additionally re-tunes a SINGLE symbol as soon as it has lost
	// enough times in a row, instead of waiting for the next scheduled
	// pass. See AutotuneOnLossConfig.
	OnLoss AutotuneOnLossConfig `json:"on_loss" yaml:"on_loss"`
}

// AutotuneOnLossConfig re-tunes one symbol reactively, the moment its recent
// record says its current config has stopped working.
//
// The scheduled pass runs on a clock and evaluates the whole watchlist, so a
// symbol whose config broke an hour after the last pass keeps trading that
// broken config until the next one — up to Interval (24h by default) of losing
// trades on a config the bot could already tell was failing. This closes that
// gap: it watches realized closes as they happen and, when one symbol strings
// together AfterLosses losses, runs exactly the same bounded search for that
// one symbol immediately.
//
// It is strictly additive to the scheduled pass and shares every one of its
// safeguards — the same MinTrades floor, the same Objective, the same
// MinReturnPct floor, and the same dual-baseline gate — so a reactive run can
// only ever apply a config that would also have been allowed to win on the
// clock. A losing streak is what makes the bot LOOK, never what makes it
// change anything: if the search finds nothing that clears those gates, the
// symbol keeps the config it has and simply keeps counting.
//
// RunOnlyForSymbols/ExcludeSymbols apply here too — a symbol the scheduler is
// not allowed to touch is not reachable through a loss streak either.
type AutotuneOnLossConfig struct {
	// Enabled turns the reactive trigger on. It does NOT override
	// AutotuneConfig.Enabled: with the parent off, nothing here runs and
	// nothing is written, preserving "autotune disabled means no automatic
	// strategy_overrides writes" as an unconditional guarantee.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// AfterLosses is the counter: how many CONSECUTIVE losing closes on one
	// symbol trigger a tune of that symbol. A winning (or breakeven) close
	// resets that symbol's counter to zero, so this fires on a symbol that
	// is going wrong now rather than on one that has merely lost that many
	// times ever. It then fires again every further AfterLosses in a row.
	//
	// The counter is NOT reset by a tune — only by a win. One honest
	// consecutive-loss count serves this and
	// ProtectionsConfig.DropAfterLosses, and a count the re-tune reset could
	// never reach the give-up threshold.
	//
	// Counting streaks rather than totals is what keeps this from
	// eventually tuning everything: a symbol alternating wins and losses is
	// working as intended and is never touched, however many losses it
	// accumulates. 0 defaults to 3. Below 2 is not recommended — a single
	// losing trade is noise, and tuning on it is how you curve-fit a live
	// config to one bad fill.
	AfterLosses int `json:"after_losses" yaml:"after_losses"`

	// Cooldown is the minimum time between two reactive tunes OF THE SAME
	// SYMBOL, measured from the last autotune apply (scheduled, manual or
	// reactive). It bounds how often one persistently losing symbol can
	// rewrite its own config: without it, a symbol in a bad regime can
	// re-tune every AfterLosses trades, and chasing a losing streak with a
	// fresh curve-fit every few trades is a good way to make it worse.
	//
	// A streak that arrives inside the cooldown is dropped, not queued —
	// the counter resets and the symbol must lose AfterLosses times in a
	// row again. 0 = no cooldown.
	Cooldown time.Duration `json:"-" yaml:"cooldown"`
}

// Validate rejects a negative counter/cooldown when the trigger is enabled.
// A disabled block is never acted on, so a stray value there is harmless.
func (c AutotuneOnLossConfig) Validate() error {
	if !c.Enabled {
		return nil
	}

	if c.AfterLosses < 0 {
		return fmt.Errorf("autotune.on_loss.after_losses must be >= 0, got %d", c.AfterLosses)
	}

	if c.Cooldown < 0 {
		return fmt.Errorf("autotune.on_loss.cooldown must be >= 0, got %v", c.Cooldown)
	}

	return nil
}

// Validate rejects a negative interval/bars/min_trades when enabled (a
// disabled autotune block is never acted on, so a stray zero there is
// harmless) and a symbol listed in both RunOnlyForSymbols and
// ExcludeSymbols — always excluded either way, but self-contradictory
// enough to be worth surfacing rather than silently accepting.
func (c AutotuneConfig) Validate() error {
	if !c.Enabled {
		return nil
	}

	if c.Interval < 0 {
		return fmt.Errorf("autotune.interval must be >= 0, got %v", c.Interval)
	}

	if c.Bars < 0 {
		return fmt.Errorf("autotune.bars must be >= 0, got %d", c.Bars)
	}

	if c.MinTrades < 0 {
		return fmt.Errorf("autotune.min_trades must be >= 0, got %d", c.MinTrades)
	}

	if err := c.OnLoss.Validate(); err != nil {
		return err
	}

	excluded := make(map[string]bool, len(c.ExcludeSymbols))
	for _, s := range c.ExcludeSymbols {
		excluded[s] = true
	}

	for _, s := range c.RunOnlyForSymbols {
		if excluded[s] {
			return fmt.Errorf(
				"autotune: %s is in both run_only_for_symbols and exclude_symbols", s)
		}
	}

	return nil
}

// TestModeConfig configures the default-on training window: while active,
// the engine reads real market data and runs its normal loop, but every
// order is routed to an internal simulated broker seeded from the real
// account's own current cash/positions instead of the real configured
// broker — so simulated fills get recorded like real ones, feeding
// internal/autotune's own real-track-record gating, without the real
// broker ever being touched. On by default so a freshly deployed or
// reconfigured bot proves itself before it can place a real order.
// Nothing about "when did it start" persists across restarts — each
// restart re-activates a fresh window if Enabled, matching how Autotune's
// own scheduled job is gated purely by config at startup.
type TestModeConfig struct {
	// Enabled turns the startup activation on. Off means the bot starts
	// trading (or dry-running, per trading_enabled) exactly as it always
	// has, with no simulated overlay.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Duration is how long a startup-activated window runs before
	// reverting to the real broker on its own. 0 defaults to 48h.
	Duration time.Duration `json:"-" yaml:"duration"`
}

// Validate rejects a negative duration. Enabled being true with Duration 0
// is not an error — 0 means "use the 48h default", exactly like
// AutotuneConfig.Interval's own zero-means-default convention.
func (c TestModeConfig) Validate() error {
	if c.Duration < 0 {
		return fmt.Errorf("test_mode.duration must be >= 0, got %v", c.Duration)
	}

	return nil
}

// NotifyConfig configures outbound alerts. An unattended bot that can only be
// observed by opening its web UI is one you will not observe.
type NotifyConfig struct {
	// Enabled is the master switch.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Events selects what is sent: "trade" (fills), "blocked" (rejected
	// orders), "halt" (circuit breakers and protections), "error" (upstream API
	// failures). Empty means trade + halt + error.
	Events []string `json:"events" yaml:"events"`
	// Webhook is a URL that receives the raw event as a JSON POST — the escape
	// hatch for anything not listed here (Slack, Mattermost, n8n, a script).
	Webhook string `json:"webhook" yaml:"webhook"`
	// DiscordWebhook posts to a Discord channel webhook URL.
	DiscordWebhook string `json:"discord_webhook" yaml:"discord_webhook"`
	// TelegramToken/TelegramChatID post to a Telegram chat via a bot.
	TelegramToken  string `json:"telegram_token"   yaml:"telegram_token"`
	TelegramChatID string `json:"telegram_chat_id" yaml:"telegram_chat_id"`

	// PushoverToken/PushoverUser send push notifications to phones via Pushover.
	// The token is the application's API token, the user is your user or group
	// key. Both are required for the sink to be active.
	PushoverToken string `json:"pushover_token" yaml:"pushover_token"`
	PushoverUser  string `json:"pushover_user"  yaml:"pushover_user"`
	// PushoverDevice optionally restricts delivery to one device name; empty
	// sends to all devices on the account.
	PushoverDevice string `json:"pushover_device" yaml:"pushover_device"`
	// PushoverPriority is the base priority for routine events (-2 silent, -1
	// quiet, 0 normal, 1 high/bypass-quiet-hours). Halts and errors are sent one
	// level above this, capped at 1.
	//
	// Priority 2 (emergency, repeats until acknowledged) is deliberately not
	// reachable: it requires retry/expire parameters and an acknowledgement flow,
	// and a bot that can wake you every 30 seconds until you tap a button is a
	// footgun, not a feature.
	PushoverPriority int `json:"pushover_priority" yaml:"pushover_priority"`

	// AppriseURL is an Apprise API endpoint — the one sink that reaches the
	// other hundred-odd services Apprise supports (email, Matrix, Gotify, ntfy,
	// Signal, SMS…) without this package growing an adapter for each.
	//
	// Point it at a stored configuration (http://host:8000/notify/mykey) or at
	// the stateless endpoint (http://host:8000/notify) together with
	// AppriseServices.
	AppriseURL string `json:"apprise_url" yaml:"apprise_url"`
	// AppriseServices is a comma-separated list of Apprise service URLs for the
	// stateless endpoint, e.g. "mailto://user:pass@gmail.com,ntfy://topic".
	// Ignored when AppriseURL points at a stored configuration.
	AppriseServices string `json:"apprise_services" yaml:"apprise_services"`
	// AppriseTag selects which entries of a stored Apprise configuration to
	// notify. Empty means Apprise's own default ("all").
	AppriseTag string `json:"apprise_tag" yaml:"apprise_tag"`
	// MinNotional suppresses fill alerts below this order value, so a busy bot
	// doesn't drown the channel in noise. Halts and errors are always sent.
	MinNotional float64 `json:"min_notional" yaml:"min_notional"`
	// Timeout bounds a single delivery attempt. 0 defaults to 10s.
	Timeout time.Duration `json:"-" yaml:"timeout"`
}

// Validate rejects a negative MinNotional or Timeout. Neither the YAML load
// path nor the API's PUT /api/settings/notify checked this before. See
// CODE_REVIEW_FINDINGS.md's "numerous sections have no range validation at
// load".
func (c NotifyConfig) Validate() error {
	if c.MinNotional < 0 {
		return fmt.Errorf("notify.min_notional must be >= 0, got %v", c.MinNotional)
	}

	if c.Timeout < 0 {
		return fmt.Errorf("notify.timeout must be >= 0, got %v", c.Timeout)
	}

	return nil
}

// AIConfig configures the optional "ai" detector: a local or remote LLM that is
// shown a compact snapshot of the same technical readings the other detectors
// use and answers with BUY/SELL/HOLD plus a confidence. It is off by default and
// fails safe — any error, timeout or unparseable answer yields HOLD, never a
// trade signal.
type AIConfig struct {
	// Enabled turns the advisor on. With it off the "ai" detector always votes
	// HOLD, even if it is listed in strategy.detectors.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Provider selects the wire protocol: "ollama" (native /api/chat) or
	// "openai" (any OpenAI-compatible /v1/chat/completions endpoint —
	// llama.cpp, LM Studio, vLLM, OpenRouter, OpenAI itself).
	Provider string `json:"provider" yaml:"provider"`
	// BaseURL is the server root without a trailing slash, e.g.
	// http://localhost:11434 for Ollama or https://api.openai.com for OpenAI.
	BaseURL string `json:"base_url" yaml:"base_url"`
	// Model is the model name, e.g. "llama3.2" or "qwen2.5:7b-instruct".
	Model string `json:"model" yaml:"model"`
	// APIKey is sent as a bearer token by the "openai" provider. Ollama needs
	// none. Override with BOT_AI_API_KEY.
	APIKey string `json:"api_key" yaml:"api_key"`
	// Timeout bounds a single completion request.
	Timeout time.Duration `json:"-" yaml:"timeout"`
	// RefreshInterval is the minimum time between calls for the same symbol.
	// Between refreshes the cached verdict is reused, so the LLM is polled at a
	// human pace rather than once per evaluation tick.
	RefreshInterval time.Duration `json:"-" yaml:"refresh_interval"`
	// MaxAge is how long a verdict stays usable. Past it the detector votes HOLD
	// instead of trading on a stale opinion (e.g. after the LLM went away).
	MaxAge time.Duration `json:"-" yaml:"max_age"`
	// Blocking makes the detector wait for the model instead of using the last
	// cached verdict while a refresh runs in the background. Off by default:
	// blocking would stall the evaluation loop (and make backtests glacial).
	Blocking bool `json:"blocking" yaml:"blocking"`
	// Temperature is the sampling temperature. 0 (the default) keeps verdicts as
	// reproducible as the model allows.
	Temperature float64 `json:"temperature" yaml:"temperature"`
	// MaxConcurrent caps simultaneous in-flight requests, so a large watchlist
	// cannot swamp a single local GPU.
	MaxConcurrent int `json:"max_concurrent" yaml:"max_concurrent"`
	// MinConfidence is the confidence below which a BUY/SELL verdict is demoted
	// to HOLD — the model's own uncertainty gate, applied before the strategy's
	// min_strength.
	MinConfidence float64 `json:"min_confidence" yaml:"min_confidence"`
	// MaxStrength caps the strength the detector can contribute (0..1). Lower it
	// to let the LLM tilt a decision without being able to carry one alone.
	MaxStrength float64 `json:"max_strength" yaml:"max_strength"`
	// SystemPrompt replaces the built-in instruction prompt when non-empty. The
	// model must still answer with the documented JSON object.
	SystemPrompt string `json:"system_prompt" yaml:"system_prompt"`
}

// CostsConfig models per-order trading commission. The commission for a trade
// of the given notional is: max(commission_min, commission_flat + pct*notional).
//
// It also models the two costs that don't appear on a statement but move real
// fills away from the price you saw: the bid-ask spread and slippage. Ignoring
// them is the single most common way a backtest flatters a strategy — a
// high-frequency strategy that "wins" by 3 bps per trade is losing money once a
// 5 bps spread is paid on every round trip.
type CostsConfig struct {
	CommissionPct  float64 `json:"commission_pct"  yaml:"commission_pct"`  // fraction of notional, e.g. 0.001 = 0.1%
	CommissionFlat float64 `json:"commission_flat" yaml:"commission_flat"` // flat fee added per order
	CommissionMin  float64 `json:"commission_min"  yaml:"commission_min"`  // minimum commission per order

	// SpreadPct is the full bid-ask spread as a fraction of price (0.0005 =
	// 5 bps). Half of it is paid on every fill: a market buy lifts the offer, a
	// market sell hits the bid.
	SpreadPct float64 `json:"spread_pct" yaml:"spread_pct"`
	// SlippagePct is additional adverse price movement per fill, as a fraction of
	// price — the cost of size, latency and a moving market between decision and
	// execution.
	SlippagePct float64 `json:"slippage_pct" yaml:"slippage_pct"`

	// MeasureSpread estimates each instrument's spread from its own bars
	// (Corwin–Schultz) instead of applying SpreadPct to everything.
	//
	// One spread_pct across a mixed book is wrong for every symbol in it: a
	// megacap trades inside a basis point and a thin European ETF listing can
	// cost twenty times that, and a backtest that charges both the same number
	// flatters one and libels the other. SpreadPct still applies as a FLOOR, so
	// a quiet stretch of history cannot estimate the cost away to nothing.
	MeasureSpread bool `json:"measure_spread" yaml:"measure_spread"`
}

// SpreadFor returns the spread fraction to charge for an instrument whose
// measured spread is measured (pass 0 when unknown). The configured SpreadPct is
// a floor, never a ceiling: an estimate is evidence that the real cost is at
// least this high, not permission to assume it is lower than you budgeted for.
func (c CostsConfig) SpreadFor(measured float64) float64 {
	if !c.MeasureSpread || measured <= c.SpreadPct {
		return c.SpreadPct
	}

	return measured
}

// FillPrice returns the price a market order of the given side actually fills
// at, moving the reference price adversely by half the spread plus slippage.
// Buys fill higher, sells fill lower; a zero-cost config returns price
// unchanged.
func (c CostsConfig) FillPrice(buy bool, price float64) float64 {
	adverse := c.SpreadPct/2 + c.SlippagePct
	if adverse <= 0 || price <= 0 {
		return price
	}

	if buy {
		return price * (1 + adverse)
	}

	return price * (1 - adverse)
}

// Commission returns the commission charged for a trade of the given notional.
func (c CostsConfig) Commission(notional float64) float64 {
	if notional < 0 {
		notional = -notional
	}

	fee := c.CommissionFlat + c.CommissionPct*notional
	if fee < c.CommissionMin {
		fee = c.CommissionMin
	}

	return fee
}

// SafetyConfig configures safety controls for automated trading, satisfying the
// "sensible safety measures" and "throttle rapid orders" requirements.
type SafetyConfig struct {
	// MaxOrdersPerMin trips a circuit breaker that halts automated trading when
	// the number of orders placed in the trailing minute reaches this value.
	// 0 disables the breaker.
	MaxOrdersPerMin int `yaml:"max_orders_per_min"`
	// MinOrderInterval paces order submission (minimum gap between orders) to
	// respect broker rate limits (Saxo allows ~1 order/second per session).
	MinOrderInterval time.Duration `yaml:"min_order_interval"`
}

// ExitsConfig configures protective exit rules applied to open positions. These
// run before strategy signals and force a sell when triggered. Percentages are
// fractions (0.05 = 5%). A zero percentage disables that rule.
type ExitsConfig struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// BypassTradingHalt lets protective exits (stop-loss, trailing stop,
	// take-profit, max-hold, scale-out) keep submitting REAL sell orders even
	// while trading is off — whether that is a manual toggle, a protections
	// halt (drawdown/daily-loss/realized-drawdown), or a circuit breaker
	// (order-rate, quote-stream-closed). Off by default: a halt freezes
	// everything, including exits, exactly as before this option existed. On,
	// it mirrors how a manual sell already always bypasses the toggle — the
	// idea being that a stop-loss existing specifically to cap a loss should
	// not itself be the thing frozen by the very drawdown it would prevent.
	// Does not affect strategy-driven entries/exits, which still fully
	// respect the toggle either way.
	BypassTradingHalt bool    `json:"bypass_trading_halt" yaml:"bypass_trading_halt"`
	StopLossPct       float64 `json:"stop_loss_pct" yaml:"stop_loss_pct"`         // sell if price falls this far below entry
	TrailingStopPct   float64 `json:"trailing_stop_pct" yaml:"trailing_stop_pct"` // sell if price falls this far below peak-since-entry
	TakeProfitPct     float64 `json:"take_profit_pct" yaml:"take_profit_pct"`     // sell if price rises this far above entry

	// ATRPeriod is the lookback for the volatility-adaptive stops below.
	// 0 defaults to 14.
	ATRPeriod int `json:"atr_period" yaml:"atr_period"`
	// ATRStopMult places the stop-loss this many ATRs below entry instead of a
	// fixed percentage. A fixed 5% stop is far too tight on a volatile
	// instrument (stopped out by ordinary noise) and far too loose on a calm
	// one; an ATR multiple is the same *statistical* distance on both. 0 keeps
	// the percentage stop. When both are set the wider of the two is used, so
	// the percentage acts as a floor.
	ATRStopMult float64 `json:"atr_stop_mult" yaml:"atr_stop_mult"`
	// ATRTrailMult is the same idea for the trailing stop: trail this many ATRs
	// below the peak. 0 keeps the percentage trail.
	ATRTrailMult float64 `json:"atr_trail_mult" yaml:"atr_trail_mult"`

	// TrailingOffsetPct is the profit above entry at which the trailing stop
	// switches to TrailingTightPct (0.03 = +3%). A single trailing distance has
	// to be two things at once: loose enough early that ordinary noise does not
	// end a trade before it works, and tight enough late that a winner is not
	// handed back. Those are contradictory, which is why one number always
	// disappoints somewhere. 0 disables the second stage.
	TrailingOffsetPct float64 `json:"trailing_offset_pct" yaml:"trailing_offset_pct"`
	// TrailingTightPct is the trailing distance used once TrailingOffsetPct is
	// reached. It should be SMALLER than TrailingStopPct — that is the point.
	// 0 keeps the single-stage trail.
	TrailingTightPct float64 `json:"trailing_tight_pct" yaml:"trailing_tight_pct"`
	// TrailingOnlyAfterOffset suppresses trailing entirely until the offset is
	// reached, leaving only the stop-loss in force. Use it when the trail is
	// meant purely to protect a gain, not to cap a loss.
	TrailingOnlyAfterOffset bool `json:"trailing_only_after_offset" yaml:"trailing_only_after_offset"`

	// MaxHold closes a position unconditionally once it has been held this long,
	// at whatever price the market offers — the time barrier of the triple-barrier
	// method. The ROI ladder decays the profit *target*, so it can only ever exit
	// at breakeven or better; nothing else in this config closes a position that
	// is simply dead money at a small loss, and capital parked in one of those is
	// capital not available to the next signal. 0 disables it.
	MaxHold time.Duration `json:"max_hold" yaml:"max_hold"`

	// PartialTakeProfit is a scale-out ladder: sell part of a position as it
	// clears successive profit thresholds, instead of holding all of it for one
	// all-or-nothing exit. Each rung fires at most once per position, in order.
	// Empty disables scaling out.
	PartialTakeProfit []PartialExit `json:"partial_take_profit" yaml:"partial_take_profit"`

	// ROI is a time-decaying profit ladder, keyed by how long the position has
	// been held: {"0s": 0.10, "30m": 0.05, "2h": 0.02, "6h": 0}. The bot takes
	// profit as soon as the gain clears the threshold for the position's current
	// age, so it demands a lot early and settles for less as a trade goes stale
	// — the entry that hasn't worked in six hours probably isn't going to.
	// A 0 threshold means "exit at any profit"; empty disables the ladder.
	ROI map[string]float64 `json:"roi" yaml:"roi"`
}

// PartialExit is one rung of the scale-out ladder: once the position is up
// ProfitPct, sell SellPct of the ORIGINAL quantity.
//
// Scaling out trades expectancy for variance. Taking a third off at +5% caps
// what that third can earn, and on the trades that would have run to +30% it
// costs real money. What it buys is that fewer trades round-trip all the way
// back to the stop, which is the failure mode people actually abandon a strategy
// over. It is off by default; turn it on deliberately, and check the effect in a
// backtest rather than assuming it is free.
type PartialExit struct {
	// ProfitPct is the gain above entry that arms this rung (0.05 = +5%).
	ProfitPct float64 `json:"profit_pct" yaml:"profit_pct"`
	// SellPct is the fraction of the position's ORIGINAL quantity to sell
	// (0.33 = a third). Measuring against the original size keeps the ladder
	// meaning what it says: three 0.33 rungs sell the position, not 70% of it.
	SellPct float64 `json:"sell_pct" yaml:"sell_pct"`
}

// ScaleInConfig adds to a position that has moved AGAINST the entry — averaging
// down, freqtrade's `adjust_trade_position` and Hummingbot's DCA executor.
//
// Read this before enabling it. Averaging down improves the average entry price
// of every trade that eventually recovers and increases the size of every trade
// that does not. It converts a series of small losses into a smaller series of
// large ones, and it is the mechanism behind most blown retail accounts. It is
// defensible only with a hard cap on the number of adds AND a stop-loss that is
// still enforced on the enlarged position — both of which this implementation
// requires. Off by default.
type ScaleInConfig struct {
	// Enabled turns the ladder on. Off ⇒ a position is entered once.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// StepPct is how far below the average entry price the position must be
	// before the next add (0.03 = 3% underwater).
	StepPct float64 `json:"step_pct" yaml:"step_pct"`
	// MaxAdds caps how many times one position may be added to. Required: a
	// ladder without a cap is a martingale. 0 disables scaling in.
	MaxAdds int `json:"max_adds" yaml:"max_adds"`
	// SizePct is each add's size as a fraction of the configured order size —
	// the same notional a fresh entry would use, after risk sizing (1.0 = a
	// full extra order, 0.5 = half). Values above 1 are martingale sizing and
	// are rejected by Validate.
	SizePct float64 `json:"size_pct" yaml:"size_pct"`
	// RequireSignal only adds when the strategy still says BUY. Without it the
	// ladder buys purely because the price fell, which is a bet on mean
	// reversion the strategy never made.
	RequireSignal bool `json:"require_signal" yaml:"require_signal"`
}

// StorageConfig configures persistence.
type StorageConfig struct {
	// Driver is "sqlite" or "none" (in-memory only).
	Driver string `yaml:"driver"`
	// Path is the SQLite database file path.
	Path string `yaml:"path"`
}

// ServerConfig configures the gin HTTP API.
type ServerConfig struct {
	Addr string `yaml:"addr"`
	// AuthToken, when non-empty, requires every /api request to present it as a
	// bearer token (Authorization: Bearer <token>) or a ?token= query parameter
	// (used by the SSE stream). Empty disables auth. Override with BOT_AUTH_TOKEN.
	AuthToken string `yaml:"auth_token"`
}

// FinnhubConfig configures the Finnhub data feed.
type FinnhubConfig struct {
	APIKey   string `yaml:"api_key"`
	RESTBase string `yaml:"rest_base"`
	WSBase   string `yaml:"ws_base"`
}

// AlpacaConfig configures the Alpaca broker. Paper mode is the default and
// strongly recommended until a strategy is proven.
type AlpacaConfig struct {
	KeyID     string `yaml:"key_id"`
	SecretKey string `yaml:"secret_key"`
	BaseURL   string `yaml:"base_url"` // e.g. https://paper-api.alpaca.markets
	Paper     bool   `yaml:"paper"`
	// UseSim, when true, ignores Alpaca entirely and uses the internal
	// simulated broker (fully offline paper trading).
	UseSim bool `yaml:"use_sim"`
	// SimCash is the starting cash for the simulated broker.
	SimCash float64 `yaml:"sim_cash"`
	// PreferredCurrency, if set, reports balances and position values
	// converted from the account's own currency (USD in practice) into this
	// currency instead. Empty means "report in whatever currency Alpaca uses".
	PreferredCurrency string `yaml:"preferred_currency"`
}

// SaxoConfig configures the Saxo Bank OpenAPI broker. Use the simulation
// gateway and a 24-hour developer token to start; switch BaseURL to the live
// gateway and supply a real OAuth token for live trading.
type SaxoConfig struct {
	// AuthMode selects authentication: "token" (a static bearer token, e.g. the
	// 24-hour SIM token) or "certificate" (unattended CBA with auto-refresh).
	AuthMode string `yaml:"auth_mode"`
	// Token is the static OAuth bearer token used when auth_mode is "token".
	Token string `yaml:"token"`
	// BaseURL is the OpenAPI gateway base, without a trailing slash:
	//   sim:  https://gateway.saxobank.com/sim/openapi
	//   live: https://gateway.saxobank.com/openapi
	BaseURL string `yaml:"base_url"`
	// AccountKey optionally pins a specific account; when empty the first
	// account returned by /port/v1/accounts/me is used.
	AccountKey string `yaml:"account_key"`
	// AssetTypes is the comma-separated instrument search scope used to map a
	// ticker to a Saxo Uic, e.g. "Stock,Etf".
	AssetTypes string `yaml:"asset_types"`
	// PreferredCurrency, if set, converts position values reported in a
	// foreign-listed instrument's own currency (e.g. USD for a US stock) into
	// this currency, and account balances from the account's own currency.
	// Empty means "report everything in whatever currency Saxo reports it in".
	PreferredCurrency string `yaml:"preferred_currency"`

	// --- certificate-based auth (auth_mode: certificate) ---
	// AppKey/AppSecret are the live application credentials (client_id/secret).
	AppKey    string `yaml:"app_key"`
	AppSecret string `yaml:"app_secret"`
	// AppURL is the application's registered URL (the "spurl" JWT claim).
	AppURL string `yaml:"app_url"`
	// UserID is the user the certificate is issued for (the "sub" JWT claim).
	UserID string `yaml:"user_id"`
	// AuthURL is the OAuth host (no trailing slash). When empty it is derived
	// from BaseURL: sim -> https://sim.logonvalidation.net,
	// live -> https://live.logonvalidation.net.
	AuthURL string `yaml:"auth_url"`
	// CertPath is a .p12/.pfx or PEM file holding the certificate + RSA key.
	CertPath     string `yaml:"cert_path"`
	CertPassword string `yaml:"cert_password"`
}

// SaxoAuthURL returns the configured OAuth host, deriving it from the gateway
// BaseURL (sim vs live) when not explicitly set.
func (s SaxoConfig) SaxoAuthURL() string {
	if s.AuthURL != "" {
		return s.AuthURL
	}

	if strings.Contains(s.BaseURL, "/sim/") {
		return "https://sim.logonvalidation.net"
	}

	return "https://live.logonvalidation.net"
}

// Trading212Config configures the Trading 212 Public API. API keys are
// generated self-service in the Trading 212 mobile app (no app approval). Use
// the demo base URL for paper trading first.
type Trading212Config struct {
	// APIKey and Secret are the two credentials shown when you generate an API
	// key in the app. They are sent as HTTP Basic auth: base64(api_key:secret).
	APIKey string `yaml:"api_key"`
	Secret string `yaml:"secret"`
	// BaseURL is the API base, without a trailing slash:
	//   demo: https://demo.trading212.com/api/v0
	//   live: https://live.trading212.com/api/v0
	BaseURL string `yaml:"base_url"`
	// ISINs maps watchlist symbols to ISINs for instruments that can't be
	// resolved by ticker (e.g. "CSPXJZ.XC": "IE00B52MJY50").
	ISINs map[string]string `yaml:"isins"`
	// PreferredCurrency picks the listing currency when an instrument has
	// several (e.g. EUR over GBX/USD to avoid FX). Default EUR.
	PreferredCurrency string `yaml:"preferred_currency"`
}

// Limits are the enforced money limits. A zero value means "no limit" for that
// dimension, except MaxOrderValue and MaxTotalInvested which, if zero, are
// treated as hard blocks (fail-closed) to avoid accidental unbounded trading.
type Limits struct {
	// Every money limit here can be written as a fixed amount, as a fraction
	// of current equity, or as both. 0 means "not set" for either half, and
	// when both are set the TIGHTER one binds — so an absolute doubles as a
	// hard backstop on a percentage ("20% of equity, but never above 5000").
	//
	// The percentages exist because a fixed amount goes stale the moment the
	// account grows: 950 is the whole book at 1000 equity and 9.5% of it at
	// 10000, and nothing says so. Percentages track the account without
	// edits. Note the direction differs by kind — the ceilings take the
	// smaller value, CashReserve (a floor) the larger. Resolve applies both.
	//
	// Position SIZING already scales on its own: max_pct_per_asset_class and
	// min_positions_per_asset_class are percentages, so a slot is a constant
	// share of equity at any account size. These fields are what stops that
	// from reaching the order.

	// MaxTotalInvested caps the total market value of all open positions.
	MaxTotalInvested float64 `json:"max_total_invested" yaml:"max_total_invested"`
	// MaxTotalInvestedPct caps it as a fraction of equity instead (0 = off).
	MaxTotalInvestedPct float64 `json:"max_total_invested_pct" yaml:"max_total_invested_pct"`
	// MaxPerPosition caps market value held in any single symbol.
	MaxPerPosition float64 `json:"max_per_position" yaml:"max_per_position"`
	// MaxPerPositionPct caps it as a fraction of equity instead (0 = off).
	MaxPerPositionPct float64 `json:"max_per_position_pct" yaml:"max_per_position_pct"`
	// MaxOrderValue caps the notional value of a single order.
	MaxOrderValue float64 `json:"max_order_value" yaml:"max_order_value"`
	// MaxOrderValuePct caps it as a fraction of equity instead (0 = off).
	MaxOrderValuePct float64 `json:"max_order_value_pct" yaml:"max_order_value_pct"`
	// MaxDailySpend caps cumulative buy notional within a rolling trading day.
	MaxDailySpend float64 `json:"max_daily_spend" yaml:"max_daily_spend"`
	// MaxDailySpendPct caps it as a fraction of equity instead (0 = off).
	MaxDailySpendPct float64 `json:"max_daily_spend_pct" yaml:"max_daily_spend_pct"`
	// CashReserve is a floor of cash that must remain uninvested.
	CashReserve float64 `json:"cash_reserve" yaml:"cash_reserve"`
	// CashReservePct is the same floor as a fraction of equity (0 = off).
	// Being a floor, the LARGER of the two binds, not the smaller.
	CashReservePct float64 `json:"cash_reserve_pct" yaml:"cash_reserve_pct"`
	// MaxOpenPositions caps the number of distinct symbols held at once.
	MaxOpenPositions int `json:"max_open_positions" yaml:"max_open_positions"`
}

// Resolve returns these limits with every percentage converted against equity
// and folded into its absolute twin, so callers work with plain amounts and
// the absolute/percentage question is answered in exactly one place.
//
// Ceilings take the tighter (smaller) of the two; CashReserve, being a floor,
// takes the larger. 0 continues to mean "no limit" for a ceiling and "no
// reserve" for the floor, so a config setting neither half is unchanged.
//
// A non-positive equity resolves every percentage to nothing rather than to
// zero: zero is "no limit" for a ceiling, and inventing an unlimited ceiling
// from a missing account value is the one reading that could let an order
// through unchecked.
func (l Limits) Resolve(equity float64) Limits {
	out := l

	out.MaxTotalInvested = tighterCeiling(l.MaxTotalInvested, l.MaxTotalInvestedPct, equity)
	out.MaxPerPosition = tighterCeiling(l.MaxPerPosition, l.MaxPerPositionPct, equity)
	out.MaxOrderValue = tighterCeiling(l.MaxOrderValue, l.MaxOrderValuePct, equity)
	out.MaxDailySpend = tighterCeiling(l.MaxDailySpend, l.MaxDailySpendPct, equity)

	if equity > 0 && l.CashReservePct > 0 {
		out.CashReserve = max(l.CashReserve, l.CashReservePct*equity)
	}

	return out
}

// tighterCeiling picks the binding value of an absolute ceiling and a
// fraction-of-equity one, where 0 means "not set" for either. Returns 0 when
// neither is set, which every caller already reads as "no ceiling".
func tighterCeiling(abs, pct, equity float64) float64 {
	var scaled float64

	if pct > 0 && equity > 0 {
		scaled = pct * equity
	}

	switch {
	case abs > 0 && scaled > 0:
		return min(abs, scaled)
	case abs > 0:
		return abs
	default:
		return scaled
	}
}

// Validate rejects a negative limit. Every field here is a ceiling or a
// floor money is measured against; a negative one does not mean "tighter",
// it inverts the check (e.g. cash_reserve: -100 *increases* spendable
// headroom instead of reserving any). The API's PUT /api/limits already
// rejects negatives; this closes the same gap for a config.yaml loaded
// directly, which used to reach Authorize with no check at all outside the
// narrower, TradingEnabled-gated ValidateForTrading. See
// CODE_REVIEW_FINDINGS.md's "numerous sections have no range validation at
// load".
func (l Limits) Validate() error {
	for name, v := range map[string]float64{
		"max_total_invested": l.MaxTotalInvested,
		"max_per_position":   l.MaxPerPosition,
		"max_order_value":    l.MaxOrderValue,
		"max_daily_spend":    l.MaxDailySpend,
		"cash_reserve":       l.CashReserve,
	} {
		if v < 0 {
			return fmt.Errorf("limits.%s must be >= 0, got %v", name, v)
		}
	}

	// The percentage halves are fractions of equity, so >1 is almost always a
	// "20" that meant 20%. Caught here for the same reason
	// max_pct_per_asset_class catches it: a 2000%-of-equity ceiling can never
	// bind, and the operator would believe they were capped when they were not.
	for name, v := range map[string]float64{
		"max_total_invested_pct": l.MaxTotalInvestedPct,
		"max_per_position_pct":   l.MaxPerPositionPct,
		"max_order_value_pct":    l.MaxOrderValuePct,
		"max_daily_spend_pct":    l.MaxDailySpendPct,
		"cash_reserve_pct":       l.CashReservePct,
	} {
		if v < 0 || v > 1 {
			return fmt.Errorf(
				"limits.%s must be a fraction in 0..1 (0.20 = 20%%), got %v", name, v)
		}
	}

	if l.MaxOpenPositions < 0 {
		return fmt.Errorf("limits.max_open_positions must be >= 0, got %d", l.MaxOpenPositions)
	}

	return nil
}

// StrategyConfig holds the detector selection, the combine mode and all
// per-indicator parameters.
type StrategyConfig struct {
	// Detectors lists which detectors to run for BUY decisions. Empty =>
	// ema_cross, rsi, macd. Available: ema_cross, rsi, macd, bollinger, vwap,
	// atr, rvol, volume_profile, trend, adx, stochastic, supertrend, donchian.
	Detectors []string `json:"detectors" yaml:"detectors"`
	// DetectorsSell is the detector set for SELL decisions; empty => same as
	// Detectors. Lets you use different indicators to exit than to enter.
	DetectorsSell []string `json:"detectors_sell" yaml:"detectors_sell"`
	// Combine merges detectors for BUY decisions: "majority", "unanimous" or
	// "weighted".
	Combine string `json:"combine" yaml:"combine"`
	// CombineSell is the merge mode for SELL decisions; empty => same as Combine.
	// Lets you be strict to enter and lenient to exit (e.g. unanimous buy,
	// majority sell).
	CombineSell string `json:"combine_sell" yaml:"combine_sell"`
	// MinStrength is the minimum combined confidence (0..1) required to BUY;
	// weaker signals are treated as HOLD. Filters noise/over-trading.
	MinStrength float64 `json:"min_strength" yaml:"min_strength"`
	// MinStrengthSell is the minimum confidence required to SELL. A negative
	// value (the default) means "inherit MinStrength"; set 0..1 to let exits
	// trigger at a lower bar than entries.
	MinStrengthSell float64 `json:"min_strength_sell" yaml:"min_strength_sell"`

	// Moving averages / RSI / MACD.
	FastMA        int     `json:"fast_ma" yaml:"fast_ma"`
	SlowMA        int     `json:"slow_ma" yaml:"slow_ma"`
	RSIPeriod     int     `json:"rsi_period" yaml:"rsi_period"`
	RSIOverbought float64 `json:"rsi_overbought" yaml:"rsi_overbought"`
	RSIOversold   float64 `json:"rsi_oversold" yaml:"rsi_oversold"`
	// RSIMode: "midline" (graded mean-reversion vote around 50, always
	// participates) or "extremes" (only votes at oversold/overbought).
	RSIMode    string `json:"rsi_mode" yaml:"rsi_mode"`
	MACDFast   int    `json:"macd_fast" yaml:"macd_fast"`
	MACDSlow   int    `json:"macd_slow" yaml:"macd_slow"`
	MACDSignal int    `json:"macd_signal" yaml:"macd_signal"`

	// Bollinger Bands.
	BBPeriod int     `json:"bb_period" yaml:"bb_period"`
	BBStdDev float64 `json:"bb_stddev" yaml:"bb_stddev"`
	// VWAP.
	VWAPPeriod int `json:"vwap_period" yaml:"vwap_period"`
	// ATR breakout.
	ATRPeriod int     `json:"atr_period" yaml:"atr_period"`
	ATRMult   float64 `json:"atr_mult" yaml:"atr_mult"`
	// Relative volume.
	RVOLPeriod    int     `json:"rvol_period" yaml:"rvol_period"`
	RVOLThreshold float64 `json:"rvol_threshold" yaml:"rvol_threshold"`
	// Volume profile.
	VPWindow       int     `json:"vp_window" yaml:"vp_window"`
	VPBuckets      int     `json:"vp_buckets" yaml:"vp_buckets"`
	VPValueAreaPct float64 `json:"vp_value_area_pct" yaml:"vp_value_area_pct"`

	// ADX (Average Directional Index): trend-strength + direction.
	ADXPeriod    int     `json:"adx_period" yaml:"adx_period"`
	ADXThreshold float64 `json:"adx_threshold" yaml:"adx_threshold"` // min ADX to vote a trend (e.g. 25)
	// Stochastic oscillator (mean reversion).
	StochKPeriod    int     `json:"stoch_k_period" yaml:"stoch_k_period"`
	StochDPeriod    int     `json:"stoch_d_period" yaml:"stoch_d_period"`
	StochOverbought float64 `json:"stoch_overbought" yaml:"stoch_overbought"` // e.g. 80
	StochOversold   float64 `json:"stoch_oversold" yaml:"stoch_oversold"`     // e.g. 20
	// Supertrend (ATR-based trend follow).
	SupertrendPeriod int     `json:"supertrend_period" yaml:"supertrend_period"`
	SupertrendMult   float64 `json:"supertrend_mult" yaml:"supertrend_mult"`
	// Donchian channel breakout.
	DonchianPeriod int `json:"donchian_period" yaml:"donchian_period"`

	// Weights assigns a per-detector multiplier (by canonical name, e.g.
	// "trend": 2.0) applied to that detector's strength when combining. Missing
	// entries default to 1.0. Lets a "weighted" combine favour some signals, and
	// scales every mode's reported strength. A zero or negative weight mutes a
	// detector's strength contribution without removing its vote.
	Weights map[string]float64 `json:"weights" yaml:"weights"`

	// Trend: a linear-regression slope over a long lookback (up to a week of
	// 1-minute bars) used to read the prevailing direction rather than reacting
	// to the last bar or two.
	//
	// TrendPeriod is the lookback in bars (1-minute). The detector uses all
	// available bars up to this, so it works before a full week has accrued.
	TrendPeriod int `json:"trend_period" yaml:"trend_period"`
	// TrendThreshold is the minimum absolute fractional change implied by the
	// fitted line across the window for it to count as a trend (deadband). E.g.
	// 0.01 = the line must move at least 1% end-to-end. Smaller values are
	// treated as "flat".
	TrendThreshold float64 `json:"trend_threshold" yaml:"trend_threshold"`
	// TrendGate, when true, makes the trend a directional filter: an up-trend
	// blocks strategy SELLs and a down-trend blocks BUYs (a flat trend gates
	// nothing). Protective exits still fire regardless. Independent of whether
	// "trend" is in the Detectors set.
	TrendGate bool `json:"trend_gate" yaml:"trend_gate"`
	// TrendPeriodSell/TrendThresholdSell independently override the lookback
	// and threshold used when the gate is vetoing a SELL (an up-trend blocking
	// it), letting "don't buy into a downtrend" and "don't cut a winner short"
	// be tuned separately. A value <=0 (the default) inherits TrendPeriod /
	// TrendThreshold — the BUY-side veto always uses those. Only read when
	// TrendGate is true.
	TrendPeriodSell    int     `json:"trend_period_sell"    yaml:"trend_period_sell"`
	TrendThresholdSell float64 `json:"trend_threshold_sell" yaml:"trend_threshold_sell"`

	// DenyGate, when true, adds a short-term veto checked only on a BUY that
	// otherwise passed every other gate: it can still deny the buy outright.
	// Unlike Weights/Combine, this never contributes a vote or nudges
	// strength — it is pass/fail, evaluated after everything else, the same
	// architectural slot as TrendGate but answering a different question
	// ("is this entry walking into a false breakout or an already-extended
	// move about to snap back?") instead of "does this fight the prevailing
	// trend?". Two independent checks, either one denies:
	//   1. False-breakout: price is at/through a fresh DenyBreakoutPeriod-bar
	//      high but relative volume is below DenyBreakoutRVOLThreshold — a
	//      breakout without volume behind it is the textbook false-breakout
	//      setup (see docs/CODE_ANALYSIS_GUIDE.md's Tier 2 strategy notes).
	//   2. Overextension: a short DenyReversalStochPeriod-bar %K is already at
	//      or past DenyReversalOverbought — buying into a move that's already
	//      stretched this far on a short lookback is the classic setup for an
	//      imminent pullback/reversal, regardless of what the longer-period
	//      trend detectors say.
	// SELL is never denied — exits must never be blocked (same rule as
	// risk.Manager.Authorize and TrendGate).
	DenyGate bool `json:"deny_gate" yaml:"deny_gate"`
	// DenyBreakoutPeriod is the short Donchian lookback (bars) for the
	// false-breakout check. <=0 disables that check specifically.
	DenyBreakoutPeriod int `json:"deny_breakout_period" yaml:"deny_breakout_period"`
	// DenyBreakoutRVOLPeriod is the relative-volume lookback for the same
	// check. <=0 falls back to RVOLPeriod.
	DenyBreakoutRVOLPeriod int `json:"deny_breakout_rvol_period" yaml:"deny_breakout_rvol_period"`
	// DenyBreakoutRVOLThreshold is the minimum relative volume a fresh high
	// must show to be trusted as a real breakout; below it, the buy is
	// denied as a likely false breakout.
	DenyBreakoutRVOLThreshold float64 `json:"deny_breakout_rvol_threshold" yaml:"deny_breakout_rvol_threshold"`
	// DenyReversalStochPeriod is the short %K lookback (bars) for the
	// overextension check. <=0 disables that check specifically.
	DenyReversalStochPeriod int `json:"deny_reversal_stoch_period" yaml:"deny_reversal_stoch_period"`
	// DenyReversalOverbought is the %K level, on that short lookback, past
	// which a buy is denied as chasing an already-extended move.
	DenyReversalOverbought float64 `json:"deny_reversal_overbought" yaml:"deny_reversal_overbought"`

	// HTFMultiple enables multi-timeframe confirmation: every HTFMultiple bars
	// are aggregated into one higher-timeframe bar (15 on 1-minute bars gives a
	// 15-minute view), and HTFDetectors are evaluated on that. 0 or 1 disables
	// it.
	//
	// The point is to stop the strategy fighting the larger trend: a buy signal
	// on 1-minute bars inside an hourly downtrend is usually noise. Note the
	// cost — confirmation needs HTFMultiple times as much history before it can
	// say anything, so the strategy's warmup grows accordingly.
	HTFMultiple int `json:"htf_multiple" yaml:"htf_multiple"`
	// HTFDetectors is the detector set evaluated on the higher timeframe. Empty
	// falls back to a plain regression trend, which is usually what you want
	// from a confirmation timeframe.
	HTFDetectors []string `json:"htf_detectors" yaml:"htf_detectors"`
	// HTFMode selects how the higher timeframe is applied: "gate" (default)
	// vetoes signals that oppose it; "vote" adds it to the detector tally like
	// any other opinion.
	HTFMode string `json:"htf_mode" yaml:"htf_mode"`
}

// ExecutionConfig controls how an approved order is sent to the venue.
//
// A market order is the simplest thing that works and the most expensive: it
// crosses the spread every time and, in size, walks the book. A limit order
// placed at or inside the touch pays the spread to you instead — at the cost of
// possibly not filling at all, which is why it needs a timeout and a fallback.
type ExecutionConfig struct {
	// Type selects the order type for automated entries and strategy exits:
	// "market" (default) or "limit".
	Type string `yaml:"type"`
	// LimitOffsetPct places a limit order this fraction *away* from the current
	// price in the passive direction — below for a buy, above for a sell
	// (0.0005 = 5 bps). 0 puts it at the reference price.
	LimitOffsetPct float64 `yaml:"limit_offset_pct"`
	// LimitTimeout is how long an unfilled limit order is left working before
	// the engine gives up on it. 0 defaults to 2 minutes.
	LimitTimeout time.Duration `yaml:"limit_timeout"`
	// MarketFallback re-sends an expired limit order as a market order. Default
	// true: an entry that silently never happened is confusing, and an *exit*
	// that never happened is dangerous. Protective exits ignore Type entirely
	// and always go to market — when a stop fires, filling matters more than
	// filling well.
	MarketFallback bool `yaml:"market_fallback"`

	// SliceNotional splits an order larger than this into several child orders
	// of at most this size, submitted MinOrderInterval apart (see safety), so a
	// large order is worked into the book instead of taken in one clip. 0
	// disables slicing.
	SliceNotional float64 `yaml:"slice_notional"`
	// MaxSlices caps how many children one order may be split into, so a very
	// large order cannot generate an unbounded stream of them. 0 defaults to 5.
	MaxSlices int `yaml:"max_slices"`
}

// EngineConfig controls evaluation cadence.
type EngineConfig struct {
	// EvalInterval is how often the engine re-evaluates strategy signals.
	EvalInterval time.Duration `yaml:"eval_interval"`
	// BarInterval is the candle size the engine aggregates live ticks into and
	// runs the strategy on. 0 defaults to 1 minute. Larger bars (e.g. 5m) trade
	// on coarser, less noisy candles.
	BarInterval time.Duration `yaml:"bar_interval"`
	// TradingEnabled gates whether signals are turned into real orders.
	// When false, the engine still computes and exposes signals (dry run).
	TradingEnabled bool `yaml:"trading_enabled"`
	// MinPositions is the coarse tier of the position-slot rule: the book
	// should spread over at least this many positions, so no single buy may
	// exceed its share of what can actually be invested. 0 = no global split.
	//
	// portfolio.min_positions_per_asset_class refines it per asset class and
	// takes precedence for the classes it names; whatever it does not claim
	// is what an unnamed class gets. Both tiers resolve in one place —
	// risk.PortfolioGuard.MaxPositionValue — and bind as a cap inside
	// risk.Manager.Authorize, alongside max_order_value and max_per_position,
	// rather than as a separate ceiling computed before Authorize ever sees
	// the order.
	MinPositions int `yaml:"min_positions"`

	// MaxRiskSizeUSD caps what risk sizing may produce, so a very generous
	// headroom cannot translate into an enormous position. 0 = only the money
	// limits apply (they still clamp every order regardless).
	MaxRiskSizeUSD float64 `yaml:"max_risk_size_usd"`
	// MinOrderSizeUSD is the smallest order worth placing. It is a REFUSAL
	// threshold, not a floor: an order the risk scaling puts below it is not
	// placed at all.
	//
	// It used to be a floor, raising such an order back up — which enlarged
	// precisely the orders the scaling had just judged too risky to hold at
	// full size, the opposite of the intent. An order below the size worth
	// paying commission on is an order not worth placing. 0 = no threshold.
	MinOrderSizeUSD float64 `yaml:"min_order_size_usd"`
	// KeepInterval is the minimum time to hold a position before a *strategy*
	// SELL is allowed (protective exits still fire immediately). 0 = disabled.
	// Stops the bot dumping a position seconds after buying on a blip.
	KeepInterval time.Duration `yaml:"keep_interval"`
	// ConfirmBuy / ConfirmSell require this many consecutive evaluations to
	// agree before acting, so a one-off reading doesn't trigger a trade. 1 (the
	// default) = act immediately; 2 = wait for a second confirming check.
	ConfirmBuy  int `yaml:"confirm_buy"`
	ConfirmSell int `yaml:"confirm_sell"`
	// AllowPyramiding permits adding to an existing position on repeated buy
	// signals. Default false: at most one entry per symbol until it is exited,
	// which avoids over-trading/churn.
	AllowPyramiding bool `yaml:"allow_pyramiding"`

	// SkipFullSlots stops evaluating symbols that cannot be bought because
	// limits.max_open_positions is already reached. Held symbols are still
	// evaluated — they need their sell signals, and their protective exits run
	// regardless — so nothing that is on the books goes unwatched.
	//
	// The saving is not CPU. It is the per-symbol work that costs money or quota:
	// LLM advisor calls, ML inference, and venue-price lookups spent forming an
	// opinion the risk manager is guaranteed to refuse.
	SkipFullSlots bool `yaml:"skip_full_slots"`

	// ScaleIn is the structured averaging-down ladder. Unlike AllowPyramiding —
	// which lets the strategy re-enter freely on any repeated BUY — this adds on
	// an explicit, capped schedule as the position moves against the entry. Read
	// ScaleInConfig's doc comment before enabling it.
	ScaleIn ScaleInConfig `yaml:"scale_in"`

	// MarketHoursCheck gates automated strategy orders to the symbol's exchange
	// session, so the bot only trades when (or just before) the venue is open and
	// the reference price is current. Protective exits are NOT gated. Default true.
	MarketHoursCheck bool `yaml:"market_hours_check"`
	// PreOpenWindow lets orders be placed this long before the exchange opens
	// (default 5m), in addition to the whole regular session.
	PreOpenWindow time.Duration `yaml:"pre_open_window"`
	// VenuePriceCheck re-confirms the execution price from market data right
	// before an automated order: the live feed may lag or be a different listing
	// than the broker actually trades (e.g. a USD vs EUR listing). The confirmed
	// price is used for order sizing and risk checks. Default true.
	VenuePriceCheck bool `yaml:"venue_price_check"`
	// MaxPriceDeviation blocks an automated BUY when the confirmed venue price
	// deviates more than this fraction from the signal price (0.02 = 2%); the
	// price moved too much since the signal. 0 disables the deviation veto (the
	// order is still resized to the confirmed price). Default 0.02.
	MaxPriceDeviation float64 `yaml:"max_price_deviation"`
	// Execution controls HOW an approved order reaches the venue: market or
	// limit, and in one clip or several.
	Execution ExecutionConfig `yaml:"execution"`

	// SyncInterval is the minimum time between broker polls for account/positions
	// and working orders (portfolio reconcile). Raise it for brokers with slow or
	// rate-limited APIs (e.g. Trading 212, which returns 500s under frequent
	// polling). 0 defaults to eval_interval/2 for the portfolio sync and 15s for
	// the order reconcile. Order paths always force an immediate post-fill sync,
	// so this only bounds the *background* refresh cadence, never trade freshness.
	SyncInterval time.Duration `yaml:"sync_interval"`
}

// Default returns a config populated with safe defaults (paper/simulated,
// trading disabled, conservative limits).
//

func Default() Config {
	return Config{
		Server: ServerConfig{Addr: ":8080"},
		Finnhub: FinnhubConfig{
			RESTBase: "https://finnhub.io/api/v1",
			WSBase:   "wss://ws.finnhub.io",
		},
		Broker: "sim",
		Alpaca: AlpacaConfig{
			BaseURL: "https://paper-api.alpaca.markets",
			Paper:   true,
			UseSim:  true,
			SimCash: 100000,
		},
		Saxo: SaxoConfig{
			BaseURL:    "https://gateway.saxobank.com/sim/openapi",
			AssetTypes: "Stock,Etf",
		},
		Trading212: Trading212Config{
			BaseURL: "https://demo.trading212.com/api/v0",
		},
		Limits: Limits{
			MaxTotalInvested: 10000,
			MaxPerPosition:   1000,
			MaxOrderValue:    500,
			MaxDailySpend:    2000,
			CashReserve:      100,
			MaxOpenPositions: 50,
		},
		Strategy: StrategyConfig{
			Detectors:       []string{"ema_cross", "rsi", "macd", "trend"},
			Combine:         "majority",
			MinStrength:     0.25,
			MinStrengthSell: -1, // inherit MinStrength unless set
			FastMA:          12, SlowMA: 26,
			RSIPeriod: 14, RSIOverbought: 70, RSIOversold: 30, RSIMode: "midline",
			MACDFast: 12, MACDSlow: 26, MACDSignal: 9,
			BBPeriod: 20, BBStdDev: 2,
			VWAPPeriod: 20,
			ATRPeriod:  14, ATRMult: 1.5,
			RVOLPeriod: 20, RVOLThreshold: 1.5,
			VPWindow: 120, VPBuckets: 24, VPValueAreaPct: 0.7,
			ADXPeriod: 14, ADXThreshold: 25,
			StochKPeriod: 14, StochDPeriod: 3, StochOverbought: 80, StochOversold: 20,
			SupertrendPeriod: 10, SupertrendMult: 3,
			DonchianPeriod: 20,
			TrendPeriod:    1950, TrendThreshold: 0.01, TrendGate: true,
			DenyGate:                  false,
			DenyBreakoutPeriod:        20,
			DenyBreakoutRVOLThreshold: 1.2,
			DenyReversalStochPeriod:   5,
			DenyReversalOverbought:    85,
		},
		Engine: EngineConfig{
			EvalInterval:      30 * time.Second,
			BarInterval:       time.Minute,
			TradingEnabled:    false,
			ConfirmBuy:        1,
			ConfirmSell:       1,
			MarketHoursCheck:  true,
			PreOpenWindow:     5 * time.Minute,
			VenuePriceCheck:   true,
			MaxPriceDeviation: 0.02,
		},
		Exits: ExitsConfig{
			Enabled:         true,
			StopLossPct:     0.05,
			TrailingStopPct: 0.08,
			TakeProfitPct:   0.20,
		},
		Storage: StorageConfig{
			Driver: "sqlite",
			Path:   "finance_bot.db",
		},
		Safety: SafetyConfig{
			MaxOrdersPerMin:  20,
			MinOrderInterval: 1100 * time.Millisecond,
		},
		Protections: defaultProtections(),
		Screener:    defaultScreener(),
		Autotune:    defaultAutotune(),
		TestMode:    defaultTestMode(),
		Notify:      NotifyConfig{Enabled: false, Timeout: 10 * time.Second},
		AI:          defaultAI(),
		ML:          defaultML(),
	}
}

// defaultML returns the (disabled) learned-detector settings. MinSkill defaults
// to a non-zero value on purpose: the failure mode that matters is a model with
// no predictive power emitting confident probabilities, and defaulting the guard
// to off would make that the easy path.
func defaultML() MLConfig {
	return MLConfig{
		Enabled:      false,
		Horizon:      10,
		Threshold:    0.005,
		TrainBars:    1500,
		RetrainEvery: 250,
		MinSamples:   200,
		BuyProb:      0.58,
		SellProb:     0.42,
		MinSkill:     0.02,
		MaxStrength:  0.5,
		Epochs:       300,
		LearningRate: 0.05,
		L2:           0.01,
	}
}

// defaultProtections returns the protections that are ON out of the box. They
// are enabled by default on purpose: they cost nothing when the strategy is
// behaving, and the failure they prevent is the expensive one.
func defaultProtections() ProtectionsConfig {
	return ProtectionsConfig{
		Enabled:           true,
		CooldownAfterLoss: 30 * time.Minute,
		StopGuardTrades:   4,
		StopGuardLookback: 24 * time.Hour,
		StopGuardLock:     4 * time.Hour,
		MaxDrawdownPct:    0.20,
		MaxDailyLossPct:   0.08,
	}
}

// defaultScreener returns the (disabled) automatic universe selection. Off by
// default: the watchlist stays whatever config and the API say it is.
func defaultScreener() ScreenerConfig {
	return ScreenerConfig{
		Enabled:        false,
		Interval:       6 * time.Hour,
		Rank:           "volume",
		MinHistoryBars: 100,
		KeepHeld:       true,
	}
}

// defaultAutotune returns the (disabled) scheduled auto-optimizer settings.
func defaultAutotune() AutotuneConfig {
	return AutotuneConfig{
		Enabled:      false,
		Interval:     24 * time.Hour,
		Bars:         1440,
		MinTrades:    10,
		Objective:    "blend",
		MinReturnPct: 0,
	}
}

// defaultTestMode returns the training-window settings that are ON out of
// the box — see TestModeConfig's own doc comment for why a safety-first
// default makes sense here where it wouldn't for autotune's own scheduled
// job (which unsupervised-writes strategy_overrides and so stays off by
// default; test mode only ever simulates, never touching the real broker,
// so there is nothing unsafe about it being on from the start).
func defaultTestMode() TestModeConfig {
	return TestModeConfig{Enabled: true, Duration: 48 * time.Hour}
}

// defaultAI returns the (disabled) LLM advisor settings, pointed at a local
// Ollama so enabling it needs nothing but `ai.enabled: true`.
func defaultAI() AIConfig {
	return AIConfig{
		Enabled:         false,
		Provider:        "ollama",
		BaseURL:         "http://localhost:11434",
		Model:           "llama3.2",
		Timeout:         20 * time.Second,
		RefreshInterval: 15 * time.Minute,
		MaxAge:          time.Hour,
		Temperature:     0,
		MaxConcurrent:   2,
		MinConfidence:   0.15,
		MaxStrength:     1,
	}
}

// ErrValidation wraps a failure from Config.Validate, as opposed to a failure
// to read or parse the file at all. The distinction matters to a caller
// deciding what to tell the operator: a read/parse failure means the
// returned Config is essentially Default() — the file's settings were not
// applied at all — while an ErrValidation failure means the file's settings
// WERE fully parsed and merged, and are being returned (and, by callers like
// backtest.LoadConfig, still run) exactly as invalid as Validate found them
// (e.g. an inverted MA period, a stop-loss ladder over 100%). "Continuing
// with best-effort values" means two very different things in these two
// cases; see backtest.LoadConfig.
var ErrValidation = errors.New("config validation failed")

// Load reads config from the YAML file at path (if it exists), layering it over
// Default(), then applies environment-variable overrides. A missing file is not
// an error — env vars alone are enough to run.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			// Two passes, because a profile has to be applied *under* the file
			// that names it: read the profile first, seed the config with it,
			// then layer the file on top so every explicit setting still wins.
			var probe struct {
				Profile string `yaml:"profile"`
			}

			if err := yaml.Unmarshal(data, &probe); err != nil {
				return cfg, fmt.Errorf("parse config %s: %w", path, err)
			}

			if cfg, err = ApplyProfile(cfg, probe.Profile); err != nil {
				return cfg, fmt.Errorf("config %s: %w", path, err)
			}

			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config %s: %w", path, err)
			}

			// Per-symbol overrides are resolved against the base *after* it is
			// final, so each inherits the fully-merged strategy.
			if cfg.SymbolStrategy, err = resolveSymbolStrategies(data, cfg.Strategy); err != nil {
				return cfg, fmt.Errorf("config %s: %w", path, err)
			}

		case os.IsNotExist(err):
			// fall through to env-only config
		default:
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%w: %w", ErrValidation, err)
	}

	return cfg, nil
}

// envSecretPaths records, per section, which of that section's YAML leaf
// paths (the same dotted keys config.Patch's leavesOf produces) were
// populated from an environment variable during the most recent applyEnv,
// rather than from config.yaml.
//
// Patch consults this to refuse writing a value for one of these paths: the
// env var will override whatever the file says again on the next restart
// regardless, so persisting a different value there would be silently inert
// — and worse, since a settings save always persists a whole section's
// current live value (not just the one field the operator actually edited),
// saving any OTHER field in the same section would drag the env-sourced
// secret into the file for the first time, defeating the documented purpose
// of supplying it via the environment instead (see applyNotifyEnv). Rebuilt
// on every applyEnv call (via resetEnvSecretPaths) so it always reflects the
// current process environment, not a stale one from an earlier LoadConfig —
// which matters for tests, which call it repeatedly under different envs.
var (
	envSecretPathsMu sync.RWMutex
	envSecretPaths   = map[string]map[string]bool{}
)

// resetEnvSecretPaths clears section's recorded paths before applyEnv
// repopulates them.
func resetEnvSecretPaths(section string) {
	envSecretPathsMu.Lock()
	defer envSecretPathsMu.Unlock()

	envSecretPaths[section] = map[string]bool{}
}

// markEnvSecret records that section's leaf path came from the environment.
func markEnvSecret(section, path string) {
	envSecretPathsMu.Lock()
	defer envSecretPathsMu.Unlock()

	if envSecretPaths[section] == nil {
		envSecretPaths[section] = map[string]bool{}
	}

	envSecretPaths[section][path] = true
}

// EnvSourced reports whether section's leaf path is currently sourced from an
// environment variable rather than config.yaml.
func EnvSourced(section, path string) bool {
	envSecretPathsMu.RLock()
	defer envSecretPathsMu.RUnlock()

	return envSecretPaths[section][path]
}

// applyEnv layers environment-variable overrides onto the config.
//
//nolint:cyclop // one guarded assignment per env var; inherently branchy
func applyEnv(cfg *Config) {
	applyProfileEnv(cfg) // first, so every other override wins over the profile

	if v := os.Getenv("FINNHUB_API_KEY"); v != "" {
		cfg.Finnhub.APIKey = v
	}

	if v := os.Getenv("ALPACA_KEY_ID"); v != "" {
		cfg.Alpaca.KeyID = v
	}

	if v := os.Getenv("ALPACA_SECRET_KEY"); v != "" {
		cfg.Alpaca.SecretKey = v
	}

	if v := os.Getenv("ALPACA_BASE_URL"); v != "" {
		cfg.Alpaca.BaseURL = v
	}

	if v := os.Getenv("ALPACA_USE_SIM"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Alpaca.UseSim = b
		}
	}

	if v := os.Getenv("BOT_BROKER"); v != "" {
		cfg.Broker = v
	}

	if v := os.Getenv("SAXO_TOKEN"); v != "" {
		cfg.Saxo.Token = v
	}

	if v := os.Getenv("SAXO_BASE_URL"); v != "" {
		cfg.Saxo.BaseURL = v
	}

	if v := os.Getenv("T212_API_KEY"); v != "" {
		cfg.Trading212.APIKey = v
	}

	if v := os.Getenv("T212_SECRET"); v != "" {
		cfg.Trading212.Secret = v
	}

	if v := os.Getenv("T212_BASE_URL"); v != "" {
		cfg.Trading212.BaseURL = v
	}

	if v := os.Getenv("SAXO_APP_SECRET"); v != "" {
		cfg.Saxo.AppSecret = v
	}

	if v := os.Getenv("SAXO_CERT_PASSWORD"); v != "" {
		cfg.Saxo.CertPassword = v
	}

	if v := os.Getenv("BOT_SERVER_ADDR"); v != "" {
		cfg.Server.Addr = v
	}

	if v := os.Getenv("BOT_AUTH_TOKEN"); v != "" {
		cfg.Server.AuthToken = v
	}

	if v := os.Getenv("BOT_STORAGE_PATH"); v != "" {
		cfg.Storage.Path = v
	}

	if v := os.Getenv("BOT_TRADING_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Engine.TradingEnabled = b
		}
	}

	applyAIEnv(cfg)
	applyNotifyEnv(cfg)

	v := os.Getenv("BOT_WATCHLIST")
	if v == "" {
		return
	}

	var syms []string

	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(strings.ToUpper(s)); s != "" {
			syms = append(syms, s)
		}
	}

	if len(syms) > 0 {
		cfg.Watchlist = syms
	}
}

// applyAIEnv layers the BOT_AI_* overrides onto the AI advisor settings, so a
// container can point the bot at an Ollama host without editing config.yaml.
func applyAIEnv(cfg *Config) {
	resetEnvSecretPaths("ai")

	if v := os.Getenv("BOT_AI_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AI.Enabled = b
		}
	}

	if v := os.Getenv("BOT_AI_PROVIDER"); v != "" {
		cfg.AI.Provider = v
	}

	if v := os.Getenv("BOT_AI_BASE_URL"); v != "" {
		cfg.AI.BaseURL = v
	}

	if v := os.Getenv("BOT_AI_MODEL"); v != "" {
		cfg.AI.Model = v
	}

	if v := os.Getenv("BOT_AI_API_KEY"); v != "" {
		cfg.AI.APIKey = v
		markEnvSecret("ai", "api_key")
	}
}

// applyProfileEnv lets BOT_PROFILE select a profile when there is no config
// file at all. It is applied before the other env overrides so those still win.
func applyProfileEnv(cfg *Config) {
	v := strings.TrimSpace(os.Getenv("BOT_PROFILE"))
	if v == "" {
		return
	}

	out, err := ApplyProfile(*cfg, v)
	if err != nil {
		return // an unknown BOT_PROFILE is ignored; Validate reports it
	}

	out.Profile = v
	*cfg = out
}

// applyNotifyEnv layers the BOT_NOTIFY_* overrides, so webhook URLs and bot
// tokens can be supplied as secrets instead of being written into config.yaml.
//
//nolint:cyclop // one guarded assignment per sink credential; inherently branchy
func applyNotifyEnv(cfg *Config) {
	resetEnvSecretPaths("notify")

	if v := os.Getenv("BOT_NOTIFY_WEBHOOK"); v != "" {
		cfg.Notify.Webhook = v
		cfg.Notify.Enabled = true

		markEnvSecret("notify", "webhook")
	}

	if v := os.Getenv("BOT_NOTIFY_DISCORD"); v != "" {
		cfg.Notify.DiscordWebhook = v
		cfg.Notify.Enabled = true

		markEnvSecret("notify", "discord_webhook")
	}

	if v := os.Getenv("BOT_NOTIFY_TELEGRAM_TOKEN"); v != "" {
		cfg.Notify.TelegramToken = v

		markEnvSecret("notify", "telegram_token")
	}

	if v := os.Getenv("BOT_NOTIFY_TELEGRAM_CHAT"); v != "" {
		cfg.Notify.TelegramChatID = v

		markEnvSecret("notify", "telegram_chat_id")
	}

	if v := os.Getenv("BOT_NOTIFY_PUSHOVER_TOKEN"); v != "" {
		cfg.Notify.PushoverToken = v

		markEnvSecret("notify", "pushover_token")
	}

	if v := os.Getenv("BOT_NOTIFY_PUSHOVER_USER"); v != "" {
		cfg.Notify.PushoverUser = v

		markEnvSecret("notify", "pushover_user")
	}

	if v := os.Getenv("BOT_NOTIFY_APPRISE_URL"); v != "" {
		cfg.Notify.AppriseURL = v
		cfg.Notify.Enabled = true

		markEnvSecret("notify", "apprise_url")
	}

	// A sink that needs two credentials only counts once both are present.
	if cfg.Notify.TelegramToken != "" && cfg.Notify.TelegramChatID != "" {
		cfg.Notify.Enabled = true
	}

	if cfg.Notify.PushoverToken != "" && cfg.Notify.PushoverUser != "" {
		cfg.Notify.Enabled = true
	}
}

// BrokerProvider returns the effective execution venue: "sim", "alpaca" or
// "saxo". An explicit broker setting wins; otherwise it is derived from the
// legacy Alpaca settings for backward compatibility.
func (c Config) BrokerProvider() string {
	switch strings.ToLower(strings.TrimSpace(c.Broker)) {
	case "saxo":
		return "saxo"
	case "alpaca":
		return "alpaca"
	case "trading212", "t212":
		return "trading212"
	case "sim":
		return "sim"
	}

	if !c.Alpaca.UseSim && c.Alpaca.KeyID != "" {
		return "alpaca"
	}

	return "sim"
}

// ValidateForTrading checks the money-ceiling invariant risk.Manager.Authorize
// relies on: MaxOrderValue == 0 and MaxTotalInvested == 0 do not mean
// "unlimited" to the caller's eye, but that is exactly what they mean to
// Authorize's caps (a cap with maxValue <= 0 is simply skipped) — so trading
// must never run with either at zero. Config.Validate calls this at load
// time; anything that can flip trading on or change limits while it is
// already on (POST /api/trading, PUT /api/limits) must call it too, or a
// config shipped with trading_enabled: false — which never went through this
// check — can be turned on live with genuinely no per-order or total-invested
// ceiling. See CODE_REVIEW_FINDINGS.md's "fail-closed money-ceiling check
// only applies at config-load time".
func (l Limits) ValidateForTrading() error {
	// Either half satisfies the requirement — a percentage is as real a
	// ceiling as an amount, and refusing a percentage-only config would make
	// the scaling form unusable. What must not happen is BOTH being unset,
	// which is trading with no ceiling at all.
	if l.MaxOrderValue <= 0 && l.MaxOrderValuePct <= 0 {
		return errors.New(
			"limits.max_order_value or limits.max_order_value_pct must be > 0 " +
				"when trading is enabled")
	}

	if l.MaxTotalInvested <= 0 && l.MaxTotalInvestedPct <= 0 {
		return errors.New(
			"limits.max_total_invested or limits.max_total_invested_pct must be > 0 " +
				"when trading is enabled")
	}

	return nil
}

// Validate checks invariants that must hold for safe operation.
//
//nolint:cyclop // a flat list of independent sanity checks
func (c Config) Validate() error {
	// Fail closed: refuse to run with trading enabled but no money ceilings.
	if c.Engine.TradingEnabled {
		if err := c.Limits.ValidateForTrading(); err != nil {
			return err
		}

		switch c.BrokerProvider() {
		case "alpaca":
			if c.Alpaca.KeyID == "" || c.Alpaca.SecretKey == "" {
				return errors.New(
					"alpaca credentials required when broker=alpaca and trading is enabled",
				)
			}

		case "trading212":
			if c.Trading212.APIKey == "" || c.Trading212.Secret == "" {
				return errors.New(
					"trading212.api_key and trading212.secret required when broker=trading212 and trading is enabled",
				)
			}

			if c.Trading212.BaseURL == "" {
				return errors.New("trading212.base_url required when broker=trading212")
			}

		case "saxo":
			if c.Saxo.BaseURL == "" {
				return errors.New("saxo.base_url required when broker=saxo")
			}

			if c.Saxo.AuthMode == "certificate" {
				if c.Saxo.AppKey == "" || c.Saxo.UserID == "" || c.Saxo.CertPath == "" {
					return errors.New(
						"saxo certificate auth requires app_key, user_id and cert_path",
					)
				}
			} else if c.Saxo.Token == "" {
				return errors.New("saxo.token required when broker=saxo and auth_mode=token")
			}
		}
	}

	if c.Strategy.FastMA >= c.Strategy.SlowMA {
		return fmt.Errorf(
			"strategy.fast_ma (%d) must be < slow_ma (%d)",
			c.Strategy.FastMA,
			c.Strategy.SlowMA,
		)
	}

	if c.Engine.EvalInterval <= 0 {
		return errors.New("engine.eval_interval must be > 0")
	}

	if !KnownProfile(c.Profile) {
		return fmt.Errorf("unknown profile %q (available: %s)",
			c.Profile, strings.Join(Profiles, ", "))
	}

	if err := c.Exits.Validate(); err != nil {
		return err
	}

	if err := c.Engine.ScaleIn.Validate(); err != nil {
		return err
	}

	if err := c.Limits.Validate(); err != nil {
		return err
	}

	if err := c.Portfolio.Validate(); err != nil {
		return err
	}

	if err := c.validatePositionSlots(); err != nil {
		return err
	}

	if err := c.Screener.Validate(); err != nil {
		return err
	}

	if err := c.Autotune.Validate(); err != nil {
		return err
	}

	if err := c.TestMode.Validate(); err != nil {
		return err
	}

	if err := c.Notify.Validate(); err != nil {
		return err
	}

	if err := c.ML.Validate(); err != nil {
		return err
	}

	return c.AI.Validate()
}

// validatePositionSlots cross-checks the two tiers of the position-slot rule,
// which live in different sections: engine.min_positions is the coarse "spread
// the budget over at least this many positions", and
// portfolio.min_positions_per_asset_class refines it per class.
//
// The per-class counts are carved OUT of the global one — whatever they do not
// claim is what an unnamed class gets (see
// risk.PortfolioGuard.MaxPositionValue). Naming more positions per class than
// the global total therefore describes a book that cannot exist, and would
// silently leave every unnamed class with no slot while making the named ones
// smaller than the operator wrote. Only checked when the global tier is
// actually set: min_positions 0 means "no global split", under which the
// per-class counts stand on their own and nothing is being carved out of
// anything.
func (c Config) validatePositionSlots() error {
	if !c.Portfolio.Enabled || c.Engine.MinPositions <= 0 {
		return nil
	}

	var total int
	for _, n := range c.Portfolio.NormalizedAssetClassSlots() {
		total += n
	}

	if total > c.Engine.MinPositions {
		return fmt.Errorf(
			"portfolio.min_positions_per_asset_class claims %d positions between its classes, "+
				"more than engine.min_positions (%d): the per-class counts come out of the "+
				"global one, so they cannot exceed it",
			total, c.Engine.MinPositions)
	}

	return nil
}

// Validate checks the exit rules. The scale-out ladder is the part that can be
// written wrong in a way nothing else would catch.
//
// StopLossPct/TrailingStopPct/TrailingTightPct >= 1 is not just an unusual
// setting, it's a silent no-op: exits/exits.go computes stop = entry*(1-pct)
// (and the trail symmetrically), so a value >= 1 makes the stop price zero or
// negative, and Triggered gates on price > 0 — a units typo like
// stop_loss_pct: 2 (meant as "2%") loads clean today and runs every position
// with no stop loss at all. See CODE_REVIEW_FINDINGS.md's stop_loss_pct
// finding.
func (e ExitsConfig) Validate() error {
	for name, pct := range map[string]float64{
		"stop_loss_pct":      e.StopLossPct,
		"trailing_stop_pct":  e.TrailingStopPct,
		"trailing_tight_pct": e.TrailingTightPct,
	} {
		if pct < 0 || pct >= 1 {
			return fmt.Errorf("exits.%s must be a fraction in [0,1) (0 disables it), got %v", name, pct)
		}
	}

	if e.TakeProfitPct < 0 {
		return fmt.Errorf("exits.take_profit_pct must be >= 0, got %v", e.TakeProfitPct)
	}

	if e.TrailingOffsetPct < 0 {
		return fmt.Errorf("exits.trailing_offset_pct must be >= 0, got %v", e.TrailingOffsetPct)
	}

	// "The ROI ladder decays the profit target, so it can only ever exit at
	// breakeven or better" (see ROI's doc comment) is a stated invariant, not
	// just documentation: a negative entry produces a target price below
	// entry, which Levels.Triggered treats identically to a legitimate
	// take-profit — exiting at a loss under a "take-profit"/"roi" label,
	// which is what stop_loss_pct/trailing_stop_pct exist for instead. See
	// CODE_REVIEW_FINDINGS.md's "negative ROI-ladder values produce a
	// take-profit that exits at a loss".
	for key, roi := range e.ROI {
		if roi < 0 {
			return fmt.Errorf(
				"exits.roi[%q] must be >= 0, got %v — a negative ROI target would exit at a "+
					"loss under a take-profit label; use stop_loss_pct/trailing_stop_pct for that",
				key, roi,
			)
		}
	}

	var total float64

	for i, r := range e.PartialTakeProfit {
		if r.ProfitPct <= 0 {
			return fmt.Errorf("exits.partial_take_profit[%d].profit_pct must be > 0", i)
		}

		if r.SellPct <= 0 || r.SellPct > 1 {
			return fmt.Errorf(
				"exits.partial_take_profit[%d].sell_pct must be a fraction in (0,1]", i)
		}

		total += r.SellPct
	}

	// A ladder that sells more than the position is a typo, not a strategy: the
	// rungs are fractions of the original quantity, so they must sum to at most
	// one whole position.
	if total > 1.0000001 {
		return fmt.Errorf(
			"exits.partial_take_profit sells %.0f%% of the position; the rungs must sum to <= 100%%",
			total*100,
		)
	}

	return nil
}

// Validate checks the averaging-down ladder. The cap and the size limit are what
// separate it from a martingale, so both are enforced rather than advised.
func (s ScaleInConfig) Validate() error {
	if !s.Enabled {
		return nil
	}

	if s.MaxAdds <= 0 {
		return errors.New(
			"engine.scale_in.max_adds must be > 0 when scale_in is enabled: " +
				"an uncapped averaging-down ladder is a martingale")
	}

	if s.StepPct <= 0 || s.StepPct >= 1 {
		return errors.New("engine.scale_in.step_pct must be a fraction in (0,1)")
	}

	if s.SizePct <= 0 || s.SizePct > 1 {
		return errors.New(
			"engine.scale_in.size_pct must be a fraction in (0,1]: " +
				"an add larger than a normal order is martingale sizing")
	}

	return nil
}

// Validate checks the AI advisor settings. Only enforced when the advisor is
// enabled: a misconfigured-but-disabled block must not stop the bot booting.
func (a AIConfig) Validate() error {
	if !a.Enabled {
		return nil
	}

	switch strings.ToLower(strings.TrimSpace(a.Provider)) {
	case "ollama", "openai", "":
	default:
		return fmt.Errorf("ai.provider %q must be \"ollama\" or \"openai\"", a.Provider)
	}

	if strings.TrimSpace(a.Model) == "" {
		return errors.New("ai.model is required when ai.enabled is true")
	}

	if strings.TrimSpace(a.BaseURL) == "" {
		return errors.New("ai.base_url is required when ai.enabled is true")
	}

	return nil
}
