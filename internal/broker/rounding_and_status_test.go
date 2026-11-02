package broker

import "testing"

// BR3-3: the "within representation error" tolerance was 1e-12 relative to
// the scaled value — half a unit at 8 dp for q ≈ 5000, at 9 dp for q ≈ 500 —
// so roundDownQty rounded UP there: 600.0000000006 at 9 dp became
// 600.000000001, an Alpaca sell above the holding, refused, exit failed.
// The small-number case it exists for (1.15 at 8 dp) must still hold.
func TestRoundDownQtyNeverRoundsUp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		q    float64
		dp   int
		want float64
	}{
		{5000.000000006, 8, 5000},
		{600.0000000006, 9, 600},
		{1.15, 8, 1.15},
		{1.50000001, 8, 1.50000001},
	}

	for _, c := range cases {
		if got := roundDownQty(c.q, c.dp); got > c.q || got != c.want {
			t.Errorf("roundDownQty(%v, %d) = %v, want %v", c.q, c.dp, got, c.want)
		}
	}
}

// BR3-4: the status list named the WORKING states and called everything else
// terminal, so an order polled during CANCELLING (or any status this code
// does not know) was booked as a confirmed fill of what had filled so far —
// usually 0. Only filled, cancelled and rejected are final.
func TestCancellingIsNotAFinalTradingStatus(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"CANCELLING", "SOME_NEW_STATUS", "NEW", "PARTIALLY_FILLED"} {
		if t212FinalStatus(s) {
			t.Errorf("%s read as final; an order in it would be booked as a confirmed fill", s)
		}
	}

	for _, s := range []string{"FILLED", "CANCELLED", "REJECTED"} {
		if !t212FinalStatus(s) {
			t.Errorf("%s did not read as final", s)
		}
	}
}
