package screener

import (
	"context"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// SC3-7: the bars a verdict carries for the probe stage were cleared only on
// verdicts that were probed, so with probing off every passing verdict kept
// its full series in the Result the engine stores until the next pass.
func TestAPassReleasesItsBars(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{"A": synth(300, 100, 0.02, 50000)}}
	res := newScreener(t, config.ScreenerConfig{Universe: []string{"A"}, Filters: []string{"history"}}, h).
		Screen(context.Background(), Inputs{})

	for _, v := range res.Verdicts {
		if len(v.candles) > 0 {
			t.Errorf("%s still holds %d bars after the pass", v.Symbol, len(v.candles))
		}
	}
}

// SC3-8: a listing whose close never moves is refused by the cost filter for
// the right reason — there is no move to clear any cost — but the reason
// read "round-trip cost unmeasurable", which sends an operator looking for a
// broken feed.
func TestAFlatListingIsRefusedForNotMoving(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{"FLAT": synth(300, 100, 0.02, 50000)}}
	res := newScreener(t, config.ScreenerConfig{
		Universe: []string{"FLAT"}, Filters: []string{"cost"}, MinCostMultiple: 2,
	}, h).Screen(context.Background(), Inputs{})

	if len(res.Verdicts) != 1 || res.Verdicts[0].Passed {
		t.Fatalf("verdicts = %+v, want FLAT refused", res.Verdicts)
	}

	if r := res.Verdicts[0].Reason; !strings.Contains(r, "no price movement") {
		t.Errorf("reason = %q, want it to say the listing does not move", r)
	}
}
