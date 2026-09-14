package screener

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// fakeHistory serves canned candles per symbol.
type fakeHistory struct {
	series map[string][]market.Candle
	fail   map[string]bool
}

// Candles returns the canned series, or an error for symbols marked to fail.
func (f fakeHistory) Candles(
	_ context.Context, symbol string, _ market.Resolution, _, _ time.Time,
) ([]market.Candle, error) {
	if f.fail[symbol] {
		return nil, errors.New("no data")
	}

	return f.series[symbol], nil
}

// synth builds n bars around price with the given per-bar range (as a fraction
// of price, which sets ATR%) and volume.
func synth(n int, price, rangePct, volume float64) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range out {
		half := price * rangePct / 2
		out[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: price, High: price + half, Low: price - half,
			Close: price, Volume: volume,
		}
	}

	return out
}

// rising builds n bars trending up by pct per bar, for momentum ranking.
func rising(n int, start, pctPerBar, volume float64) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px := start

	for i := range out {
		px *= 1 + pctPerBar
		out[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: px, High: px * 1.005, Low: px * 0.995, Close: px, Volume: volume,
		}
	}

	return out
}

// newScreener builds an enabled screener over the given history.
func newScreener(t *testing.T, cfg config.ScreenerConfig, h History) *Screener {
	t.Helper()

	cfg.Enabled = true

	s := New(cfg, h, market.Res1Min, Options{})
	if s == nil {
		t.Fatal("New returned nil for an enabled config")
	}

	return s
}

// TestFiltersRejectAndExplain checks each filter rejects and says why — a
// silently-dropped symbol is impossible to debug.
func TestFiltersRejectAndExplain(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{
		"GOOD":   synth(200, 100, 0.02, 50000),
		"PENNY":  synth(200, 0.40, 0.02, 50000),
		"THIN":   synth(200, 100, 0.02, 10),
		"QUIET":  synth(200, 100, 0.0001, 50000),
		"WILD":   synth(200, 100, 0.5, 50000),
		"SHORT":  synth(20, 100, 0.02, 50000),
		"BROKEN": nil,
	}, fail: map[string]bool{"BROKEN": true}}

	s := newScreener(t, config.ScreenerConfig{
		Universe: []string{"GOOD", "PENNY", "THIN", "QUIET", "WILD", "SHORT", "BROKEN"},
		MinPrice: 1, MinAvgVolume: 1000,
		MinVolatilityPct: 0.2, MaxVolatilityPct: 10,
		MinHistoryBars: 100,
	}, h)

	res := s.Screen(t.Context(), Inputs{})

	if !slices.Contains(res.Selected, "GOOD") {
		t.Errorf("selected = %v, want GOOD to survive", res.Selected)
	}

	reasons := make(map[string]string, len(res.Verdicts))
	for _, v := range res.Verdicts {
		if !v.Passed {
			reasons[v.Symbol] = v.Reason
		}
	}

	for sym, want := range map[string]string{
		"PENNY":  "price",
		"THIN":   "volume",
		"QUIET":  "volatility",
		"WILD":   "volatility",
		"SHORT":  "bars",
		"BROKEN": "history",
	} {
		got, rejected := reasons[sym]
		if !rejected {
			t.Errorf("%s was not rejected", sym)
			continue
		}

		if !contains(got, want) {
			t.Errorf("%s rejected with %q, want a reason mentioning %q", sym, got, want)
		}
	}
}

// TestHeldSymbolsSurvive checks a symbol with an open position is never dropped:
// removing it would strand the position with no exits.
// TestAppendMissingIsSortedNotMapOrder reproduces the "Result.Selected
// ordering for held/pinned symbols is nondeterministic (Go map iteration)"
// finding: appendMissing used to range directly over its map argument, and
// Go deliberately randomises map iteration order — so repeated calls with
// the identical input could append the same set of symbols in a different
// order each time, contradicting rankAndCap's own "ties break on symbol so
// a pass is reproducible" guarantee. The fix must produce the same
// (sorted) order on every call.
func TestAppendMissingIsSortedNotMapOrder(t *testing.T) {
	t.Parallel()

	add := map[string]bool{
		"ZETA": true, "ALPHA": true, "MIKE": true, "BRAVO": true, "KILO": true,
		"ECHO": true, "NOVEMBER": true, "CHARLIE": true, "GOLF": true, "DELTA": true,
	}

	want := []string{"ALPHA", "BRAVO", "CHARLIE", "DELTA", "ECHO", "GOLF", "KILO", "MIKE", "NOVEMBER", "ZETA"}

	// Go deliberately randomises map iteration order on every range, not
	// just once per process — so calling this enough times on the same map
	// would have caught the pre-fix code ranging straight over it almost
	// certainly, without needing to rebuild the map between calls.
	for i := range 20 {
		got := appendMissing(nil, add)

		if !slices.Equal(got, want) {
			t.Fatalf("iteration %d: appendMissing = %v, want %v (sorted)", i, got, want)
		}
	}
}

// TestAppendMissingSkipsSymbolsAlreadyPresent confirms the fix doesn't
// change the "no duplicates" contract — held and pinned overlap constantly.
func TestAppendMissingSkipsSymbolsAlreadyPresent(t *testing.T) {
	t.Parallel()

	got := appendMissing([]string{"AAPL"}, map[string]bool{"AAPL": true, "MSFT": true})

	if want := []string{"AAPL", "MSFT"}; !slices.Equal(got, want) {
		t.Errorf("appendMissing = %v, want %v", got, want)
	}
}

func TestHeldSymbolsSurvive(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{
		"GOOD": synth(200, 100, 0.02, 50000),
		"JUNK": synth(200, 0.10, 0.02, 5),
	}}

	s := newScreener(t, config.ScreenerConfig{
		Universe: []string{"GOOD", "JUNK"},
		MinPrice: 1, MinAvgVolume: 1000, KeepHeld: true,
	}, h)

	res := s.Screen(t.Context(), Inputs{Held: []string{"JUNK"}})

	if !slices.Contains(res.Selected, "JUNK") {
		t.Errorf("selected = %v, want the held JUNK kept despite failing filters", res.Selected)
	}

	for _, v := range res.Verdicts {
		if v.Symbol == "JUNK" && !contains(v.Reason, "position open") {
			t.Errorf("JUNK verdict = %q, want it to say the position kept it", v.Reason)
		}
	}
}

// TestHeldSymbolSurvivesMaxSymbols checks the cap cannot evict a held position
// either.
func TestHeldSymbolSurvivesMaxSymbols(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{
		"BIG":  synth(200, 100, 0.02, 900000),
		"MID":  synth(200, 100, 0.02, 500000),
		"HELD": synth(200, 100, 0.02, 1000),
	}}

	s := newScreener(t, config.ScreenerConfig{
		Universe: []string{"BIG", "MID", "HELD"}, MaxSymbols: 2,
		Rank: "volume", KeepHeld: true,
	}, h)

	res := s.Screen(t.Context(), Inputs{Held: []string{"HELD"}})

	if !slices.Contains(res.Selected, "HELD") {
		t.Errorf("selected = %v, want HELD kept past the max_symbols cap", res.Selected)
	}
}

// TestRankingOrders checks each ranking picks the intended winner.
func TestRankingOrders(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{
		"VOL":   synth(200, 100, 0.01, 900000), // most volume
		"SWING": synth(200, 100, 0.08, 1000),   // widest range
		"MOVER": rising(200, 100, 0.002, 1000), // strongest momentum
	}}

	for rank, want := range map[string]string{
		"volume":     "VOL",
		"volatility": "SWING",
		"momentum":   "MOVER",
	} {
		s := newScreener(t, config.ScreenerConfig{
			Universe: []string{"VOL", "SWING", "MOVER"}, MaxSymbols: 1, Rank: rank,
		}, h)

		res := s.Screen(t.Context(), Inputs{})
		if len(res.Selected) != 1 || res.Selected[0] != want {
			t.Errorf("rank=%s selected %v, want [%s]", rank, res.Selected, want)
		}
	}
}

// TestUndefinedMomentumRanksBelowNegativeMomentum reproduces the "undefined
// momentum ranks as exactly 0" finding: a candidate with too little history
// to measure momentum (fewer than momentumBars+1 bars — a freshly-listed
// symbol) used to default to Momentum == 0, sorting ABOVE any symbol with
// genuinely negative momentum instead of behind every real reading, since
// ranking sorts descending. With rank: momentum and a cap of 1, a
// well-measured falling symbol must still be preferred over one whose
// momentum could not be measured at all.
func TestUndefinedMomentumRanksBelowNegativeMomentum(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{
		"FALLING": rising(25, 100, -0.002, 1000), // measurable, genuinely negative
		"SHORT":   rising(15, 100, 0.002, 1000),  // too few bars: momentum undefined
	}}

	s := newScreener(t, config.ScreenerConfig{
		Universe: []string{"FALLING", "SHORT"}, MaxSymbols: 1, Rank: "momentum",
		Filters: []string{"price"}, // isolate ranking from the history filter
	}, h)

	res := s.Screen(t.Context(), Inputs{})

	if len(res.Selected) != 1 || res.Selected[0] != "FALLING" {
		t.Errorf("selected %v, want [FALLING] — undefined momentum must rank below "+
			"even a genuinely negative reading, not tie with (or beat) it at 0", res.Selected)
	}
}

// TestEmptyUniverseScreensWatchlist checks that with no configured universe the
// screener filters the current watchlist and never invents symbols.
func TestEmptyUniverseScreensWatchlist(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{
		"AAA": synth(200, 100, 0.02, 50000),
		"BBB": synth(200, 0.10, 0.02, 5),
		"CCC": synth(200, 100, 0.02, 50000),
	}}

	s := newScreener(t, config.ScreenerConfig{MinPrice: 1, MinAvgVolume: 1000}, h)

	res := s.Screen(t.Context(), Inputs{Current: []string{"AAA", "BBB"}})

	if slices.Contains(res.Selected, "CCC") {
		t.Errorf("selected = %v, want no symbol outside the watchlist", res.Selected)
	}

	if !slices.Contains(res.Selected, "AAA") || slices.Contains(res.Selected, "BBB") {
		t.Errorf("selected = %v, want [AAA] only", res.Selected)
	}
}

// TestDisabledReturnsNil checks the off switch.
func TestDisabledReturnsNil(t *testing.T) {
	t.Parallel()

	if New(config.ScreenerConfig{Enabled: false}, fakeHistory{}, market.Res1Min, Options{}) != nil {
		t.Error("disabled config built a screener")
	}

	if New(config.ScreenerConfig{Enabled: true}, nil, market.Res1Min, Options{}) != nil {
		t.Error("screener built without a history source")
	}

	var s *Screener

	if s.Interval() != 0 {
		t.Error("nil screener reports an interval")
	}

	if res := s.Screen(t.Context(), Inputs{}); len(res.Selected) != 0 {
		t.Error("nil screener selected symbols")
	}
}

// contains is a small substring helper that keeps the assertions readable.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

// indexOf returns the index of needle in haystack, or -1.
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}
