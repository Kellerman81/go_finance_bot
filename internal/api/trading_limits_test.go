package api

import (
	"net/http"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestSetTradingRefusesWhenMoneyCeilingsAreZero reproduces the "fail-closed
// check only applies at config-load time" finding: config.Validate only
// requires max_order_value/max_total_invested > 0 when trading_enabled: true
// is already set in the file. A config shipped with trading_enabled: false
// (which skips that check entirely) and zeroed limits used to become
// genuinely unbounded — risk.Manager treats a zero cap as "not enabled", not
// "unlimited" — the moment POST /api/trading turned it on live.
func TestSetTradingRefusesWhenMoneyCeilingsAreZero(t *testing.T) {
	s := newTestServer(t) // built over a zero-value config.Limits

	code, body := do(t, s, http.MethodPost, "/api/trading", `{"enabled":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("enabling trading with zero money ceilings = %d, want 400: %s", code, body)
	}

	if s.eng.Status().TradingEnabled {
		t.Fatal("trading was enabled despite the refusal")
	}
}

// A config with real ceilings must still be able to turn trading on.
func TestSetTradingSucceedsWithCeilingsSet(t *testing.T) {
	s := newTestServer(t)

	s.rm.SetLimits(config.Limits{MaxOrderValue: 1000, MaxTotalInvested: 5000})

	code, body := do(t, s, http.MethodPost, "/api/trading", `{"enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("PUT trading = %d: %s", code, body)
	}

	if !s.eng.Status().TradingEnabled {
		t.Fatal("trading was not enabled")
	}
}

// Turning trading off is always safe and must never be blocked by the
// ceiling check.
func TestSetTradingDisablingIsNeverBlocked(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPost, "/api/trading", `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("disabling trading = %d, want 200: %s", code, body)
	}
}

// TestPutLimitsRefusesZeroingCeilingsWhileTradingIsLive covers the other way
// the same hole opens: PUT /api/limits never went through config.Validate at
// all, so a live edit to zero could disable the caps out from under trading
// that is already running, with no restart and no re-validation anywhere in
// the loop.
func TestPutLimitsRefusesZeroingCeilingsWhileTradingIsLive(t *testing.T) {
	s := newTestServer(t)

	s.rm.SetLimits(config.Limits{MaxOrderValue: 1000, MaxTotalInvested: 5000})
	s.eng.SetTradingEnabled(true)

	code, body := do(t, s, http.MethodPut, "/api/limits",
		`{"max_order_value":0,"max_total_invested":5000}`)
	if code != http.StatusBadRequest {
		t.Fatalf("zeroing max_order_value while trading is live = %d, want 400: %s", code, body)
	}

	if got := s.rm.Limits().MaxOrderValue; got != 1000 {
		t.Fatalf("max_order_value = %v, want the refusal to leave it untouched at 1000", got)
	}
}

// The same zeroed limits are fine to save while trading is off — that is an
// ordinary, currently-inert config edit.
func TestPutLimitsAllowsZeroCeilingsWhileTradingIsOff(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/limits",
		`{"max_order_value":0,"max_total_invested":0}`)
	if code != http.StatusOK {
		t.Fatalf("PUT limits while trading is off = %d: %s", code, body)
	}
}
