package autotune

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// eligibilityCfg enables autotune and the reactive trigger, with the symbol
// filters under test.
func eligibilityCfg(runOnly, exclude []string) config.AutotuneConfig {
	return config.AutotuneConfig{
		Enabled:           true,
		RunOnlyForSymbols: runOnly,
		ExcludeSymbols:    exclude,
		OnLoss:            config.AutotuneOnLossConfig{Enabled: true, AfterLosses: 2},
	}
}

// A symbol the scheduler is not allowed to touch must not become reachable
// through a losing streak. Keeping a hand-tuned override out of autotune's
// reach has to mean out of ALL of autotune's reach, or the exclusion is a
// false guarantee — the reactive path would quietly rewrite exactly the
// configs an operator fenced off.
func TestRunLossPassRespectsTheSymbolFilters(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	// A config path that does not exist: if an ineligible symbol were tuned,
	// applyResult's write would fail loudly rather than passing silently.
	missing := filepath.Join(t.TempDir(), "nope", "config.yaml")

	cases := map[string]config.AutotuneConfig{
		"not in the allow-list": eligibilityCfg([]string{"AAPL"}, nil),
		"in the deny-list":      eligibilityCfg(nil, []string{"GOOG"}),
		"in both lists":         eligibilityCfg([]string{"GOOG"}, []string{"GOOG"}),
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			var buf strings.Builder

			runLossPass(context.Background(), eng, rm, pf, cfg, missing, "GOOG", captureLog(&buf))

			if !strings.Contains(buf.String(), "not eligible") {
				t.Errorf("GOOG was not reported ineligible: %s", buf.String())
			}

			if _, err := os.Stat(missing); err == nil {
				t.Error("an ineligible symbol still reached the config write")
			}
		})
	}
}

// An eligible symbol gets past the filter and into the search. It will almost
// certainly find nothing worth applying on mock data — which is the point:
// a losing streak makes the bot LOOK, it does not make it change anything.
func TestRunLossPassRunsAnEligibleSymbol(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	var buf strings.Builder

	runLossPass(context.Background(), eng, rm, pf,
		eligibilityCfg(nil, nil), filepath.Join(t.TempDir(), "config.yaml"),
		"AAPL", captureLog(&buf))

	if strings.Contains(buf.String(), "not eligible") {
		t.Fatalf("an unfiltered symbol was reported ineligible: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "re-tuning after a losing streak") {
		t.Errorf("the reactive run was not logged: %s", buf.String())
	}
}

// Only one search runs at a time. A reactive request arriving while another
// holder has the slot does not run — and reports it, so Schedule can retry it
// (AT3-6) instead of losing it until N more losses in a row.
func TestRunLossPassSkipsWhileASearchIsRunning(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	if !TryAcquire() {
		t.Fatal("TryAcquire failed on an otherwise-idle semaphore")
	}
	defer Release()

	var buf strings.Builder

	ran := runLossPass(context.Background(), eng, rm, pf,
		eligibilityCfg(nil, nil), filepath.Join(t.TempDir(), "config.yaml"),
		"AAPL", captureLog(&buf))

	if ran {
		t.Error("runLossPass reported the request served while the slot was held; it would not be retried")
	}

	if !strings.Contains(buf.String(), "already running") {
		t.Errorf("a reactive run proceeded while the slot was held: %s", buf.String())
	}
}

// A request that lands after either switch was turned off is dropped. The
// check is against the LIVE config rather than the one in force when the
// streak was counted, so "autotune: off" takes effect on requests already in
// flight — otherwise that switch would not be the guarantee it claims to be.
func TestRunLossPassDropsWhenDisabledLive(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	cases := map[string]config.AutotuneConfig{
		"autotune off": {
			Enabled: false,
			OnLoss:  config.AutotuneOnLossConfig{Enabled: true, AfterLosses: 2},
		},
		"on_loss off": {
			Enabled: true,
			OnLoss:  config.AutotuneOnLossConfig{Enabled: false, AfterLosses: 2},
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			var buf strings.Builder

			runLossPass(context.Background(), eng, rm, pf, cfg,
				filepath.Join(t.TempDir(), "config.yaml"), "AAPL", captureLog(&buf))

			if !strings.Contains(buf.String(), "disabled live") {
				t.Errorf("a disabled trigger still ran: %s", buf.String())
			}
		})
	}
}

// captureLog writes structured log output into buf so a test can assert on
// which branch a pass actually took.
func captureLog(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// AT3-6: a loss re-tune that found the run slot busy was dropped, and the
// streak publishes again only after another after_losses losses in a row — a
// symbol whose 3rd loss landed during a screener probe was not re-tuned until
// its 6th. A busy request is retried after busyRetry, once per symbol.
func TestABusyLossRequestIsRetried(t *testing.T) {
	t.Parallel()

	fire := make(chan time.Time, 1)
	armed := 0
	l := newLossRetries(func(time.Duration) <-chan time.Time {
		armed++

		return fire
	})

	busy := true
	runs := map[string]int{}
	run := func(symbol string) bool {
		runs[symbol]++

		return !busy
	}

	l.serve("AAPL", run)
	l.serve("AAPL", run) // the same streak again: one entry, one timer

	if armed != 1 || len(l.pending) != 1 {
		t.Fatalf("armed %d timers for %d pending symbols, want 1 and 1", armed, len(l.pending))
	}

	busy = false

	l.retry(run)

	if runs["AAPL"] != 3 {
		t.Errorf("AAPL ran %d times, want the retry to serve it (3)", runs["AAPL"])
	}

	if len(l.pending) != 0 || l.due != nil {
		t.Errorf("served request still pending (%v) or the timer still armed", l.pending)
	}
}
