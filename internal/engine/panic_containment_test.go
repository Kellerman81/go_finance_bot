package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// panickingStrategy stands in for an indicator with an off-by-one.
type panickingStrategy struct{}

func (panickingStrategy) Name() string    { return "panicking" }
func (panickingStrategy) WarmupBars() int { return 1 }
func (panickingStrategy) Evaluate(string, []market.Candle) strategy.Signal {
	var bars []float64

	return strategy.Signal{Price: bars[3]} // index out of range
}

// CM3-4: the strategy runs in evaluateSignals' worker goroutines, and recover
// only works in the goroutine that panicked — so a detector's index panic was
// out of runSupervised's reach and killed the live process. The panic must
// reach the caller's goroutine, where runSupervised can log it and disarm
// trading. (Without the fix this test does not fail politely: the test binary
// dies, which is the incident.)
func TestAnEvaluationWorkerPanicReachesTheCaller(t *testing.T) {
	e, _ := forgetEngine()
	e.barsStore = newBarStore(maxBars, time.Minute)
	e.start = panickingStrategy{}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("evaluateSignals returned normally after its strategy panicked")
		}

		if msg := r.(string); !strings.Contains(msg, "X") || !strings.Contains(msg, "index out of range") {
			t.Errorf("re-raised panic %q does not carry the symbol and the strategy's own panic", msg)
		}
	}()

	e.evaluateSignals([]string{"X"})
}

// The other goroutines the engine starts itself — a limit-order watcher, the
// name resolver — ran outside runSupervised too. A panic in one is contained,
// recorded, and disarms trading as runSupervised does for the Run loop.
func TestAPanicInAnEngineGoroutineIsContainedAndDisarms(t *testing.T) {
	e, _ := forgetEngine()
	e.tradingEnabled = true

	done := make(chan struct{})

	go func() {
		defer close(done)
		defer e.contain("test worker")

		var m map[string]int

		m["boom"]++ // nil map write
	}()

	<-done

	deadline := time.Now().Add(2 * time.Second)
	for armed(e) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if armed(e) {
		t.Error("trading is still armed after an engine goroutine panicked")
	}

	found := false

	for _, en := range errlog.Recent() {
		if strings.Contains(en.Message, "test worker panicked") {
			found = true
		}
	}

	if !found {
		t.Error("the contained panic was not recorded")
	}
}

// armed reads the trading switch under the engine lock.
func armed(e *Engine) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.tradingEnabled
}
