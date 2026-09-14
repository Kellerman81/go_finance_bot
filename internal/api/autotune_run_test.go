package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/autotune"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// The Settings tab's "Run autotune now" button (POST /api/autotune/run) —
// a manual, on-demand trigger for exactly the same unsupervised pass
// Schedule performs on its own clock, for trying it out (or catching up
// between scheduled ticks) without waiting.

// TestRunAutotuneNowStartsAndReturnsImmediately checks the handler doesn't
// block on the pass itself — a full pass can run for minutes, far past any
// reasonable HTTP timeout, so this must return as soon as the run has
// started.
func TestRunAutotuneNowStartsAndReturnsImmediately(t *testing.T) {
	s := newTestServer(t)

	start := time.Now()

	code, body := do(t, s, http.MethodPost, "/api/autotune/run", "")
	if code != http.StatusOK {
		t.Fatalf("run = %d: %s", code, body)
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("handler took %v — it must return immediately, not block on the pass", elapsed)
	}

	if !strings.Contains(body, `"started":true`) {
		t.Errorf("body = %s, want started:true", body)
	}

	// Let the background goroutine finish before the test server's
	// t.Cleanup tears down the engine out from under it.
	waitUntilIdle(t)
}

// TestRunAutotuneNowRejectsWhenAlreadyBusy checks the shared semaphore is
// respected up front: triggering a run while one (a scheduled pass, the
// manual per-symbol button, or another RunOnce) is already in flight must
// fail fast with a clear reason, not silently queue or double-run.
func TestRunAutotuneNowRejectsWhenAlreadyBusy(t *testing.T) {
	s := newTestServer(t)

	acquireSlot(t)
	defer autotune.Release()

	code, body := do(t, s, http.MethodPost, "/api/autotune/run", "")
	if code != http.StatusConflict {
		t.Fatalf("run while busy = %d, want 409: %s", code, body)
	}
}

// TestRunAutotuneNowWorksRegardlessOfEnabled checks the manual trigger is
// NOT gated on autotune.enabled — unlike Schedule's own ticker (which
// never starts at all unless it was enabled at process startup), this is a
// deliberate one-off action an operator can use to see what a pass would
// do before ever turning the scheduled job on.
func TestRunAutotuneNowWorksRegardlessOfEnabled(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetAutotuneConfig(config.AutotuneConfig{Enabled: false})

	code, body := do(t, s, http.MethodPost, "/api/autotune/run", "")
	if code != http.StatusOK {
		t.Fatalf("run with autotune disabled = %d: %s", code, body)
	}

	waitUntilIdle(t)
}

// waitUntilIdle blocks until the shared autotune run slot is free, so a
// test doesn't race its own background RunOnce goroutine past t.Cleanup.
// acquireSlot claims the shared run slot, retrying briefly rather than failing
// on the first attempt.
//
// A single TryAcquire is not safe here. autotune.Busy() probes by acquiring and
// releasing, so waitUntilIdle reports "idle" during the window where a previous
// test's background RunOnce has been launched but has not yet reached its own
// TryAcquire — the goroutine then claims the slot while the NEXT test is
// starting. Retrying closes that window without coupling the tests to each
// other's goroutine scheduling, which is what made this fail only under the
// load of a full ./... run.
func acquireSlot(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if autotune.TryAcquire() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("could not claim the autotune run slot within 5s — it is held by something else")
}

func waitUntilIdle(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !autotune.Busy() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("autotune run slot still held after 5s — a background RunOnce may have leaked past the test")
}

// TestSettingsFragmentHasAutotuneRunNowButton checks the button is rendered
// specifically in the autotune section (not, say, duplicated onto every
// settings block by an over-broad template condition).
func TestSettingsFragmentHasAutotuneRunNowButton(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/v2/f/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	if !strings.Contains(body, `data-post="/api/autotune/run"`) {
		t.Errorf("no Run autotune now button in the settings fragment: %s", body)
	}

	if strings.Count(body, `data-post="/api/autotune/run"`) != 1 {
		t.Errorf("expected exactly one Run autotune now button, found %d",
			strings.Count(body, `data-post="/api/autotune/run"`))
	}
}
