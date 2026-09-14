package api

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestLimitsFragmentShowsTheLiveConfigValues reproduces the "/v2 money-limits
// fields render empty" finding: the settings fragment's "limits" block builds
// its Values map with asMap(s.rm.Limits()) (JSON tags, snake_case), and hands
// it to the same "form" partial keyed by limitFields' declared Name. If those
// names don't match config.Limits' json tags exactly, every money-limit
// input renders empty on the Settings page.
func TestLimitsFragmentShowsTheLiveConfigValues(t *testing.T) {
	s := newTestServer(t)

	s.rm.SetLimits(config.Limits{
		MaxTotalInvested: 50000,
		MaxPerPosition:   5000,
		MaxOrderValue:    2500,
		MaxDailySpend:    10000,
		CashReserve:      1000,
		MaxOpenPositions: 12,
	})

	rec := fetch(t, s, "/v2/f/settings")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	prefix := "settings.limits"

	for name, want := range map[string]string{
		"max_total_invested": "50000",
		"max_per_position":   "5000",
		"max_order_value":    "2500",
		"max_daily_spend":    "10000",
		"cash_reserve":       "1000",
		"max_open_positions": "12",
	} {
		if got := inputValue(t, body, prefix, name); got != want {
			t.Errorf("%s input = %q, want %q", name, got, want)
		}
	}
}

// TestPutLimitsRoundTripsThroughTheV2Form reproduces the save side of the
// same finding: the v2 form posts field values under the names declared in
// limitFields, which decodeForm turns straight into the JSON body putLimits
// binds into config.Limits. If a declared name doesn't match the struct's
// own json tag, that field silently keeps its zero value instead of the
// submission — a save through the UI would quietly zero out the bot's money
// limits (max_order_value: 0 disables the cap in risk.Manager rather than
// meaning "unlimited") instead of applying what was typed.
func TestPutLimitsRoundTripsThroughTheV2Form(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/limits", map[string][]string{
		"max_total_invested": {"50000"},
		"max_per_position":   {"5000"},
		"max_order_value":    {"2500"},
		"max_daily_spend":    {"10000"},
		"cash_reserve":       {"1000"},
		"max_open_positions": {"12"},
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	got := s.rm.Limits()
	want := config.Limits{
		MaxTotalInvested: 50000,
		MaxPerPosition:   5000,
		MaxOrderValue:    2500,
		MaxDailySpend:    10000,
		CashReserve:      1000,
		MaxOpenPositions: 12,
	}

	if got != want {
		t.Errorf("Limits() = %+v, want %+v — the submission was not applied", got, want)
	}
}
