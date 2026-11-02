package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// fiveBarHistory serves five bars for anything: too few for any symbol to
// pass a min_history_bars of 10, so every unprotected symbol is dropped.
type fiveBarHistory struct{}

func (fiveBarHistory) Candles(context.Context, string, market.Resolution, time.Time, time.Time) ([]market.Candle, error) {
	base := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	c := make([]market.Candle, 5)

	for i := range c {
		c[i] = market.Candle{Time: base.Add(time.Duration(i) * time.Minute), Open: 10, High: 10.1, Low: 9.9, Close: 10, Volume: 100}
	}

	return c, nil
}

// screenHeldEngine is a pinEngine with the parts rescreen reads.
func screenHeldEngine(keepHeld bool) *Engine {
	e := pinEngine(&pinStore{})
	e.pf = portfolio.New()
	e.prices = NewPriceCache()
	e.rm = risk.New(config.Limits{}, config.CostsConfig{})
	e.spreads = map[string]float64{}
	e.unconfirmed = map[string]UnconfirmedOrder{}
	e.SetHistorySource(fiveBarHistory{})
	e.SetScreenerConfig(config.ScreenerConfig{
		Enabled: true, KeepHeld: keepHeld, Filters: []string{"history"}, MinHistoryBars: 10,
	})

	return e
}

// SC3-2: heldSymbols read openPos alone. An unconfirmed buy is kept out of
// openPos until reconciled, and a broker holding inside the adopt grace has
// no record yet — so a pass that rejected them dropped both, and the fill
// that then arrived ran with no tick feed: no ATR stop, no strategy SELL.
func TestRescreenKeepsAnUnconfirmedBuyAndAnUntrackedHolding(t *testing.T) {
	e := screenHeldEngine(true)
	e.unconfirmed["o1"] = UnconfirmedOrder{Symbol: "AAPL", Side: broker.Buy, Qty: 3, OrderID: "o1"}
	e.pf.SyncPositions([]portfolio.Holding{{Symbol: "MSFT", Qty: 2, AvgPrice: 400, LastPrice: 400}})

	if err := e.AddScreened("AAPL", "MSFT", "GOOG"); err != nil {
		t.Fatal(err)
	}

	e.rescreen(context.Background())

	got := e.Watchlist()
	if !slices.Contains(got, "AAPL") {
		t.Errorf("watchlist %v: AAPL dropped while its buy is unconfirmed", got)
	}

	if !slices.Contains(got, "MSFT") {
		t.Errorf("watchlist %v: MSFT dropped while the broker holds it (before adoption)", got)
	}

	if slices.Contains(got, "GOOG") {
		t.Errorf("watchlist %v: the unheld, failing GOOG survived — the pass protected everything", got)
	}
}

// The other half of SC3-2: adoption never touches the watchlist, so a held
// symbol that is off it came back only through Selected, which takes
// keep_held — with keep_held off it never came back at all, contrary to the
// flag's own documentation (it only changes a verdict's wording).
func TestAHeldSymbolOffTheWatchlistComesBackWithKeepHeldOff(t *testing.T) {
	e := screenHeldEngine(false)
	e.openPos["AAPL"] = &OpenPosition{Qty: 3}

	if err := e.AddScreened("MSFT"); err != nil {
		t.Fatal(err)
	}

	e.rescreen(context.Background())

	if got := e.Watchlist(); !slices.Contains(got, "AAPL") {
		t.Errorf("watchlist %v: the held AAPL is still untracked after a pass with keep_held off", got)
	}
}
