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
	"time"

	"github.com/goccy/go-yaml"
)

// Config is the full runtime configuration for the bot.
type Config struct {
	// Server is the gin HTTP API listen address, e.g. ":8080".
	Server ServerConfig `yaml:"server"`

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

	// Strategy holds technical-indicator parameters.
	Strategy StrategyConfig `yaml:"strategy"`

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
}

// CostsConfig models per-order trading commission. The commission for a trade
// of the given notional is: max(commission_min, commission_flat + pct*notional).
type CostsConfig struct {
	CommissionPct  float64 `yaml:"commission_pct"`  // fraction of notional, e.g. 0.001 = 0.1%
	CommissionFlat float64 `yaml:"commission_flat"` // flat fee added per order
	CommissionMin  float64 `yaml:"commission_min"`  // minimum commission per order
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
	Enabled         bool    `yaml:"enabled"`
	StopLossPct     float64 `yaml:"stop_loss_pct"`     // sell if price falls this far below entry
	TrailingStopPct float64 `yaml:"trailing_stop_pct"` // sell if price falls this far below peak-since-entry
	TakeProfitPct   float64 `yaml:"take_profit_pct"`   // sell if price rises this far above entry
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
	// MaxTotalInvested caps the total market value of all open positions.
	MaxTotalInvested float64 `yaml:"max_total_invested"`
	// MaxPerPosition caps market value held in any single symbol.
	MaxPerPosition float64 `yaml:"max_per_position"`
	// MaxOrderValue caps the notional value of a single order.
	MaxOrderValue float64 `yaml:"max_order_value"`
	// MaxDailySpend caps cumulative buy notional within a rolling trading day.
	MaxDailySpend float64 `yaml:"max_daily_spend"`
	// CashReserve is a floor of cash that must remain uninvested.
	CashReserve float64 `yaml:"cash_reserve"`
	// MaxOpenPositions caps the number of distinct symbols held at once.
	MaxOpenPositions int `yaml:"max_open_positions"`
}

// StrategyConfig holds the detector selection, the combine mode and all
// per-indicator parameters.
type StrategyConfig struct {
	// Detectors lists which detectors to run for BUY decisions. Empty =>
	// ema_cross, rsi, macd. Available: ema_cross, rsi, macd, bollinger, vwap,
	// atr, rvol, volume_profile, trend, adx, stochastic, supertrend, donchian.
	Detectors []string `yaml:"detectors"`
	// DetectorsSell is the detector set for SELL decisions; empty => same as
	// Detectors. Lets you use different indicators to exit than to enter.
	DetectorsSell []string `yaml:"detectors_sell"`
	// Combine merges detectors for BUY decisions: "majority", "unanimous" or
	// "weighted".
	Combine string `yaml:"combine"`
	// CombineSell is the merge mode for SELL decisions; empty => same as Combine.
	// Lets you be strict to enter and lenient to exit (e.g. unanimous buy,
	// majority sell).
	CombineSell string `yaml:"combine_sell"`
	// MinStrength is the minimum combined confidence (0..1) required to BUY;
	// weaker signals are treated as HOLD. Filters noise/over-trading.
	MinStrength float64 `yaml:"min_strength"`
	// MinStrengthSell is the minimum confidence required to SELL. A negative
	// value (the default) means "inherit MinStrength"; set 0..1 to let exits
	// trigger at a lower bar than entries.
	MinStrengthSell float64 `yaml:"min_strength_sell"`

	// Moving averages / RSI / MACD.
	FastMA        int     `yaml:"fast_ma"`
	SlowMA        int     `yaml:"slow_ma"`
	RSIPeriod     int     `yaml:"rsi_period"`
	RSIOverbought float64 `yaml:"rsi_overbought"`
	RSIOversold   float64 `yaml:"rsi_oversold"`
	// RSIMode: "midline" (graded mean-reversion vote around 50, always
	// participates) or "extremes" (only votes at oversold/overbought).
	RSIMode    string `yaml:"rsi_mode"`
	MACDFast   int    `yaml:"macd_fast"`
	MACDSlow   int    `yaml:"macd_slow"`
	MACDSignal int    `yaml:"macd_signal"`

	// Bollinger Bands.
	BBPeriod int     `yaml:"bb_period"`
	BBStdDev float64 `yaml:"bb_stddev"`
	// VWAP.
	VWAPPeriod int `yaml:"vwap_period"`
	// ATR breakout.
	ATRPeriod int     `yaml:"atr_period"`
	ATRMult   float64 `yaml:"atr_mult"`
	// Relative volume.
	RVOLPeriod    int     `yaml:"rvol_period"`
	RVOLThreshold float64 `yaml:"rvol_threshold"`
	// Volume profile.
	VPWindow       int     `yaml:"vp_window"`
	VPBuckets      int     `yaml:"vp_buckets"`
	VPValueAreaPct float64 `yaml:"vp_value_area_pct"`

	// ADX (Average Directional Index): trend-strength + direction.
	ADXPeriod    int     `yaml:"adx_period"`
	ADXThreshold float64 `yaml:"adx_threshold"` // min ADX to vote a trend (e.g. 25)
	// Stochastic oscillator (mean reversion).
	StochKPeriod    int     `yaml:"stoch_k_period"`
	StochDPeriod    int     `yaml:"stoch_d_period"`
	StochOverbought float64 `yaml:"stoch_overbought"` // e.g. 80
	StochOversold   float64 `yaml:"stoch_oversold"`   // e.g. 20
	// Supertrend (ATR-based trend follow).
	SupertrendPeriod int     `yaml:"supertrend_period"`
	SupertrendMult   float64 `yaml:"supertrend_mult"`
	// Donchian channel breakout.
	DonchianPeriod int `yaml:"donchian_period"`

	// Weights assigns a per-detector multiplier (by canonical name, e.g.
	// "trend": 2.0) applied to that detector's strength when combining. Missing
	// entries default to 1.0. Lets a "weighted" combine favour some signals, and
	// scales every mode's reported strength. A zero or negative weight mutes a
	// detector's strength contribution without removing its vote.
	Weights map[string]float64 `yaml:"weights"`

	// Trend: a linear-regression slope over a long lookback (up to a week of
	// 1-minute bars) used to read the prevailing direction rather than reacting
	// to the last bar or two.
	//
	// TrendPeriod is the lookback in bars (1-minute). The detector uses all
	// available bars up to this, so it works before a full week has accrued.
	TrendPeriod int `yaml:"trend_period"`
	// TrendThreshold is the minimum absolute fractional change implied by the
	// fitted line across the window for it to count as a trend (deadband). E.g.
	// 0.01 = the line must move at least 1% end-to-end. Smaller values are
	// treated as "flat".
	TrendThreshold float64 `yaml:"trend_threshold"`
	// TrendGate, when true, makes the trend a directional filter: an up-trend
	// blocks strategy SELLs and a down-trend blocks BUYs (a flat trend gates
	// nothing). Protective exits still fire regardless. Independent of whether
	// "trend" is in the Detectors set.
	TrendGate bool `yaml:"trend_gate"`
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
	// OrderSizeUSD is the default notional sizing for a buy signal, before
	// risk limits clamp it down. Used when MinPositions is 0.
	OrderSizeUSD float64 `yaml:"order_size_usd"`
	// MinPositions, when > 0, caps each buy at max_total_invested / MinPositions
	// so the budget always supports at least this many concurrent positions
	// (spread capital across multiple holdings instead of one big order).
	MinPositions int `yaml:"min_positions"`
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
		},
		Engine: EngineConfig{
			EvalInterval:      30 * time.Second,
			BarInterval:       time.Minute,
			TradingEnabled:    false,
			OrderSizeUSD:      250,
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
	}
}

// Load reads config from the YAML file at path (if it exists), layering it over
// Default(), then applies environment-variable overrides. A missing file is not
// an error — env vars alone are enough to run.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config %s: %w", path, err)
			}

		case os.IsNotExist(err):
			// fall through to env-only config
		default:
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// applyEnv layers environment-variable overrides onto the config.
//
//nolint:cyclop // one guarded assignment per env var; inherently branchy
func applyEnv(cfg *Config) {
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

// Validate checks invariants that must hold for safe operation.
//
//nolint:cyclop // a flat list of independent sanity checks
func (c Config) Validate() error {
	// Fail closed: refuse to run with trading enabled but no money ceilings.
	if c.Engine.TradingEnabled {
		if c.Limits.MaxOrderValue <= 0 {
			return errors.New("limits.max_order_value must be > 0 when trading is enabled")
		}

		if c.Limits.MaxTotalInvested <= 0 {
			return errors.New("limits.max_total_invested must be > 0 when trading is enabled")
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

	return nil
}
