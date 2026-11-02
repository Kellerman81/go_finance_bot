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
(paper or live) orders — all behind **hard, always-enforced m
oney limits**.

## Contents

**Getting started**
- [Quick start](#quick-start)
  - [**The settings that actually matter**](#the-settings-that-actually-matter)
- [Supported brokers](#supported-brokers)
- [Architecture](#architecture)

**Configuring the bot**
- [Settings and config.yaml](#settings-and-configyaml)
- [Profiles](#profiles)
- [Web server and API access](#web-server-and-api-access)
- [Market data feed](#market-data-feed)
- [Crypto data](#crypto-data)
- [Bars](#bars)
- [Symbols and broker tickers](#symbols-and-broker-tickers)
- [Finding symbols](#finding-symbols)
- [Screener (automatic universe selection)](#screener-automatic-universe-selection)
- [Detectors & combiner](#detectors--combiner)
  - [The detectors](#the-detectors)
    - [`ema_cross`](#ema_cross--ema-crossover-trend)
    - [`rsi`](#rsi--relative-strength-index-mean-reversion)
    - [`macd`](#macd--moving-average-convergencedivergence-momentum)
    - [`bollinger`](#bollinger--bollinger-bands-mean-reversion)
    - [`stochastic`](#stochastic--stochastic-oscillator-mean-reversion)
    - [`trend`](#trend--least-squares-regression-slope-trend)
    - [`supertrend`](#supertrend--atr-banded-trend-follow)
    - [`adx`](#adx--average-directional-index-trend-strength-filter)
    - [`donchian`](#donchian--channel-breakout)
    - [`atr`](#atr--volatility-breakout)
    - [`vwap`](#vwap--volume-weighted-average-price-volume)
    - [`rvol`](#rvol--relative-volume-volume-confirmation)
    - [`volume_profile`](#volume_profile--value-area-and-point-of-control-volume)
    - [`ai`](#ai--llm-advisor)
    - [`ml`](#ml--learned-classifier)
- [Per-symbol strategy parameters](#per-symbol-strategy-parameters)
- [Multi-timeframe confirmation](#multi-timeframe-confirmation)
- [AI advisor (the `ai` detector)](#ai-advisor-the-ai-detector)
- [ML detector (learned signal)](#ml-detector-learned-signal)
- [Money limits (the hard requirement)](#money-limits-the-hard-requirement)
- [Position sizing](#position-sizing)
- [Portfolio limits (concentration)](#portfolio-limits-concentration)
- [Entries](#entries)
- [Order execution](#order-execution)
- [Protective exits](#protective-exits)
- [Protections](#protections)
- [Trading costs](#trading-costs)
- [Autotune (unsupervised re-optimisation)](#autotune-unsupervised-re-optimisation)
- [Test mode (simulated trading over the real feed)](#test-mode-simulated-trading-over-the-real-feed)
- [Notifications](#notifications)
- [Persistence](#persistence)
- [Order rate limiting and the circuit breaker](#order-rate-limiting-and-the-circuit-breaker)
- [Automated-trading safety (Saxo live-cert mapping)](#automated-trading-safety-saxo-live-cert-mapping)

**Offline tools**
- [Backtesting](#backtesting)
- [Parameter optimizer](#parameter-optimizer)

**Running it**
- [Web UI](#web-ui)
- [API](#api)
- [Notes & next steps](#notes--next-steps)

## Quick start

```bash
go run ./cmd/bot                         # runs offline (mock feed + sim broker)
# optional: copy data/config.example.yaml to data/config.yaml to customise;
# defaults run without one
```

### The settings that actually matter

The bot starts with **no config file at all** — offline mock feed, simulated
broker, `trading_enabled: false`. Everything below has a shipped default. But
the defaults are a demo, not your account, and four of them are wrong for
everybody.

Work down this list in order. Each row links to its chapter.

**1 — Must be set before the bot is useful**

| setting | why | default |
|---|---|---|
| [`watchlist:`](#symbols-and-broker-tickers) | nothing to trade without it (or a [screener](#screener-automatic-universe-selection) that fills it) | empty |
| [`finnhub.api_key`](#market-data-feed) *or* [`feed: "binance"`](#crypto-data) | without one you are on the mock feed — fine for clicking around the UI, useless for decisions | mock |
| [`server.auth_token`](#web-server-and-api-access) | one token is full trading control. Empty means **no auth at all** | empty |

**2 — Must be set before real money moves**

| setting | why | default |
|---|---|---|
| [`broker:`](#supported-brokers) | `sim` is the in-memory paper broker | `sim` |
| broker credentials | [Alpaca](#supported-brokers) key/secret, [Trading 212](#supported-brokers) api_key/secret, or [Saxo](#supported-brokers) token/certificate. **Validated at startup** — the bot refuses to run with trading on and creds missing | empty |
| [`limits:`](#money-limits-the-hard-requirement) | the defaults assume a ~10,000 account: 500/order, 1,000/position, 10,000 total. On a 1,000 account that is no limit at all | see chapter |
| [`engine.trading_enabled`](#going-live-paper-first) | the final switch. **Leave it false until everything above is deliberate** | `false` |

**3 — Should be reviewed, not just accepted**

| setting | why | default |
|---|---|---|
| [`exits:`](#protective-exits) | a 5% stop and a 20% take-profit are round numbers, not a decision about your instruments | on, 5% / 20% |
| [`protections:`](#protections) | drawdown and daily-loss halts. **On by default** at 20% / 8% — generous numbers you should either shrink or consciously accept | on |
| [`safety.max_orders_per_min`](#order-rate-limiting-and-the-circuit-breaker) | the runaway-loop circuit breaker. **On by default** at 20/min, which a healthy small book never approaches — lower it | `20` |
| [`portfolio:`](#portfolio-limits-concentration) | stops six positions from being one bet | off |
| [`costs:`](#trading-costs) | commission and spread your broker really charges; wrong here makes every backtest lie | zeros |

Everything else has a defensible default and can wait until you have a reason
to change it.

> **The order matters.** Set the limits *before* the credentials, and the
> credentials *before* `trading_enabled`. Each step is recoverable on its own;
> doing them in the other order means the first thing that works is a live
> order against demo limits.

### Docker

Prebuilt multi-arch images (amd64 + arm64) are published to Docker Hub at
[kellerman81/go_finance_bot](https://hub.docker.com/r/kellerman81/go_finance_bot)
on every release — no checkout or build needed:

```bash
docker run -d --name go_finance_bot \
  -p 8080:8080 \
  -v "$PWD/data:/app/data" \
  kellerman81/go_finance_bot:latest
curl localhost:8080/health
```

Or in [docker-compose.yml](docker-compose.yml), drop the `build:` block and set
`image: kellerman81/go_finance_bot:latest`. Pin a version tag (e.g. `:1.2`)
instead of `:latest` for reproducible deployments.

Alternatively build it yourself: a multi-stage [Dockerfile](Dockerfile) compiles
a static, CGO-free binary into a small Alpine image;
[docker-compose.yml](docker-compose.yml) builds and runs it:

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
| `alpaca` | Alpaca (US) | set `alpaca.key_id`/`secret_key`, `base_url` (paper endpoint by default). `paper: true` must agree with `base_url` — over the live endpoint it is refused at load, since it was only ever a log label; `use_sim` has no effect with `broker: alpaca` (the bot calls Alpaca), and startup says so in the log and diagnostics — use `broker: sim` for the simulator |
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

   Listings quoted in a minor unit — London's pence (`GBX`/`GBp`), Johannesburg's
   cents (`ZAc`), Tel Aviv's agorot (`ILA`) — are converted through their major
   currency: 72p is 0.72 GBP. They used to be valued as if a penny were a pound,
   which inflated equity and every percentage limit a hundredfold. A rate that
   cannot be fetched is retried after two minutes, not on every sync, and is
   reported once per outage.

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
   sizes are floored — set `limits.max_order_value` accordingly.
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
| Data feed | `internal/market` | `DataProvider` interface, Finnhub (WS+REST), Binance (crypto, public), offline Mock; Yahoo Finance for warmup history |
| Indicators | `internal/strategy/indicators` | SMA, EMA, RSI, MACD, ATR, VWAP, Bollinger, RVOL, Volume Profile, ADX, Stochastic, Supertrend, Donchian, regression Trend |
| AI advisor | `internal/ai` | optional LLM (Ollama / OpenAI-compatible) that votes on a normalised indicator snapshot |
| ML | `internal/ml` | optional logistic-regression classifier, retrained on a rolling window |
| Strategy | `internal/strategy` | pluggable **detectors** + a combiner that compares/merges them |
| Portfolio | `internal/portfolio` | cash, positions, mark-to-market |
| **Risk** | `internal/risk` | **enforces money limits on every order** |
| **Protections** | `internal/protect` | cooldowns, stop guard, drawdown & daily-loss halts |
| Exits | `internal/exits` | stop-loss / trailing / take-profit / ATR stops / ROI ladder — one rule engine shared by the live bot and the backtester |
| Screener | `internal/screener` | optional automatic universe selection |
| Notifications | `internal/notify` | webhook / Discord / Telegram / Pushover / Apprise alerts |
| Broker | `internal/broker` | `Broker` interface, Alpaca (US) + Saxo (EU) + Trading 212 (EU) + Sim |
| Engine | `internal/engine` | ties feed → strategy → risk → broker |
| Persistence | `internal/storage` | SQLite (pure-Go) trade log + state |
| Backtest | `internal/backtest`, `cmd/backtest` | replay history, report metrics |
| API | `internal/api` | gin REST server |

## Settings and config.yaml

Changes made in the web interface apply immediately **and are written back to
`config.yaml`**, so they survive a restart.

The write is a surgical edit, not a re-serialisation. The file is parsed to a
YAML syntax tree, only the scalars that changed are replaced, and the result is
written atomically (temp file + rename — this file holds live broker
credentials). Everything else is carried through untouched: comments, key order,
blank lines, commented-out blocks, and any key the settings screen does not know
about.

That is not fussiness. Measured on a real 679-line config, the obvious
implementation — unmarshal, mutate, marshal — produced 304 lines with **none** of
the 342 comment lines, and silently deleted the whole `strategy_overrides` block
(`SymbolStrategy` is `yaml:"-"`, derived at load time, so a marshal cannot emit
it).

Two safeguards worth knowing:

- **The result is verified before it is written.** The patched document is
  re-read and compared against the settings that were applied; if it would not
  read back the same, nothing is written and the UI says so. This is what stops a
  multi-line value such as `ai.system_prompt` from being quietly reflowed.
- **A failed save never reverts the live change.** The bot keeps running the new
  value and the UI reports "applied, but not saved" — telling you plainly rather
  than leaving you to discover it at the next restart. A rule that spans
  sections — per-class slots against `min_positions`, money ceilings against
  the file's `trading_enabled` — is checked **before** the change is applied,
  and a save that breaks it is refused outright, so the running bot never
  holds a combination the loader would reject.
- **Only what you changed is written.** A save compares each value with what
  the file already loads to — your file over the defaults and the active
  `profile:` — so a value the profile supplies is not copied into the file and
  keeps following the profile, and a later change to a default still reaches
  you.
- **A section written with a YAML alias or merge key (`<<: *base`) cannot be
  saved from the UI**, and the save says so: editing through it would change
  every other user of the anchor. Write that section out in full.
- **The portfolio form's `sectors` and `asset_classes` maps** are written as
  whole blocks (symbol keys such as `SAP.DE` contain dots, which the leaf
  editor cannot address), only when they changed; comments inside those two
  blocks are not kept.

Run with `--config <path>` and that is the file that gets edited.

**Environment overrides** (`BOT_TRADING_ENABLED`, `BOT_BROKER`, the broker and
notifier credentials, …) win over the file. A boolean one accepts
`true`/`false`, `on`/`off` and `yes`/`no`; anything else stops the bot at
startup rather than being ignored — `BOT_TRADING_ENABLED=off` used to leave
trading on. **A settings save never writes an env-supplied value into the
file** — not the secrets, and not `BOT_TRADING_ENABLED` or the `BOT_AI_*`
switches either: before, one save moved the kill switch from the environment
into the file, and removing the variable no longer turned trading off.

## Profiles

The settings above interact. A strategy that trades often needs different exits
and different protections than one that waits for high conviction, and picking
each knob independently is how people end up with a bot that takes 40 trades a
day behind a 20% stop. A profile is a set of choices made *together*:

```yaml
profile: conservative   # conservative | balanced | aggressive
```

It seeds strategy, exits, sizing and protections before the rest of your file is
applied, so anything you set explicitly still wins. `BOT_PROFILE` selects the
profile the same way — with or without a config file, and in place of the
file's own `profile:` when both are set — and is applied under the file too, so
it never overrides a setting the file spells out. An unknown name is an error at
startup. A profile is a starting point to tune from — not a
recommendation, and never a substitute for backtesting your own data.

## Web server and API access

```yaml
server:
  addr: ":8080"           # listen address
  auth_token: ""          # shared bearer token; empty = no auth
```

`addr` is what the gin HTTP server binds. `:8080` listens on every interface —
fine behind Docker on a private network, **not** fine on a machine reachable
from the internet. Bind `127.0.0.1:8080` and put a reverse proxy in front if
the host is exposed. The proxy must **forward the `Host` header**
(`proxy_set_header Host $host;` in nginx): the `/v2` settings forms **and every
`/api` write** refuse a browser request whose `Origin` (or `Sec-Fetch-Site`)
names another site, as do the `/v2` fragment loads — which is what stops
another web page from posting to the bot. With auth off (the default) a
bodyless cross-site POST could otherwise start test mode, clear a halt and
switch trading back on, or start an autotune pass. A proxy that rewrites
`Host` makes every save look cross-site.

`auth_token` is a single shared bearer token, compared in constant time. It is
accepted either as `Authorization: Bearer <token>` or as a `?token=` query
parameter — the query form exists because the browser's `EventSource` cannot
set headers, and it is the weaker of the two: query strings land in proxy logs
and `Referer` headers.

> **There are no user accounts.** One token grants full trading control —
> placing orders, changing limits, flipping the kill switch. Treat it like a
> broker password, not an API key.

The `/v2` UI shell and its static assets are deliberately unauthenticated; the
data behind them is not. Every `/api/*` route requires the token when one is
set.

## Market data feed

```yaml
feed: ""                  # "" = auto, "binance" = public crypto data
finnhub:
  api_key: ""             # enables the live WebSocket tick stream
binance_base_url: ""      # override for a regional mirror
```

`feed` selects where live prices come from:

| value | source | needs |
|---|---|---|
| `""` (default) | Finnhub when `finnhub.api_key` is set, otherwise the offline mock | a key, for live data |
| `"binance"` | Binance public REST, polled every 5s | nothing |

**The feed is independent of the broker.** Binance data is read-only, so crypto
can be tracked and paper-traded against `broker: sim` while real orders keep
going only to the equity brokers. See [Crypto data](#crypto-data).

Historical bars come from a separate path — Yahoo by default (unauthenticated,
paced and retried across mirror hosts), wrapped in a 15-second cache and a
fallback chain. That is why backtests and indicator warmup work even with no
`finnhub.api_key` set.

With neither a Finnhub key nor `feed: binance`, the bot runs against a mock
feed: useful for exercising the UI and the plumbing, useless for decisions.

## Crypto data

```yaml
feed: "binance"            # public market data, no API key
watchlist: ["BTCUSDT", "ETHUSDT"]
broker: "sim"
```

**Read-only, by design.** The bot can track, signal on, backtest and
paper-trade crypto through Binance's public endpoints — but it places no crypto
orders. An exchange execution adapter carries withdrawal-capable keys and can't
be meaningfully tested without a funded account; shipping one that had never
filled a real order would be worse than shipping none. Execution stays on the
audited broker adapters.

Crypto trades continuously, so the market-hours gate is a no-op for these
symbols and warmup history is always available. A pair is recognised as crypto
once the Binance feed has priced it, whatever its quote currency (`BTCBRL`,
`ETHJPY` …), so it never inherits the US equity session — exits included.

Two limits worth knowing:

- **One symbol Binance does not list** (a typo, a delisted pair, a stock the
  screener added) no longer stops every other symbol's ticks: Binance rejects a
  batched request whole, so the feed then prices each symbol on its own, sets
  the refused one aside and says so once in the diagnostics.
- **History is paged**, 1000 klines per request, up to 30 pages (about 20 days
  of one-minute bars, a month of 15-minute ones). A wider window keeps its
  newest part and says so once, rather than silently returning only the last
  1000 bars — a 30-day one-minute screen used to judge BTCUSDT on under a day.

## Bars

Live bars are aggregated from the tick feed at `engine.bar_interval` (1 minute by
default). **An interval with no ticks still produces a bar** — flat, at the last
close, with zero volume — because every period in this bot is counted in bars.

That matters more than it sounds. `trend_period: 1950` is documented as "about a
week of 1-minute bars"; the EMAs are 12 and 26 bars; ATR is 14. If a quiet
minute produced no bar, then on a thinly-traded instrument each of those silently
covered far more time than it claims — a "week" of trend could be a month. Worse,
warmup seeds from the provider's candles, which *do* include every interval, so a
symbol's history changed shape partway along, and the backtester (fed evenly
spaced bars) measured a different strategy than the live engine ran.

A gap longer than 90 minutes is treated as a closed market and **not** filled:
manufacturing bars for a shut venue would be a lie, and 17 hours of overnight
silence is a thousand fake one-minute bars — a third of the history the engine
keeps. Zero volume is the marker that distinguishes a filled bar from a traded
one, in the API and on the chart alike.

**What this does and does not touch.** Anything counted in *bars* reads a fixed
amount of time now — every indicator period, the trend gate, `htf_multiple`
(which aggregates by bar count), and the ML detector's `horizon` and
`train_bars`. Anything counted in *wall clock* was never affected:
`keep_interval`, `max_hold` and the ROI ladder all measure from the entry
timestamp. `confirm_buy` / `confirm_sell` count evaluations, not bars, so they
are unaffected too.

**Volume indicators use the bars that traded** as their baseline, not every
interval. RVOL asks "is this bar unusually active?", and a thin instrument spends
most intervals empty; averaging those in drags the baseline toward zero, so any
bar that traded reads as a surge. On real numbers, 3 traded bars out of 20 gives
each an RVOL near 6.7 against a default threshold of 1.5 — the detector would
fire on every traded minute. VWAP needs no special case: it weights by volume, so
an empty interval contributes nothing.

## Symbols and broker tickers

A broker may name an instrument differently from your watchlist: Trading 212
lists `PRAJL.XC` as `PR1Jd`. The bot resolves each watched symbol to its broker
ticker **when the symbol is added**, not when an order is first placed, so the
positions the broker reports come back under the names the bot uses.

That timing matters. The reverse mapping lives in memory, so resolving it lazily
meant that after a restart a long-held position reported under the broker's name
and matched no open-position record — and a position with no record is one the
bot is not managing: no ROI ladder, no `max_hold`, a trailing peak that restarts
from scratch, and a protective exit that would try to sell a name the broker
cannot resolve. The **Reconciliation** panel on the Positions screen shows any
symbol where the two sides still disagree.

Resolution is best-effort and bounded: a symbol this broker does not offer is
still tracked (signals, charts, paper positions), and a broker that is slow or
down delays start-up rather than preventing it.

## Finding symbols

Nobody remembers that Lanxess is `LXS.DE`. The watchlist screen has a search box:
type a company name and it lists matches as **ticker + full company name +
exchange**, and adding one is a click.

The exchange is not decoration. `BAYN.DE` and `BAYRY` are both "Bayer" — the
XETRA listing and the US ADR — and they are different instruments in different
currencies. Two rows that read identically apart from the venue is exactly the
case the picker exists to disambiguate.

**Results are checked against your broker.** Searching "lanxess" returns seven
listings — XETRA, Milan, Frankfurt, Vienna, Düsseldorf, Stuttgart, Munich — and
your account can usually trade one of them. Each row is resolved against the
broker's own instrument list and tagged **tradeable** or **not at your broker**,
with the tradeable ones floated to the top. Picking a listing your broker does
not carry is how you reach `instrument-invisible` at the first buy.

A listing the broker lacks stays addable, just de-emphasised: it is still worth
tracking, and the broker may simply not have loaded its metadata yet. And when
the broker cannot answer at all — the simulator, or a real broker before its
instrument list loads — **nothing is tagged either way**. Silence is not a
verdict.

Search goes to Yahoo Finance, which answers in the **same symbol convention the
warmup history uses**, so anything the picker offers is something the engine can
actually fetch candles for. Results Yahoo cannot chart (partner indices, news
entities) are filtered out — adding one would put a symbol on the watchlist that
warms up to nothing. Search needs no API key, and the box is hidden entirely when
the configured provider cannot search.

Company names are also shown beside the ticker in the watchlist itself, and are
cached and persisted: a name never changes, so re-fetching twenty of them on
every start would be a round trip per symbol to re-learn that `BAYN.DE` is Bayer.
Adding from a search result carries the name across for free; symbols added by
hand (or from before this existed) are backfilled in the background, at most
eight per pass. A symbol whose name cannot be resolved simply renders as its
ticker — `PRAJL.XC` and `UNPRF` are expected to stay that way.

## Screener (automatic universe selection)

A fixed watchlist trades whatever you picked months ago, including instruments
that have since gone illiquid or stopped moving. With the screener on, the
tradeable set is re-derived from recent history on a schedule:

```yaml
screener:
  enabled: true
  universe: ["AAPL","MSFT","NVDA","AMD","TSLA"]   # empty = filter the current watchlist
  interval: 6h
  max_symbols: 25             # keep the best N by `rank`
  rank: "volume"              # volume | volatility | momentum
  min_price: 5
  min_avg_volume: 100000
  min_volatility_pct: 0.5     # ATR%: too quiet is untradeable...
  max_volatility_pct: 8       # ...too wild is unsurvivable
  min_history_bars: 100       # never trade what the strategy can't warm up on
  max_spread_pct: 0.005       # drop what can't clear its own cost of entry
  keep_held: true
  keep_manual: false          # true = only ever ADD to your watchlist

  # The chain, in the order it runs. Empty = all of them, default order.
  filters: [history, price, volume, volatility, spread]
```

**Symbols you hold are never dropped**, whatever the filters say — removing one
would strand the position with no exits, no trailing stop and no way to sell it.
"Hold" here includes a buy whose fill is not yet confirmed, a limit order still
resting and a broker holding the bot has not adopted yet — and a held symbol
that is missing from the watchlist is added back on the next pass, whatever
`keep_held` says (`keep_held` only changes how its verdict reads). The screener
decides what may be *entered*, not what may be forgotten. The last pass,
including the reason each candidate was rejected, is at `GET /api/screener`, and
`POST /api/screener/rescreen` runs one immediately (409 while a pass is already
running). A pass runs off the evaluation loop, so even a long one — probe
backtests and searches included — never delays a stop.

**A candidate is judged on the bars it asked for.** When the history source can
only serve coarser bars than the profile's `resolution` (Yahoo falls back to
hourly or daily for an illiquid listing), the candidate is refused as "too few
bars at resolution …", naming what was served. Judged as the requested
resolution, the substitute bars flattered exactly the thin listings the screener
exists to keep out: four times the volume per bar and a bar count that cleared
`min_history_bars`.

### Keeping your own picks (`keep_manual`)

By default the screener owns the whole watchlist: it re-derives the tracked set
each pass, so a symbol you added by hand disappears as soon as it stops ranking
in the top `max_symbols`.

Set `keep_manual: true` to make it purely additive instead:

| | `keep_manual: false` (default) | `keep_manual: true` |
|---|---|---|
| Your `watchlist:` entries | dropped when they stop ranking | **never dropped** |
| Symbols added in the web UI | dropped when they stop ranking | **never dropped** |
| Screener discoveries | tracked while they rank | tracked while they rank |
| What `max_symbols` counts | every tracked symbol | **discoveries only** |

So `max_symbols: 25` with `keep_manual: true` means "run the 25 best-ranked names
you can find *alongside* my list", not "hold me to 25 names in total".

Discoveries stay temporary — they are dropped again once they stop qualifying,
and they are marked *auto* in the web interface. To adopt one permanently, add it
by hand: adding a symbol that is already tracked pins it without re-warming it.
Removing a symbol by hand un-pins it.

The distinction survives a restart. On first upgrade nothing is pinned except
what `watchlist:` names — an existing persisted watchlist is as likely to be a
screener discovery as your own pick, and guessing "yours" would freeze a whole
screened universe in place.

**The filter chain.** Filters are named and ordered rather than being a fixed
block of bounds. Evaluation stops at the first failure, so a verdict names the
binding constraint instead of listing everything wrong with a candidate. Naming
them buys two things a pile of magic zeroes does not: a filter can be removed
outright rather than disabled by guessing which value means "no bound", and a
costly check can be ordered behind a cheap one that rejects most candidates
first. An unknown name is skipped, not fatal — a typo in a screening filter must
not take a running bot down.

**The spread filter** is the one that needs explaining. No free candle feed
reports a bid-ask spread, so it is estimated from the bars themselves using the
Corwin–Schultz (2012) high-low estimator: a bar's range mixes volatility, which
grows with the square root of time, and the spread, which does not, so comparing
one bar's range with the range over two bars isolates the spread. It matters
because the spread is paid on both legs of a round trip — half on the way in,
half on the way out, one full spread in all — and is invisible on a chart — an
instrument can look perfectly tradeable and still be unable to clear its own cost
of entry. The estimate is reported per candidate as `spread_pct` in
`GET /api/screener`, and feeds `costs.measure_spread` (see **Trading costs**).

### Screening by asset class

One set of screening rules cannot judge a broad ETF and a liquid megacap. An
accumulating tracker measured on one-minute bars has no intraday character to
find: its ATR%, spread estimate and momentum all describe the tick rather than
the fund. The same bars are exactly right for GOOGL.

So every setting above can be overridden per asset class, and each profile
**inherits every field it does not name** — the same contract
`strategy_overrides` gives, so setting one value cannot silently zero the rest.
Candidates are classified by the same pipeline the portfolio allocation caps
use, so a symbol screened as an ETF is the same ETF to both.

Class keys are matched the way the classifier names classes, aliases
included: a profile, a `universe_by_asset_class` entry, a
`max_pct_per_asset_class` / `min_positions_per_asset_class` key and a
hand-written `asset_classes:` label written `ETF`, `ETFs`, `stocks` or
`equity` all mean `etf` or `stock`. Matched literally, a key the classifier
never emits configured nothing and loaded without complaint.

```yaml
screener:
  resolution: "15"            # base: bar size to judge on (1|5|15|60|D)
  lookback_days: 30           # base: how much history to pull

  universe_by_asset_class:
    etf:   ["SEC0.DE", "IWDA.AS", "EUNL.DE"]
    stock: ["GOOGL", "NVDA", "ASML.AS"]

  by_asset_class:
    etf:
      resolution: "D"         # judge SEC0.DE on daily bars ...
      lookback_days: 365      # ... over a year
      max_symbols: 8
      rank: "trend"
      filters: [history, price, trend, drawdown, cost]
      min_trend_r2: 0.5
      min_avg_volume: 20000   # trackers trade far thinner than megacaps
    stock:
      resolution: "15"        # intraday character is the point here
      max_symbols: 15
      rank: "momentum"
```

**`max_symbols` is per class**, which is the point of capping by type. Under one
shared cap the classes compete on a single metric: ranked on volume, a megacap
beats every ETF, and "8 ETFs and 15 stocks" quietly becomes "23 stocks".

**`lookback_days` is the window judged, within what the data source serves.**
Yahoo limits intraday history by bar size: 1-minute bars reach back 5 days,
5- and 15-minute bars a month, hourly bars two years; daily bars have no limit.
A longer lookback at a fine resolution is capped there — use a coarser
`resolution` for a longer window. A shorter lookback really is shorter:
`lookback_days: 10` at `"D"` judges ten daily bars, so `min_history_bars` has
to fit inside it. Only the resolution asked for is windowed: when Yahoo has too
few bars at it and substitutes a coarser interval (recorded in the diagnostics
tab), that fallback keeps its full history.

### Sector diversification (`max_symbols_per_sector`)

The screener ranks on trend, volume or momentum — all of which correlate hard
*within* an industry. Left alone it fills every slot with one sector, and
`portfolio.max_positions_per_sector` then refuses most of those buys at order
time. You pay for a feed subscription, a warmup history and a stream of
`blocked` trade rows for symbols that were never buyable.

```yaml
screener:
  max_symbols: 5
  max_symbols_per_sector: 2     # match portfolio.max_positions_per_sector
```

Set it to the guard's own number, or a little above so a closing position has a
replacement ready. Three properties:

- **Counted across all asset classes**, because the guard is too — a per-class
  budget would let two tech stocks *and* two tech ETFs through a limit of two.
- **Held positions spend it**, because the guard counts them: with two tech
  names held and a cap of 2, no tech discovery is picked. They are still kept,
  never dropped. A pinned symbol you do not hold spends nothing.
- **A sector at its cap is skipped, not a stop.** The walk continues down the
  ranking, so the freed slots go to the next industry rather than being lost.
  That substitution is the entire point.
- **Unsectored symbols are exempt**, matching `checkSector`. Yahoo reports no
  sector for funds, so ETFs are normally unsectored — an ETF's industry
  exposure is a property of its holdings, not of the instrument.

Sectors are resolved *during* screening, not read from the post-adoption cache.
`resolveNames` only labels symbols already on the watchlist, so a cache-only
lookup would see nothing for exactly the candidates being judged, and the cap
would never bind. The label lands in the same map the guard reads, so both talk
about the same sectors.

Each verdict carries its `sector`, so `GET /api/screener` shows why a candidate
was passed over — and the `/v2` watchlist view renders it as a column alongside
the asset class, the resolution it was judged on, trend R², own drawdown, cost
multiple and the probe result.

The `/v2` positions view also carries an **allocation panel**: value, share of
equity, cap and remaining headroom per asset class, with an "at cap" marker and
a row for unclassified holdings (which count against no cap at all). It uses the
same equity denominator the limit divides by, so the panel and the limit cannot
disagree.

**One thing to check in your own config:** auto-learned sectors are Yahoo's
GICS names, lower-cased (`technology`, `financial services`). If your `sectors:`
block uses custom labels (`tech`), the two are *different buckets* — a cap of 2
then permits 2 `tech` plus 2 `technology`. Matching your labels to Yahoo's makes
both layers share one namespace.

A symbol nothing can classify is screened against the base settings rather than
skipped — applying the wrong class's rules would be worse than applying the
general ones.

### Three more ways to say "good"

Named in `filters:` like any other check.

**`trend`** fits a least-squares line and bounds both its slope and its R².
Slope says which way and how fast; R² says whether "trend" is even the right
word. A sideways chop that happens to end higher has a real positive slope and
an R² near zero — momentum alone cannot tell it from a clean advance, and
trading it as a trend is how a trend-following config bleeds out in a range.
Available as a rank too, where it scores slope × R²: slope alone rewards an
unrepeatable spike, R² alone rewards a tidy line going nowhere.

**`drawdown`** bounds the candidate's own peak-to-trough decline. A falling
knife is certainly moving, so it passes a volatility band, and can pass momentum
on a dead-cat bounce.

**`cost`** is the one that decides whether an instrument can be profitable *at
all*. It requires the typical bar-to-bar move to be some multiple of the
instrument's own round trip — one spread (half paid on each fill) plus
commission and slippage on both legs. Something that moves
0.1% a bar against a 0.15% round trip loses money at every win rate, and no
strategy setting fixes that. The move is measured close-to-close rather than as
bar range: a trade captures the change in price, and the bar range is what the
spread estimate is derived from in the first place, so dividing one by the other
would compare a quantity against itself.

### Does it actually perform? (`probe`)

Every filter so far answers *is this instrument tradeable* — enough history,
enough liquidity, moves enough to clear its costs. None of them answers the
question you actually have: **would my strategy have made money on it?**

```yaml
screener:
  probe:
    enabled: true
    backtest: true            # run the LIVE strategy over the candidate
    autotune: false           # also search for its BEST configuration
    max_candidates: 10        # per class, best-first
    min_trades: 5
    min_expectancy_pct: 0
```

`backtest` asks "would what I am running now have worked here?" — the honest
question for adopting a symbol into a book that trades one configuration.
`autotune` asks something different: "could this work if I tuned for it?" That
finds symbols your current settings happen to suit badly, but the number is only
achievable if you then tune for it (via `autotune.on_loss` or a per-symbol
override), so it must not be read as what you would earn.

Three properties worth knowing:

- **It only ever demotes or rejects.** An instrument that cannot clear its own
  spread does not become tradeable because a backtest of it looked good —
  failed candidates never reach the probe at all.
- **It is bounded.** Only the top `max_candidates` per class are probed, taken
  best-first from the cheap ranking. A candidate beyond the cutoff keeps its
  ranking; not probed is not the same as probed badly.
- **A thin result is ignored, not trusted.** Below `min_trades` the candidate
  keeps its cheap-filter ranking, with the probe still recorded so you can see
  why it was not used. The autotune search is held to the same floor: a
  winner thinner than `min_trades` leaves the backtest result standing.
- **It runs on the live bar size.** The probe aggregates the profile's bars
  to `engine.bar_interval` before replaying the strategy; a profile whose bars
  are COARSER (a daily ETF profile against a 1-minute strategy) is not
  probed, and the diagnostics say so once — every period would span days.
- **With both legs on, searched candidates rank first.** A candidate whose
  search could not run (the run slot was busy) keeps its backtest score, but
  ranks below every candidate that was searched; the two scores are not on
  one scale.

### Starting from nothing (`suggest_universe`)

An empty `universe:` makes the screener filter-only: it can drop symbols but
never find any. "Screen a universe" is also useless advice if you do not yet
know what belongs in one.

`suggest_universe: true` seeds any class with no universe of your own from a
built-in starter pool of large, liquid instruments spanning US and European
listings, in the symbol convention the history provider speaks. They are a place
to begin, not a recommendation — nothing in them skips a single filter, cost
check or probe. `GET /api/settings` returns the pools as `screener_suggestions`
so the UI can offer them.

It is settable **per class**, which is usually what you want — the two halves of
a book are rarely at the same stage. You may know exactly which five stocks to
screen and have no idea which trackers are worth looking at:

```yaml
screener:
  universe_by_asset_class:
    stock: ["GOOGL", "NVDA", "ASML.AS"]   # curated
  by_asset_class:
    stock:
      suggest_universe: false             # screen only those
    etf:
      suggest_universe: true              # no list of my own — suggest some
```

A per-class value overrides the shared one in both directions: it can seed one
class while the shared switch is off, or opt one class out while it is on.

**A class with a pool of its own is never seeded**, whichever switch is set. A
curated list is a decision, and quietly appending a dozen suggestions to it
would dilute that rather than help. Note that "its own" means a
`universe_by_asset_class` entry or a profile `universe` — the shared top-level
`universe:` is not class-specific, so a class inheriting it still counts as
having none.

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

### The detectors

Each one reads a single indicator and returns BUY, SELL or HOLD with a strength
in 0..1. A detector with nothing to say votes HOLD and, under `unanimous`, is
ignored rather than counted against the others.

They fall into four families, and mixing families is the point — three
trend-followers agreeing tells you less than a trend-follower and a
volume confirmation agreeing.

| family | detectors | what it answers |
|---|---|---|
| Trend | `ema_cross`, `macd`, `trend`, `supertrend`, `adx` | which way is it going? |
| Mean reversion | `rsi`, `bollinger`, `stochastic` | has it gone too far? |
| Breakout | `donchian`, `atr` | has it broken out of its range? |
| Volume | `vwap`, `rvol`, `volume_profile` | is the move backed by participation? |
| Learned | `ai`, `ml` | what does a model make of all of it? |

#### `ema_cross` — EMA crossover (trend)

```yaml
fast_ma: 12     # fast EMA period, in bars
slow_ma: 26     # slow EMA period
```

The classic. Fast EMA above slow → BUY bias; below → SELL bias. Strength grows
with the gap between them, so a wide separation votes harder than a fresh
crossover.

Cheap, always opinionated, and late by construction — it confirms a move that
has already begun. Pair it with something that votes earlier.

> EMAs are seed-dependent: a windowed series and a full-history series disagree
> for the first 3–4 periods until the seed decays. Live and backtest can differ
> slightly on the very first bars of a symbol for this reason.

#### `rsi` — Relative Strength Index (mean reversion)

```yaml
rsi_period: 14
rsi_oversold: 30
rsi_overbought: 70
rsi_mode: "midline"   # "midline" | "extremes"
```

`rsi_mode` changes this detector more than any threshold does:

- **`midline`** (default) — a graded vote around 50. Below 50 leans BUY, above
  leans SELL, strength scaling with the distance. It **always participates**,
  which matters under `unanimous`: a detector that always has an opinion is a
  detector that can always block.
- **`extremes`** — votes only outside `rsi_oversold` / `rsi_overbought`, and
  HOLDs in between. Far quieter, and under `unanimous` it stops being a veto
  most of the time.

If a `unanimous` strategy never fires, `rsi_mode: midline` is the first thing
to check.

#### `macd` — moving-average convergence/divergence (momentum)

```yaml
macd_fast: 12
macd_slow: 26
macd_signal: 9
```

Votes on the sign of the histogram — the MACD line minus its signal line.
Positive → BUY, negative → SELL, strength from the magnitude.

Momentum rather than level: it turns when the *rate* of the trend changes, so
it leads `ema_cross` slightly and whipsaws more in a flat market.

#### `bollinger` — Bollinger Bands (mean reversion)

```yaml
bb_period: 20
bb_stddev: 2
```

Price below the lower band → BUY; above the upper → SELL. The bands are a
moving average ± `bb_stddev` standard deviations, so they widen in volatile
conditions and tighten in calm ones — the same absolute move votes differently
depending on the regime.

Its failure mode is the strong trend: price riding the upper band means
"powerful uptrend", and this detector reads it as "sell, too far up".

#### `stochastic` — stochastic oscillator (mean reversion)

```yaml
stoch_k_period: 14
stoch_d_period: 3
stoch_overbought: 80
stoch_oversold: 20
```

Where the close sits inside the recent high/low range. %K at or below
`stoch_oversold` → BUY; at or above `stoch_overbought` → SELL. `stoch_d_period`
smooths %K into %D.

Similar question to `rsi`, different arithmetic: RSI measures the size of
up-moves against down-moves, the stochastic measures *position within the
range*. They disagree often enough to be worth running together.

#### `trend` — least-squares regression slope (trend)

```yaml
trend_period: 1950      # bars to regress over (~1 US trading week of 1-min bars)
trend_threshold: 0.01   # min end-to-end move of the fitted line to count as a trend
trend_gate: true        # also VETO counter-trend signals — ON in the shipped config
```

Fits a straight line through up to `trend_period` bars and votes on the
end-to-end move of that line, normalised against the fitted level so the
threshold means the same thing on a €15 ETF and a €900 share.

**`trend_gate` changes its role entirely, and it ships ON.** With it, this
stops being one vote among many and becomes a veto: no buys in a downtrend, no
strategy sells in an uptrend, regardless of what every other detector says.
Protective exits still fire — the gate never traps a position. If your strategy
seems to ignore good signals in one direction, this is the first thing to
check.

> A documented discontinuity: when the fitted intercept is ≤ 0 the detector
> falls back to the last close as its denominator, so the normalisation jumps.

#### `supertrend` — ATR-banded trend follow

```yaml
supertrend_period: 10
supertrend_mult: 3       # larger = fewer flips
```

An ATR-scaled band that flips sides when price crosses it. Above the band →
BUY, below → SELL. `supertrend_mult` is the sensitivity dial: raise it to sit
through noise, lower it to turn sooner and more often.

Because the band is volatility-scaled it holds through ordinary wobble in a
calm instrument while still turning quickly in a violent one.

#### `adx` — Average Directional Index (trend strength filter)

```yaml
adx_period: 14
adx_threshold: 25   # minimum ADX to vote at all
```

The only detector that measures **whether there is a trend at all** before
saying which way. Below `adx_threshold` it HOLDs; above it, +DI versus −DI sets
the direction.

That makes it most useful as a companion: it stays silent in the chop where the
mean-reversion detectors do their best work, and speaks up in the trends where
they do their worst.

#### `donchian` — channel breakout

```yaml
donchian_period: 20
```

Close above the highest high of the last `donchian_period` bars → BUY; below
the lowest low → SELL. No smoothing, no thresholds — the range is broken or it
is not.

The oldest idea here and still one of the sharpest, at the cost of buying
strength rather than weakness. Expect it to disagree with every mean-reversion
detector by design.

#### `atr` — volatility breakout

```yaml
atr_period: 14
atr_mult: 1.5
```

Votes when the move from the prior close exceeds `atr_mult` × ATR — an unusual
move *for this instrument*, which is what makes it comparable across a
watchlist holding both a calm ETF and a volatile small cap.

Direction comes from the move's sign, so it is a confirmation of something
already happening rather than a prediction.

#### `vwap` — volume-weighted average price (volume)

```yaml
vwap_period: 20
```

Price above VWAP → BUY bias, below → SELL bias. VWAP is where the average
participant transacted, so trading above it means buyers are paying up.

#### `rvol` — relative volume (volume confirmation)

```yaml
rvol_period: 20
rvol_threshold: 1.5
```

Compares current volume against the trailing average of bars that actually
traded. Above `rvol_threshold`, it confirms the direction of the latest move;
below, it HOLDs.

A confirmation detector, not a signal on its own: it has no view on direction
until something else moves.

#### `volume_profile` — value area and point of control (volume)

```yaml
vp_window: 120
vp_buckets: 24
vp_value_area_pct: 0.7
```

Buckets the window's closes by price, weighted by volume, then finds the
**point of control** (the busiest price) and the **value area** around it
holding `vp_value_area_pct` of the volume. Price outside the value area votes
toward returning to it.

The most expensive detector here, and the most sensitive to the volume caveat
below.

> **The volume caveat applies to all three.** The live engine builds bars from
> the quote stream, which carries no trade sizes — so a live bar's "volume" is
> its **tick count**, while backtests use real share volume from CSV or Yahoo
> history. `vwap`, `rvol` and `volume_profile` therefore behave measurably
> differently live than they did in your backtest. Weight them accordingly, or
> prefer the other families for live decisions.

#### `ai` — LLM advisor

Reads the same normalised indicator snapshot and votes. Off the hot path,
collapses to HOLD on any error, timeout or low confidence. No account,
position or balance data is ever sent. See
[AI advisor](#ai-advisor-the-ai-detector).

#### `ml` — learned classifier

A logistic regression — not deep learning — retrained per symbol on a rolling
window over the same 16-feature vector, predicting whether the next N bars are
up. Walk-forward labelled, no lookahead. See
[ML detector](#ml-detector-learned-signal).

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
  signals become HOLD. Higher → fewer, higher-conviction trades. Outside 0..1
  it is refused at load and in the form, for the base strategy as for an
  override: `25` (meant as 25%) used to load and silently stop every buy. On the
  synthetic sample, raising it from 0 → 0.30 took win rate **43% → 100%**,
  Sharpe **0.4 → 2.5**, and losses **4 → 0**.
- `engine.allow_pyramiding: false` (default) — at most one entry per symbol
  until it's exited, so the bot can't keep adding to a position every bar.

> Note: a slightly-upward random walk is *designed* to reward buy-and-hold, so
> beating the benchmark on synthetic data isn't the goal — minimising losses and
> trade quality is. Validate real edge by backtesting **real** CSV history
> (`-csv`), especially across choppy/down markets.

## Per-symbol strategy parameters

One set of parameters for a volatile small cap and a short-duration bond ETF is a
compromise that suits neither. An RSI period tuned on NVDA says nothing useful
about IBCI.DE, and a 14-bar ATR breakout on an instrument that moves 5 bps a day
fires on noise.

```yaml
strategy:
  detectors: ["ema_cross", "rsi", "macd"]
  min_strength: 0.25
  rsi_period: 14

strategy_overrides:
  NVDA:
    min_strength: 0.45      # demand more conviction on a volatile name
  IBCI.DE:
    detectors: ["trend"]    # a quiet bond ETF: trend only
    min_strength: 0.50
```

**Overrides inherit.** An override that names only `min_strength` keeps every
other parameter from the base — otherwise writing one field would silently zero
the RSI period, the MACD lengths and the detector list, and that symbol would
trade on a config nobody wrote. List fields (like `detectors`) are replaced
wholesale, which is the useful semantics for "trade this one differently".

Resolution order is profile → `strategy:` → `strategy_overrides:`. A bad override
fails the config load naming the symbol, rather than leaving one symbol quietly
broken. The detector table and each trade's indicator snapshot use the symbol's
own parameters, so the UI shows what actually drove a decision.

## Multi-timeframe confirmation

A buy signal on 1-minute bars inside an hourly downtrend is usually noise
dressed up as an entry. Rather than guess, the strategy can re-run real
detectors on genuinely coarser bars:

```yaml
strategy:
  htf_multiple: 15            # aggregate every 15 bars into one
  htf_detectors: ["trend"]    # what to run on them (default: trend)
  htf_mode: "gate"            # "gate" vetoes counter-trend signals; "vote" just adds an opinion
```

Aggregation is by **bar count**, not wall-clock time, so it behaves identically
whether the base bars are 1-minute live ticks or daily candles from a CSV, and
the newest aggregate always ends on the newest bar (so the reading never
flickers as a bucket fills). The cost is warmup: confirmation needs
`htf_multiple` times as much history before it can say anything, and the
strategy's warmup grows accordingly. The live engine keeps 3000 bars, so a
strategy whose warmup needs more could never leave warmup — every detector of
every symbol would vote HOLD forever while a backtest kept trading. Such a
save is refused, and one that reaches the engine anyway (a hand-edited
file) is reported in the diagnostics. `ai` and `ml` cannot be
`htf_detectors`: they have no symbol on the coarser bars and would always
abstain, so that is refused at load too.

Each side is gated on its own: the deny gate and `min_strength` /
`min_strength_sell` are applied to the BUY and the SELL candidate before one is
chosen, so a sub-threshold BUY no longer suppresses a SELL that qualifies.

## AI advisor (the `ai` detector)

An optional detector that asks a **local or remote LLM** what it makes of the
current setup, and folds its answer in as one more vote. Ollama is the default,
so it runs **fully offline on your own machine** with no API key and no data
leaving the host:

```bash
ollama pull llama3.2 && ollama serve
```

```yaml
ai:
  enabled: true
  provider: "ollama"                 # or "openai" for any OpenAI-compatible endpoint
  base_url: "http://localhost:11434"
  model: "llama3.2"
  refresh_interval: 15m              # min time between calls per symbol
  max_strength: 0.4                  # cap its influence (see below)

strategy:
  detectors: ["trend", "rsi", "macd", "ai"]
```

**What the model actually sees.** Not raw candles — LLMs are poor at arithmetic
over long number lists, and a few hundred bars would cost more tokens than the
answer is worth. It gets a compact, *normalised* snapshot of the same readings
the rule-based detectors vote on, so a $3 stock and a $3000 one read alike:

```
symbol: AAPL
price: 231.4400
return_last_bar_pct: +0.12
return_5_bars_pct: +1.03
return_20_bars_pct: +3.51
ema_fast_vs_slow_pct: +0.84
rsi: 61.2
macd_histogram_pct_of_price: +0.041
bollinger_position: 0.78 (0=lower band, 1=upper band)
atr_pct_of_price: 1.12
adx: 28.4 (+DI 26.1, -DI 14.9)
stochastic: %K 71.3, %D 68.0
relative_volume: 1.34 (1.0 = average)
range_position: 0.81 (0=period low, 1=period high)
regression_trend_pct: +4.20 (fit quality R2 0.71)
```

Readings that can't be computed yet are **omitted**, never sent as a zero the
model would read as real data. The answer must be one JSON object —
`{"action":"BUY|SELL|HOLD","confidence":0..1,"reason":"..."}` — which becomes a
normal detector vote (Ollama's `format: json` mode constrains the sampler, so
small local models comply reliably).

**It cannot stall the bot.** Verdicts are cached per symbol and refreshed in the
background, so an evaluation tick — or a backtest bar — costs a map lookup, not
a model round-trip. At most `max_concurrent` requests are in flight, and at most
one per symbol per `refresh_interval` (and only when the readings actually
moved: an unchanged snapshot reuses the cached answer rather than buying the
same opinion twice).

**It fails to HOLD.** Advisor disabled, model unreachable, request timed out,
answer unparseable, verdict older than `max_age`, confidence below
`min_confidence` — every one of those is a HOLD. The advisor can decline to have
an opinion; it can never invent a trade. Failures land in `GET /api/errors` (and
the UI's error panel) under source `ai`, and `GET /api/ai` reports the model,
call count and last error.

**Keep it on a leash.** `max_strength` caps the strength this detector can
contribute: at `0.4` the LLM can tilt a marginal decision but a `majority` or
`weighted` combine still needs the real indicators to agree. `strategy.weights`
works on it like any other detector. Treat it as a sentiment-ish overlay, not a
strategy — it is a language model pattern-matching on numbers, not an edge.

**Backtesting it is deliberately awkward.** The background-refresh design that
keeps live trading fast means a backtest races past the model and mostly reads
an empty cache; `cmd/backtest` and `cmd/optimize` warn when `ai` is in the
detector set for exactly that reason. Setting `ai.blocking: true` makes the
backtest actually wait for the model — correct, and *very* slow. Do not enable
live trading on the back of results the advisor never participated in.

| Setting | Meaning |
|---------|---------|
| `enabled` | master switch; off ⇒ the detector exists but always votes HOLD |
| `provider` | `ollama` (native `/api/chat`) or `openai` (any `/v1/chat/completions`: llama.cpp, LM Studio, vLLM, OpenRouter, OpenAI) |
| `base_url` / `model` / `api_key` | endpoint, model name, bearer token (`BOT_AI_API_KEY`) |
| `timeout` | per-request deadline (default 20s) |
| `refresh_interval` | minimum time between calls for one symbol (default 15m) |
| `max_age` | verdicts older than this are not used (default 1h). Must not be shorter than `refresh_interval`, or every verdict would expire before the next arrived — refused at load |
| `blocking` | wait for the model instead of using the cache — backtests only |
| `temperature` | 0 (default) = as reproducible as the model allows |
| `max_concurrent` | in-flight request cap (default 2) — protects a single local GPU |
| `min_confidence` | below this the model's own answer is demoted to HOLD. A BUY/SELL whose confidence is missing, unreadable (`"0,8"`, `"7/10"`) or not above 0 is never a vote, whatever this is set to |
| `max_strength` | ceiling on the strength it contributes (0..1) |
| `system_prompt` | replaces the built-in instruction prompt |

Env overrides: `BOT_AI_ENABLED`, `BOT_AI_PROVIDER`, `BOT_AI_BASE_URL`,
`BOT_AI_MODEL`, `BOT_AI_API_KEY`.

## ML detector (learned signal)

A logistic-regression classifier over the **same** normalised indicator readings
the rule-based detectors vote on, retrained on a rolling window, estimating the
probability that the next `horizon` bars are up.

```yaml
ml:
  enabled: true
  horizon: 10            # predict the next 10 bars
  threshold: 0.005       # "up" means +0.5% or better over that window
  train_bars: 1500       # rolling training window
  retrain_every: 250     # refit every 250 new bars
  min_skill: 0.02        # its held-out votes must beat chance by 2pp to vote
  max_strength: 0.5      # cap its influence

strategy:
  detectors: ["trend", "macd", "ml"]
```

**Every prediction is out-of-sample.** The model trains only on rows whose
forward window has already closed, then predicts the current bar. That makes a
backtest of it a continuous walk-forward rather than a fit to the data it's
scored on — the distinction between a plausible ML signal and a fantasy. Two
tests pin this down: the sample builder must leave the final `horizon` bars
unlabelled, and adding future bars must not change any sample already built.

**`min_skill` is the important setting.** Bar-to-bar direction on liquid
instruments is close to unpredictable from indicators alone, and a model that
learned nothing still emits confident-looking probabilities. `min_skill` is the
margin by which the model's votes must beat a vote cast by chance; below it the
detector says so and votes HOLD:

```
ml: skill 0.004 below min_skill 0.020 (votes hit 0.072 vs chance 0.068, 31 votes)
```

Skill is measured the way the detector trades, and out of sample. A twin of
the model is fitted on the older three quarters of the training window (minus
`horizon` rows, whose labels would read into the rest) and votes BUY/SELL on
the newest quarter at `buy_prob`/`sell_prob`. Its hit rate is compared with
what a vote on the same side gets by chance: the held-out rally rate for a BUY,
its complement for a SELL. Fewer than 20 held-out votes count as no skill.
It used to be training accuracy at a 0.5 cutoff against the majority class —
with a 0.5% rally label no model ever outputs p > 0.5, so skill was always
exactly 0 and the shipped `min_skill` silenced the detector whatever it had
learned. Scored on its own training rows instead, a model of a pure random
walk read a skill of 0.20: the rows it votes on are the rows it fits best.

Expect to see that message a lot. It is the honest answer, not a malfunction.

**Its features follow the strategy it votes in.** The indicator periods the
model reads (RSI, MACD, ADX, the trend window, …) are the asking strategy's,
not a set fixed at startup: a strategy edit, a per-symbol override and an
optimizer trial each get features on their own periods, and a change refits
the symbol's model. Each training row's features read the history before the
window as well, so the first rows of a window see the same full trend window
and settled indicator seeds that a live prediction does.

**Why regression and not something bigger.** With ~16 features and a few
thousand noisy samples the bottleneck is signal in the data, not model capacity;
a gradient-boosted forest fits this better and generalises no better. Logistic
regression is also inspectable — the reason string names the features driving the
lean, since standardised weights are directly comparable:

```
ml: p(up 10 bars)=0.612 BUY (acc 0.58 vs base 0.54, n=1240) [trend_pct+0.41 adx+0.22 rsi-0.19]
```

| Setting | Meaning |
|---------|---------|
| `enabled` | off ⇒ the `ml` detector exists but always votes HOLD |
| `horizon` / `threshold` | the label: is the close `horizon` bars ahead at least `threshold` higher? |
| `train_bars` / `retrain_every` / `min_samples` | rolling window, refit cadence (in **bars**, so backtest and live agree), and the floor to fit at all |
| `buy_prob` / `sell_prob` | the vote thresholds, as distances from 0.5 applied around the window's rally rate (default 0.58 / 0.42 = ±0.08). The label is "rose by `threshold` or not", so p(up) of a calibrated model sits near that rate; read against 0.5 it voted SELL on nearly every bar. When rallies are rare the SELL line falls below zero and the model never sells — it cannot have learned a fall |
| `min_skill` | how far its held-out votes must beat chance before it may vote (see above) |
| `max_strength` | ceiling on the strength it contributes (0..1); 0 = the default, 1 — not mute. Outside 0..1 is refused at load |
| `epochs` / `learning_rate` / `l2` | fitting hyperparameters; `l2` is the brake on overfitting the window. The intercept starts at the window's rally rate, so 300 epochs fit a calibrated p(up) rather than one about twice the rate the vote lines centre on |

Fitting is deterministic (full-batch descent, no shuffling), so a model can be
reasoned about and a backtest reproduced. `GET /api/ml` reports how many symbols
have models, refit and failure counts, and the mean skill across them — if that
average sits at zero, the models are finding nothing.

Warmup is much longer than a technical detector's: a training window plus the
horizon whose labels aren't knowable yet. Backtests are correspondingly slower
(one refit per `retrain_every` bars per symbol).

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

A buy still in flight — a limit order resting at the broker, or an order the
broker did not confirm — counts against `max_total_invested` and, for a symbol
not yet held, against `max_open_positions`, so a second buy cannot spend the
headroom the first already claimed (a refusal then reads "N held or in
flight"). An unconfirmed order keeps its claim until you reconcile it in the
diagnostics tab.

The bot refuses to start with trading enabled unless `max_order_value` and
`max_total_invested` are set.

### Limits that scale with the account

Every money limit above has a `_pct` twin taking a **fraction of current
equity** instead of a fixed amount. `0` means "not set" for either half, and
when both are set the **tighter** one binds. That gives three modes:

| written as | behaviour |
|---|---|
| `max_total_invested: 950` | fixed ceiling, whatever the account does |
| `max_total_invested_pct: 0.95` | scales with the account forever |
| **both** | **"use 95%, up to 5000"** — scales while small, then stops |

The third is usually the one you want:

```yaml
limits:
  max_total_invested: 5000       # never more than 5000...
  max_total_invested_pct: 0.95   # ...and never more than 95% of equity
```

| equity | resolves to | binding half |
|---|---|---|
| 1,000 | 950 | 95% of equity |
| 5,000 | 4,750 | 95% of equity |
| 6,000 | 5,000 | the 5000 ceiling (crossover ≈ 5,263) |
| 50,000 | 5,000 | the 5000 ceiling |

A fixed amount goes stale the moment the account grows — 950 is the whole book
at 1000 equity and 9.5% of it at 10000, and nothing says so. Set the amounts to
`0` and use percentages alone and the book scales itself:

```yaml
limits:
  max_total_invested: 0
  max_total_invested_pct: 0.95
  max_per_position: 0
  max_per_position_pct: 0.20
  max_order_value: 0
  max_order_value_pct: 0.20
  cash_reserve: 0
  cash_reserve_pct: 0.05
```

Position *sizing* already scaled: `max_pct_per_asset_class` and
`min_positions_per_asset_class` are fractions, so a slot is a constant share of
equity at any account size. These limits were what stopped that reaching the
order.

> **`cash_reserve_pct` resolves the other way.** It is a floor, not a ceiling,
> so the **larger** of the two halves wins — the more conservative reserve. The
> ceilings take the smaller. Resolving the reserve like a ceiling would leave a
> grown account with a token buffer.

Equity includes unrealised P&L, so a winning book raises its own limits and
buys more near the top — that is what a percentage allocation means. The other
direction is harmless: tighter limits gate buys only, never exits.

**Spreading capital across positions:** set `engine.min_positions: N` so no
single buy takes more than its share of what can actually be invested. `0`
leaves the per-class slots (`portfolio.min_positions_per_asset_class`) to do
it. Editable live in the UI ("Position sizing") or via `PUT /api/sizing`.

## Position sizing

There is no fixed order size. Every buy starts from the **money-limit
headroom** — the tightest of `max_order_value`, `max_per_position`, the
position slot, `max_total_invested`, `max_daily_spend` and `cash_reserve` —
and risk scales it down from there. Nothing to keep in sync with `limits:`,
because the limits *are* the size.

```yaml
engine:
  min_positions: 4           # spread the budget over at least this many
  max_risk_size_usd: 5000    # absolute cap on what sizing may produce
  min_order_size_usd: 100    # below this, refuse the order rather than place it
```

**`min_positions`** stops one buy taking the lot: each position is capped at
its share of what can actually be invested — equity less `cash_reserve`, capped
at `max_total_invested`. `portfolio.min_positions_per_asset_class` refines it
per asset class and wins for the classes it names; whatever it does not claim
is what an unnamed class gets. See [Portfolio limits](#portfolio-limits-concentration).

**Risk scaling** is always applied when a stop is configured. The headroom is
reduced by how much wider this trade's actual stop is than the `stop_loss_pct`
baseline. Buying 500 of a calm ETF behind a 2% stop risks 10; buying 500 of a
volatile small cap behind a stop four times wider is a different trade, and it
gets proportionally less capital. It can only *reduce* a position, never
enlarge one — a way to take less risk, not a licence to take more.

> **It does nothing until an ATR stop can out-widen `stop_loss_pct`.** With no
> ATR stop every trade uses the same fixed percentage, so the ratio is exactly
> 1 and every trade gets the full headroom.
>
> That threshold is harder to reach than it looks. ATR averages *per-bar* true
> ranges, so on a 1-minute feed it is tiny — measured on AAPL, ATR(14)/price is
> **0.074%**, meaning a 5% stop would need `atr_stop_mult: 68`. Lengthening
> `atr_period` does not help (390 bars gives 0.096%); only a coarser
> `bar_interval` does. At daily bars ATR(14) is 2.4% and the textbook `2` lands
> right on a 5% stop — which is where that number comes from.
>
> So on 1-minute bars, risk sizing is inert unless you lower `stop_loss_pct` or
> trade coarser bars. It is not broken; it has nothing to measure. See the
> calibration table in `data/config.example.yaml` under `exits:`.

**`min_order_size_usd` refuses, it does not round up.** An order the scaling
puts below it is not placed. Raising it back up would enlarge exactly the
trades the scaling just judged too risky to hold at full size — which is what
this setting used to do, and it had the sizing backwards.

The refusal is recorded once per symbol, not once per evaluation: the loop
revisits every symbol every `eval_interval`, and a stop stays wide for as long
as it stays wide.

## Portfolio limits (concentration)

The money limits are per-order and per-symbol: they will happily let the bot hold
twenty positions that are really one bet. Ten US megacap tech names at 300 each
is not a 3000 diversified book — it is 3000 of Nasdaq with extra commission, and
it draws down like it.

```yaml
portfolio:
  enabled: true
  max_per_sector: 400            # combined market value per sector label
  max_positions_per_sector: 3
  correlation_threshold: 0.8     # above this, two symbols are the same bet
  max_correlated_positions: 2
  correlation_lookback: 200      # bars

  # Share of account equity per asset class, as a FRACTION (0.60 = 60%).
  max_pct_per_asset_class:
    etf: 0.60
    stock: 0.30
    crypto: 0.05

sectors:
  AAPL: tech
  MSFT: tech
  XOM: energy
```

Correlations are computed on the same bars the strategy trades, and in a
backtest only on the history that existed at that bar — a correlation measured
over the whole dataset would let a decision on bar 100 be influenced by bar 5000.

**`sector_caps_per_asset_class`** decides whether a tech ETF and a tech stock
compete for the same slot. Off (the default) they do — a sector falls together
whatever wrapper it comes in, which is what a sector cap exists to catch. On,
they are counted separately: a sector ETF holds dozens of companies while a
stock holds one, so one unit of each is not one unit of the same risk, and
`max_pct_per_asset_class` already bounds how much of the book each wrapper may
take. With a cap of 2 that reads "two single-stock bets **and** two diversified
bets per sector". The screener's `max_symbols_per_sector` follows the same
setting, so selection and enforcement never disagree, and a refusal names the
bucket it was counted in (`etf tech` rather than just `tech`).

Three deliberate choices: an unmapped symbol isn't subject to the sector caps
(there is nothing to compare it against, though the correlation check still
applies); a symbol with no history is **allowed** rather than blocked forever;
and topping up a position already held is never blocked by the sector or
correlation caps, since the concentration decision was made when it was opened.
As everywhere else, exits are never gated.

### Asset allocation (`max_pct_per_asset_class`)

The sector and correlation caps ask "is this the same bet I already have on?".
This one asks a different question: **does the book still look like the
allocation I chose?** A plan of "mostly trackers, a small single-stock sleeve"
is not enforced by any per-order limit — the bot will happily drift into the
opposite one good signal at a time, and every individual order will have looked
reasonable.

Once a class is at its share of equity, further **buys** in it are refused and
recorded as blocked with a reason. Unlike the other caps this one **does** apply
to topping up a position you already hold, including scale-ins: a top-up spends
the same budget, and "no more than 30% in ETFs" that stops applying the moment
you already own the ETF is not a limit at all. Sells and exits are never
affected.

Like the sector and correlation caps, this gates the bot's own buying. A
**manual** order placed through the UI or `POST /api/order` still passes the
hard money limits in `limits:` but is not subject to the portfolio caps — an
explicit human order is treated as an override, not a signal.

The denominator is account equity (cash + holdings), not invested capital.
That is what "30% of the portfolio" means to the person writing it, and it is
the only denominator that does not move as a result of the order being judged —
buying converts cash into a holding, leaving equity unchanged.

Percentages are **fractions**, like every other `_pct` setting: `0.30` is 30%.
Writing `30` is rejected at load rather than silently read as 3000%, which would
be a cap that can never bind on an operator who believes they are capped.

**You do not normally need to classify anything by hand.** Every tracked symbol
is classified from the data provider's instrument search, falling back to the
broker's own instrument list (Saxo's `AssetType`, Trading 212's instrument
`type`) for what the provider cannot place. The result is cached in the database
and survives restarts. Provider spellings are normalised, so Yahoo's `EQUITY`,
Saxo's `Stock` and Trading 212's `STOCK` all land in the same `stock` bucket.

Classes: `stock`, `etf` (including ETC/ETN wrappers), `fund`, `bond`, `crypto`,
`option`, `future`, `cfd`, `currency`, `index`. A class you set no cap for is
unconstrained.

**`unclassified` is a cappable class.** A symbol nothing could place is never
guessed into a real class — it is budgeted under the reserved `unclassified`
key instead:

```yaml
portfolio:
  max_pct_per_asset_class:
    stock: 0.70
    etf: 0.30
    unclassified: 0.10   # everything no source could name
```

Leave the key out and that bucket is unconstrained, as before. Write it and the
unknown positions are budgeted like any other class — including symbols not yet
bought, which is the difference that matters: the alternative is spotting an
unclassified holding in the allocation panel and hand-writing an
`asset_classes:` entry for it, one symbol at a time, after the fact.

### Splitting a class budget (`min_positions_per_asset_class`)

A percentage cap is spent first-come-first-served. One early conviction buy can
take the whole 30% of ETFs; the cap holds, and the class is then closed to every
other candidate — the limit did its job and the diversification it was written
for never happened. This divides the budget instead of hoping the sizing leaves
some:

```yaml
portfolio:
  max_pct_per_asset_class:
    stock: 0.70
    etf: 0.30
  min_positions_per_asset_class:
    stock: 2      # no single stock position may exceed 35% of equity
    etf: 3        # no single ETF position may exceed 10% of equity
```

It is the per-class twin of `engine.min_positions`, which splits
`max_total_invested` the same way. The slot is a ceiling on the **position**,
not on the order: each buy — a scale-in add included — is clamped to what is
left of that symbol's slot, so a ladder cannot walk one name up to the whole
class budget. The smaller of the slot and `limits.max_order_value` wins, and a
buy that hits it is sized down rather than refused (the trade's reason reads
`clamped by min_positions_per_asset_class`).

A ceiling, never a floor: it does not make the bot buy N positions, and it is
not a target allocation. `0` or `1` means "no split". A class named here needs a
`max_pct_per_asset_class` entry — without a budget there is nothing to divide —
and once `portfolio.enabled` is true, naming one that has no budget is refused at
load rather than sitting there enforcing nothing. With the block off both
settings are inert, so they can be staged together and switched on later.

Unlike the sector and correlation caps, this one is enforced in the risk manager
alongside the money limits rather than in the portfolio guard, so it applies to
**manual** orders too. That is deliberate: it sizes rather than refuses, one
enforcement point means the live engine, the scale-in ladder and the backtester
cannot drift apart on it, and a hand-placed order that quietly consumes a whole
class budget is the same problem whoever placed it.

**`0` means "no cap", not "blocked".** A class listed at `0` is treated exactly
like a class you never mentioned. There is currently no way to express "never
buy this class"; what controls that is your watchlist and screener universe,
since nothing else can enter the book.

**In practice only `stock` and `etf` are cappable**, because only those are
tradeable at all — on every supported broker, for three reasons that have
nothing to do with which broker you pick:

- the engine is **long-only** (`risk.Authorize` refuses a sell with no position
  held), so anything needing a short leg is out;
- an `Order` is `{symbol, side, qty, type, limit}` — no strike, expiry, contract
  multiplier, margin or leverage exists anywhere in `broker`, `engine` or `risk`;
- the adapters are cash-equity adapters. Trading 212 speaks only `/equity/...`
  (its Invest universe; the CFD product has no public API). Alpaca hardcodes
  `time_in_force: "day"`, which its crypto venue rejects. Saxo floors every
  order to whole units, ships scoped to `"Stock,Etf"`, and sends no
  `ToOpenClose`, which derivatives require.

A bond ETF classifies as `etf` and trades normally; an individual bond does not.
Crypto is data-only by design — trackable, backtestable and paper-tradeable
against `broker: sim`, never ordered at a live broker. The other labels remain
useful to the screener's `by_asset_class`, they are just not something a cap can
bind on.

To correct a classification, or to cover a symbol nothing can place, use the
override block (or the Asset class map field in the settings UI):

```yaml
asset_classes:
  IWDA.AS: etf
  PRAJL.XC: stock
```

## Entries

Three settings decide when a *new* position — or an addition to one — may open.

```yaml
engine:
  allow_pyramiding: false   # let repeated BUY signals add to an open position
  skip_full_slots: false    # stop evaluating symbols that cannot be bought

  scale_in:                 # averaging down, on an explicit capped schedule
    enabled: false
    step_pct: 0.03          # 3% below the AVERAGE entry before the next add
    max_adds: 2             # hard cap — required
    size_pct: 0.5           # each add is half a normal order
    require_signal: true    # only add while the strategy still says BUY
```

**`skip_full_slots`** is a quota saving, not a CPU one. With
`max_open_positions` reached, a BUY on an unheld symbol cannot be authorized, so
the work of deciding on one is spent to be refused — and that work includes LLM
advisor calls and ML inference. Held symbols are always evaluated: they still
need their sell signals, and their protective exits run either way.

**`scale_in` is averaging down.** Read this before enabling it. It improves the
entry price of every trade that eventually recovers and increases the size of
every trade that does not. It converts a series of small losses into a smaller
series of large ones, and it is the mechanism behind most blown retail accounts.
Three things are enforced rather than advised, because they are what separates it
from a martingale:

- **`max_adds` is required** and must be > 0. An uncapped ladder has no bounded
  worst case.
- **`size_pct` may not exceed 1.** An add larger than a normal order is
  martingale sizing; the config loader and the API both reject it.
- **The stop-loss still applies to the enlarged position.** Nothing here exempts
  a scaled-in position from the exits.

Each add is measured against the *average* entry, which every add pulls down, so
the ladder tightens on itself rather than firing repeatedly through one slide.
It is a separate pass from `allow_pyramiding` on purpose: pyramiding adds because
the strategy still likes the symbol, scale-in adds because the price fell. With
`require_signal` off, the ladder buys purely on the drawdown — a mean-reversion
bet the strategy never made.

## Order execution

A market order is the simplest thing that works and the most expensive: it
crosses the spread every time and, in size, walks the book.

```yaml
engine:
  execution:
    type: "limit"             # or "market" (default)
    limit_offset_pct: 0.0005  # rest 5 bps inside the touch
    limit_timeout: 2m         # give up on an unfilled order after this
    market_fallback: true     # then re-send it as a market order
    slice_notional: 2000      # split bigger orders into clips of this size
    max_slices: 5
```

A limit order is placed *passively* — below the market for a buy, above for a
sell — so the spread is earned rather than paid, at the risk of not filling;
hence the timeout and fallback. Slicing works a large order into the book
instead of taking it in one clip. Only the Saxo adapter paces the children
(by `safety.min_order_interval`); on Alpaca, Trading 212 and the simulator they
are submitted back-to-back, so slicing there caps each clip's size but does not
spread the clips over time.

**Protective exits ignore all of this and always go to market.** A stop resting
unfilled at a limit price while the market runs away from it is precisely the
outcome the stop existed to prevent.

**`type: "limit"` needs a broker that can place one.** Trading 212's API
exposes only a market-order endpoint, so against it a limit order is sent to
market and crosses the spread anyway — `limit_offset_pct`, `limit_timeout` and
`market_fallback` then have nothing to act on. The adapter says so once per
run in the diagnostics tab's API-errors list rather than letting the setting
look effective. Saxo places real limit orders and can list and cancel them.

**A resting limit also needs a broker that can list and cancel it**, because
the timeout and fallback work by finding the order and cancelling it. Alpaca's
adapter does neither, and neither does the simulated broker test mode uses —
there an expired limit sell used to block stop-losses for the symbol for good,
and buy reservations were never released. So on such a broker `type: "limit"`
falls back to market orders, and says so once per broker in the diagnostics
tab.

**The market fallback is a new order, and is treated as one.** It goes out up
to `limit_timeout` after the decision that sized it, so a fallback **buy** must
pass the same gates any buy does at that moment: if trading was switched off,
a drawdown halt tripped or the breaker fired while the limit rested, no
fallback is sent, and it is re-authorised against the money limits as they
stand. A fallback **sell** is an exit and is never gated.

It also re-sends only what did **not** fill. A limit order can fill partly at
any point while it rests, and the cancel does not say how much — so the bot
reads the broker's position after the cancel, books the part that filled and
sends only the remainder. When that cannot be pinned on one order (another
slice of the same symbol is still resting, or a buy of it is unconfirmed) a
buy fallback is skipped and recorded instead: under-buying is the safe side,
and any shares that did fill are adopted as an untracked holding. A sell
fallback is simply capped at what is still held.

**Limit prices go out at the precision the broker accepts.** Alpaca takes four
decimals below $1 and two above; the price is rounded toward the safe side — a
buy limit down, a sell limit up — so a rounding step never pays more, or
accepts less, than the limit meant. **Saxo** prices — limit, bracket legs and
modifications, after the currency conversion — are put on the instrument's own
tick grid (`TickSize` / `TickSizeScheme` from its instrument details), to the
nearest tick; Saxo refuses a price off the grid, and every converted price is
off it. A modification keeps the order's own duration (a day order stays a day
order), and the open-orders list reports prices in your reporting currency, the
one a modification is typed in — or names the currency when it cannot convert.

**Exchange sessions.** Without a broker trading calendar (Trading 212, Alpaca),
orders — exits included — are held outside the venue's session, which the bot
reads from the symbol's suffix. The German regional exchanges keep their own
hours rather than Xetra's 09:00–17:30: `.F` (Frankfurt), `.SG`, `.MU`, `.DU`,
`.HM`, `.HA`, `.BE` and `.BM` all trade shares and ETPs 08:00–22:00. A stop on a Stuttgart
listing that triggers at 18:00 therefore goes out at 18:00, not the next
morning. Holidays and auctions are not modelled.

## Protective exits

Before any strategy signal, every open position is checked against the rules in
`internal/exits`. A triggered exit always wins (force-sells):

- `stop_loss_pct` — sell when price falls this far below entry
- `take_profit_pct` — sell when price rises this far above entry
- `trailing_stop_pct` — sell when price falls this far below the peak since entry

Exits respect the trading toggle (recorded as dry-run when trading is off) and
still pass through the risk manager. **Protective exits always go to market**
regardless of the execution settings below: when a stop fires, filling matters
more than filling well.

**Volatility-adaptive stops.** A fixed 5% stop is far too tight on a volatile
instrument — ordinary noise stops you out — and far too loose on a calm one. An
ATR multiple is the same *statistical* distance on both:

```yaml
exits:
  atr_period: 14
  atr_stop_mult: 2      # stop two ATRs below entry
  atr_trail_mult: 3     # trail three ATRs below the peak
```

When both a percentage and an ATR multiple are set, the **wider** stop wins, so
the percentage acts as a floor — a momentarily tiny ATR can't place the stop on
top of the current price and guarantee an instant exit. With no ATR available
yet, the percentage rules apply unchanged. An ATR so large that `entry −
mult × ATR` would fall below zero cannot place an ATR stop. A **new entry** is
then refused — it would otherwise go out at full size behind the narrow
percentage stop, the most volatile entry with the least room. A **held**
position keeps its percentage stop (the stop reads the current ATR, so a crash
bar must not widen it to nothing), and with ATR-only exits it gets a stop at 1%
of the entry rather than none. The ATR trail behaves the same way.

**Time-decaying profit ladder (`roi`).** Demand a lot early, settle for less as
a trade goes stale — the entry that hasn't worked in six hours probably isn't
going to:

```yaml
exits:
  roi:
    "0s":  0.10   # 10% straight away
    "30m": 0.05   # after 30 minutes, 5% is enough
    "2h":  0.02
    "6h":  0      # after six hours, exit at any profit
```

The ladder and `take_profit_pct` combine into whichever target is nearer. Keys
are Go durations without a dot — write `"90m"`, not `"1.5h"`: the settings
editor addresses a value by a dotted path, so a dotted key could never be saved
from the UI, and it is refused at load.

**Two-stage trailing stop.** One trailing distance has to be two contradictory
things at once: loose enough early that ordinary noise doesn't end a trade before
it works, and tight enough late that a winner isn't handed back. So it is two:

```yaml
exits:
  trailing_stop_pct: 0.10          # loose, while the trade is finding its feet
  trailing_offset_pct: 0.05        # once up 5%…
  trailing_tight_pct: 0.02         # …trail 2% below the peak instead
  trailing_only_after_offset: false # true = no trail at all below the offset
```

With `trailing_only_after_offset`, the stop-loss is the only downside rule until
the position is far enough ahead — use it when the trail exists to protect a
gain, not to cap a loss. With no offset set, the trail behaves exactly as before.

`atr_trail_mult` still applies in the tight stage, and the wider trail wins
there too: past the offset the distance is the larger of `trailing_tight_pct`
and `atr_trail_mult × ATR`. With `trailing_tight_pct: 0.02`, `atr_trail_mult: 3`,
an ATR of 1.5 and a peak of 110, the trail is 105.50 (3 ATR), not 107.80 (2%).
That is deliberate — a tight trail inside the instrument's own noise band is
the exit the ATR multiple exists to prevent — but it means the tight stage
only tightens on instruments whose ATR is small relative to it.

**Adding to a position re-bases the trail.** The stage is judged from the peak
against the *average* entry, and a scale-in or pyramid moves the average under
a peak reached against the old one. Left alone, averaging down from 100 to 97.5
under a 104.5 peak would read as a 7% gain, arm the tight stage, and put the
trail above the price — selling the enlarged position at a loss on the next
check. So an add resets the peak to the new position's level and keeps the
trail that was in force just before it as a **floor**: the trail starts again
from the new basis but never sits below where it already was. The floor is
saved with the position, survives a restart, and lapses if you switch every
trailing distance off — or **change** the trailing settings: the floor is the
trail at the settings of the add, and kept after you widen a 3% trail to 10% it
would hold exactly the positions you scaled into at the old tight level. The
backtester applies the same rule.

**Time barrier (`max_hold`).** The third barrier of the triple-barrier method:

```yaml
exits:
  max_hold: 72h   # close unconditionally after three days
```

This closes a gap nothing else covers. The ROI ladder decays the profit *target*,
so it can only ever exit at breakeven or better; a position sitting at −3% for
three weeks trips no stop, meets no target, and quietly occupies a slot the next
signal needed. `max_hold` exits it at whatever the market offers. A price barrier
broken in the same bar always wins — that's the difference between filling at
the stop and filling at the close.

`max_hold` measures **wall clock** from the entry timestamp, not trading time:
weekends, holidays and overnights all count. `720h` is 30 *calendar* days, about
21 trading days; for 30 trading days use `~1000h`.

That also makes it the only exit that can newly trigger while the venue is shut.
Every other barrier is compared against a price, and out of hours the price cache
is frozen at the last tick — a barrier that did not trip at the close will not
trip overnight. The clock does not stop.

**Exits defer while the venue is closed.** With `engine.market_hours_check: true`
a triggered exit waits for the session to reopen instead of being submitted to a
venue that cannot fill it. `checkExits` runs every `eval_interval`, so without
this a barrier tripping on a Saturday resubmits every 30 seconds until Monday —
thousands of rejected orders against a rate-limited broker. Nothing is lost by
waiting: `max_hold` only grows, and a price barrier is re-tested at the open
against a fresh quote rather than Friday's stale one. It also restores backtest
parity, since historical candles only exist during sessions and the backtester
could never fire a 03:00 Saturday exit in the first place. Symbols the exchange
calendar does not recognise are treated as open, so a missing mapping can never
strand a position.

**Scale-out ladder (`partial_take_profit`).** Sell part of a position as it
clears successive gains instead of holding all of it for one all-or-nothing exit:

```yaml
exits:
  partial_take_profit:
    - profit_pct: 0.05
      sell_pct: 0.33   # a third off at +5%
    - profit_pct: 0.12
      sell_pct: 0.33   # another third at +12%
```

Each rung fires at most once per position, and fractions are of the **original**
size — otherwise three 0.33 rungs would sell 70% of the position rather than all
of it. A gap clearing several rungs takes all of them at once. Progress is
persisted, so a restart mid-ladder doesn't re-sell a rung.

Be clear about the trade: **scaling out trades expectancy for variance.** Taking
a third off at +5% caps what that third can earn, and on the trades that would
have run to +30% it costs real money. What it buys is that fewer trades round-trip
all the way back to the stop. It is off by default; measure it in a backtest.

The downside rules always outrank the ladder — a trailing stop tripping on the
way back down from a rung closes the position rather than shaving it.

> The live engine and the backtester call the **same** rule engine
> (`internal/exits`), differing only in what they compare the levels against: a
> live tick, or a bar's high and low. That's deliberate — two copies of "is this
> position stopped out?" is how a backtest quietly stops describing the bot.

## Protections

Money limits cap what is at stake at any one moment. They cannot see *patterns*:
a strategy that is wrong about a symbol four times in an hour, or one whose
equity has slid 20% below its peak, is inside every per-order limit while it
loses steadily. `internal/protect` watches outcomes over time and takes the bot
off the field.

```yaml
protections:
  enabled: true
  cooldown_after_loss: 30m    # no new entry in a symbol for this long after a loss
  stop_guard_trades: 4        # N protective stop-outs of one symbol...
  stop_guard_lookback: 24h    # ...within this window...
  stop_guard_lock: 4h         # ...locks that symbol for this long
  max_drawdown_pct: 0.20      # halt ALL automated trading this far below the equity peak
  max_daily_loss_pct: 0.08    # halt after losing this much of the day's starting equity

  low_profit_trades: 4        # N closed trades in one symbol...
  low_profit_lookback: 168h   # ...within this window (7 days)...
  low_profit_min_pnl: 0       # ...netting no more than this...
  low_profit_lock: 24h        # ...lock that symbol for this long
```

**The low-profit guard** catches a failure the others cannot see. The stop guard
counts *stop-outs* — a symbol behaving unlike the backtest. The drawdown halt
watches the whole account. Neither notices a symbol the strategy keeps trading to
a small net loss: it trips no stop, moves the account very little, and just bleeds
commission and spread. This one counts money, per symbol, over a window. It waits
for `low_profit_trades` closes before judging — two losses on a strategy that wins
six times in ten is ordinary, and locking on that would remove symbols for
behaving exactly as designed. Set `low_profit_min_pnl` above zero to demand the
symbol actually pays for itself rather than merely avoiding a loss.

Two invariants: protections gate **entries only** — an exit is never blocked, so
a cooldown can't trap you in a losing position — and a halt is **sticky**. It
stays until `POST /api/protections/resume`, which also re-bases the drawdown peak
to current equity. A protection that cleared itself on a timer would let an
unattended bot walk straight back into whatever was draining the account.

**One trade, one verdict.** The cooldown, stop guard and low-profit guard judge
a *round trip*, once, when the position is fully closed — on the net P&L of all
its tranches. A trade that sells half at +5% on a scale-out rung and stops the
rest out at −2% is a winner and starts no cooldown; a stop-loss the broker fills
in two slices is one stop toward `stop_guard_trades`, not two. The
realized-drawdown halt is a running sum and takes each tranche as it lands. The
backtester applies the same rules, including `max_realized_drawdown_pct`.

**Deposits and withdrawals look like P&L.** The equity halts compare account
equity against a peak and the day's starting value, and nothing tells them a
change came from a cash transfer rather than trading. Withdrawing 8% of the
account trips `max_daily_loss_pct: 0.08`; a larger withdrawal can trip
`max_drawdown_pct` — annoying, but it fails safe. A same-day **deposit** is the
dangerous direction: it lifts equity above the day's start and hides that day's
real trading losses from `max_daily_loss_pct`. Move money while trading is off,
and after a withdrawal use `POST /api/protections/resume?rebase=1` to re-base
the peak. Without `rebase=1` a resume with no halt active is refused (409) and
re-bases nothing: a stale tab or a double click used to forgive a drawdown
that was building.

State and locks are at `GET /api/protections`; a single symbol can be released
with `DELETE /api/protections/lock/:symbol`.

### Giving up on a symbol (`drop_after_losses`)

Every other per-symbol guard locks an instrument out for a while and then lets
it back in. That is right for a bad patch and wrong for a bad match: some
instruments simply do not suit the strategy, and a lockout that expires hands
capital back to them every time it does.

```yaml
protections:
  drop_after_losses: 8      # 0 = never
```

After that many **consecutive** losing closes the symbol is removed from the
watchlist *and* its entry in `watchlist:` is commented out:

```yaml
watchlist:
  - SAP.DE
  # - MRVL   # dropped by drop_after_losses: 8 consecutive losing closes (max 8), 2026-09-12
```

The file edit is the point. Startup re-adds every symbol in `watchlist:`, so a
drop that only changed the running state would be undone by the next restart.

**Commented, never deleted.** A deletion is a decision you cannot see, review or
reverse. A comment keeps the symbol, the reason and the date, and un-dropping it
is removing one `#`.

Three rules make it safe to leave on:

- **A win or breakeven close resets the count**, so it only fires on a symbol
  that is losing and not recovering.
- **A symbol with an open position is never dropped** — that would strand it
  with no exits, no trailing stop and no way to sell. The streak keeps counting
  while held, so it drops on the close after the position is flat.
- **It only edits its own `watchlist:` block.** A flow-style list
  (`watchlist: [A, B]`) has no per-symbol line to comment, so it is refused with
  a clear error rather than being reformatted.

If you also use `autotune.on_loss`, set this well above it — the intent is
"re-tune a few times, and give up only if that did not help". Both read one
consecutive-loss counter, which resets only on a win, so `on_loss` re-tunes
every N in a row while the count climbs toward this threshold. At the threshold
the drop wins and no re-tune is requested.

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

**Friction** — the bid-ask spread and slippage — is charged on every simulated
fill on top of commission:

```yaml
costs:
  spread_pct: 0.0005    # FULL spread; half is paid on each fill
  slippage_pct: 0.0002  # extra adverse move per fill
  measure_spread: true  # …or estimate it per instrument instead
```

With `measure_spread`, each symbol is charged the spread estimated from its **own**
bars rather than one number applied to everything. That matters on a mixed book:
a megacap trades inside a basis point where a thin European ETF listing can cost
twenty times that, and charging both the same flatters one and libels the other.
The backtester measures from the series it was given; the live engine uses the
figures the screener produced. **`spread_pct` remains a floor** — an estimate is
evidence that the real cost is at least this high, not permission to assume a
symbol is cheaper to trade than you budgeted for.

## Autotune (unsupervised re-optimisation)

**Off by default, and the only feature in this bot that rewrites your trading
config without asking.** Read this section before enabling it.

```yaml
autotune:
  enabled: false
  interval: 24h             # restart-only to change; 0 also means 24h
  bars: 1440                # candle window per symbol per run
  min_trades: 10            # reject a candidate that barely traded
  objective: "blend"        # what "better" means
  min_return_pct: 0         # floor on the winner's own backtested return
  run_only_for_symbols: []  # allow-list; empty = every watchlist symbol
  exclude_symbols: []       # removed regardless of the allow-list
  on_loss:
    enabled: false
    after_losses: 3         # consecutive losing closes that trigger a re-tune
    cooldown: 6h            # minimum gap between reactive re-tunes per symbol
```

### What it does

On each pass it runs the same bounded parameter search the UI's "optimize this
symbol" button runs — then, unlike the button, **writes the winner straight
into `strategy_overrides` and into the live engine**, with no confirmation
step. The next evaluation trades the new parameters.

### What stops a bad proposal

Four gates, all of which must pass:

| gate | rejects |
|---|---|
| `min_trades` | a config that only "won" because it barely traded |
| `objective` | whichever candidate scores worst on your chosen metric |
| `min_return_pct` | a winner that beat a weak baseline while still losing money |
| dual baseline | a winner that beats an isolated backtest but not the symbol's **real** track record |

That last one matters most. An isolated backtest hands one symbol the full
account cash for the whole run — something live trading never does, since
capital is shared across the watchlist. So a proposal must beat both the
what-if *and* the symbol's actual history since the last autotune apply, on
win rate and expectancy specifically. The record's win rate is read smoothed
by its size — (wins + 1) / (trades + 2) — so one winning close counts as 67%,
ten in a row as 92%: a thin record is a low hurdle, a long one a high hurdle,
and neither is impassable. Read raw, one winning close was a 100% win rate that
a candidate measured over ten or more trades almost never matches, and the
symbol stayed un-tunable until it lost.

`min_trades` defaults to 10, deliberately stricter than the manual button's
floor of 3: a thin sample is a smaller risk on a human-reviewed click than on
an unsupervised, every-symbol, real-money scheduled write.

### `objective` — what "better" means

| value | ranks on |
|---|---|
| `blend` (default) | return and win rate, penalising drawdown |
| `return` / `winrate` | exactly one of those, alone |
| `sharpe` / `sortino` / `calmar` | risk-adjusted return |
| `profitfactor` / `expectancy` | trade quality, independent of position size |

`calmar` and `profitfactor` score smoothed ratios — CAGR ÷ (max drawdown + 1
point), and (gross win + m) ÷ (gross loss + m) with m the mean P&L per trade —
because the raw ratios are undefined (reported as 0) for a run with no drawdown
or no losing trade, and scoring that 0 ranked a flawless incumbent below every
candidate.

An unrecognised value falls back to `blend` rather than failing the load. This
only picks the search winner — the gate against real history always compares
win rate and expectancy whatever you set here.

### `on_loss` — reacting instead of waiting

With `on_loss.enabled`, a symbol that closes `after_losses` trades in a row at
a loss is re-tuned immediately rather than at the next scheduled pass.

It adds **no new way to win**: a streak only decides *when* to look, and the
proposal still has to clear all four gates above. `cooldown` bounds how often
one symbol can trigger it — checked again when the request is served, so a
request that waited behind a scheduled pass which has just tuned the same
symbol is dropped.

The loss counter resets only on a win, never on a tune — so it is "losses in a
row", not "losses since we last acted". The same counter feeds
`protections.drop_after_losses`, which is checked first and wins: a symbol bad
enough to drop is dropped rather than re-tuned. See
[Giving up on a symbol](#giving-up-on-a-symbol-drop_after_losses).

### Scoping it

`run_only_for_symbols` is an allow-list; empty means every watchlist symbol.
`exclude_symbols` is applied afterwards, so a symbol in both lists is excluded
— which is how you keep a hand-tuned override out of the scheduler's reach
without maintaining an allow-list of everything else.

### Before you enable it

- `interval` and the bar window are **restart-only**; the ticker is built once
  when the background goroutine starts.
- A symbol with fewer cached bars than the search needs is skipped for that
  run, silently.
- Only one search runs at a time process-wide, shared with the manual button
  and the screener probe. A reactive `on_loss` request that arrives while
  another holder has the slot is **retried five minutes later** — once per
  symbol however often it collided, and judged again on `enabled` and the
  cooldown when it runs; a scheduled pass that finds the slot busy also tries
  again five minutes later rather than waiting a whole `interval`. "Run
  autotune now" answers "started" only when it really got the slot, and a
  panic inside that pass is recorded rather than taking the bot down.
- A search's winner is a difference from the strategy the symbol ran when the
  search **began**. If you save the strategy while a search runs, its result is
  discarded — applied to the new base it would be a configuration no trial
  ever ran — and the next pass searches the new one. A shared-strategy save
  also re-applies every symbol's override on top of the new base at once, so a
  symbol trades live exactly what a restart would give it.
- Shutting the bot down stops a running search; a search that finishes after
  shutdown began applies nothing.
- Everything it writes is visible in `strategy_overrides` in your config file
  and in the UI's per-symbol view. If you do not recognise a parameter there,
  autotune put it there.

## Test mode (simulated trading over the real feed)

```yaml
test_mode:
  enabled: false
  duration: 48h           # 0 also means 48h
```

Test mode runs the whole decision path — live prices, real signals, real
sizing and risk checks — but swaps the broker for an in-memory simulator
**seeded from your actual account**: its cash and its open positions, at the
moment the window starts. It reverts to the real broker on its own when
`duration` expires.

It is the honest middle ground between `trading_enabled: false` (which
computes signals but never exercises the order path at all) and live trading.
Orders are placed, filled and closed, so `keep_interval`, the exit ladders and
realized P&L all behave as they will for real.

Fills use your configured cost model — commission, spread and slippage — so a
simulated result is not flattered by frictionless fills.

Trades taken in a window are marked `Simulated` in the trade log, so they never
get confused with real fills.

**Nothing simulated outlives the window.** The bot's own record of open
positions — entry prices, trailing peaks, scale-out progress — is set aside when
the window opens and restored when it closes, and nothing simulated is written
to the database: not the positions, not the day's spend. A simulated buy used
to leave a position record for stock you never owned (which then tripped a false
drawdown halt), and a simulated sale deleted the record of a real holding.

Three things it refuses to do:

- **It will not start while a protection halt is active.** Resume the halt
  deliberately first, rather than letting a fresh simulated window paper over a
  real one.
- **It will not start while a real limit order is resting.** Its watcher would
  wake inside the window and settle the real order against the simulator.
- **It ends early if the trade-rate circuit breaker trips.** A bot placing
  orders fast enough to trip it is not one worth continuing to exercise.

Start and stop it from the UI, or set `enabled: true` to open a window
automatically at startup.

## Notifications

An unattended bot that can only be observed by opening its web UI is one you
will not observe.

```yaml
notify:
  enabled: true
  events: ["trade", "halt", "error", "audit"]   # also "blocked" (noisy)
  min_notional: 0                      # suppress fill alerts below this order value

  webhook: ""                          # any URL — gets the event as a JSON POST
  discord_webhook: ""
  telegram_token: ""
  telegram_chat_id: ""

  pushover_token: ""                   # application/API token from pushover.net
  pushover_user: ""                    # user or group key
  pushover_device: ""                  # optional: one device only
  pushover_priority: 0                 # -2 silent … 1 high; halts/errors get +1

  apprise_url: ""                      # Apprise API server endpoint
  apprise_services: ""                 # stateless mode: comma-separated service URLs
  apprise_tag: ""                      # stored config: which entries to notify
```

| Sink | Reaches |
|------|---------|
| `webhook` | anything that accepts a JSON POST (Slack, Mattermost, n8n, your own script) |
| `discord_webhook` | a Discord channel |
| `telegram_*` | a Telegram chat, via a bot |
| `pushover_*` | phones, with a real title and priority — a halt can bypass quiet hours while a routine fill doesn't |
| `apprise_*` | the ~100 services [Apprise](https://github.com/caronc/apprise) supports — email, Matrix, ntfy, Gotify, Signal, SMS — through one [Apprise API](https://github.com/caronc/apprise-api) server, so this repo doesn't need an adapter per service |

The kinds: `trade` (fills), `halt` (a circuit breaker or protection stopped
trading, or an order the broker never confirmed), `error` (a failed order —
with the broker's error text — or an upstream API failure, or an instrument the
broker refuses), `audit` (the bot changing itself: an autotune apply, a
screener pass changing the watchlist, a cancelled, modified or bracket-submitted
order), and `blocked` (an order the limits refused; noisy). A subscription
missing `audit` does not hear about those. An unknown kind is refused at load.
Each message is cut to the sink's limit rather than rejected by it, and a
webhook that answers with a redirect counts as a failed delivery — write the
final `https://` URL. A **halt** that meets a 429 or a 5xx is retried up to three
times, honouring the sink's `Retry-After` (capped at 30s); routine events are
not retried. On shutdown the bot waits, briefly, for the halts and errors still
queued to go out.

Every configured sink gets every subscribed event; a sink needing two
credentials stays inactive until both are set. Apprise takes either a stored
configuration (`http://apprise:8000/notify/mykey`) or the stateless endpoint
(`http://apprise:8000/notify` plus `apprise_services`), and event kinds map onto
Apprise's own types (trade→success, blocked→warning, halt/error→failure).

Pushover's priority 2 (emergency, repeats until acknowledged) is deliberately
unreachable: it needs a retry/expire and acknowledgement flow, and a trading bot
that can wake you every 30 seconds until you tap a button is a footgun.

Delivery never blocks trading: events are queued and sent by a background
worker, and dropped when the queue backs up — a slow chat server must not slow
the trading loop. Counters (sent / failed / dropped) are at `GET /api/notify`,
and failures land in `GET /api/errors` under source `notify`. Env overrides:
`BOT_NOTIFY_WEBHOOK`, `BOT_NOTIFY_DISCORD`, `BOT_NOTIFY_TELEGRAM_TOKEN`,
`BOT_NOTIFY_TELEGRAM_CHAT`, `BOT_NOTIFY_PUSHOVER_TOKEN`,
`BOT_NOTIFY_PUSHOVER_USER`, `BOT_NOTIFY_APPRISE_URL`.

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

## Order rate limiting and the circuit breaker

```yaml
safety:
  max_orders_per_min: 10      # trip the breaker above this rate (0 = off)
  min_order_interval: 1100ms  # minimum gap between submissions
```

Two different protections against a bot that has started misbehaving
mechanically — as opposed to `protections:`, which watches whether it is
*losing money*.

### `max_orders_per_min` — the circuit breaker

A runaway loop is the failure mode that costs the most the fastest. If the
engine submits this many orders inside a rolling minute, the breaker trips and
**automated trading is switched off**: not throttled, not delayed, off. It
stays off until you re-enable it deliberately from the UI or the API.

Tripping also records a `halted` trade record with the reason, logs at `Error`,
and ends any active [test mode](#test-mode-simulated-trading-over-the-real-feed)
window.

**It only counts automated orders.** Orders you place by hand through the API
or the UI pass through unguarded — if you are clicking the button, you already
know how many orders you are placing.

Set it low enough that normal operation never approaches it. With
`eval_interval: 30s` and a handful of watchlist symbols, a healthy bot places
orders in ones and twos per minute, not tens.

### `min_order_interval` — pacing

The minimum gap the **Saxo** adapter leaves between submissions. Saxo enforces
roughly one order per second per session and rejects bursts, so the default of
1100ms sits just outside it.

It is a pace, not a limit: orders queue behind it rather than being refused.
The other adapters do their own pacing internally (Trading 212 per endpoint
group, Alpaca not at all), so this setting reaches only Saxo.

> These two are also what satisfies the "throttle rapid orders" and "stop if
> trades exceed a limit" items on Saxo's live-application checklist — see
> [Automated-trading safety](#automated-trading-safety-saxo-live-cert-mapping).

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

### How a bar is simulated

Each bar is replayed in the order a real session experiences it:

1. **At the open** — orders decided on the *previous* bar are filled.
2. **During the bar** — protective exits are tested against the **high and
   low**, so a stop pierced intrabar fires even if the bar recovered by its
   close, exactly as the live tick-driven engine would fire it.
3. **At the close** — the strategy is evaluated and its decision is queued for
   the next bar's open.

Nothing is ever decided and filled on the same bar. Deciding on a bar's close
and filling at that same close is a look-ahead: at the moment the close is
known, it is no longer tradeable. A stop the market **gapped through** fills at
the open, not at the stop price — you cannot get out at a level the market
jumped over — and when a bar touches both the stop and the target, the stop
wins, because the order of the high and low inside a bar is unknowable.

Two orderings inside a bar follow what the live bot, evaluating on ticks,
would see first:

- **`max_hold` fills at the open.** The position's age is measured at the
  bar's start, so a barrier due on this bar was due at its open — where live
  exits on its first tick. Only a stop or target the open itself is already
  through names the exit instead; a dive later in the bar happens to a
  position live has already closed.
- **A scale-in above the stop comes first.** On a falling bar the add's
  threshold (`-step_pct`) is crossed before a deeper stop, so the add is made
  and the stop then judged against the re-based average — as live does. When
  the open, or the threshold itself, is already through a stop or trail, the
  exit comes first and no add is made.

Fills also pay the spread and slippage from `costs.spread_pct` /
`costs.slippage_pct`. Free liquidity at the exact reference price is the single
most flattering assumption a backtest can make.

Position sizing follows the live rules too, `engine.min_positions` included:
`cmd/backtest` and the optimizer's limit-respecting runs cap each buy at one
slot of the investable pot, as the live risk manager does. The optimizer's
relaxed-limits mode and autotune deliberately do not — they measure one symbol
on a synthetic budget, where a slot of that budget is not the live slot.

**Asset classes and sectors.** Live layers the classes and sectors it learned
from the provider under `asset_classes:` / `sectors:`. `cmd/backtest` has only
the file's maps unless told where the learned ones are:

```bash
cp /path/to/finance_bot.db /tmp/probe.db
go run ./cmd/backtest -csv data/set.csv -db /tmp/probe.db
```

The database is opened read-only — nothing is migrated or written — but point
it at a copy anyway. With portfolio caps on and no `-db`, a run warns about
every symbol a cap cannot see: those count as unclassified, which they would
not be live, and run uncapped.

The protections judge a whole round trip, as live does: a position sold in
tranches is one trade to the cooldown, stop guard and low-profit lock, on its
net P&L, and `max_realized_drawdown_pct` halts the run (reported under
`HaltedAt`) exactly when it would halt the bot.

### What is reported

Returns (total and annualised CAGR), the equal-weighted buy & hold benchmark,
max drawdown and exposure; the risk-adjusted ratios **Sharpe**, **Sortino**
(downside deviation only) and **Calmar** (CAGR ÷ max drawdown), all annualised
from the observed bar spacing so they're comparable with published figures; and
trade quality: win rate, **profit factor**, **expectancy** per round trip and
average win/loss. Expectancy is usually the number that matters — a 90% win rate
is worthless if the losses are ten times the size of the wins.

### Breakdowns

The aggregates say whether the strategy made money. These say which parts of it
did, which is what changes a configuration:

- **P&L by exit rule** — usually the most actionable table in the report. An exit
  kind whose total is negative is a rule losing money on every trade it closes.
  A negative `stop-loss` row is expected (that is its job); a negative
  `take-profit` or `roi` row means the targets are set wrong.
- **P&L by symbol** — which names carried the result and which bled.
- **Monthly returns**, with a bar chart, so one lucky month can't hide inside a
  total.
- **Holding periods** — min/median/mean/max plus average winner vs average loser.
  The report calls out the classic failure mode explicitly when losers are held
  more than twice as long as winners.

### Monte Carlo (`-montecarlo N`)

```bash
go run ./cmd/backtest -csv data/set.csv -montecarlo 5000 -ruin 0.25
```

A backtest reports one number because it saw one sequence of events. Resample the
same trades and the endpoint barely moves, but the *path* changes a lot — and the
path decides whether you'd still have been trading at the end:

```
Return   p5   -3.10%   p25    1.80%   median    4.90%   p75    8.10%   p95   13.40%
Max DD   median  6.20%   p95  14.80%   p99  19.10%   worst  24.30%
Probability of profit:  82.5%      probability of a 25% drawdown:   0.4%
```

Two strategies with an identical 20% return are not equivalent if one reaches a
45% drawdown in a tenth of its orderings. It declines to run below 10 round trips
— bootstrapping three trades produces a confident-looking distribution of three
numbers — and it flags the case where the 5th percentile is a loss.

**What it cannot tell you:** the bootstrap resamples trades independently, so it
assumes they're i.i.d. They aren't. It can't see losing streaks that cluster in
one regime, can't invent outcomes the strategy never produced, and says nothing
about a market that behaves unlike the sample. Treat the spread as a *floor* on
uncertainty, never a ceiling.

Strategy params, money limits and exit rules are read from `config.yaml`.
Wins/losses are counted **net of round-trip commission** (consistent with the
net-of-costs returns), and the engine's churn damping is modelled too:
`confirm_buy`/`confirm_sell` require the signal to repeat on N consecutive
**bars** (the backtest analogue of evaluation ticks) and `keep_interval` blocks
strategy sells until a position has been held that long — so backtest and
optimizer results reflect the same behaviour settings the live engine trades
with.

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

Three modes:
- **Grid** (default) — exhaustive over the structural axes.
- **Random** (`-random N`) — N samples across the full space including every
  indicator parameter.
- **Guided** (`-random N -generations G`) — the same budget spent in G
  generations: one random, then each subsequent generation bred from the best
  configurations found so far, with the mutation rate decaying from broad
  exploration to local refinement. It concentrates the budget where the results
  are, which is *not* a free lunch: on noise it will converge on noise, and
  converge harder than random search would. Judge it with `-walkforward`, not by
  the leaderboard it prints.

### Refinement — the axes the grid can't afford

The grid is already ~2000 combinations, and every axis added *multiplies* it. So
after the main search a second pass sweeps one axis at a time around the winner
(`-refine`, on by default):

- **the SELL side** — `detectors_sell`, `combine_sell`, `min_strength_sell`. The
  grid only ever searches the BUY detectors, so without this nothing asks
  whether the indicators that are best at deciding when to *enter* are also best
  at deciding when to *leave*. There's no reason they should be: entering wants
  several indicators agreeing, exiting wants whichever one notices first that
  the move is over.
- **behaviour** — `confirm_buy`, `confirm_sell`, `keep_interval`.
- **adaptive exits** — `atr_stop_mult`, `atr_trail_mult`.

Sweeping one axis at a time costs the *sum* of the axis lengths instead of their
product — dozens of backtests rather than tens of thousands. The trade-off is
real and stated in the output: coordinate descent finds a local optimum and
cannot discover parameters that only work when changed together.

The emitted config now includes everything that was tuned, and reports when an
axis was left alone (which is useful — it means the grid's choice survived a
direct challenge).

### What is deliberately not searched

**`costs.*`** — spread, slippage and commission are measurements of your broker
and your market, not choices. An optimiser handed them would drive them to zero
and report a better strategy, which is lying to yourself with extra steps. Set
them to what your venue actually charges.

**`protections.*`** — cooldowns, the stop guard and the drawdown halt can only
ever *cost* return in-sample, so any return-seeking search switches them off.
Instead they are now **applied during every backtest**, and a run that would have
tripped a halt says so:

```
⚠️  A protection would have HALTED the live bot during this run:
    2026-03-11 14:05: max drawdown 21.4% >= 20.0% (peak 118420.00, now 93060.00)
    The equity curve continues past that point; the live one would not.
```

Ranking (`-rank`): `blend` (return + win-rate − drawdown, default), `return`,
`winrate`, `sharpe`, `sortino`, `calmar`, `profitfactor` or `expectancy`.
`-min-trades` drops configs with too few trades (so a lucky 1-trade run can't
top the board); identical outcomes are de-duplicated so the board shows
genuinely distinct configs.

### Walk-forward analysis (`-walkforward N`)

```bash
go run ./cmd/optimize -csv data/set.csv -walkforward 4 -rank calmar
```

**This is the number to believe.** An ordinary search reports the best result on
the data it searched, which is the definition of an in-sample optimum — with
enough combinations, something always fits the noise. Walk-forward splits the
history into N+1 segments, selects parameters on past data only, and scores them
on the *next, unseen* segment. A strategy that only works when you already know
the answer is exposed as one:

```
fold  trainBars  testBars oosReturn%  oosScore     win%  trades  in-sample pick
1           300       300       1.72    -0.417      0.0       1  [ema_cross+macd] majority ms=0.20 ...
2           600       300      -0.15    -2.453      0.0       2  [ema_cross+rsi+macd+vwap] weighted ...

Mean out-of-sample return: 0.03%   mean score: -2.232   over 3 folds
```

A negative mean out-of-sample return means the in-sample winners did not
generalise — treat any single-run leaderboard from that dataset as overfitted.

The grid also searches **capital deployment** (order size as a fraction of cash)
— the dominant driver of total return. By default the optimizer **relaxes the
money limits** so deployment isn't capped while searching (pass `-respect-limits`
to keep your config's caps). The best-config output shows the per-trade ceiling
(as `engine.max_risk_size_usd`, the live key with that meaning) and the limits
needed to realise it — apply your own safety limits before going live, knowing
tighter limits reduce return proportionally.

**Modes and flags.** `-sensitivity`, `-walkforward` and a search (`-random` or
the grid) are separate modes; a flag the chosen mode would ignore is an error,
not a silent no-op — `-walkforward 5 -random 2000` is refused rather than run as
a grid per fold. `-generations` and `-seed` need `-random`.

**Interrupting.** The first Ctrl+C stops the search and keeps what it found:
the un-refined result is checkpointed (`-checkpoint`), the refinement pass is
skipped, and the report printed. A second Ctrl+C ends the process at once.

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

## Web UI

**`http://localhost:8080/v2/`** — tabbed, translated, lazily loaded, strict CSP.
The root `/` permanently redirects there.

> **The original single-page UI was removed.** It served from `/` and had
> fallen behind: no `autotune` or `test_mode` settings existed in it at all, so
> anyone managing the bot through it could not see those features. It also had
> no CSP, a known stored-XSS path to the auth token it held as a JS global, and
> an auth flow that popped a browser prompt once per failed fetch — a stack of
> them on any page with a token set. Everything it did, `/v2` does, and more.

### v2

Same features, but the HTML is built **here**, not in the browser. Every screen is
a Go template rendered from the same values the API returns; the client fetches a
fragment and puts it on the page.

| | JavaScript |
|---|---|
| original single page | 908 lines, one file |
| v2, first attempt | 3,459 lines across 20 modules |
| **v2 now** | **238 lines of code** in one file (4 KB gzipped) |

What is left does four things — fetch a fragment, swap it in, post a form, ask
before something destructive — driven by `data-` attributes on server-rendered
markup. It knows nothing about trading, settings, formatting or translation.

It exists at all for one reason: the API token must travel in an `Authorization`
header rather than a URL, and a browser following a link cannot set headers. So
navigation is fetch-and-swap rather than `<a href>`.

```
internal/api/
  render.go         template engine, i18n lookup, formatting verbs
  formfields.go     every editable setting, declared once; form decoding
  fragments.go      one handler per screen
  chartsvg.go       the price chart, as SVG
  forms.go          form posts, replayed through the existing JSON handlers
  web2/templates/   the screens
  web2/static/      app.css and the 238-line client
  web2/i18n/        en.json, de.json
```

**Three things this buys beyond size.**

*Escaping is contextual and automatic.* `html/template` knows whether a value
lands in text, an attribute or a URL. The browser code had to be disciplined
about never building markup from data; now it cannot, because it does not build
markup.

*Validation stopped being written twice.* The client had 267 lines parsing
durations, rung ladders and `key:value` maps and range-checking them — all of
which this process already did on the way in. A form post is now decoded by its
declared field type and handed to **the same handler the JSON API uses**, so
`config.Validate`, the scale-in cap and the redaction marker all keep applying,
in one place.

*The chart is SVG.* That removed ~400 lines of canvas code and gained things it
had hand-rolled: resolution independence by construction (no `devicePixelRatio`
scaling), native `<title>` tooltips per candle instead of a crosshair with
hit-testing, and reflow without a resize listener. It still draws candles, volume
and this bot's own executions — filled markers for real fills, hollow for dry-run
or blocked.

**Security.** Unchanged and slightly stronger: same strict CSP with no
`unsafe-inline`, `nosniff`, `no-referrer`, `DENY` framing. The token still
defaults to `sessionStorage` and never appears in a URL — the event stream is
read with `fetch` so it can carry a header. Fragments sit behind the same token
as `/api`; only the shell (which contains no data) is served without one.
Templates and translation catalogs are rendered, never served.

**Languages.** English and German, chosen by `?lang`, a cookie, or
`Accept-Language`. Adding one is a JSON file plus a line in `languages`. Missing
keys fall back to English. The catalogs are no longer downloaded — the text is
already in the HTML — which is why first paint dropped from 33 KB to 7 KB of
assets.

**The trade-off, stated plainly:** filtering the trade log and switching chart
timeframes now cost a round-trip where the client-side version cost none. On a
bot you are running locally that is imperceptible; over a slow link it is not.

### Live settings

`GET /api/settings` returns every live-editable block in one response — `costs`,
`exits`, `entries`, `protections`, `portfolio` (+ `sectors`), `screener`,
`notify`, `ai`, `ml` — and `PUT /api/settings/<section>` applies one of them.
Sections are applied individually so a rejected block cannot half-apply its
neighbours. (`GET/PUT /api/exits` still works, for anything built against
it; its PUT accepts `max_hold` both as a duration string and as the nanosecond
integer its GET serves.)

Four details are worth knowing:

- **Secrets are redacted.** The Pushover token and user key, the Telegram bot
  token, the Discord and generic webhook URLs, the Apprise service string and
  the LLM API key all read back as `********`. Submitting that marker unchanged
  keeps the stored value, so saving a form that never displayed a secret cannot
  erase it. Sending a real value replaces it.
- **Durations are strings**, exactly as in `config.yaml`: `"30m"`, `"2h"`,
  `"0s"` — not nanosecond integers.
- **`exits`, `screener` and `PUT /api/strategy` keep what you do not send.**
  The body is applied onto the settings in force, so a key it leaves out keeps
  its value instead of resetting to zero — the strategy form has no `htf_*`
  fields and the screener form cannot show `by_asset_class`, and saving either
  used to wipe them. A map you *do* send replaces the old one whole, so
  deleting a key works. The other sections (and `PUT /api/limits`, `sizing`)
  still take the body as the complete block.
- **Editing protections preserves state.** An active drawdown halt and any
  symbol lockouts survive a threshold change; clearing a halt stays the
  deliberate act of `POST /api/protections/resume`.
- **The screening *interval* needs a restart**, because the ticker was started
  when the engine began running. The response says so rather than pretending
  otherwise; the filters themselves take effect on the next pass, and
  `POST /api/screener/rescreen` (the **Re-screen now** button) runs one
  immediately.

Changes are applied to the running process, not written back to `config.yaml` —
edit the file too if they should survive a restart.

## API

All `/api` routes are **unauthenticated by default**. Set `server.auth_token`
(or `BOT_AUTH_TOKEN`) to require a token on every request — sent as
`Authorization: Bearer <token>`, or as a `?token=` query parameter for the SSE
stream. Do this before exposing the port beyond localhost: the API can place
orders and toggle live trading.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | liveness, plus `auth_required` so the UI knows whether to ask for a token (unauthenticated by design) |
| GET | `/v2/` | the v2 shell; `/v2/f/:name` renders a screen, `/v2/static/*` the stylesheet and client |
| POST | `/v2/form/*` | form posts, replayed through the JSON handlers above |
| GET | `/api/status` | engine status, broker, spend today |
| GET | `/api/dashboard` | combined snapshot (status, positions, open, signals, metrics) for the web UI |
| GET | `/api/metrics` | runtime engine metrics |
| GET | `/api/events` | live SSE stream of signals/trades (feeds the UI activity panel) |
| GET | `/api/quotes` | latest price per symbol |
| GET | `/api/positions` | portfolio snapshot (cash, holdings, equity) |
| GET | `/api/open` | durable open-position records (bought, not yet sold; survives restart) |
| DELETE | `/api/open/:symbol` | forget a position closed **outside** the bot — places no order and books no P&L |
| POST | `/api/open/:symbol/adopt` | give a broker holding the bot has no record of a full record now, instead of waiting out the automatic adoption; 409 while an order for the symbol is still in flight (a resting limit, an unconfirmed buy, a reservation) — the order's own fill would be booked twice |
| GET | `/api/orderable` | per-watchlist symbol: is it tradeable on the broker, and to which ticker (Trading 212) |
| GET | `/api/detectors` | every detector's signal per symbol (incl. inactive ones) |
| GET | `/api/ai` | LLM advisor status: provider, model, call/error counts, last error |
| GET | `/api/ml` | learned-detector status: models trained, failures, mean skill above base rate |
| GET | `/api/chart/:symbol` | candles + this bot's executions for the price chart (`?bars=`, `?res=`) |
| GET | `/api/protections` | halt state, drawdown vs the equity peak, locked symbols |
| POST | `/api/protections/resume` | clear a halt and re-base the drawdown peak; 409 when nothing is halted, unless `?rebase=1` asks for the re-base alone |
| DELETE | `/api/protections/lock/:symbol` | release one symbol's cooldown / stop-guard lock |
| GET | `/api/screener` | last universe-selection pass, incl. why each candidate was rejected |
| POST | `/api/screener/rescreen` | run a screening pass now instead of waiting out the interval; 409 while one is already running |
| GET | `/api/notify` | alert sinks and delivery counters (sent / failed / dropped) |
| GET | `/api/settings` | every live-editable block in one response — costs, exits, entries, protections, portfolio, sectors, screener, notify, ai, ml (secrets redacted) |
| PUT | `/api/settings/:section` | live-edit one block: `costs`, `exits`, `entries`, `protections`, `portfolio`, `screener`, `notify`, `ai`, `ml` |
| GET / PUT | `/api/strategy` | read / live-edit the strategy (detectors, combine, periods) |
| GET / PUT | `/api/exits` | read / live-edit the stop-loss / trailing / take-profit |
| GET / PUT | `/api/sizing` | read / live-edit position sizing (`min_positions`, `max_risk_size_usd`, `min_order_size_usd`) |
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
- **Backtest results changed** when next-bar-open fills, intrabar exit checks
  and spread/slippage landed. Older numbers from this repo were optimistic in
  three specific ways (same-bar fills, stops that only triggered on a close,
  free liquidity); a strategy that looked profitable then may not now. That is
  the point — re-run anything you were relying on.
- Not implemented, deliberately: crypto **execution** (data only — see
  [Crypto data](#crypto-data)), and short selling. Every position is long.
- Natural extensions: per-symbol strategy parameters, an options/futures asset
  class, and portfolio-level correlation limits (nothing currently stops the bot
  holding twenty names that all move together).

