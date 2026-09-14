package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// currencyBroker is a broker.Broker that also resolves per-symbol FX rates,
// for exercising resolveFXRates/refreshFXRates without a real broker.
type currencyBroker struct {
	broker.Broker

	rates map[string]float64 // symbol -> rate; missing/zero => error
}

func (b *currencyBroker) Name() string { return "currency-test" }

func (b *currencyBroker) InstrumentCurrency(_ context.Context, symbol string) (string, float64, error) {
	rate, ok := b.rates[symbol]
	if !ok {
		return "", 1, errors.New("unknown symbol")
	}

	return "USD", rate, nil
}

// fxTestEngine builds a minimally-wired engine for exercising price
// conversion paths (ingest, warmup, venuePrice) without a full New(...) call.
func fxTestEngine(brk broker.Broker) *Engine {
	return &Engine{
		brk:       brk,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		prices:    NewPriceCache(),
		pf:        portfolio.New(),
		barsStore: newBarStore(1000, time.Minute),
		fxRates:   make(map[string]float64),
	}
}

func TestResolveFXRatesPopulatesFromCurrencyResolver(t *testing.T) {
	t.Parallel()

	brk := &currencyBroker{rates: map[string]float64{"NVDA": 0.86}}
	e := fxTestEngine(brk)

	e.resolveFXRates([]string{"NVDA", "UNKNOWN"})

	if got := e.fxRateFor("NVDA"); got != 0.86 {
		t.Errorf("fxRateFor(NVDA) = %v, want 0.86", got)
	}

	// A symbol the broker couldn't resolve must default to 1 (no conversion),
	// not zero or a stale/wrong rate — a missing rate must never silently
	// zero out a price.
	if got := e.fxRateFor("UNKNOWN"); got != 1 {
		t.Errorf("fxRateFor(UNKNOWN) = %v, want 1 (safe default)", got)
	}
}

func TestFxRateForDefaultsToOneWhenNeverResolved(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(plainBroker{})

	if got := e.fxRateFor("NVDA"); got != 1 {
		t.Errorf("fxRateFor on an untouched cache = %v, want 1", got)
	}
}

// A broker without CurrencyResolver must not be an error — most brokers are
// single-currency and never implement it.
func TestResolveFXRatesIgnoresBrokersWithoutAResolver(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(plainBroker{})

	e.resolveFXRates([]string{"AAPL"}) // must not panic

	if got := e.fxRateFor("AAPL"); got != 1 {
		t.Errorf("fxRateFor(AAPL) = %v, want 1", got)
	}
}

// This is the core of the whole fix: a live tick in the instrument's native
// currency must be converted before it reaches the price cache, the
// portfolio mark, or the bar store — otherwise every consumer downstream
// (sizing, risk limits, exits, PnL) mixes a native-currency price with the
// broker's already-converted, preferred-currency money values.
func TestIngestConvertsNativePriceByCachedFXRate(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(plainBroker{})
	e.fxRates["NVDA"] = 0.86
	e.barsStore.add("NVDA")

	e.ingest(market.Quote{Symbol: "NVDA", Price: 224.72, Time: time.Now()})

	got, ok := e.prices.Get("NVDA")
	if !ok {
		t.Fatal("no price cached after ingest")
	}

	want := 224.72 * 0.86
	if got != want {
		t.Errorf("cached price = %v, want %v (converted)", got, want)
	}

	series := e.barsStore.series("NVDA")
	if len(series) == 0 {
		t.Fatal("no bar recorded after ingest")
	}

	if series[len(series)-1].Close != want {
		t.Errorf("bar close = %v, want %v (converted)", series[len(series)-1].Close, want)
	}
}

// A symbol with no known FX rate (not yet resolved, or a single-currency
// broker) must pass the price through unchanged — the safe default of 1.
func TestIngestLeavesPriceUnchangedWithoutAKnownRate(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(plainBroker{})

	e.ingest(market.Quote{Symbol: "AAPL", Price: 190, Time: time.Now()})

	got, ok := e.prices.Get("AAPL")
	if !ok || got != 190 {
		t.Errorf("cached price = %v (ok=%v), want 190 unconverted", got, ok)
	}
}

// venueHistory is a HistorySource stub returning one fixed candle per symbol.
type venueHistory struct {
	close float64
}

func (h venueHistory) Candles(
	_ context.Context, _ string, _ market.Resolution, _, _ time.Time,
) ([]market.Candle, error) {
	return []market.Candle{{Close: h.close}}, nil
}

// venuePrice feeds order sizing/risk checks directly, so it must return the
// same currency as e.prices — otherwise confirmVenuePrice's deviation check
// compares a converted signal price against a native-currency venue price
// and either blocks every foreign-currency order or waves through a real
// mispricing.
func TestVenuePriceAppliesCachedFXRate(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(plainBroker{})
	e.history = venueHistory{close: 100}
	e.fxRates["NVDA"] = 0.86

	got, _, ok := e.venuePrice("NVDA")
	if !ok {
		t.Fatal("venuePrice reported no price")
	}

	if want := 100 * 0.86; got != want {
		t.Errorf("venuePrice = %v, want %v (converted)", got, want)
	}
}
