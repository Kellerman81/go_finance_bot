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
	// blockFor delays every lookup, for exercising the per-symbol deadline.
	blockFor time.Duration
}

func (b *currencyBroker) Name() string { return "currency-test" }

func (b *currencyBroker) InstrumentCurrency(ctx context.Context, symbol string) (string, float64, error) {
	if b.blockFor > 0 {
		select {
		case <-time.After(b.blockFor):
		case <-ctx.Done():
			return "", 1, ctx.Err()
		}
	}

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

// TestResolveFXRatesGivesEachSymbolItsOwnDeadline reproduces FX-1's first half.
//
// resolveFXRates used to open ONE context for the whole loop. Against a paced
// broker that cannot work: Trading 212 spaces its instrument-metadata reads 50s
// apart, so a shared 30s deadline expired before the first symbol resolved and
// every symbol behind it silently kept the default rate of 1 — on a 120-symbol
// watchlist, most of the book priced in its native currency while the account,
// the money limits and the venue-price check all spoke the preferred one.
//
// A slow FIRST symbol must not consume the budget of the ones behind it.
//
// Not parallel: it shortens the package-level timeout.
func TestResolveFXRatesGivesEachSymbolItsOwnDeadline(t *testing.T) {
	restore := resolveTimeout
	resolveTimeout = 150 * time.Millisecond

	t.Cleanup(func() { resolveTimeout = restore })

	// Every lookup takes longer than ONE symbol's budget but far less than
	// three of them, so a shared deadline resolves at most the first.
	brk := &currencyBroker{
		blockFor: 100 * time.Millisecond,
		rates:    map[string]float64{"A": 0.9, "B": 0.8, "C": 0.7},
	}
	e := fxTestEngine(brk)

	e.resolveFXRates([]string{"A", "B", "C"})

	for sym, want := range map[string]float64{"A": 0.9, "B": 0.8, "C": 0.7} {
		if got := e.fxRateFor(sym); got != want {
			t.Errorf("%s rate = %v, want %v — a symbol later in the list was "+
				"starved by the ones ahead of it and is priced unconverted", sym, got, want)
		}
	}
}

// TestSetFXRateRescalesAlreadyBuiltPrices reproduces FX-1's second half.
//
// Prices are converted once, as they enter: warmup multiplies the seeded
// candles and ingest multiplies each tick. So a rate that arrives LATE leaves
// the whole bar history on the old scale while anything computed fresh —
// venuePrice above all — uses the new one. In production that produced a
// permanent 4.8% "venue price deviates from signal" on STX, which is exactly
// the USD/EUR rate, blocking every entry in it.
//
// Nothing else ever rewrites a bar, so this never heals on its own.
func TestSetFXRateRescalesAlreadyBuiltPrices(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(&currencyBroker{})
	e.peaks = make(map[string]float64)

	// A series and a tick built while the rate was still the default 1.
	base := time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC)
	e.barsStore.add("STX")
	e.barsStore.seed("STX", []market.Candle{
		{Time: base, Open: 700, High: 710, Low: 690, Close: 701.3192, Volume: 5},
	})
	e.prices.Set("STX", 701.3192)
	e.peaks["STX"] = 710

	// The broker's real rate arrives afterwards.
	e.setFXRate("STX", 0.95201)

	const want = 701.3192 * 0.95201

	series := e.barsStore.series("STX")
	if len(series) == 0 {
		t.Fatal("no bars stored")
	}

	if got := series[len(series)-1].Close; !closeTo(got, want) {
		t.Errorf("bar close = %.4f, want %.4f — the stored series is still on "+
			"the old scale and will disagree with venuePrice by the exchange rate",
			got, want)
	}

	if got, _ := e.prices.Get("STX"); !closeTo(got, want) {
		t.Errorf("cached price = %.4f, want %.4f", got, want)
	}

	if got := e.peaks["STX"]; !closeTo(got, 710*0.95201) {
		t.Errorf("trailing-stop peak = %.4f, want %.4f — a peak left on the old "+
			"scale moves every trailing stop for this symbol by the rate",
			got, 710*0.95201)
	}

	// Volume is not denominated in anything and must be untouched.
	if got := series[len(series)-1].Volume; got != 5 {
		t.Errorf("volume = %v, want the unscaled 5", got)
	}
}

// An unchanged rate must not touch anything — refreshFXRates re-resolves the
// whole watchlist hourly, and rewriting every bar each time for no reason
// would be pure churn.
func TestSetFXRateIsANoOpWhenUnchanged(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(&currencyBroker{})
	e.peaks = make(map[string]float64)

	e.setFXRate("X", 0.95)
	e.prices.Set("X", 100)
	e.setFXRate("X", 0.95)

	if got, _ := e.prices.Get("X"); got != 100 {
		t.Errorf("price = %v, want the untouched 100", got)
	}
}

// closeTo compares prices to within a cent.
func closeTo(a, b float64) bool { return a-b < 0.01 && b-a < 0.01 }
