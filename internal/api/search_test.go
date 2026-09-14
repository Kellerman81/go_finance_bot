package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// Nobody remembers that Lanxess is LXS.DE. The picker exists so an operator can
// type a company name, see the ticker alongside it, and add the right listing —
// BAYN.DE and BAYRY are both "Bayer" and are not the same instrument.

// The watchlist must show both the ticker and the company name.
func TestWatchlistShowsSymbolAndName(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("LXS.DE"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	s.eng.SetSymbolName("LXS.DE", "LANXESS Aktiengesellschaft")

	body := fetch(t, s, "/v2/f/watchlist").Body.String()

	if !strings.Contains(body, "LXS.DE") {
		t.Error("the ticker is missing from the watchlist")
	}

	if !strings.Contains(body, "LANXESS Aktiengesellschaft") {
		t.Error("the company name is missing.\nA list of LXS.DE, XD5E.DE and " +
			"IBCI.DE tells the operator nothing about what the bot holds.")
	}
}

// A symbol whose name is not known yet must still render, as its ticker alone.
func TestWatchlistRendersASymbolWithNoName(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("PRAJL.XC"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	rec := fetch(t, s, "/v2/f/watchlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "PRAJL.XC") {
		t.Error("a symbol with no resolved name vanished from the list")
	}
}

// With no searching provider the picker is hidden rather than shown broken.
func TestSearchFormHiddenWithoutAProvider(t *testing.T) {
	s := newTestServer(t)

	if s.eng.CanSearch() {
		t.Skip("the test server has a searching provider")
	}

	if strings.Contains(fetch(t, s, "/v2/f/watchlist").Body.String(), "/v2/f/search") {
		t.Error("the search form is offered though no provider can search")
	}
}

// An empty query renders nothing rather than an error or an empty-result notice
// — the panel starts empty and should stay quiet until asked.
func TestSearchWithNoQueryIsQuiet(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/v2/f/search")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	if strings.Contains(rec.Body.String(), "class=\"empty\"") {
		t.Error("an untouched search box already reports a result")
	}
}

// "Search is unavailable" must not be rendered as "nothing found": one is a
// configuration problem, the other means try another word.
func TestSearchReportsUnavailabilityDistinctly(t *testing.T) {
	s := newTestServer(t)

	if s.eng.CanSearch() {
		t.Skip("the test server has a searching provider")
	}

	body := fetch(t, s, "/v2/f/search?q=bayer").Body.String()

	if strings.Contains(body, "Nothing found") {
		t.Error("an unavailable searcher was reported as an empty result set")
	}

	if !strings.Contains(body, "not available") {
		t.Errorf("the unavailability message is missing from:\n%s", body)
	}
}

// Adding from a result carries the name, so the row reads properly at once
// rather than after the background resolver catches up.
func TestAddingFromSearchRecordsTheName(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/watchlist", map[string][]string{
		"symbols": {"BAYN.DE"},
		"name":    {"Bayer Aktiengesellschaft"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if got := s.eng.SymbolName("BAYN.DE"); got != "Bayer Aktiengesellschaft" {
		t.Errorf("name = %q, want it taken from the picked result — it is already "+
			"in hand, so looking it up again would be a wasted request", got)
	}
}

// A plain typed add carries no name and must not invent one.
func TestPlainAddDoesNotInventAName(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/watchlist", map[string][]string{
		"symbols": {"MSFT"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if got := s.eng.SymbolName("MSFT"); got != "" {
		t.Errorf("name = %q, want empty until it is actually resolved", got)
	}
}

// searchingHistory is a history source that can also search, which is the shape
// the Yahoo provider has in production.
type searchingHistory struct{}

func (searchingHistory) Candles(
	context.Context, string, market.Resolution, time.Time, time.Time,
) ([]market.Candle, error) {
	return nil, nil
}

func (searchingHistory) Search(
	_ context.Context, _ string, _ int,
) ([]market.Instrument, error) {
	return []market.Instrument{
		{Symbol: "BAYN.DE", Name: "Bayer Aktiengesellschaft", Exchange: "XETRA", Type: "EQUITY"},
		{Symbol: "BAYRY", Name: "Bayer Aktiengesellschaft", Exchange: "Other OTC", Type: "EQUITY"},
	}, nil
}

// A result row must show the ticker, the company name AND the venue. The last
// one is not decoration: both rows here are "Bayer" and only the exchange tells
// the XETRA listing from the US ADR.
func TestSearchResultsShowSymbolNameAndVenue(t *testing.T) {
	s := newTestServer(t)
	s.eng.SetHistorySource(searchingHistory{})

	body := fetch(t, s, "/v2/f/search?q=bayer").Body.String()

	for _, want := range []string{"BAYN.DE", "BAYRY", "Bayer Aktiengesellschaft", "XETRA", "Other OTC"} {
		if !strings.Contains(body, want) {
			t.Errorf("the results are missing %q.\nTwo listings of one company "+
				"differ only by venue; without it the choice is a coin flip.", want)
		}
	}
}

// Each result posts exactly one symbol, with its name attached.
func TestSearchResultAddsOneSymbolWithItsName(t *testing.T) {
	s := newTestServer(t)
	s.eng.SetHistorySource(searchingHistory{})

	body := fetch(t, s, "/v2/f/search?q=bayer").Body.String()

	if !strings.Contains(body, `name="symbols" value="BAYN.DE"`) {
		t.Error("a result does not post its symbol")
	}

	if !strings.Contains(body, `name="name" value="Bayer Aktiengesellschaft"`) {
		t.Error("a result does not carry its name, so adding it would cost a " +
			"lookup for something already in hand")
	}
}

// With a searching provider the picker is offered on the watchlist screen.
func TestSearchFormShownWithAProvider(t *testing.T) {
	s := newTestServer(t)
	s.eng.SetHistorySource(searchingHistory{})

	if !strings.Contains(fetch(t, s, "/v2/f/watchlist").Body.String(), "/v2/f/search") {
		t.Error("the search form is missing though the provider can search")
	}
}

// A search for one company returns every venue it lists on: the screenshot that
// prompted this showed Lanxess on XETRA, Milan, Frankfurt, Vienna, Dusseldorf,
// Stuttgart and Munich. An account can usually trade one of them, and picking
// another produces an order the broker refuses outright — a failure the operator
// only discovers at the first buy.

// lanxessHistory returns the multi-venue result set from the screenshot.
type lanxessHistory struct{ searchingHistory }

func (lanxessHistory) Search(_ context.Context, _ string, _ int) ([]market.Instrument, error) {
	return []market.Instrument{
		{Symbol: "1LXS.MI", Name: "LANXESS Aktiengesellschaft", Exchange: "Milan", Type: "EQUITY"},
		{Symbol: "LXS.DE", Name: "LANXESS Aktiengesellschaft", Exchange: "XETRA", Type: "EQUITY"},
		{Symbol: "LX9B.SG", Name: "Lanxess AG", Exchange: "Stuttgart", Type: "EQUITY"},
	}, nil
}

// resolvingBroker trades only the symbols it was given.
type resolvingBroker struct {
	broker.Broker

	has map[string]bool
}

func (*resolvingBroker) Name() string { return "resolving" }

func (b *resolvingBroker) ResolveSymbol(_ context.Context, symbol string) (string, error) {
	if b.has[symbol] {
		return symbol + "_EQ", nil
	}

	return "", errors.New("no instrument")
}

// The listing the broker can trade must be marked and must come first, whatever
// order the search provider returned them in.
func TestTradeableResultsAreMarkedAndFloatToTheTop(t *testing.T) {
	s := newTestServerWith(t, &resolvingBroker{has: map[string]bool{"LXS.DE": true}})
	s.eng.SetHistorySource(lanxessHistory{})

	body := fetch(t, s, "/v2/f/search?q=lanxess").Body.String()

	tradeable := strings.Index(body, "LXS.DE")
	milan := strings.Index(body, "1LXS.MI")

	if tradeable < 0 || milan < 0 {
		t.Fatalf("results missing (LXS.DE=%d 1LXS.MI=%d)", tradeable, milan)
	}

	if tradeable > milan {
		t.Error("the listing the broker can trade is below one it cannot.\n" +
			"The provider's order put Milan first; broker availability has to win.")
	}

	if !strings.Contains(body, "tradeable") {
		t.Error("no marker on the listing the broker offers")
	}

	if !strings.Contains(body, "not at your broker") {
		t.Error("no marker on the listings the broker lacks")
	}
}

// A listing the broker lacks stays addable: it is still worth tracking, and the
// broker may simply not have loaded its metadata.
func TestUntradeableResultsAreStillAddable(t *testing.T) {
	s := newTestServerWith(t, &resolvingBroker{has: map[string]bool{"LXS.DE": true}})
	s.eng.SetHistorySource(lanxessHistory{})

	body := fetch(t, s, "/v2/f/search?q=lanxess").Body.String()

	if !strings.Contains(body, `name="symbols" value="1LXS.MI"`) {
		t.Error("a listing the broker lacks cannot be added at all; it should be " +
			"de-emphasised, not withheld")
	}
}

// The important negative: a broker that cannot answer must not have its silence
// rendered as "your broker cannot trade this". The simulator has no instrument
// list at all, and neither does a real broker before its metadata loads.
func TestUnknownBrokerAvailabilityIsNotARefusal(t *testing.T) {
	s := newTestServer(t)
	s.eng.SetHistorySource(lanxessHistory{})

	body := fetch(t, s, "/v2/f/search?q=lanxess").Body.String()

	if strings.Contains(body, "not at your broker") {
		t.Error("a broker that cannot answer was reported as refusing every " +
			"listing.\nThe sim broker is not a SymbolResolver, and a real one has " +
			"no metadata until it loads — neither is a verdict.")
	}

	if strings.Contains(body, "tradeable") {
		t.Error("a broker that cannot answer was reported as offering every listing")
	}
}
