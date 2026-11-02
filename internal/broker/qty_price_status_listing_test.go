package broker

import "testing"

// BR2-5: a bare Floor(q*p)/p turned a broker-reported 0.29 into 0.28 at 2 dp
// (0.29*100 is 28.999999999999996), so a full exit left 0.01 behind — above
// the dust threshold, for a second order or an adoption.
func TestRoundDownQtyKeepsExactDecimals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		q    float64
		dp   int
		want float64
	}{
		{0.29, 2, 0.29}, {1.15, 2, 1.15}, {1.15, 8, 1.15}, {2.01, 9, 2.01},
		{0.299, 2, 0.29}, // genuinely below the next unit still rounds DOWN
	}

	for _, c := range cases {
		if got := roundDownQty(c.q, c.dp); got != c.want {
			t.Errorf("roundDownQty(%v, %d) = %v, want %v", c.q, c.dp, got, c.want)
		}
	}
}

// BR2-6: pending_cancel, pending_replace, held, stopped and suspended are not
// final. Read as terminal they confirmed a zero fill — a "stopped" market
// order's shares later arrived as an untracked holding.
func TestAlpacaNonFinalStatusesKeepPolling(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"stopped", "held", "pending_cancel", "pending_replace", "suspended", "new", ""} {
		if !alpacaWorkingStatus(s) {
			t.Errorf("%q read as terminal; it is not a final Alpaca status", s)
		}
	}

	for _, s := range []string{"filled", "canceled", "expired", "rejected", "replaced", "done_for_day"} {
		if alpacaWorkingStatus(s) {
			t.Errorf("%q read as working; it is final", s)
		}
	}
}

// BR2-7: a flat 2 dp sent a sub-$1 buy limit of 0.4567 as 0.46 — paying up to
// ~0.7% more than the limit meant. Below $1 Alpaca takes 4 dp; rounding goes
// toward the side's conservative edge.
func TestAlpacaLimitPricePrecisionAndDirection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		px   float64
		side Side
		want string
	}{
		{0.4567, Buy, "0.4567"},
		{0.45678, Buy, "0.4567"},
		{0.45671, Sell, "0.4568"},
		{12.345, Buy, "12.34"},
		{12.341, Sell, "12.35"},
		{12.34, Sell, "12.34"}, // exact stays exact, not nudged up a cent
	}

	for _, c := range cases {
		if got := alpacaLimitPrice(c.px, c.side); got != c.want {
			t.Errorf("alpacaLimitPrice(%v, %s) = %s, want %s", c.px, c.side, got, c.want)
		}
	}
}

// BR2-8: a US ISIN does not make a line American — AAPL_GB_EQ trades in
// London with one. Taking the first ISIN match let the payload's order pick
// the LSE line for "AAPL".
func TestPickListingPrefersTheRealUSTicker(t *testing.T) {
	t.Parallel()

	got := pickListing([]t212Inst{
		{Ticker: "AAPL_GB_EQ", ISIN: "US0378331005", Currency: "GBX"},
		{Ticker: "AAPL_US_EQ", ISIN: "US0378331005", Currency: "USD"},
	}, "US", "")

	if got.Ticker != "AAPL_US_EQ" {
		t.Errorf("picked %s, want AAPL_US_EQ", got.Ticker)
	}
}
