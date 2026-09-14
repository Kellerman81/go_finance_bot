package autotune

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// Only one search runs at a time. A reactive request arriving while a
// scheduled pass holds the slot is dropped rather than queued: by the time a
// long pass finishes, that pass has very likely re-evaluated the symbol
// anyway, and a stale backlog would fire searches for streaks that no longer
// exist.
func TestRunLossPassSkipsWhileASearchIsRunning(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	if !TryAcquire() {
		t.Fatal("TryAcquire failed on an otherwise-idle semaphore")
	}
	defer Release()

	var buf strings.Builder

	runLossPass(context.Background(), eng, rm, pf,
		eligibilityCfg(nil, nil), filepath.Join(t.TempDir(), "config.yaml"),
		"AAPL", captureLog(&buf))

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
