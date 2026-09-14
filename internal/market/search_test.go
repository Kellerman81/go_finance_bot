package market

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A trimmed copy of a real Yahoo search response: the Bayer query returns the
// XETRA listing, the US ADR, and an index Yahoo cannot price.
const bayerSearch = `{"quotes":[
  {"symbol":"BAYN.DE","shortname":"BAYER AG NA","longname":"Bayer Aktiengesellschaft",
   "exchDisp":"XETRA","quoteType":"EQUITY","isYahooFinance":true},
  {"symbol":"BAYRY","shortname":"Bayer AG","longname":"Bayer Aktiengesellschaft",
   "exchDisp":"Other OTC","quoteType":"EQUITY","isYahooFinance":true},
  {"symbol":"^BAYX","shortname":"Some Partner Index",
   "exchDisp":"Index","quoteType":"INDEX","isYahooFinance":false}
]}`

// A trimmed real response for a symbol with a sector, and one for a fund
// (ETFs carry no sector at all — Yahoo simply omits the field).
const sapSearchWithSector = `{"quotes":[
  {"symbol":"SAP.DE","shortname":"SAP SE","longname":"SAP SE",
   "exchDisp":"XETRA","quoteType":"EQUITY","sector":"Technology","isYahooFinance":true}
]}`

const etfSearchWithNoSector = `{"quotes":[
  {"symbol":"XMUS.DE","shortname":"Xtrackers MSCI USA Swap UCITS ETF 1C","longname":"Xtrackers MSCI USA Swap UCITS ETF 1C",
   "exchDisp":"XETRA","quoteType":"ETF","isYahooFinance":true}
]}`

// searchServer serves body for any request and records the query it saw.
func searchServer(t *testing.T, body string) (*YahooHistory, *string) {
	t.Helper()

	var seen string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RequestURI()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	return y, &seen
}

// Searching by company name must return the ticker plus a name a human can
// recognise — the whole point is that nobody remembers LXS.DE is Lanxess.
func TestSearchReturnsSymbolAndName(t *testing.T) {
	t.Parallel()

	y, _ := searchServer(t, bayerSearch)

	got, err := y.Search(t.Context(), "bayer", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 priceable ones: %+v", len(got), got)
	}

	if got[0].Symbol != "BAYN.DE" || got[0].Name != "Bayer Aktiengesellschaft" {
		t.Errorf("first result = %+v, want BAYN.DE / Bayer Aktiengesellschaft", got[0])
	}

	if got[0].Exchange != "XETRA" {
		t.Errorf("exchange = %q, want XETRA.\nTwo listings of one company differ "+
			"only by venue and currency; without it the picker is a coin flip.",
			got[0].Exchange)
	}
}

// The sector Yahoo already returns alongside the name must reach the caller —
// it feeds the portfolio concentration cap without needing a hand-maintained
// symbol->sector map for every name that already resolves here.
func TestSearchExtractsSector(t *testing.T) {
	t.Parallel()

	y, _ := searchServer(t, sapSearchWithSector)

	got, err := y.Search(t.Context(), "sap", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(got) != 1 || got[0].Sector != "Technology" {
		t.Fatalf("got %+v, want one result with Sector = Technology", got)
	}
}

// An ETF has no sector, and that must come through as a genuinely empty
// string, not a placeholder — a caller (setAutoSector) uses emptiness itself
// to mean "nothing learned, still worth reconsidering on a future pass."
func TestSearchLeavesSectorEmptyForAFund(t *testing.T) {
	t.Parallel()

	y, _ := searchServer(t, etfSearchWithNoSector)

	got, err := y.Search(t.Context(), "xmus", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(got) != 1 || got[0].Sector != "" {
		t.Fatalf("got %+v, want one result with an empty Sector", got)
	}
}

// Yahoo mixes in entities it cannot chart. Adding one would put a symbol on the
// watchlist that warms up to nothing and never produces a signal.
func TestSearchDropsUnpriceableResults(t *testing.T) {
	t.Parallel()

	y, _ := searchServer(t, bayerSearch)

	got, err := y.Search(t.Context(), "bayer", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, r := range got {
		if r.Symbol == "^BAYX" {
			t.Error("an entry Yahoo does not price was offered as addable")
		}
	}
}

// The limit is passed upstream and clamped, so neither a caller nor a crafted
// query can ask for an unbounded response.
func TestSearchClampsTheLimit(t *testing.T) {
	t.Parallel()

	y, seen := searchServer(t, bayerSearch)

	if _, err := y.Search(t.Context(), "bayer", 5000); err != nil {
		t.Fatalf("Search: %v", err)
	}

	if want := "quotesCount=25"; !contains(*seen, want) {
		t.Errorf("request was %q, want it to carry %q", *seen, want)
	}
}

// An empty query must not become an upstream request at all.
func TestSearchIgnoresAnEmptyQuery(t *testing.T) {
	t.Parallel()

	y, seen := searchServer(t, bayerSearch)

	got, err := y.Search(t.Context(), "   ", 10)
	if err != nil || got != nil {
		t.Errorf("Search(blank) = %v, %v; want nil, nil", got, err)
	}

	if *seen != "" {
		t.Errorf("a blank query still called upstream: %q", *seen)
	}
}

// Lookup answers "what is this symbol called?" and must match exactly, not
// merely well — BAYRY is a different instrument from BAYN.DE.
func TestLookupMatchesTheExactSymbol(t *testing.T) {
	t.Parallel()

	y, _ := searchServer(t, bayerSearch)

	got, ok := y.Lookup(t.Context(), "bayn.de")
	if !ok {
		t.Fatal("Lookup found nothing for a symbol present in the results")
	}

	if got.Symbol != "BAYN.DE" {
		t.Errorf("got %q, want BAYN.DE", got.Symbol)
	}

	if _, ok := y.Lookup(t.Context(), "BAY"); ok {
		t.Error("Lookup matched a symbol that is only a prefix of a real one")
	}
}

// An upstream failure must be an error, not an empty result that the caller
// would cache as "this symbol has no name".
func TestSearchReportsUpstreamFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	if _, err := y.Search(t.Context(), "bayer", 10); err == nil {
		t.Error("a 429 was reported as success")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
