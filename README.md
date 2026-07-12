# go_finance_bot

> ## ⚠️ WARNING — USE AT YOUR OWN RISK
>
> **This software trades real money when connected to a live broker. All
> trading happens entirely at your own risk.** The authors and contributors
> accept **no responsibility or liability whatsoever for any financial
> losses**, missed profits, broker fees, tax consequences, or any other
> damages arising from the use of this software — whether caused by bugs,
> bad signals, market conditions, API failures, misconfiguration, or
> anything else.
>
> This project is provided **as is, without warranty of any kind**, for
> educational purposes. It is **not financial advice**. Automated trading
> strategies can and do lose money — past (backtested) performance does not
> predict future results. Always start with the offline simulator and a
> paper/demo account, set conservative money limits, and never trade money
> you cannot afford to lose.

A Go trading bot that tracks a few hundred ETFs/shares in near real time,
decides when to buy and sell using technical indicators, and can place
(paper or live) orders — all behind **hard, always-enforced money limits**.

## Supported brokers

| Broker | Region | Paper/demo | Live | Notes |
|--------|--------|------------|------|-------|
| **Sim** (built-in) | — | ✅ default | — | offline in-memory simulator, no account needed |
| **Alpaca** | US | ✅ paper endpoint (default) | ✅ | commission-free US stocks/ETFs |
| **Saxo Bank** | EU | ✅ simulation gateway (default) | ✅ (app approval required) | native bracket/OCO orders, modify & cancel |
| **Trading 212** | EU | ✅ demo endpoint (default) | ✅ (self-service API key) | fractional shares; no market-data stream, rate-limited |

Selected via the `broker:` config setting — see [Going live](#going-live-paper-first)
for per-broker setup. Every broker starts on its paper/demo environment by
default, and all orders pass the same [money limits](#money-limits-the-hard-requirement).

## Architecture

```
                Finnhub WS/REST  ──▶  market.DataProvider
 (or offline Mock feed)                      │ live ticks + warmup candles
                                             ▼
                                     engine.Engine
            ┌──────────────────────────┼───────────────────────────┐
            ▼                          ▼                            ▼
   per-symbol candle history   strategy.Technical            portfolio.Portfolio
   (built from ticks)          (EMA cross + RSI + MACD)      (cash, positions, MTM)
                                       │ Signal                     │ snapshot
                                       ▼                            │
                                 risk.Manager  ◀────────────────────┘
                                 (money limits: clamps or blocks every order)
                                       │ approved order
                                       ▼
                                 broker.Broker
              (Alpaca  |  Saxo  |  Trading 212  |  internal Sim)
```

The **gin HTTP API** exposes everything for tracking and control.

| Layer | Package | Purpose |
|-------|---------|---------|
| Data feed | `internal/market` | `DataProvider` interface, Finnhub (WS+REST), offline Mock; Yahoo Finance for warmup history |
| Indicators | `internal/strategy/indicators` | SMA, EMA, RSI, MACD, ATR, VWAP, Bollinger, RVOL, Volume Profile, ADX, Stochastic, Supertrend, Donchian, regression Trend |
| Strategy | `internal/strategy` | pluggable **detectors** + a combiner that compares/merges them |
| Portfolio | `internal/portfolio` | cash, positions, mark-to-market |
| **Risk** | `internal/risk` | **enforces money limits on every order** |
| Exits | `internal/engine` (`exits.go`) | stop-loss / trailing-stop / take-profit |
| Broker | `internal/broker` | `Broker` interface, Alpaca (US) + Saxo (EU) + Trading 212 (EU) + Sim |
| Engine | `internal/engine` | ties feed → strategy → risk → broker |
| Persistence | `internal/storage` | SQLite (pure-Go) trade log + state |
| Backtest | `internal/backtest`, `cmd/backtest` | replay history, report metrics |
| API | `internal/api` | gin REST server |

## Detectors & combiner

The strategy is a set of **detectors** (one indicator each) merged by a combiner.
Configure which to run and how to merge them:

```yaml
strategy:
  detectors: ["rsi", "macd", "bollinger", "vwap", "atr", "rvol", "volume_profile",
              "trend", "adx", "stochastic", "supertrend", "donchian"]
  combine: "unanimous"       # BUY merge: majority | unanimous | weighted
  combine_sell: "majority"   # SELL merge (empty = same as combine) — be strict to
                             # enter, quick to exit
```

**Asymmetric entry/exit:** the BUY and SELL sides are configured independently —
`detectors`/`combine`/`min_strength` drive buys and `detectors_sell`/
`combine_sell`/`min_strength_sell` drive sells (each `*_sell` empty/−1 = inherit
the buy setting). So you can enter strictly (all detectors agree, high
conviction) and exit readily (a majority of different detectors at a lower bar).
Protective exits — stop-loss / trailing / take-profit — sell on price alone,
independent of all this.

**Don't churn (holding & confirmation):**

```yaml
engine:
  keep_interval: 2h      # min hold before a *strategy* sell (exits still fire). 0 = off
  confirm_buy: 2         # need N consecutive evaluations agreeing before buying
  confirm_sell: 2        # ditto for selling — a single down-tick won't dump a position
```

`confirm_*` require the signal to persist for N×`eval_interval` before acting;
`keep_interval` blocks strategy sells for a minimum holding time after entry
(protective exits bypass it). All three are editable live in the UI
("Holding & confirmation") or via `PUT /api/behavior`.

| Detector | Signal logic |
|----------|--------------|
| `ema_cross` | fast EMA vs slow EMA (trend) |
| `rsi` | `rsi_mode: midline` → graded mean-reversion vote around 50 (always participates); `extremes` → only votes at oversold/overbought |
| `macd` | histogram sign (momentum) |
| `bollinger` | price below lower band → buy, above upper → sell (mean reversion) |
| `vwap` | price above/below VWAP (trend bias) |
| `atr` | move > `atr_mult`×ATR vs prior close (volatility breakout) |
| `rvol` | high relative volume confirms the latest move |
| `volume_profile` | price outside the value area vs the point of control |
| `trend` | least-squares regression slope over up to a week of bars — follows the prevailing direction; with `trend_gate: true` it also blocks buys in a downtrend / strategy sells in an uptrend |
| `adx` | Average Directional Index: only votes when ADX ≥ `adx_threshold` (a real trend), then +DI vs −DI sets the direction |
| `stochastic` | %K at/below `stoch_oversold` → buy, at/above `stoch_overbought` → sell (mean reversion) |
| `supertrend` | price above/below the ATR-based Supertrend band (trend follow); `supertrend_mult` sets flip sensitivity |
| `donchian` | close breaking above the `donchian_period` highest high → buy, below the lowest low → sell (breakout) |

> Volume caveat: the live engine builds its bars (size `engine.bar_interval`,
> default 1m — larger bars trade on coarser, less noisy candles) from the quote
> stream, which has no trade sizes, so a live bar's "volume" is its **tick count**. The volume-driven
> detectors (`vwap`, `rvol`, `volume_profile`) therefore run on tick counts live
> but on real share volume in backtests (CSV/Yahoo history) — expect their live
> behaviour to differ from backtested results.

**Comparison:** with more than one detector, each is evaluated and the signal
`reason` shows every detector's vote, e.g.
`rsi:BUY(0.60), macd:HOLD(0.00), vwap:SELL(0.30) => BUY by majority`.
Merge modes: **majority** (more votes wins), **unanimous** (all opinionated
detectors must agree), **weighted** (sign of the strength-weighted net).

**Per-detector weights:** `strategy.weights` multiplies each detector's strength
when combining (missing entries default to 1.0), so a `weighted` merge can trust
some signals more than others; a zero or negative weight mutes a detector's
strength contribution without removing its vote:

```yaml
strategy:
  weights:
    trend: 2.0    # count the regression trend double
    rsi: 0.5
```

**Tuning for fewer losses:** two knobs cut over-trading (the main cause of
death-by-a-thousand-losses on noisy data):

- `strategy.min_strength` (0..1) — minimum combined confidence to act; weaker
  signals become HOLD. Higher → fewer, higher-conviction trades. On the
  synthetic sample, raising it from 0 → 0.30 took win rate **43% → 100%**,
  Sharpe **0.4 → 2.5**, and losses **4 → 0**.
- `engine.allow_pyramiding: false` (default) — at most one entry per symbol
  until it's exited, so the bot can't keep adding to a position every bar.

> Note: a slightly-upward random walk is *designed* to reward buy-and-hold, so
> beating the benchmark on synthetic data isn't the goal — minimising losses and
> trade quality is. Validate real edge by backtesting **real** CSV history
> (`-csv`), especially across choppy/down markets.

## Trading costs

A commission model folds the true cost of trading into order sizing, the money
limits and backtest P&L:

```yaml
costs:
  commission_pct: 0.001   # 0.1% of notional
  commission_flat: 1.0    # flat fee per order
  commission_min: 2.0     # minimum per order
```

`commission = max(min, flat + pct × notional)`. Buys reserve cash for the fee
(cash-reserve and daily-spend limits include it), the simulated broker debits it
on every fill, and backtests report returns **net of costs**.

## Protective exits

Before any strategy signal, every open position is checked against
`internal/engine/exits.go`. A triggered exit always wins (force-sells):

- `stop_loss_pct` — sell when price falls this far below entry
- `take_profit_pct` — sell when price rises this far above entry
- `trailing_stop_pct` — sell when price falls this far below the peak since entry

Exits respect the trading toggle (recorded as dry-run when trading is off) and
still pass through the risk manager.

## Persistence

With `storage.driver: sqlite`, trades, the watchlist, the rolling daily spend
and **open positions** are written to a pure-Go SQLite file (`finance_bot.db`) —
no cgo, inspectable with any SQLite tool. On startup the bot restores recent
trades, the watchlist, today's spend and **what it bought but hasn't sold yet**.
Set `driver: none` for in-memory-only operation.

**Open positions** are persisted **immediately** on every buy/sell (not just at
shutdown): a buy writes the symbol, quantity, entry price/time and trailing-stop
high-water mark; a closing sell deletes the row. So after a crash or restart the
bot still knows exactly what is open — including the trailing-stop peak — even
for the in-memory simulated broker. View them at `GET /api/open`.

## Backtesting

Replay history through the **same** strategy, risk limits, exits and simulated
broker used live, then get a metrics report:

```bash
# synthetic data (offline, no setup)
go run ./cmd/backtest -generate AAPL,MSFT,NVDA -bars 500 -cash 50000

# your own CSV: columns symbol,time,open,high,low,close,volume
go run ./cmd/backtest -csv data/aapl.csv

# full machine-readable output (includes every trade)
go run ./cmd/backtest -csv data/aapl.csv -json
```

**Save & reuse a dataset** — generate (or load) once, then replay the *exact same*
candles across every backtest/optimize/sensitivity run:

```bash
go run ./cmd/backtest -generate AAPL,MSFT,NVDA -bars 5000 -save data/set.csv
go run ./cmd/backtest -csv data/set.csv                 # identical data, reused
go run ./cmd/optimize -csv data/set.csv -random 5000    # same data for tuning
```

`-save` writes the standard `symbol,time,open,high,low,close,volume` CSV that
`-csv` reads, so a fixed dataset keeps config comparisons apples-to-apples.

Reported: total return, equal-weighted buy & hold benchmark, max drawdown,
(scaled) Sharpe, trade count, and win rate. Strategy params, money limits and
exit rules are read from `config.yaml`. Wins/losses are counted **net of
round-trip commission** (consistent with the net-of-costs returns), and the
engine's churn damping is modelled too: `confirm_buy`/`confirm_sell` require
the signal to repeat on N consecutive **bars** (the backtest analogue of
evaluation ticks) and `keep_interval` blocks strategy sells until a position
has been held that long — so backtest and optimizer results reflect the same
behaviour settings the live engine trades with.

## Parameter optimizer

`cmd/optimize` grid-searches detector sets, combine mode, `min_strength` and the
exit levels against the same data, ranks every combination, and prints a
leaderboard plus the best config as paste-ready YAML:

```bash
go run ./cmd/optimize -generate AAPL,MSFT,NVDA -bars 500 -rank blend
go run ./cmd/optimize -csv data.csv -rank winrate -min-trades 10 -top 20
# random search ALSO tunes indicator periods (MA/RSI/MACD/BB/ATR/RVOL/VWAP/VP/
# ADX/Stochastic/Supertrend/Donchian):
go run ./cmd/optimize -csv data.csv -random 5000 -rank return -seed 1
```

Two modes:
- **Grid** (default) — exhaustively searches the structural axes (detector set,
  combine, `min_strength`, deployment, exits).
- **Random** (`-random N`) — samples N configurations across the *full* space
  **including every indicator parameter** (fast/slow MA, RSI period+thresholds,
  MACD, Bollinger, VWAP, ATR, RVOL, Volume-Profile, ADX period+threshold,
  Stochastic %K/%D+levels, Supertrend period+multiplier, Donchian period). A
  full grid over the
  indicator space would be millions of combos; random search covers it in N
  samples (reproducible via `-seed`). The best config is printed with all tuned
  periods, ready to paste.

Ranking (`-rank`): `blend` (return + win-rate − drawdown, default), `return`
(most earnings), `winrate` (most wins), or `sharpe`. `-min-trades` drops configs
with too few trades (so a lucky 1-trade run can't top the board); identical
outcomes are de-duplicated so the board shows genuinely distinct configs.

The grid also searches **capital deployment** (order size as a fraction of cash)
— the dominant driver of total return. By default the optimizer **relaxes the
money limits** so deployment isn't capped while searching (pass `-respect-limits`
to keep your config's caps). The best-config output shows the `order_size_usd`
and the limits needed to realise it — apply your own safety limits before going
live, knowing tighter limits reduce return proportionally.

### Sensitivity (one-factor-at-a-time)

To *see* each parameter being tested in isolation — change one, compare, move to
the next — use `-sensitivity`:

```bash
go run ./cmd/optimize -config config.yaml -generate AAPL,MSFT,NVDA -sensitivity -deploy 0.5
```

From a baseline config it varies **one** parameter across its candidate values
(holding the rest fixed) and prints the result + `Δret` (return change vs
baseline) for each. This proves many distinct configs were tested and shows
which knobs actually matter — e.g. parameters feeding a detector that isn't in
your `detectors` list show `Δret +0.00` (no effect), and exits that never
trigger show no change.

**Performance:** the grid runs ~1300 backtests, so on large datasets it takes a
while; a live `progress N/total` line is printed to stderr so you can see it
advancing (it is not hung). Each backtest bounds its per-bar indicator lookback
(matching the live engine), so runtime is linear in bars — but for fast
iteration prefer `-random 300`-ish and a few thousand bars over a giant grid on
50k+ bars.

> Optimise on **real** history and hold back a period for out-of-sample
> checking — grid-search on one dataset will happily overfit.

## Money limits (the hard requirement)

Every order — automated **and** manual — passes through `risk.Manager.Authorize`,
which clamps order size down to fit and **fails closed** (rejects) if a limit
has no headroom:

- `max_order_value` — notional cap per single order
- `max_per_position` — market-value cap per symbol
- `max_total_invested` — cap on total invested across all positions
- `max_daily_spend` — rolling per-day buy-notional cap
- `cash_reserve` — cash that must stay uninvested
- `max_open_positions` — cap on distinct symbols held

The bot refuses to start with trading enabled unless `max_order_value` and
`max_total_invested` are set.

**Spreading capital across positions:** set `engine.min_positions: N` to cap each
buy at `max_total_invested / N`, so the budget always supports at least N
concurrent holdings instead of one large order. `0` uses the fixed
`order_size_usd`. Editable live in the UI ("Position sizing") or via
`PUT /api/sizing`.

## Automated-trading safety (Saxo live-cert mapping)

Controls that satisfy Saxo's live-application checklist:

| Requirement | Where |
|-------------|-------|
| Throttle rapid orders | Saxo adapter paces orders to `safety.min_order_interval` (~1/sec, Saxo's session limit) |
| Stop if trades/time exceeds a limit | engine **circuit breaker**: `safety.max_orders_per_min` trips and **halts** automated trading |
| Prevent invalid orders | reject wrong side, negative/NaN/Inf qty, bad limit price (broker) + money limits clamp oversized orders (risk) |
| Handle rejections gracefully | broker errors are caught, recorded as `error` trades, and never crash the engine |
| Final confirmation / dry run | `engine.trading_enabled: false` computes and shows signals without placing orders |
| Daily review of activity | every decision persisted to SQLite + `GET /api/trades` |
| Win/loss performance tracking | realized P&L + win/loss recorded on every closing sell, with the entry/exit detector snapshots preserved for later analysis — `GET /api/stats` and the "Trade performance" panel in the web UI |
| Order types & combinations | native **bracket** (entry + stop-loss + take-profit), **modify** and **cancel** via the `/api/orders/*` endpoints (Saxo `AdvancedBroker`) |

The automated engine places **day market orders** and manages exits itself, but
the API also exposes **native Saxo bracket/OCO orders with modify & cancel**, so
the multi-leg order-type scenarios can be exercised directly. Bracket entries
still pass through the money-limit risk manager and order-rate guard.

## Quick start

```bash
go run ./cmd/bot                         # runs offline (mock feed + sim broker)
# optional: copy data/config.example.yaml to data/config.yaml to customise;
# defaults run without one
```

### Docker

A multi-stage [Dockerfile](Dockerfile) compiles a static, CGO-free binary into a
small Alpine image; [docker-compose.yml](docker-compose.yml) builds and runs it:

```bash
docker compose up --build -d        # build + start
docker compose logs -f              # follow logs
curl localhost:8080/health
docker compose down                 # stop
```

- A **single mounted directory** `./data` holds *both* `config.yaml` and the
  SQLite DB — edit `data/config.yaml` on the host; the DB persists in the same
  dir, so trades and **open positions survive container restarts**. Nothing is
  baked into the image.
- A `/health` healthcheck and `restart: unless-stopped` keep it running.

Config lives at **`data/config.yaml`** (the default `-config` path); the DB
location comes from `storage.path` (or `BOT_STORAGE_PATH`) — `data/finance_bot.db`
in the shipped config; without any config file the built-in default is
`./finance_bot.db`. The same layout is used for local runs.

Out of the box it runs **fully offline**: a synthetic price feed and an
in-memory simulated broker, with `trading_enabled: false` (dry-run — signals are
computed and shown but no orders are placed).

### Going live (paper first!)

1. Get a free [Finnhub](https://finnhub.io) API key → set `finnhub.api_key`
   (or `FINNHUB_API_KEY`).
2. Pick a broker via `broker:` and provide its credentials (below).
3. Flip `engine.trading_enabled: true` only when you trust the signals.

The `broker` setting selects the execution venue — the `Broker` interface means
the rest of the bot is unchanged:

| `broker` | Venue | Setup |
|----------|-------|-------|
| `sim` | offline simulator | nothing — uses `alpaca.sim_cash` as starting balance |
| `alpaca` | Alpaca (US) | set `alpaca.key_id`/`secret_key`, `base_url` (paper endpoint by default) |
| `saxo` | **Saxo Bank OpenAPI (EU)** | set `saxo.token` (or `SAXO_TOKEN`); `base_url` defaults to the simulation gateway |
| `trading212` | **Trading 212 (EU)** | set `trading212.api_key` + `trading212.secret` (or `T212_API_KEY`/`T212_SECRET`); `base_url` defaults to the demo endpoint |

#### Trading 212 (EU, no approval)

1. In the Trading 212 app → **Settings → API (Beta)**, generate an **API key +
   secret** (self-service, **no app-approval gate** — unlike Saxo live; available
   for Invest and Stocks ISA accounts, not CFD/SIPP). The secret is shown **only
   once** — store it. Set `broker: trading212`, `trading212.api_key` +
   `trading212.secret` (or `T212_API_KEY`/`T212_SECRET`). They are sent as HTTP
   Basic auth (`Authorization: Basic base64(key:secret)`).
2. Starts on the **demo** endpoint (`https://demo.trading212.com/api/v0`); switch
   `base_url` to `https://live.trading212.com/api/v0` for real money. The key is
   tied to its environment, so the base URL must match the key.
3. The adapter resolves plain symbols (`AAPL`) to Trading 212 tickers
   (`AAPL_US_EQ`) via instrument metadata, supports fractional shares, and paces
   requests to respect the API's strict per-endpoint rate limits.
4. Order capabilities: market orders, plus **list working orders** (`GET
   /api/orders/open`) and **cancel** (`DELETE /api/orders/:id`). It has no native
   bracket/OCO or order-modify, so those `/api/orders/*` endpoints return a clear
   "not supported" for Trading 212 (use Saxo for native brackets).
5. **Symbol mapping**: watchlist symbols are resolved to Trading 212 tickers via
   the instrument metadata, handling exchange suffixes and ISINs — e.g.
   `LXS.DE → LXSd_EQ` (German listing), `SAP.DE → SAPd_EQ` (the German SAP, not
   Canadian "Saputo"), `AAPL → AAPL_US_EQ`. When an instrument has several
   currency listings, `trading212.preferred_currency` (default `EUR`) picks one.
   For instruments not resolvable by ticker, map them to an ISIN:
   ```yaml
   trading212:
     preferred_currency: "EUR"
     isins:
       CSPXJ.SW: "IE00B52MJY50"   # -> the EUR listing
       UNPRF:    "GB00BVZK7T90"
   ```
   Check what your watchlist can trade with `GET /api/orderable`. Note EU retail
   accounts can't buy US-domiciled ETFs (e.g. `SPY`, `VOO`) on Trading 212 — they
   show as not orderable regardless.

> Trading 212's API has **no market-data stream** and is heavily rate-limited, so
> keep Finnhub for near-real-time tracking and use Trading 212 only for execution.
> The adapter is built to the documented API and unit-tested for request shape,
> but validate on the demo endpoint with your own key before live use.

#### Saxo (EU)

1. Sign in at [developer.saxo](https://www.developer.saxo) and copy a **24-hour
   simulation token** → set `saxo.token` (or `SAXO_TOKEN`), `broker: saxo`.
2. On start the bot resolves your account via `/port/v1/accounts/me`, maps each
   ticker to a Saxo **Uic** via instrument search (`asset_types: "Stock,Etf"`),
   and submits day market orders. Saxo trades **whole shares**, so fractional
   sizes are floored — set `order_size_usd` accordingly.
3. For live trading, swap `saxo.base_url` to
   `https://gateway.saxobank.com/openapi`. **The token environment and the
   gateway must match** — a SIM token on the live gateway (or vice versa) returns
   `401`.

**Auth modes** (`saxo.auth_mode`):

- **`token`** (default) — a static bearer token. Fine for the 24-hour SIM token,
  but it must be replaced by hand when it expires.
- **`certificate`** — unattended live auth with **no daily re-paste**. The bot
  signs a JWT assertion (RS256) with your Saxo certificate and exchanges it for
  an access token via the `personal-jwt` grant, then auto-refreshes in the
  background. Requires an **approved live application + certificate** from Saxo
  (request via [developer.saxo](https://www.developer.saxo) → link your live
  account → download the certificate from MyAccount). Configure:

  ```yaml
  saxo:
    auth_mode: "certificate"
    app_key:   "<live client_id>"
    app_secret: "<live client_secret>"   # or SAXO_APP_SECRET
    app_url:   "<registered app URL>"     # JWT "spurl" claim
    user_id:   "<user the cert is for>"   # JWT "sub" claim
    cert_path: "saxo.p12"                 # .p12/.pfx or PEM cert+key
    cert_password: "<pw>"                 # or SAXO_CERT_PASSWORD
    base_url:  "https://gateway.saxobank.com/openapi"
  ```

> The 24-hour SIM token expires daily and the adapter surfaces a clear `401`
> (now naming the token/gateway mismatch). Certificate auth avoids this.
> The crypto path (cert loading, JWT signing) is unit-tested, but the live token
> exchange and order placement should be validated against your own Saxo
> credentials before relying on them.

> Live money (any broker): the money limits still apply to every order. Prove
> the strategy in paper/sim first.

## Web UI

A single self-contained page is served at **`http://localhost:8080/`** (embedded
in the binary, no build step). It covers:

- **Watchlist** — add/remove symbols.
- **Signals — all detectors** — every detector's current call per symbol (even
  ones not in the active set), so you can see what each would do; the active set
  is outlined in blue.
- **Strategy** editor — edit detectors, combine mode, `min_strength` and every
  indicator period and **apply live** (no restart); each field has an inline
  explanation.
- **Protective exits** editor — stop-loss / trailing / take-profit, applied live.
- **Open positions** — with a **Sell** button per holding.
- **Start/Stop trading** toggle.
- **API error log** — recent failures from Finnhub / Saxo / Alpaca / Yahoo /
  Trading 212, colour-coded by source.

## API

All `/api` routes are **unauthenticated by default**. Set `server.auth_token`
(or `BOT_AUTH_TOKEN`) to require a token on every request — sent as
`Authorization: Bearer <token>`, or as a `?token=` query parameter for the SSE
stream. Do this before exposing the port beyond localhost: the API can place
orders and toggle live trading.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | liveness |
| GET | `/api/status` | engine status, broker, spend today |
| GET | `/api/dashboard` | combined snapshot (status, positions, open, signals, metrics) for the web UI |
| GET | `/api/metrics` | runtime engine metrics |
| GET | `/api/events` | live SSE stream of signals/trades (feeds the UI activity panel) |
| GET | `/api/quotes` | latest price per symbol |
| GET | `/api/positions` | portfolio snapshot (cash, holdings, equity) |
| GET | `/api/open` | durable open-position records (bought, not yet sold; survives restart) |
| GET | `/api/orderable` | per-watchlist symbol: is it tradeable on the broker, and to which ticker (Trading 212) |
| GET | `/api/detectors` | every detector's signal per symbol (incl. inactive ones) |
| GET / PUT | `/api/strategy` | read / live-edit the strategy (detectors, combine, periods) |
| GET / PUT | `/api/exits` | read / live-edit the stop-loss / trailing / take-profit |
| GET / PUT | `/api/sizing` | read / live-edit position sizing (`order_size_usd`, `min_positions`) |
| GET / PUT | `/api/behavior` | read / live-edit holding & confirmation (`keep_interval`, `confirm_buy/sell`) |
| GET | `/api/errors` | recent API errors (Finnhub/Saxo/Alpaca/Yahoo/Trading 212) |
| GET | `/api/signals` | latest buy/sell/hold signal per symbol |
| GET | `/api/trades` | recent trade decisions (filled/dry-run/blocked), incl. realized P&L/win-loss and the detector snapshot at decision time on closing sells |
| GET | `/api/stats` | win/loss performance over a period — `?period=week\|month\|all` (default `week`): win rate, total/avg P&L, per-symbol breakdown |
| GET | `/api/watchlist` | tracked symbols |
| POST | `/api/watchlist` | `{"symbols":["AAPL","MSFT"]}` add symbols |
| DELETE | `/api/watchlist/:symbol` | stop tracking a symbol |
| GET | `/api/limits` | current money limits + spend today |
| PUT | `/api/limits` | replace money limits |
| POST | `/api/trading` | `{"enabled":true}` toggle live trading |
| POST | `/api/orders` | `{"symbol":"AAPL","side":"buy","qty":2}` manual market order (risk-checked) |
| POST | `/api/orders/bracket` | native entry + stop-loss + take-profit (Saxo). `{"symbol":"AAPL","side":"buy","qty":10,"entry_type":"limit","entry_price":150,"stop_loss":140,"take_profit":170}` |
| GET | `/api/orders/open` | list working (unfilled) orders |
| PATCH | `/api/orders/:id` | modify a working order: `{"symbol":"AAPL","side":"sell","qty":10,"price":160,"type":"limit"}` |
| DELETE | `/api/orders/:id` | cancel order(s); `:id` may be comma-separated |

Example:

```bash
curl localhost:8080/api/signals
curl -X POST localhost:8080/api/watchlist -d '{"symbols":["TSLA","AMD"]}'
curl -X PUT  localhost:8080/api/limits -d '{"max_order_value":300,"max_total_invested":5000,"max_per_position":800,"max_daily_spend":1500,"cash_reserve":200,"max_open_positions":40}'
curl -X POST localhost:8080/api/trading -d '{"enabled":true}'
```

## Notes & next steps

- Finnhub's free tier may not serve historical `/stock/candle` data; the engine
  then warms up purely from the live tick stream (give it time to fill bars).
- The strategy is intentionally simple and **not** financial advice. Backtest and
  tune before risking real money.
- Natural extensions: persistence (positions/trades), backtesting harness,
  stop-loss/trailing-stop exits, and per-symbol strategy parameters.
