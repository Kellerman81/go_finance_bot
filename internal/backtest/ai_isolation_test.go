package backtest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// aiSeries is n one-minute bars compounding by step from start, starting at t0.
func aiSeries(n int, start, step float64, t0 time.Time) []market.Candle {
	out := make([]market.Candle, n)
	p := start
	for i := range out {
		p *= step
		out[i] = market.Candle{Symbol: "AAPL", Time: t0.Add(time.Duration(i) * time.Minute),
			Open: p, High: p * 1.002, Low: p * 0.998, Close: p, Volume: 1000}
	}
	return out
}

// ST3-1 / AI3-2: an in-process backtest — what the strategy tab's field
// optimizer runs, with no guard like autotune's — used the LIVE ai advisor:
// replayed bars got today's verdict (lookahead), and a due refresh fed
// historical features overwrote the verdict the live loop trades on. Live read
// BUY, a backtest ran over three-month-old bars, live then read SELL.
func TestABacktestDoesNotTouchTheLiveAIVerdict(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		n := calls.Add(1)
		act := "SELL"
		if n == 1 {
			act = "BUY"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"{\"action\":\"`+act+`\",\"confidence\":0.9}"}}`)
	}))
	defer srv.Close()

	if err := ai.Configure(config.AIConfig{Enabled: true, Provider: "ollama", BaseURL: srv.URL,
		Model: "m", RefreshInterval: time.Millisecond, MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ai.Configure(config.AIConfig{}) }()

	sc := config.Default().Strategy
	sc.Detectors = []string{"ai"}
	sc.TrendGate = false
	sc.MinStrength = 0.1

	live := strategy.New(sc)
	liveBars := aiSeries(3000, 100, 1.0005, time.Now().Add(-3000*time.Minute))
	live.Evaluate("AAPL", liveBars)
	deadline := time.Now().Add(2 * time.Second)
	var sig strategy.Signal
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		if sig = live.Evaluate("AAPL", liveBars); sig.Action == strategy.Buy {
			break
		}
	}
	if sig.Action != strategy.Buy {
		t.Fatalf("live never got its BUY: %+v", sig)
	}

	hist := aiSeries(400, 300, 0.999, time.Now().AddDate(0, -3, 0))
	bt := &Backtester{Strategy: strategy.New(sc), Limits: testLimits(), Cash: 100000}
	bt.Run(map[string][]market.Candle{"AAPL": hist})
	time.Sleep(200 * time.Millisecond)

	after := live.Evaluate("AAPL", liveBars)
	if after.Action != strategy.Buy {
		t.Errorf("a backtest on historical bars replaced the live verdict: live now %s", after.Action)
	}
}
