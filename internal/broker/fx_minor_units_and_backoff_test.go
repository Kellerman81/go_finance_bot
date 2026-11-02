package broker

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// t212FXServer serves instruments with the given currency per ticker and a
// portfolio of the given positions.
func t212FXServer(t *testing.T, insts, portfolio []map[string]any) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(insts)
	})
	mux.HandleFunc("/equity/metadata/exchanges", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/equity/portfolio", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(portfolio)
	})
	mux.HandleFunc("/equity/account/cash", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"free": 1000.0, "total": 1000.0})
	})
	mux.HandleFunc("/equity/account/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"currencyCode": "EUR"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

// BR3-1: Trading 212 quotes London lines in GBX (pence), and the ECB-backed
// rate API has no GBX, so every lookup failed and the 1:1 stopgap valued pence
// as euros: 1000 shares at 72p counted as 72000 instead of about 830 —
// inflating equity and every pct limit, and tripping the drawdown halt when
// the position was sold. A minor unit converts through its major currency.
func TestAPenceQuotedPositionIsConvertedThroughPounds(t *testing.T) {
	fx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("base") != "GBP" {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)

			return
		}

		_, _ = w.Write([]byte(`{"rates":{"EUR":1.15}}`))
	}))
	defer fx.Close()

	srv := t212FXServer(t,
		[]map[string]any{{"ticker": "VODl_EQ", "shortName": "VOD", "isin": "GB00BH4HKS39", "currencyCode": "GBX", "type": "STOCK"}},
		[]map[string]any{{"ticker": "VODl_EQ", "quantity": 1000.0, "averagePrice": 70.0, "currentPrice": 72.0, "ppl": 17.0}},
	)

	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.fx.baseURL = fx.URL

	pos, err := c.GetPositions(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if want := 1000 * 72 * 0.01 * 1.15; math.Abs(pos[0].MarketValue-want) > 1e-6 {
		t.Errorf("GBX position valued at %v, want %v (pence → pounds → euros)", pos[0].MarketValue, want)
	}
}

// BR3-2: a failed rate lookup was retried on every call under the broker's
// 15–20 s client timeout. With the rate API hanging, one sync with five US
// holdings waited five timeouts — on the goroutine that runs checkExits, so
// stops fired minutes late. A failure is remembered for a while.
func TestAFailingRatePairIsNotRefetchedPerPosition(t *testing.T) {
	var calls atomic.Int32

	fx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond)
	}))
	defer fx.Close()

	srv := t212FXServer(t,
		[]map[string]any{
			{"ticker": "AAPL_US_EQ", "shortName": "AAPL", "isin": "US0378331005", "currencyCode": "USD"},
			{"ticker": "MSFT_US_EQ", "shortName": "MSFT", "isin": "US5949181045", "currencyCode": "USD"},
			{"ticker": "NVDA_US_EQ", "shortName": "NVDA", "isin": "US67066G1040", "currencyCode": "USD"},
		},
		[]map[string]any{
			{"ticker": "AAPL_US_EQ", "quantity": 1.0, "averagePrice": 1.0, "currentPrice": 1.0},
			{"ticker": "MSFT_US_EQ", "quantity": 1.0, "averagePrice": 1.0, "currentPrice": 1.0},
			{"ticker": "NVDA_US_EQ", "quantity": 1.0, "averagePrice": 1.0, "currentPrice": 1.0},
		},
	)

	errlog.Clear()

	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.fx.baseURL = fx.URL
	c.fx.http = &http.Client{Timeout: 100 * time.Millisecond}

	for range 2 {
		c.limiters = map[string]*t212RateLimiter{}

		if _, err := c.GetPositions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("one failing pair was fetched %d times over two syncs of three positions, want 1", n)
	}

	// EL3-2: and reported once. Six entries — one per position per sync, under
	// the broker's own source — evicted that broker's order rejections from
	// the diagnostics list during an outage.
	fxEntries := 0

	for _, e := range errlog.Recent() {
		if strings.Contains(e.Message, "fx rate") {
			fxEntries++

			if strings.Count(e.Message, "fx rate") > 1 {
				t.Errorf("the message repeats its prefix: %q", e.Message)
			}
		}
	}

	if fxEntries != 1 {
		t.Errorf("%d fx entries in the diagnostics list for one failing pair, want 1", fxEntries)
	}
}

// BR3-2: the converter has its own short timeout, not the broker's order
// client's 15–20 s.
func TestTheRateLookupHasItsOwnShortTimeout(t *testing.T) {
	c := newFXConverter(&http.Client{Timeout: 20 * time.Second})

	if c.http.Timeout != fxTimeout {
		t.Errorf("fx timeout %v, want %v", c.http.Timeout, fxTimeout)
	}
}
