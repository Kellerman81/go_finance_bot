package config

import "testing"

// A fixed money limit goes stale the moment the account grows: 950 is the
// whole book at 1000 equity and 9.5% of it at 10000, with nothing to say so.
// The percentage halves track the account instead. These pin the resolution
// rules, because getting the DIRECTION wrong on any one of them would quietly
// loosen a money limit rather than fail.
func TestLimitsResolvePicksTheBindingHalf(t *testing.T) {
	t.Parallel()

	l := Limits{
		MaxTotalInvested: 950, MaxTotalInvestedPct: 0.95,
		MaxPerPosition: 200, MaxPerPositionPct: 0.20,
		MaxOrderValue: 200, MaxOrderValuePct: 0.20,
		MaxDailySpend: 500, MaxDailySpendPct: 0.50,
		CashReserve: 50, CashReservePct: 0.05,
	}

	// At the equity the fixed amounts were written for, the two halves agree
	// and nothing changes — the migration case.
	at1k := l.Resolve(1000)
	if at1k.MaxTotalInvested != 950 || at1k.MaxOrderValue != 200 || at1k.CashReserve != 50 {
		t.Errorf("at equity 1000 = %+v, want the written amounts unchanged", at1k)
	}

	// Ten times the account: every CEILING stays pinned to its fixed amount,
	// because the tighter of the two binds. This is the backstop property —
	// an absolute is a hard ceiling a percentage cannot grow past.
	at10k := l.Resolve(10000)
	if at10k.MaxTotalInvested != 950 || at10k.MaxOrderValue != 200 {
		t.Errorf("at equity 10000 = %+v, want the ceilings held at their fixed amounts", at10k)
	}

	// The reserve is a FLOOR, so the larger wins and it DOES grow: 5% of
	// 10000 is 500, well above the fixed 50. Taking the smaller here would
	// quietly shrink the reserve to half a percent of the book.
	if at10k.CashReserve != 500 {
		t.Errorf("cash_reserve at equity 10000 = %v, want 500 (the larger floor)",
			at10k.CashReserve)
	}
}

// 0 means "not set" for either half, in both directions.
func TestLimitsResolveTreatsZeroAsUnset(t *testing.T) {
	t.Parallel()

	// Percentage only: the ceiling scales freely.
	pctOnly := Limits{MaxOrderValuePct: 0.20, CashReservePct: 0.05}
	if got := pctOnly.Resolve(10000); got.MaxOrderValue != 2000 || got.CashReserve != 500 {
		t.Errorf("percentage-only resolve = %+v, want 2000 / 500", got)
	}

	// Absolute only: unchanged at any equity.
	absOnly := Limits{MaxOrderValue: 200, CashReserve: 50}
	if got := absOnly.Resolve(10000); got.MaxOrderValue != 200 || got.CashReserve != 50 {
		t.Errorf("absolute-only resolve = %+v, want 200 / 50", got)
	}

	// Neither: still no ceiling, which every caller reads as unlimited.
	if got := (Limits{}).Resolve(10000); got.MaxOrderValue != 0 {
		t.Errorf("unset resolve = %v, want 0 (no ceiling)", got.MaxOrderValue)
	}
}

// A missing or non-positive equity must not turn a percentage ceiling into 0,
// because 0 reads as "no limit" — that is the one direction that could let an
// order through unchecked.
func TestLimitsResolveWithNoEquityDoesNotInventAnUnlimitedCeiling(t *testing.T) {
	t.Parallel()

	l := Limits{MaxOrderValue: 200, MaxOrderValuePct: 0.20}

	for _, eq := range []float64{0, -100} {
		if got := l.Resolve(eq); got.MaxOrderValue != 200 {
			t.Errorf("equity %v resolved max_order_value to %v, want the fixed 200 to stand",
				eq, got.MaxOrderValue)
		}
	}
}

// The percentages are fractions, so >1 is almost always a "20" that meant 20%.
func TestLimitsRejectAPercentageAboveOne(t *testing.T) {
	t.Parallel()

	if err := (Limits{MaxOrderValuePct: 20}).Validate(); err == nil {
		t.Error("max_order_value_pct of 20 (meaning 2000% of equity) was accepted")
	}

	if err := (Limits{CashReservePct: -0.1}).Validate(); err == nil {
		t.Error("a negative cash_reserve_pct was accepted")
	}

	if err := (Limits{MaxOrderValuePct: 0.2, CashReservePct: 1}).Validate(); err != nil {
		t.Errorf("valid fractions were rejected: %v", err)
	}
}

// Trading with no ceiling at all stays refused, but a percentage-only config
// is a real ceiling and must be allowed through — otherwise the scaling form
// of every limit would be unusable live.
func TestValidateForTradingAcceptsEitherHalf(t *testing.T) {
	t.Parallel()

	pctOnly := Limits{MaxOrderValuePct: 0.2, MaxTotalInvestedPct: 0.95}
	if err := pctOnly.ValidateForTrading(); err != nil {
		t.Errorf("a percentage-only config was refused for trading: %v", err)
	}

	absOnly := Limits{MaxOrderValue: 200, MaxTotalInvested: 950}
	if err := absOnly.ValidateForTrading(); err != nil {
		t.Errorf("an absolute-only config was refused for trading: %v", err)
	}

	if err := (Limits{MaxTotalInvested: 950}).ValidateForTrading(); err == nil {
		t.Error("no order-value ceiling of either kind was accepted for trading")
	}

	if err := (Limits{MaxOrderValue: 200}).ValidateForTrading(); err == nil {
		t.Error("no total-invested ceiling of either kind was accepted for trading")
	}
}

// "Use 95% of equity, up to 5000" is the mode most operators actually want,
// and it is nothing more than the tighter half winning — but it is the one
// combination that is not obvious from reading either field on its own, so it
// is pinned explicitly rather than left implied by TestLimitsResolve above.
func TestLimitsCombineScalingWithAHardCeiling(t *testing.T) {
	t.Parallel()

	l := Limits{MaxTotalInvested: 5000, MaxTotalInvestedPct: 0.95}

	for _, tc := range []struct {
		equity, want float64
		binding      string
	}{
		{1000, 950, "95% of equity"},
		{5000, 4750, "95% of equity"},
		{6000, 5000, "the 5000 ceiling"},
		{50000, 5000, "the 5000 ceiling"},
	} {
		if got := l.Resolve(tc.equity).MaxTotalInvested; got != tc.want {
			t.Errorf("equity %v resolved to %v, want %v (%s)",
				tc.equity, got, tc.want, tc.binding)
		}
	}
}
