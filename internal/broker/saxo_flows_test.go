package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// fakeSaxo is an in-process Saxo OpenAPI covering the account, instrument,
// working-order, order, bracket and chart endpoints. It records the
// requests it receives so a test can check what actually went out.
type fakeSaxo struct {
	mu       sync.Mutex
	accounts string // JSON for /port/v1/accounts/me
	working  string // JSON for /port/v1/orders/me
	posted   map[string]any
	patched  map[string]any
	deleted  string
	chartQ   url.Values
}

func (f *fakeSaxo) server(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /port/v1/accounts/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.accounts))
	})
	mux.HandleFunc("GET /ref/v1/instruments", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("Keywords"), "BOND") {
			_, _ = w.Write([]byte(`{"Data":[{"Identifier":9,"AssetType":"","Symbol":"BOND:xx","CurrencyCode":"EUR"}]}`))

			return
		}

		_, _ = w.Write([]byte(`{"Data":[{"Identifier":211,"AssetType":"Stock","Symbol":"AAPL:xnas","CurrencyCode":"USD"}]}`))
	})
	mux.HandleFunc("GET /ref/v1/instruments/details/211/Stock", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"TickSize":0.05}`))
	})
	mux.HandleFunc("GET /port/v1/orders/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.working))
	})
	mux.HandleFunc("POST /trade/v2/orders", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&f.posted)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"OrderId":"E1","Orders":[{"OrderId":"S1"},{"OrderId":"T1"}]}`))
	})
	mux.HandleFunc("PATCH /trade/v2/orders", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&f.patched)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"OrderId":"M1"}`))
	})
	mux.HandleFunc("DELETE /trade/v2/orders/{ids}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = r.PathValue("ids") + "?" + r.URL.RawQuery
		f.mu.Unlock()
	})
	mux.HandleFunc("GET /chart/v3/charts", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.chartQ = r.URL.Query()
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"Data":[
			{"Time":"2026-10-30T14:00:00Z","Open":1,"High":2,"Low":0.5,"Close":1.5,"Volume":10},
			{"Time":"2026-10-30T14:05:00Z","Open":1.5,"High":2.5,"Low":1,"Close":2,"Volume":20}]}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

// TestNewSaxoResolvesTheConfiguredAccount guards account selection on a
// multi-account login: the configured account_key must be the one orders go
// to, an unknown key falls back to the first account rather than failing
// startup, and a login with no accounts at all is an error, never a client
// with empty keys that sends every order to nowhere.
func TestNewSaxoResolvesTheConfiguredAccount(t *testing.T) {
	t.Parallel()

	f := &fakeSaxo{accounts: `{"Data":[{"AccountKey":"A1","ClientKey":"C1"},{"AccountKey":"A2","ClientKey":"C2"}]}`}
	srv := f.server(t)

	s, err := NewSaxo(srv.URL+"/", NewStaticTokenSource("tok"), "A2", "", "")
	if err != nil {
		t.Fatalf("NewSaxo: %v", err)
	}

	if s.accountKey != "A2" || s.clientKey != "C2" || s.assetTypes != "Stock,Etf" {
		t.Errorf("resolved %q/%q types %q, want A2/C2 Stock,Etf", s.accountKey, s.clientKey, s.assetTypes)
	}

	s, err = NewSaxo(srv.URL, NewStaticTokenSource("tok"), "NOPE", "Stock", "eur")
	if err != nil {
		t.Fatalf("NewSaxo(unknown key): %v", err)
	}

	if s.accountKey != "A1" || s.preferCcy != "EUR" {
		t.Errorf("unknown key resolved to %q (prefer %q), want the first account A1, EUR", s.accountKey, s.preferCcy)
	}

	if s.Name() != "saxo" {
		t.Errorf("Name = %q", s.Name())
	}

	f.accounts = `{"Data":[]}`

	if _, err := NewSaxo(srv.URL, NewStaticTokenSource("tok"), "", "", ""); err == nil {
		t.Error("a login with no accounts built a client")
	}
}

// TestSaxoHistoryCandles guards Saxo as a warmup source: the requested
// window must become a bar Count at the resolution's Horizon, capped at
// Saxo's 1200 and defaulted when the window is empty, and bars come back
// with the caller's symbol, not Saxo's exchange-qualified one.
func TestSaxoHistoryCandles(t *testing.T) {
	t.Parallel()

	f := &fakeSaxo{}
	srv := f.server(t)

	h := NewSaxoHistory(srv.URL, NewStaticTokenSource("tok"), "")

	to := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)

	bars, err := h.Candles(context.Background(), "AAPL", market.Res5Min, to.Add(-100*time.Minute), to)
	if err != nil {
		t.Fatalf("Candles: %v", err)
	}

	if len(bars) != 2 || bars[0].Symbol != "AAPL" || bars[1].Close != 2 || bars[1].Volume != 20 {
		t.Errorf("bars = %+v", bars)
	}

	if f.chartQ.Get("Horizon") != "5" || f.chartQ.Get("Count") != "20" || f.chartQ.Get("Uic") != "211" {
		t.Errorf("chart query = %v, want Horizon 5, Count 20, Uic 211", f.chartQ)
	}

	_, _ = h.Candles(context.Background(), "AAPL", market.Res1Min, to.Add(-30*24*time.Hour), to)
	if f.chartQ.Get("Count") != "1200" {
		t.Errorf("a month of minutes asked for Count %s, want the 1200 cap", f.chartQ.Get("Count"))
	}

	_, _ = h.Candles(context.Background(), "AAPL", market.Res1Day, to, to)
	if f.chartQ.Get("Count") != "200" || f.chartQ.Get("Horizon") != "1440" {
		t.Errorf("empty window asked for Count %s Horizon %s, want 200 / 1440",
			f.chartQ.Get("Count"), f.chartQ.Get("Horizon"))
	}

	for res, want := range map[market.Resolution]int{
		market.Res1Min: 1, market.Res15Min: 15, market.Res1Hour: 60, "x": 1,
	} {
		if got := horizonMinutes(res); got != want {
			t.Errorf("horizonMinutes(%q) = %d, want %d", res, got, want)
		}
	}
}

// saxoClient builds a resolved client against f without the account call.
func saxoClient(t *testing.T, f *fakeSaxo) *Saxo {
	t.Helper()

	srv := f.server(t)
	s := NewSaxoHistory(srv.URL, NewStaticTokenSource("tok"), "")
	s.accountKey, s.clientKey = "ACC", "CK"

	return s
}

// TestSaxoOpenOrdersAndModify guards the working-order view and BR2-2 /
// BR3-7: OpenOrders must report each order's own type and duration, and a
// modification must keep both — re-sending a stop leg as "Limit" fills it
// at once, and re-sending a DayOrder as GoodTillCancel lets it outlive the
// session. An order no longer working, or a listing that fails, refuses the
// modification instead of guessing.
func TestSaxoOpenOrdersAndModify(t *testing.T) {
	t.Parallel()

	f := &fakeSaxo{working: `{"Data":[{"OrderId":"O1","Uic":211,"BuySell":"Sell","Amount":10,"Price":180,
		"OpenOrderType":"StopIfTraded","Status":"Working","Duration":{"DurationType":"DayOrder"},
		"DisplayAndFormat":{"Symbol":"AAPL:xnas","Currency":"USD"}}]}`}
	s := saxoClient(t, f)
	ctx := context.Background()

	orders, err := s.OpenOrders(ctx)
	if err != nil || len(orders) != 1 {
		t.Fatalf("OpenOrders = %v, %v", orders, err)
	}

	o := orders[0]
	if o.OrderID != "O1" || o.Symbol != "AAPL" || o.Side != Sell || o.Type != "StopIfTraded" ||
		o.Qty != 10 || o.Price != 180 || o.Duration != "DayOrder" {
		t.Errorf("open order = %+v", o)
	}

	res, err := s.ModifyOrder(ctx, OrderModification{
		OrderID: "O1", Symbol: "AAPL", Side: Sell, Type: Limit, Qty: 10.9999999999, Price: 175.03,
	})
	if err != nil {
		t.Fatalf("ModifyOrder: %v", err)
	}

	if res.ID != "M1" || res.Status != "replaced" {
		t.Errorf("modify result = %+v", res)
	}

	if f.patched["OrderType"] != "StopIfTraded" {
		t.Errorf("modified OrderType = %v, want the working StopIfTraded kept", f.patched["OrderType"])
	}

	if d, _ := f.patched["OrderDuration"].(map[string]any); d["DurationType"] != "DayOrder" {
		t.Errorf("modified duration = %v, want DayOrder kept", f.patched["OrderDuration"])
	}

	if f.patched["OrderPrice"] != 175.05 {
		t.Errorf("modified price = %v, want 175.05 on the 0.05 grid", f.patched["OrderPrice"])
	}

	if _, err := s.ModifyOrder(ctx, OrderModification{OrderID: "GONE", Symbol: "AAPL", Qty: 1}); err == nil ||
		!strings.Contains(err.Error(), "not working") {
		t.Errorf("modifying an order that is not working: err = %v", err)
	}

	if _, err := s.ModifyOrder(ctx, OrderModification{Symbol: "AAPL", Qty: 1}); err == nil {
		t.Error("a modification without an order id was sent")
	}

	if _, err := s.ModifyOrder(ctx, OrderModification{OrderID: "O1", Symbol: "AAPL", Qty: 0.5}); err == nil {
		t.Error("a modification flooring to 0 shares was sent")
	}

	f.working = `not json`

	if _, err := s.workingOrder(ctx, "O1"); err == nil || !strings.Contains(err.Error(), "cannot confirm") {
		t.Errorf("a failed listing: err = %v, want a refusal", err)
	}
}

// TestSaxoCancelOrders guards the cancel path: several ids go in ONE
// request with the account key, and an empty list is refused rather than
// sent as DELETE /trade/v2/orders/ — which Saxo may read as "all".
func TestSaxoCancelOrders(t *testing.T) {
	t.Parallel()

	f := &fakeSaxo{}
	s := saxoClient(t, f)

	if err := s.CancelOrders(context.Background()); err == nil {
		t.Error("CancelOrders with no ids was sent")
	}

	if err := s.CancelOrders(context.Background(), "O1", "O2"); err != nil {
		t.Fatalf("CancelOrders: %v", err)
	}

	if f.deleted != "O1,O2?AccountKey=ACC" {
		t.Errorf("DELETE = %q, want O1,O2?AccountKey=ACC", f.deleted)
	}
}

// TestSaxoPlaceBracket guards Saxo's native bracket, the only real OCO this
// codebase has: inverted protective legs are refused before anything is
// sent, quantities floor to whole shares, every price lands on the tick
// grid, and the related-order ids map back to stop and take-profit in the
// order they were declared — mixing them up would cancel the wrong leg.
func TestSaxoPlaceBracket(t *testing.T) {
	t.Parallel()

	f := &fakeSaxo{}
	s := saxoClient(t, f)
	s.SetMinOrderInterval(time.Millisecond)

	ctx := context.Background()

	for _, bad := range []BracketOrder{
		{Symbol: "AAPL", Side: "hold", Qty: 1},
		{Symbol: "AAPL", Side: Buy, Qty: 0},
		{Symbol: "AAPL", Side: Buy, Qty: 1, EntryType: Limit},
		{Symbol: "AAPL", Side: Buy, Qty: 1, StopLoss: 200, TakeProfit: 150},
		{Symbol: "AAPL", Side: Sell, Qty: 1, StopLoss: 150, TakeProfit: 200},
		{Symbol: "AAPL", Side: Buy, Qty: 0.4},
	} {
		if _, err := s.PlaceBracket(ctx, bad); err == nil {
			t.Errorf("bracket %+v was accepted", bad)
		}
	}

	if f.posted != nil {
		t.Fatalf("a refused bracket reached the broker: %v", f.posted)
	}

	res, err := s.PlaceBracket(ctx, BracketOrder{
		Symbol: "AAPL", Side: Buy, Qty: 3.7, EntryType: Limit, EntryPrice: 170.02,
		StopLoss: 160.01, TakeProfit: 190.08,
	})
	if err != nil {
		t.Fatalf("PlaceBracket: %v", err)
	}

	if res.EntryID != "E1" || res.StopID != "S1" || res.TakeProfitID != "T1" || len(res.Orders) != 3 {
		t.Errorf("result = %+v", res)
	}

	if res.Orders[1].Side != Sell || res.Orders[0].Qty != 3 {
		t.Errorf("legs = %+v, want the protective legs on the sell side, 3 whole shares", res.Orders)
	}

	if f.posted["OrderPrice"] != 170.0 {
		t.Errorf("entry price = %v, want 170 on the 0.05 grid", f.posted["OrderPrice"])
	}

	// Only a take-profit: the first related id is the take-profit.
	res, err = s.PlaceBracket(ctx, BracketOrder{Symbol: "AAPL", Side: Buy, Qty: 1, TakeProfit: 190})
	if err != nil || res.StopID != "" || res.TakeProfitID != "S1" {
		t.Errorf("TP-only bracket: %+v, %v; want the first related id as the take-profit", res, err)
	}

	got := s.bracketOnTickGrid(ctx, instrument{Uic: 211, AssetType: "Stock"},
		BracketOrder{EntryType: Market, EntryPrice: 170.02, StopLoss: 160.01, TakeProfit: 0})
	if got.EntryPrice != 170.02 || got.StopLoss != 160.0 || got.TakeProfit != 0 {
		t.Errorf("bracketOnTickGrid = %+v: a market entry and an absent leg must be left alone", got)
	}
}

// TestSaxoInstrumentLookups guards the classifier and currency resolvers the
// engine falls back to: AssetClass reports Saxo's AssetType and refuses an
// instrument with none (a blank class would bucket the symbol as
// "unclassified" without saying why); with no preferred currency
// InstrumentCurrency and fxRateForCurrency are 1:1 and resolved.
func TestSaxoInstrumentLookups(t *testing.T) {
	t.Parallel()

	f := &fakeSaxo{}
	s := saxoClient(t, f)
	ctx := context.Background()

	if c, err := s.AssetClass(ctx, "AAPL"); err != nil || c != "Stock" {
		t.Errorf("AssetClass(AAPL) = %q, %v", c, err)
	}

	if _, err := s.AssetClass(ctx, "BOND"); err == nil {
		t.Error("an instrument with no AssetType was classified")
	}

	ccy, rate, err := s.InstrumentCurrency(ctx, "AAPL")
	if err != nil || ccy != "USD" || rate != 1 {
		t.Errorf("InstrumentCurrency = %q, %v, %v; want USD 1", ccy, rate, err)
	}

	if r := s.fxRateForCurrency(ctx, "USD"); r != 1 {
		t.Errorf("fxRateForCurrency with no preferCcy = %v, want 1", r)
	}
}

// TestSaxoPaceSpacesOrders guards Saxo's ~1 order/second session limit:
// pace must hold the second order back by the configured interval, and a
// cancelled context must abort the wait rather than send late.
func TestSaxoPaceSpacesOrders(t *testing.T) {
	t.Parallel()

	s := &Saxo{}
	s.SetMinOrderInterval(80 * time.Millisecond)

	ctx := context.Background()
	_ = s.pace(ctx)

	start := time.Now()
	_ = s.pace(ctx)

	if gap := time.Since(start); gap < 60*time.Millisecond {
		t.Errorf("second order went out after %v, want ~80ms", gap)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	if err := s.pace(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("pace on a cancelled context = %v, want context.Canceled", err)
	}
}

// TestSaxoTokenSources guards both auth modes. The static source must refuse
// to hand out an empty token (a request with "Bearer " fails confusingly far
// from the config typo). The certificate source must cache an access token,
// renew it through the cheap refresh grant while the refresh token lives,
// fall back to a fresh JWT assertion when the refresh is refused, and surface
// the endpoint's error when even that fails.
func TestSaxoTokenSources(t *testing.T) {
	t.Parallel()

	if _, err := NewStaticTokenSource("").Token(context.Background()); err == nil {
		t.Error("an empty static token was handed out")
	}

	if tok, err := NewStaticTokenSource("abc").Token(context.Background()); err != nil || tok != "abc" {
		t.Errorf("static Token = %q, %v", tok, err)
	}

	var (
		mu      sync.Mutex
		grants  []string
		refuse  bool
		counter int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		user, pass, _ := r.BasicAuth()

		mu.Lock()
		defer mu.Unlock()

		grants = append(grants, r.Form.Get("grant_type"))

		if user != "app" || pass != "secret" {
			t.Errorf("basic auth = %q/%q", user, pass)
		}

		if refuse || (r.Form.Get("grant_type") == "refresh_token" && counter > 1) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"nope"}`))

			return
		}

		counter++
		// expires_in 0: the token is stale at once, so every call renews.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "AT" + string(rune('0'+counter)), "expires_in": 0,
			"refresh_token": "RT", "refresh_token_expires_in": 3600,
		})
	}))
	t.Cleanup(srv.Close)

	path, _, _ := writeTestCert(t)

	src, err := NewCertTokenSource(CertConfig{
		AppKey: "app", AppSecret: "secret", UserID: "u", AuthURL: srv.URL + "/", CertPath: path,
	})
	if err != nil {
		t.Fatalf("NewCertTokenSource: %v", err)
	}

	ctx := context.Background()

	if tok, err := src.Token(ctx); err != nil || tok != "AT1" {
		t.Fatalf("first Token = %q, %v", tok, err)
	}

	if tok, err := src.Token(ctx); err != nil || tok != "AT2" {
		t.Fatalf("second Token = %q, %v (want a refresh)", tok, err)
	}

	// The refresh is refused now: a new assertion must be minted.
	if tok, err := src.Token(ctx); err != nil || tok != "AT3" {
		t.Fatalf("third Token = %q, %v (want a fresh assertion)", tok, err)
	}

	mu.Lock()
	want := "urn:saxobank:oauth:grant-type:personal-jwt,refresh_token,refresh_token," +
		"urn:saxobank:oauth:grant-type:personal-jwt"
	if got := strings.Join(grants, ","); got != want {
		t.Errorf("grants = %s\nwant     %s", got, want)
	}

	refuse = true
	mu.Unlock()

	if _, err := src.Token(ctx); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("Token with every grant refused: err = %v, want the endpoint's error", err)
	}

	// A token still inside its lifetime is served from the cache.
	src.access, src.accessExp = "CACHED", time.Now().Add(time.Hour)

	if tok, _ := src.Token(ctx); tok != "CACHED" {
		t.Errorf("a live token was not served from cache: %q", tok)
	}
}

// TestLoadCertificateErrors guards the startup failure messages for a bad
// certificate: a missing file, a .p12 that is not one, and a PEM with only
// half the pair must each be an error naming the problem, never a nil key
// that panics at the first signature.
func TestLoadCertificateErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, _, err := loadCertificate(filepath.Join(dir, "missing.pem"), ""); err == nil {
		t.Error("a missing file loaded")
	}

	p12 := filepath.Join(dir, "bad.P12")
	_ = os.WriteFile(p12, []byte("not a pkcs12 bundle"), 0o600)

	if _, _, err := loadCertificate(p12, "pw"); err == nil || !strings.Contains(err.Error(), "pkcs12") {
		t.Errorf("a corrupt .p12: err = %v", err)
	}

	full, _, _ := writeTestCert(t)
	data, _ := os.ReadFile(full)
	certOnly := filepath.Join(dir, "cert-only.pem")
	_ = os.WriteFile(certOnly, data[:strings.Index(string(data), "-----BEGIN PRIVATE KEY")], 0o600)

	if _, _, err := loadCertificate(certOnly, ""); err == nil {
		t.Error("a PEM without a key loaded")
	}

	if _, err := NewCertTokenSource(CertConfig{AppKey: "a", UserID: "u", CertPath: full}); err == nil {
		t.Error("a cert source without auth_url was built")
	}
}
