package screener

import (
	"context"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// classMap answers from a fixed table — the engine's real classifier reaches a
// persistent cache and a provider lookup, neither of which a unit test wants.
type classMap map[string]string

func (c classMap) AssetClass(_ context.Context, symbol string) string { return c[symbol] }

// resHistory records which resolution each symbol was requested at, so a test
// can prove a class profile actually changed what was fetched rather than only
// what was compared.
type resHistory struct {
	series map[string][]market.Candle
	asked  map[string]market.Resolution
}

func (r *resHistory) Candles(
	_ context.Context, symbol string, res market.Resolution, _, _ time.Time,
) ([]market.Candle, error) {
	if r.asked == nil {
		r.asked = map[string]market.Resolution{}
	}

	r.asked[symbol] = res

	return r.series[symbol], nil
}

// trending builds a clean upward series: a straight line plus a fixed bar
// range, so R² is high and the slope is known.
func trending(n int, start, perBar, rangePct, volume float64) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range out {
		px := start + perBar*float64(i)
		half := px * rangePct / 2

		out[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: px, High: px + half, Low: px - half, Close: px, Volume: volume,
		}
	}

	return out
}

// The core of the rework: a symbol is judged on the bar size written for its
// asset class. An ETF fetched at one-minute resolution is being measured on
// noise; the profile has to change what is actually requested.
func TestPerClassResolutionAndLookbackAreUsed(t *testing.T) {
	t.Parallel()

	hist := &resHistory{series: map[string][]market.Candle{
		"IWDA.AS": trending(300, 100, 0.05, 0.01, 50000),
		"GOOGL":   trending(300, 150, 0.05, 0.01, 50000),
	}}

	s := New(config.ScreenerConfig{
		Enabled:    true,
		Universe:   []string{"IWDA.AS", "GOOGL"},
		Resolution: "15",
		Filters:    []string{"history"},
		ByAssetClass: map[string]config.ScreenerProfile{
			"etf": {Resolution: ptrStr("D"), LookbackDays: ptrInt(365)},
		},
	}, hist, market.Res1Min, Options{
		Classify: classMap{"IWDA.AS": "etf", "GOOGL": "stock"},
	})

	s.Screen(context.Background(), Inputs{})

	if got := hist.asked["IWDA.AS"]; got != market.Res1Day {
		t.Errorf("ETF fetched at %q, want daily bars from its profile", got)
	}

	if got := hist.asked["GOOGL"]; got != market.Res15Min {
		t.Errorf("stock fetched at %q, want the base 15m resolution", got)
	}
}

// max_symbols per class is the point of capping by type: one shared cap ranked
// across classes lets whichever class scores higher on the shared metric take
// every slot, so "8 ETFs and 2 stocks" silently becomes "10 stocks".
func TestMaxSymbolsIsPerAssetClass(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{}
	classes := classMap{}

	// Stocks carry far more volume than the ETFs, so a shared volume ranking
	// would hand them every slot.
	for _, sym := range []string{"S1", "S2", "S3", "S4"} {
		series[sym] = trending(300, 100, 0.05, 0.02, 1_000_000)
		classes[sym] = "stock"
	}

	for _, sym := range []string{"E1", "E2", "E3", "E4"} {
		series[sym] = trending(300, 100, 0.05, 0.02, 1000)
		classes[sym] = "etf"
	}

	s := New(config.ScreenerConfig{
		Enabled:  true,
		Universe: keysOf(series),
		Rank:     "volume",
		Filters:  []string{"history"},
		ByAssetClass: map[string]config.ScreenerProfile{
			"etf":   {MaxSymbols: ptrInt(2)},
			"stock": {MaxSymbols: ptrInt(1)},
		},
	}, fakeHistory{series: series}, market.Res1Min, Options{Classify: classes})

	got := s.Screen(context.Background(), Inputs{}).Selected

	var etfs, stocks int

	for _, sym := range got {
		if strings.HasPrefix(sym, "E") {
			etfs++
		} else {
			stocks++
		}
	}

	if etfs != 2 {
		t.Errorf("selected %d ETFs from %v, want exactly the class cap of 2", etfs, got)
	}

	if stocks != 1 {
		t.Errorf("selected %d stocks from %v, want exactly the class cap of 1", stocks, got)
	}
}

// A class with no profile, and a symbol nothing could classify, both fall back
// to the base configuration rather than being skipped — an unclassifiable
// candidate must still be screenable.
func TestUnclassifiedCandidatesUseTheBaseProfile(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{"MYSTERY": trending(300, 100, 0.05, 0.02, 50000)}

	s := New(config.ScreenerConfig{
		Enabled:  true,
		Universe: []string{"MYSTERY"},
		Filters:  []string{"history", "price"},
		MinPrice: 5,
		ByAssetClass: map[string]config.ScreenerProfile{
			"etf": {MinPrice: ptrFloat(10000)}, // would reject it, if it applied
		},
	}, fakeHistory{series: series}, market.Res1Min, Options{Classify: classMap{}})

	res := s.Screen(context.Background(), Inputs{})

	if len(res.Selected) != 1 {
		t.Fatalf("selected %v, want the unclassified candidate screened by the base rules",
			res.Selected)
	}

	if res.Verdicts[0].AssetClass != "" {
		t.Errorf("asset class = %q, want empty for an unclassifiable symbol",
			res.Verdicts[0].AssetClass)
	}
}

// Without a classifier at all, everything is screened by the base config —
// the pre-existing behaviour, unchanged.
func TestNoClassifierScreensEverythingAsBase(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{"X": trending(300, 100, 0.05, 0.02, 50000)}

	s := New(config.ScreenerConfig{
		Enabled: true, Universe: []string{"X"}, Filters: []string{"history"},
	}, fakeHistory{series: series}, market.Res1Min, Options{})

	if got := s.Screen(context.Background(), Inputs{}).Selected; len(got) != 1 {
		t.Errorf("selected %v, want the candidate screened without a classifier", got)
	}
}

// Suggestions exist for the operator who wants the screener to find things but
// has no idea what to list. An empty universe otherwise makes it filter-only.
func TestSuggestUniverseSeedsThePool(t *testing.T) {
	t.Parallel()

	s := New(config.ScreenerConfig{
		Enabled: true, SuggestUniverse: true,
		ByAssetClass: map[string]config.ScreenerProfile{"etf": {}},
	}, fakeHistory{series: map[string][]market.Candle{}}, market.Res1Min, Options{})

	pool := s.pool(nil)
	if len(pool) == 0 {
		t.Fatal("suggestions produced no candidates")
	}

	for _, want := range SuggestedUniverse("etf") {
		if !containsStr(pool, want) {
			t.Fatalf("pool %v is missing suggested ETF %q", pool, want)
		}
	}

	// Without the switch, an unconfigured screener still falls back to the
	// current watchlist — filtering only, never adding.
	off := New(config.ScreenerConfig{Enabled: true},
		fakeHistory{series: map[string][]market.Candle{}}, market.Res1Min, Options{})

	if got := off.pool([]string{"HELD"}); len(got) != 1 || got[0] != "HELD" {
		t.Errorf("pool = %v, want the watchlist fallback with suggestions off", got)
	}
}

// Every suggested symbol must be usable as written: upper-case, trimmed, and
// in the Yahoo convention the history source speaks.
func TestSuggestedUniversesAreWellFormed(t *testing.T) {
	t.Parallel()

	for _, class := range SuggestedClasses() {
		pool := SuggestedUniverse(class)
		if len(pool) == 0 {
			t.Errorf("class %q has an empty starter pool", class)
		}

		seen := map[string]bool{}

		for _, sym := range pool {
			if sym != strings.ToUpper(strings.TrimSpace(sym)) {
				t.Errorf("%s: %q is not normalised", class, sym)
			}

			if seen[sym] {
				t.Errorf("%s: %q listed twice", class, sym)
			}

			seen[sym] = true
		}
	}

	// The lookup normalises, so a differently-cased class still finds its pool.
	if len(SuggestedUniverse("ETF")) == 0 {
		t.Error("SuggestedUniverse(\"ETF\") found nothing")
	}

	if SuggestedUniverse("nonsense") != nil {
		t.Error("an unknown class returned a pool")
	}
}

// ---- the new filters ----

// A sideways chop that ends higher has real positive momentum and a near-zero
// R². Momentum alone cannot tell it from a clean advance; the trend filter is
// what refuses it.
func TestTrendFilterRejectsNoise(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	chop := make([]market.Candle, 300)

	for i := range chop {
		// A big oscillation plus a tiny drift: ends higher, but no line fits.
		px := 100 + 10*math.Sin(float64(i)/2) + 0.002*float64(i)
		chop[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: px, High: px + 0.5, Low: px - 0.5, Close: px, Volume: 50000,
		}
	}

	series := map[string][]market.Candle{
		"CHOP":  chop,
		"CLEAN": trending(300, 100, 0.05, 0.01, 50000),
	}

	s := New(config.ScreenerConfig{
		Enabled: true, Universe: []string{"CHOP", "CLEAN"},
		Filters: []string{"history", "trend"}, MinTrendR2: 0.8,
	}, fakeHistory{series: series}, market.Res1Min, Options{})

	res := s.Screen(context.Background(), Inputs{})

	if len(res.Selected) != 1 || res.Selected[0] != "CLEAN" {
		t.Fatalf("selected %v, want only the cleanly trending symbol", res.Selected)
	}

	for _, v := range res.Verdicts {
		if v.Symbol == "CHOP" && !strings.Contains(v.Reason, "R²") {
			t.Errorf("CHOP rejected for %q, want the trend reason", v.Reason)
		}
	}
}

// A falling knife is certainly moving and can pass a volatility band; its own
// drawdown is what refuses it.
func TestDrawdownFilterRejectsAFallingKnife(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{
		"CRASH":  trending(300, 200, -0.5, 0.01, 50000), // 200 -> 50
		"STEADY": trending(300, 100, 0.05, 0.01, 50000),
	}

	s := New(config.ScreenerConfig{
		Enabled: true, Universe: []string{"CRASH", "STEADY"},
		Filters: []string{"history", "drawdown"}, MaxDrawdownPct: 0.25,
	}, fakeHistory{series: series}, market.Res1Min, Options{})

	res := s.Screen(context.Background(), Inputs{})

	if len(res.Selected) != 1 || res.Selected[0] != "STEADY" {
		t.Fatalf("selected %v, want the falling knife refused", res.Selected)
	}
}

// The one screening number that decides whether an instrument can be
// profitable at all: something that barely moves against its own round trip
// loses money at every win rate.
func TestCostFilterRejectsAnInstrumentThatCannotPayForItself(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{
		// Barely moves, and wide bars imply an expensive spread.
		"DEAD": walk(400, 100, 0.00005, 0.002, 7),
		// Moves half a percent a bar through tight bars — a liquid name.
		"ALIVE": walk(400, 100, 0.01, 0.0005, 7),
	}

	s := New(config.ScreenerConfig{
		Enabled: true, Universe: []string{"DEAD", "ALIVE"},
		Filters: []string{"history", "cost"}, MinCostMultiple: 2,
	}, fakeHistory{series: series}, market.Res1Min,
		Options{Costs: config.CostsConfig{CommissionPct: 0.001}})

	res := s.Screen(context.Background(), Inputs{})

	if len(res.Selected) != 1 || res.Selected[0] != "ALIVE" {
		t.Fatalf("selected %v, want only the instrument that clears its costs", res.Selected)
	}

	for _, v := range res.Verdicts {
		if v.Symbol == "DEAD" && !strings.Contains(v.Reason, "round-trip cost") {
			t.Errorf("DEAD rejected for %q, want the cost reason", v.Reason)
		}
	}
}

// ---- measurement maths ----

func TestTrendFitAndDrawdownMaths(t *testing.T) {
	t.Parallel()

	// A perfect line: R² is 1 and the slope is exactly the per-bar step over
	// the mean price.
	line := make([]float64, 100)
	for i := range line {
		line[i] = 100 + float64(i)
	}

	slope, r2, ok := trendFit(line)
	if !ok {
		t.Fatal("a perfect line was unmeasurable")
	}

	if r2 < 0.999 {
		t.Errorf("R² = %v, want ~1 for a straight line", r2)
	}

	if want := 1.0 / mean(line); math.Abs(slope-want) > 1e-9 {
		t.Errorf("slope = %v, want %v (per-bar step over mean price)", slope, want)
	}

	// A flat series has no variance to explain. Reporting R² = 1 there would
	// let a dead instrument pass a trend bound.
	flat := make([]float64, 100)
	for i := range flat {
		flat[i] = 50
	}

	if _, _, ok := trendFit(flat); ok {
		t.Error("a flat series reported a measurable trend")
	}

	// Too few bars to fit anything.
	if _, _, ok := trendFit([]float64{1, 2, 3}); ok {
		t.Error("three points reported a measurable trend")
	}

	if got := maxDrawdown([]float64{100, 120, 60, 90}); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("maxDrawdown = %v, want 0.5 (120 down to 60)", got)
	}

	if got := maxDrawdown([]float64{10, 20, 30}); got != 0 {
		t.Errorf("maxDrawdown = %v, want 0 for a monotonic rise", got)
	}
}

// walk builds a random-walk series with a chosen typical bar-to-bar move and a
// chosen bar width — the two quantities the cost filter compares. A pure trend
// is unusable here: the Corwin-Schultz spread estimator reads a strong drift as
// unmeasurable, which is a real property of the estimator rather than a quirk
// of the test.
func walk(n int, price, movePct, rangePct float64, seed int64) []market.Candle {
	rng := rand.New(rand.NewSource(seed))
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	px := price

	for i := range out {
		px *= 1 + movePct*(rng.Float64()*2-1)
		half := px * rangePct / 2

		out[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: px, High: px + half, Low: px - half, Close: px, Volume: 50000,
		}
	}

	return out
}

// The cost measurement is only meaningful if its two halves are independent:
// an instrument that moves a lot through tight bars must score far above one
// that barely moves through wide ones.
func TestCostMultipleSeparatesMovementFromSpread(t *testing.T) {
	t.Parallel()

	measureOf := func(c []market.Candle) Measures {
		cols := columns(c)
		m := measure(cols)
		m.measureCost(cols, 0.001)

		return m
	}

	alive := measureOf(walk(400, 100, 0.01, 0.0005, 7))
	dead := measureOf(walk(400, 100, 0.00005, 0.002, 7))

	if !alive.CostOK || !dead.CostOK {
		t.Fatal("cost was unmeasurable on a random walk")
	}

	if alive.CostMultiple() < 2 {
		t.Errorf("liquid instrument scored %.3f, want >= 2", alive.CostMultiple())
	}

	if dead.CostMultiple() > 0.5 {
		t.Errorf("dead instrument scored %.3f, want well below 1", dead.CostMultiple())
	}

	// An unmeasurable spread must not read as a cheap one.
	var none Measures
	none.Price = 100
	none.measureCost(columns(walk(50, 100, 0.01, 0.0005, 1)), 0.001)

	if none.CostOK {
		t.Error("cost was reported measurable with no spread estimate")
	}
}

// ---- helpers ----

func ptrStr(s string) *string     { return &s }
func ptrInt(i int) *int           { return &i }
func ptrFloat(f float64) *float64 { return &f }

func keysOf(m map[string][]market.Candle) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func containsStr(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}

	return false
}

// Seeding is per class, which is the point: the two halves of a book are
// rarely at the same stage. You may know exactly which stocks you want to
// screen and have no idea which trackers are worth looking at.
func TestSuggestUniverseIsPerAssetClass(t *testing.T) {
	t.Parallel()

	yes := true

	s := New(config.ScreenerConfig{
		Enabled: true,
		// The global switch is OFF; only the ETF profile asks to be seeded.
		SuggestUniverse: false,
		UniverseByAssetClass: map[string][]string{
			"stock": {"MYPICK1", "MYPICK2"},
		},
		ByAssetClass: map[string]config.ScreenerProfile{
			"etf":   {SuggestUniverse: &yes},
			"stock": {},
		},
	}, fakeHistory{series: map[string][]market.Candle{}}, market.Res1Min, Options{})

	pool := s.pool(nil)

	for _, want := range SuggestedUniverse("etf") {
		if !containsStr(pool, want) {
			t.Fatalf("pool is missing suggested ETF %q: %v", want, pool)
		}
	}

	// The curated stock pool is used as written, and NOT diluted with
	// suggestions — a list someone chose is a decision.
	if !containsStr(pool, "MYPICK1") || !containsStr(pool, "MYPICK2") {
		t.Errorf("pool %v lost the curated stock picks", pool)
	}

	for _, unwanted := range SuggestedUniverse("stock") {
		if containsStr(pool, unwanted) {
			t.Errorf("pool %v was seeded with stock suggestions that were not asked for", pool)
		}
	}
}

// A class profile can also turn seeding OFF while the shared switch is on —
// the inheritance runs both ways.
func TestPerClassSuggestCanOptOutOfTheGlobalSwitch(t *testing.T) {
	t.Parallel()

	no := false

	s := New(config.ScreenerConfig{
		Enabled:         true,
		SuggestUniverse: true,
		ByAssetClass: map[string]config.ScreenerProfile{
			"stock": {SuggestUniverse: &no},
		},
	}, fakeHistory{series: map[string][]market.Candle{}}, market.Res1Min, Options{})

	pool := s.pool(nil)

	for _, unwanted := range SuggestedUniverse("stock") {
		if containsStr(pool, unwanted) {
			t.Fatalf("pool %v seeded a class that opted out", pool)
		}
	}

	// Every other class still is.
	for _, want := range SuggestedUniverse("etf") {
		if !containsStr(pool, want) {
			t.Fatalf("pool %v is missing suggested ETF %q", pool, want)
		}
	}
}

// A class with a pool of its own is never seeded, whichever switch is set:
// appending suggestions to a curated list dilutes the decision behind it.
func TestSuggestionsNeverDiluteACuratedPool(t *testing.T) {
	t.Parallel()

	yes := true

	for name, cfg := range map[string]config.ScreenerConfig{
		"global switch": {
			Enabled: true, SuggestUniverse: true,
			UniverseByAssetClass: map[string][]string{"etf": {"MYETF"}},
		},
		"per-class switch": {
			Enabled:              true,
			UniverseByAssetClass: map[string][]string{"etf": {"MYETF"}},
			ByAssetClass: map[string]config.ScreenerProfile{
				"etf": {SuggestUniverse: &yes},
			},
		},
		"profile universe": {
			Enabled: true, SuggestUniverse: true,
			ByAssetClass: map[string]config.ScreenerProfile{
				"etf": {Universe: []string{"MYETF"}},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := New(cfg, fakeHistory{series: map[string][]market.Candle{}},
				market.Res1Min, Options{})

			pool := s.pool(nil)
			if !containsStr(pool, "MYETF") {
				t.Fatalf("pool %v lost the curated ETF", pool)
			}

			for _, unwanted := range SuggestedUniverse("etf") {
				if containsStr(pool, unwanted) {
					t.Fatalf("pool %v diluted a curated ETF pool with %q", pool, unwanted)
				}
			}
		})
	}
}

// The shared switch still seeds every class when nothing is named — the
// operator who wants the screener to find things but has no idea what to list.
func TestGlobalSuggestSeedsEveryClass(t *testing.T) {
	t.Parallel()

	s := New(config.ScreenerConfig{Enabled: true, SuggestUniverse: true},
		fakeHistory{series: map[string][]market.Candle{}}, market.Res1Min, Options{})

	pool := s.pool(nil)

	for _, class := range SuggestedClasses() {
		for _, want := range SuggestedUniverse(class) {
			if !containsStr(pool, want) {
				t.Fatalf("pool is missing %q from class %q", want, class)
			}
		}
	}
}
