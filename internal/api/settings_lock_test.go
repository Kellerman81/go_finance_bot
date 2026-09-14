package api

import (
	"net/http"
	"testing"
	"time"
)

// TestPutSettingsSerializesOnSettingsMu reproduces the "concurrent PUT
// /api/settings/:section requests have no serialization at the handler
// level" finding: config.Patch's own patchMu (CRIT-10) only protects the
// file write itself. Two concurrent requests to the SAME section could
// still interleave in memory — each persistSection call reads back "the
// live value" from the engine AFTER applying, so request A's response and
// file write could end up reflecting request B's value instead of its own,
// silently. putSettings must hold s.settingsMu for its whole apply-then-
// persist sequence.
//
// This proves the lock is actually acquired (deterministically, by holding
// it externally and observing the handler block until released) rather
// than trying to reproduce the original interleaving race directly, which
// has no reliable trigger without internal test hooks.
func TestPutSettingsSerializesOnSettingsMu(t *testing.T) {
	s := newTestServer(t)

	s.settingsMu.Lock() // simulate a settings write already in flight

	done := make(chan struct{})

	go func() {
		defer close(done)

		do(t, s, http.MethodPut, "/api/settings/costs", `{"commission_pct":0.01}`)
	}()

	select {
	case <-done:
		t.Fatal("PUT /api/settings/:section completed while settingsMu was held — " +
			"putSettings does not acquire it")
	case <-time.After(50 * time.Millisecond):
		// Expected: blocked behind the externally-held lock.
	}

	s.settingsMu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PUT /api/settings/:section never completed after settingsMu was released")
	}
}

// The legacy endpoints that write settings outside /api/settings/:section
// (putLimits, putStrategy, putExits, putSizing, putBehavior) share the same
// apply-then-persist shape and must acquire the same lock. Checked here via
// putLimits as a representative — the fix adds the identical two lines to
// each handler.
func TestPutLimitsSerializesOnSettingsMu(t *testing.T) {
	s := newTestServer(t)

	s.settingsMu.Lock()

	done := make(chan struct{})

	go func() {
		defer close(done)

		do(t, s, http.MethodPut, "/api/limits", `{"max_order_value":100}`)
	}()

	select {
	case <-done:
		t.Fatal("PUT /api/limits completed while settingsMu was held")
	case <-time.After(50 * time.Millisecond):
	}

	s.settingsMu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PUT /api/limits never completed after settingsMu was released")
	}
}
