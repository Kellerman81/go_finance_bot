package screener

import (
	"context"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// sectorMap answers from a fixed table; the engine's real one reaches a
// persistent cache and a provider lookup.
type sectorMap map[string]string

func (m sectorMap) Sector(_ context.Context, symbol string) string { return m[symbol] }

// volumeSeries builds candidates whose volume ranking is exactly the order
// given, so a test controls precisely who wins a slot.
func volumeSeries(order ...string) map[string][]market.Candle {
	out := make(map[string][]market.Candle, len(order))
	for i, sym := range order {
		out[sym] = trending(300, 100, 0.05, 0.01, float64(1_000_000-i*1000))
	}

	return out
}

// The bug this closes: the screener ranks on metrics that correlate hard
// within an industry, so it fills every slot with one sector — and the
// portfolio guard then refuses most of those buys at order time. The
// difference is a feed subscription and a warmup history per symbol that was
// never buyable.
func TestSectorCapDiversifiesTheSelection(t *testing.T) {
	t.Parallel()

	// The four highest-volume names are all tech; the energy names rank below.
	series := volumeSeries("T1", "T2", "T3", "T4", "E1", "E2")
	sectors := sectorMap{
		"T1": "technology", "T2": "technology", "T3": "technology", "T4": "technology",
		"E1": "energy", "E2": "energy",
	}

	cfg := config.ScreenerConfig{
		Enabled: true, Universe: keysOf(series), Rank: "volume",
		Filters: []string{"history"}, MaxSymbols: 4, MaxSymbolsPerSector: 2,
	}

	got := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors}).Screen(context.Background(), Inputs{}).Selected

	if len(got) != 4 {
		t.Fatalf("selected %v, want the cap of 4 filled", got)
	}

	counts := map[string]int{}
	for _, sym := range got {
		counts[sectors[sym]]++
	}

	if counts["technology"] != 2 {
		t.Errorf("selected %d tech names from %v, want exactly the sector cap of 2", counts["technology"], got)
	}

	// The slots the tech names could not take go to the next sector down
	// rather than being left empty — the substitution is the point.
	if counts["energy"] != 2 {
		t.Errorf("selected %d energy names from %v, want the freed slots reused", counts["energy"], got)
	}
}

// Without the cap the same input fills every slot with one sector — proving
// the test above measures the cap rather than the ranking.
func TestWithoutTheSectorCapOneIndustryTakesEverything(t *testing.T) {
	t.Parallel()

	series := volumeSeries("T1", "T2", "T3", "T4", "E1", "E2")
	sectors := sectorMap{
		"T1": "technology", "T2": "technology", "T3": "technology", "T4": "technology",
		"E1": "energy", "E2": "energy",
	}

	cfg := config.ScreenerConfig{
		Enabled: true, Universe: keysOf(series), Rank: "volume",
		Filters: []string{"history"}, MaxSymbols: 4, // no MaxSymbolsPerSector
	}

	got := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors}).Screen(context.Background(), Inputs{}).Selected

	for _, sym := range got {
		if sectors[sym] != "technology" {
			t.Fatalf("selected %v, want all four tech names with no cap set", got)
		}
	}
}

// The budget is shared across asset classes, matching the portfolio guard,
// which does not care which class a position sits in. A per-class budget would
// let two tech stocks AND two tech ETFs through a limit of two.
func TestSectorCapIsSharedAcrossAssetClasses(t *testing.T) {
	t.Parallel()

	series := volumeSeries("S1", "S2", "S3", "F1", "F2", "F3")
	sectors := sectorMap{
		"S1": "technology", "S2": "technology", "S3": "technology",
		"F1": "technology", "F2": "technology", "F3": "technology",
	}
	classes := classMap{
		"S1": "stock", "S2": "stock", "S3": "stock",
		"F1": "etf", "F2": "etf", "F3": "etf",
	}

	cfg := config.ScreenerConfig{
		Enabled: true, Universe: keysOf(series), Rank: "volume",
		Filters: []string{"history"}, MaxSymbolsPerSector: 2,
		ByAssetClass: map[string]config.ScreenerProfile{
			"stock": {MaxSymbols: ptrInt(3)},
			"etf":   {MaxSymbols: ptrInt(3)},
		},
	}

	got := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors, Classify: classes}).Screen(context.Background(), Inputs{}).Selected

	if len(got) != 2 {
		t.Errorf("selected %v, want 2 total — one sector budget shared by both classes", got)
	}
}

// An unsectored symbol is exempt, matching risk.PortfolioGuard.checkSector.
// Yahoo reports no sector for funds, so this is the normal case for ETFs.
func TestUnsectoredSymbolsAreExemptFromTheCap(t *testing.T) {
	t.Parallel()

	series := volumeSeries("T1", "T2", "T3", "U1", "U2", "U3")
	sectors := sectorMap{"T1": "technology", "T2": "technology", "T3": "technology"}

	cfg := config.ScreenerConfig{
		Enabled: true, Universe: keysOf(series), Rank: "volume",
		Filters: []string{"history"}, MaxSymbolsPerSector: 1,
	}

	got := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors}).Screen(context.Background(), Inputs{}).Selected

	var tech, unsectored int

	for _, sym := range got {
		if sectors[sym] == "" {
			unsectored++
		} else {
			tech++
		}
	}

	if tech != 1 {
		t.Errorf("selected %d sectored names from %v, want the cap of 1", tech, got)
	}

	if unsectored != 3 {
		t.Errorf("selected %d unsectored names from %v, want all 3 exempt", unsectored, got)
	}
}

// Without a Sectorizer the cap cannot bind — there is nothing to group by, and
// inventing a grouping would be worse than not capping.
func TestSectorCapIsInertWithoutASectorizer(t *testing.T) {
	t.Parallel()

	series := volumeSeries("T1", "T2", "T3")

	cfg := config.ScreenerConfig{
		Enabled: true, Universe: keysOf(series), Rank: "volume",
		Filters: []string{"history"}, MaxSymbolsPerSector: 1,
	}

	got := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{}).Screen(context.Background(), Inputs{}).Selected

	if len(got) != 3 {
		t.Errorf("selected %v, want all three with no Sectorizer installed", got)
	}
}

// The verdict carries the sector it was grouped under, so GET /api/screener can
// explain why a candidate was passed over rather than leaving it a mystery.
func TestVerdictsCarryTheSector(t *testing.T) {
	t.Parallel()

	series := volumeSeries("T1")
	sectors := sectorMap{"T1": " Technology "}

	res := New(config.ScreenerConfig{
		Enabled: true, Universe: []string{"T1"}, Filters: []string{"history"},
	}, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors}).Screen(context.Background(), Inputs{})

	if got := res.Verdicts[0].Sector; got != "technology" {
		t.Errorf("verdict sector = %q, want it normalised to %q", got, "technology")
	}
}

// SC3-6: a held position spends its sector's budget, because the guard's
// max_positions_per_sector counts it — with HELD in technology and a cap of 2,
// only ONE tech discovery can still be bought, and picking two meant the guard
// refused one of them on every evaluation. The held symbol is still kept.
// This test used to assert the opposite (held never counted), which is the
// defect SC3-6 describes.
func TestHeldSymbolsSpendTheirSectorBudget(t *testing.T) {
	t.Parallel()

	series := volumeSeries("HELD", "T1", "T2")
	sectors := sectorMap{"HELD": "technology", "T1": "technology", "T2": "technology"}

	cfg := config.ScreenerConfig{
		Enabled: true, Universe: keysOf(series), Rank: "volume",
		Filters: []string{"history"}, MaxSymbolsPerSector: 2, KeepHeld: true,
	}

	got := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors}).Screen(context.Background(),
		Inputs{Held: []string{"HELD"}}).Selected

	if !containsStr(got, "HELD") {
		t.Fatalf("selected %v, want the held symbol kept", got)
	}

	if n := countPrefix(got, "T"); n != 1 {
		t.Errorf("selected %d tech discoveries from %v beside a held tech position under a cap of 2, want 1",
			n, got)
	}
}

// A pinned symbol that is NOT held spends nothing: the guard counts
// positions, not watchlist entries, so the cap of 1 still has its slot.
func TestPinnedUnheldSymbolsDoNotSpendSectorSlots(t *testing.T) {
	t.Parallel()

	series := volumeSeries("PIN", "T1", "T2")
	sectors := sectorMap{"PIN": "technology", "T1": "technology", "T2": "technology"}

	cfg := config.ScreenerConfig{
		Enabled: true, Universe: keysOf(series), Rank: "volume",
		Filters: []string{"history"}, MaxSymbolsPerSector: 1, KeepManual: true,
	}

	got := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors}).Screen(context.Background(),
		Inputs{Pinned: []string{"PIN"}}).Selected

	if !containsStr(got, "PIN") || countPrefix(got, "T") != 1 {
		t.Errorf("selected %v, want PIN kept and one tech discovery", got)
	}
}

// countPrefix counts the symbols in syms that start with prefix.
func countPrefix(syms []string, prefix string) int {
	n := 0

	for _, sym := range syms {
		if strings.HasPrefix(sym, prefix) {
			n++
		}
	}

	return n
}

// The screener must count its sector budget the same way the guard counts its
// own, or diversifying here just moves the disagreement rather than fixing it.
func TestScreenerSectorBudgetFollowsThePerClassSetting(t *testing.T) {
	t.Parallel()

	series := volumeSeries("S1", "S2", "F1", "F2")
	sectors := sectorMap{"S1": "tech", "S2": "tech", "F1": "tech", "F2": "tech"}
	classes := classMap{"S1": "stock", "S2": "stock", "F1": "etf", "F2": "etf"}

	cfg := config.ScreenerConfig{
		Enabled: true, Universe: keysOf(series), Rank: "volume",
		Filters: []string{"history"}, MaxSymbolsPerSector: 1,
		ByAssetClass: map[string]config.ScreenerProfile{
			"stock": {MaxSymbols: ptrInt(5)},
			"etf":   {MaxSymbols: ptrInt(5)},
		},
	}

	shared := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors, Classify: classes}).
		Screen(context.Background(), Inputs{}).Selected

	if len(shared) != 1 {
		t.Errorf("selected %v with a shared budget, want 1 across both classes", shared)
	}

	perClass := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Sectors: sectors, Classify: classes, SectorCapsPerAssetClass: true}).
		Screen(context.Background(), Inputs{}).Selected

	if len(perClass) != 2 {
		t.Fatalf("selected %v with a per-class budget, want 2 (one stock, one etf)", perClass)
	}

	got := map[string]int{}
	for _, sym := range perClass {
		got[classes[sym]]++
	}

	if got["stock"] != 1 || got["etf"] != 1 {
		t.Errorf("selected %v, want exactly one of each class", perClass)
	}
}
