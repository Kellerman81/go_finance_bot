package market

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Instrument is one result of a symbol search: the ticker to trade and enough
// context for a human to tell two similarly-named listings apart.
//
// The same company is listed on several venues with different symbols and
// currencies — BAYN.DE and BAYRY are both Bayer — so Exchange is not decoration.
// Picking the wrong one is how you end up holding an unhedged ADR you did not
// mean to buy.
type Instrument struct {
	// Symbol is what goes on the watchlist, in the provider's own convention.
	Symbol string `json:"symbol"`
	// Name is the company or fund name, e.g. "Bayer Aktiengesellschaft".
	Name string `json:"name"`
	// Exchange is the listing venue, e.g. "XETRA".
	Exchange string `json:"exchange"`
	// Type is the instrument class, e.g. "EQUITY" or "ETF".
	Type string `json:"type"`
	// Sector is Yahoo's GICS-style sector label, e.g. "Technology" — empty
	// for funds (ETFs have no sector) and anything Yahoo has no
	// classification for. Feeds risk.PortfolioGuard's sector concentration
	// cap without needing a hand-maintained symbol->sector map for every
	// name that already resolves here.
	Sector string `json:"sector"`
}

// Searcher finds instruments by name or ticker. A provider implements it when
// it can answer "what is this company's symbol?" — the question an operator
// actually has, since almost nobody knows that Lanxess is LXS.DE.
type Searcher interface {
	// Search returns at most limit matches for query, best first.
	Search(ctx context.Context, query string, limit int) ([]Instrument, error)
}

// searchLimit bounds what any caller may ask for. A symbol picker is scanned by
// eye, so a long list is worse than a short one, and the cap also keeps a
// hostile query from turning into a large upstream response.
const searchLimit = 25

// Search finds instruments matching query using Yahoo's search endpoint, which
// answers in the same symbol convention as the chart endpoint this provider
// already uses for warmup — so a symbol picked here is one the engine can
// actually fetch history for.
//
// That correspondence is the reason to search Yahoo rather than the broker.
// Trading 212's instrument metadata has better names, but its tickers
// (BAYNd_EQ) are not what the watchlist or the history source speak, and the
// broker mapping is already handled downstream by resolveTicker/ISIN.
func (y *YahooHistory) Search(
	ctx context.Context,
	query string,
	limit int,
) ([]Instrument, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	if limit <= 0 || limit > searchLimit {
		limit = searchLimit
	}

	if err := y.wait(ctx); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("q", query)
	q.Set("quotesCount", fmt.Sprint(limit))
	q.Set("newsCount", "0")
	q.Set("listsCount", "0")

	var raw struct {
		Quotes []yahooSearchHit `json:"quotes"`
	}

	if err := y.getJSON(ctx, "/v1/finance/search?"+q.Encode(), &raw); err != nil {
		return nil, err
	}

	out := make([]Instrument, 0, len(raw.Quotes))

	for _, r := range raw.Quotes {
		// Yahoo mixes in results it cannot price (indices from partner feeds,
		// currencies, the odd news entity). A symbol with no chart behind it
		// would warm up to nothing and sit on the watchlist doing nothing.
		if r.Symbol == "" || !r.Priceable {
			continue
		}

		out = append(out, Instrument{
			Symbol:   r.Symbol,
			Name:     firstNonEmpty(r.LongName, r.ShortName, r.Symbol),
			Exchange: firstNonEmpty(r.ExchDisp, r.Exchange),
			Type:     strings.ToUpper(r.QuoteType),
			Sector:   r.Sector,
		})
	}

	return out, nil
}

// yahooSearchHit is one entry of the search response (distinct from
// yahooQuote, which is the chart endpoint's OHLC row). Only the naming and
// sector fields are read; the endpoint returns a good deal more (industry,
// score, type display strings, ...) that this bot has no use for.
type yahooSearchHit struct {
	Symbol    string `json:"symbol"`
	ShortName string `json:"shortname"`
	LongName  string `json:"longname"`
	ExchDisp  string `json:"exchDisp"`
	Exchange  string `json:"exchange"`
	QuoteType string `json:"quoteType"`
	// Sector is absent for funds/ETFs and anything Yahoo has no
	// classification for, present (e.g. "Technology") for most equities.
	Sector string `json:"sector"`
	// Priceable reports that Yahoo itself charts this entry. The response also
	// carries partner indices and news entities, which would warm up to nothing.
	Priceable bool `json:"isYahooFinance"`
}

// Lookup returns the instrument details for an exact symbol, or ok=false when
// the provider has nothing matching. It is Search constrained to an exact hit,
// which is what naming an already-tracked symbol needs.
func (y *YahooHistory) Lookup(ctx context.Context, symbol string) (Instrument, bool) {
	matches, err := y.Search(ctx, symbol, searchLimit)
	if err != nil {
		return Instrument{}, false
	}

	for _, m := range matches {
		if strings.EqualFold(m.Symbol, symbol) {
			return m, true
		}
	}

	return Instrument{}, false
}

// getJSON performs one paced GET against a Yahoo host and decodes the body.
// Unlike fetch it does not retry: a search is interactive, and a user waiting on
// a picker is better served by a fast empty list than a slow correct one.
func (y *YahooHistory) getJSON(ctx context.Context, path string, out any) error {
	var lastErr error

	for _, base := range y.endpoints() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}

		req.Header.Set("User-Agent", yahooUserAgent)

		resp, err := y.http.Do(req)
		if err != nil {
			lastErr = err

			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()

			lastErr = fmt.Errorf("yahoo: %s returned %d", path, resp.StatusCode)

			continue
		}

		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()

		if err != nil {
			return fmt.Errorf("yahoo: decode %s: %w", path, err)
		}

		return nil
	}

	return lastErr
}

// firstNonEmpty returns the first non-blank string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}

	return ""
}
