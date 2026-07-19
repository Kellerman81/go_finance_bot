package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/gorilla/websocket"
)

// Finnhub is a DataProvider backed by Finnhub: a WebSocket feed for live trades
// and a REST endpoint for historical candles. It auto-reconnects the WebSocket
// and re-subscribes the active symbol set on reconnect.
type Finnhub struct {
	apiKey   string
	restBase string
	wsBase   string

	http *http.Client
	log  *slog.Logger

	out chan Quote

	mu      sync.Mutex
	conn    *websocket.Conn
	symbols map[string]struct{}
	closed  bool
	done    chan struct{}
	last    map[string]Quote // latest tick per symbol (for LastQuote)
}

// NewFinnhub constructs a Finnhub provider and starts its connection loop.
func NewFinnhub(apiKey, restBase, wsBase string, log *slog.Logger) *Finnhub {
	if log == nil {
		log = slog.Default()
	}

	f := &Finnhub{
		apiKey:   apiKey,
		restBase: restBase,
		wsBase:   wsBase,
		http:     &http.Client{Timeout: 15 * time.Second},
		log:      log.With("component", "finnhub"),
		out:      make(chan Quote, 1024),
		symbols:  make(map[string]struct{}),
		done:     make(chan struct{}),
		last:     make(map[string]Quote),
	}

	go f.connectLoop()

	return f
}

// Quotes returns the live tick channel.
func (f *Finnhub) Quotes() <-chan Quote { return f.out }

// LastQuote returns the latest tick seen for symbol.
func (f *Finnhub) LastQuote(symbol string) (Quote, bool) {
	f.mu.Lock()

	q, ok := f.last[symbol]
	f.mu.Unlock()

	return q, ok
}

// Subscribe registers symbols for live streaming.
func (f *Finnhub) Subscribe(symbols ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return errors.New("provider closed")
	}

	for _, s := range symbols {
		if s == "" {
			continue
		}

		f.symbols[s] = struct{}{}
		if f.conn == nil {
			continue
		}

		if err := f.send("subscribe", s); err != nil {
			f.log.Warn("subscribe failed", "symbol", s, "err", err)
		}
	}

	return nil
}

// Unsubscribe stops streaming the given symbols.
func (f *Finnhub) Unsubscribe(symbols ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, s := range symbols {
		delete(f.symbols, s)

		if f.conn != nil {
			_ = f.send("unsubscribe", s)
		}
	}

	return nil
}

// Close stops the connection loop and closes the tick channel.
func (f *Finnhub) Close() error {
	f.mu.Lock()

	if f.closed {
		f.mu.Unlock()

		return nil
	}

	f.closed = true
	close(f.done)

	if f.conn != nil {
		_ = f.conn.Close()
	}

	f.mu.Unlock()

	return nil
}

// send writes a subscribe/unsubscribe control frame. Caller must hold f.mu.
func (f *Finnhub) send(typ, symbol string) error {
	return f.conn.WriteJSON(map[string]string{"type": typ, "symbol": symbol})
}

// connectLoop dials the websocket, resubscribes, and reads ticks until Close,
// reconnecting with exponential backoff on failure.
func (f *Finnhub) connectLoop() {
	backoff := time.Second
	for {
		select {
		case <-f.done:
			close(f.out)

			return

		default:
		}

		if err := f.runConn(); err != nil {
			f.log.Warn("websocket connection ended", "err", err, "retry_in", backoff)
			errlog.Recordf("finnhub", "websocket: %s", err.Error())
		}

		select {
		case <-f.done:
			close(f.out)

			return

		case <-time.After(backoff):
		}

		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// runConn dials, re-subscribes, and pumps messages until an error or close.
//
//nolint:cyclop // connection lifecycle (dial, resubscribe, pump) reads best as one flow
func (f *Finnhub) runConn() error {
	u := fmt.Sprintf("%s?token=%s", f.wsBase, url.QueryEscape(f.apiKey))

	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if resp != nil {
		// The handshake response body is safe to close: on a successful upgrade
		// gorilla replaces it with a no-op reader.
		_ = resp.Body.Close()
	}

	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	f.mu.Lock()

	if f.closed {
		f.mu.Unlock()

		_ = conn.Close()
		return nil
	}

	f.conn = conn
	for s := range f.symbols {
		if err := f.send("subscribe", s); err != nil {
			f.log.Warn("resubscribe failed", "symbol", s, "err", err)
		}
	}

	f.mu.Unlock()
	f.log.Info("websocket connected", "symbols", len(f.symbols))

	defer func() {
		f.mu.Lock()

		f.conn = nil
		f.mu.Unlock()

		_ = conn.Close()
	}()

	for {
		var msg finnhubMessage

		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		if msg.Type != "trade" {
			continue // ping, error, etc.
		}

		for _, t := range msg.Data {
			q := Quote{
				Symbol: t.Symbol,
				Price:  t.Price,
				Volume: t.Volume,
				Time:   time.UnixMilli(t.Time),
			}

			f.mu.Lock()

			f.last[t.Symbol] = q
			f.mu.Unlock()

			select {
			case f.out <- q:
			case <-f.done:
				return nil
			default:
				// Drop tick if consumer is slow; the latest price wins anyway.
			}
		}
	}
}

// finnhubTrade is one trade entry in a websocket trade message.
type finnhubTrade struct {
	Symbol string  `json:"s"`
	Price  float64 `json:"p"`
	Time   int64   `json:"t"`
	Volume float64 `json:"v"`
}

// finnhubMessage is the envelope of a websocket message.
type finnhubMessage struct {
	Type string         `json:"type"`
	Data []finnhubTrade `json:"data"`
}

// Candles fetches historical OHLCV bars via the Finnhub REST API. Note that the
// /stock/candle endpoint requires a paid plan for some symbol classes; callers
// should treat an error here as "no warmup history" and fall back to building
// candles from the live stream.
func (f *Finnhub) Candles(
	ctx context.Context,
	symbol string,
	res Resolution,
	from, to time.Time,
) ([]Candle, error) {
	endpoint := fmt.Sprintf("%s/stock/candle?symbol=%s&resolution=%s&from=%d&to=%d&token=%s",
		f.restBase, url.QueryEscape(symbol), res, from.Unix(), to.Unix(), url.QueryEscape(f.apiKey))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("candles request: %w", err)
	}

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("candles request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("candles status %d", resp.StatusCode)
	}

	var raw struct {
		C      []float64 `json:"c"`
		H      []float64 `json:"h"`
		L      []float64 `json:"l"`
		O      []float64 `json:"o"`
		V      []float64 `json:"v"`
		T      []int64   `json:"t"`
		Status string    `json:"s"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode candles: %w", err)
	}

	if raw.Status != "ok" {
		return nil, fmt.Errorf("no candle data for %s (status=%q)", symbol, raw.Status)
	}

	out := make([]Candle, len(raw.C))
	for i := range raw.C {
		out[i] = Candle{
			Symbol: symbol,
			Open:   raw.O[i],
			High:   raw.H[i],
			Low:    raw.L[i],
			Close:  raw.C[i],
			Volume: raw.V[i],
			Time:   time.Unix(raw.T[i], 0),
		}
	}

	return out, nil
}
