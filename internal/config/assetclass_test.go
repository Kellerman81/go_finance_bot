package config

import (
	"strings"
	"testing"
	"time"
)

// The scale trap this validation exists for: "30" meaning 30% is read as 3000%
// of equity — a cap that can never bind, on an operator who believes they are
// capped. Catching it at load is the only place it can be caught, since the
// guard cannot tell an intended 30% from an intended 3000%.
func TestAssetClassCapMustBeAFraction(t *testing.T) {
	t.Parallel()

	c := PortfolioConfig{MaxPctPerAssetClass: map[string]float64{"etf": 30}}

	err := c.Validate()
	if err == nil {
		t.Fatal("a cap of 30 (meaning 30%) was accepted as a fraction")
	}

	if !strings.Contains(err.Error(), "etf") {
		t.Errorf("error %q does not name the offending class", err)
	}

	if err := (PortfolioConfig{
		MaxPctPerAssetClass: map[string]float64{"etf": -0.1},
	}).Validate(); err == nil {
		t.Error("a negative cap was accepted")
	}

	if err := (PortfolioConfig{
		MaxPctPerAssetClass: map[string]float64{" ": 0.3},
	}).Validate(); err == nil {
		t.Error("a cap on an empty class name was accepted")
	}

	if err := (PortfolioConfig{
		MaxPctPerAssetClass: map[string]float64{"etf": 0.3, "stock": 1},
	}).Validate(); err != nil {
		t.Errorf("a valid set of fractions was rejected: %v", err)
	}
}

// Hand-written config must reach the guard in the same spelling the classifier
// produces, and no-op entries must not make the guard think it has work to do.
func TestNormalizedAssetClassCaps(t *testing.T) {
	t.Parallel()

	got := PortfolioConfig{MaxPctPerAssetClass: map[string]float64{
		"  ETF ": 0.3,
		"Stock":  0.6,
		"bond":   0, // no cap
		"crypto": -1,
	}}.NormalizedAssetClassCaps()

	want := map[string]float64{"etf": 0.3, "stock": 0.6}
	if len(got) != len(want) {
		t.Fatalf("caps = %v, want %v", got, want)
	}

	for class, pct := range want {
		if got[class] != pct {
			t.Errorf("caps[%q] = %v, want %v", class, got[class], pct)
		}
	}

	// Nothing configured, and nothing but no-ops, must both report "no caps"
	// so the guard can skip building for them.
	if got := (PortfolioConfig{}).NormalizedAssetClassCaps(); got != nil {
		t.Errorf("caps from an empty config = %v, want nil", got)
	}

	if got := (PortfolioConfig{
		MaxPctPerAssetClass: map[string]float64{"etf": 0},
	}).NormalizedAssetClassCaps(); got != nil {
		t.Errorf("caps from an all-zero config = %v, want nil", got)
	}
}

// The slot counts normalise like the caps they divide — same hand-written
// file, same case and whitespace mistakes — and a count that cannot bind (0,
// negative, or 1 on a class whose own budget already caps it) is dropped
// rather than carried as a live setting. 1 on a class WITHOUT a budget is a
// real ceiling and stays (RK2-4: dropping it left such a class uncapped).
func TestNormalizedAssetClassSlots(t *testing.T) {
	t.Parallel()

	got := PortfolioConfig{
		MaxPctPerAssetClass: map[string]float64{"bond": 0.2},
		MinPositionsPerAssetClass: map[string]int{
			" Stock ": 2,
			"ETF":     3,
			"bond":    1,  // the whole budget in one position: no split
			"crypto":  1,  // no budget: one position's share of the leftover
			"cash":    0,  // absent, spelled out
			"fund":    -1, // nonsense
		},
	}.NormalizedAssetClassSlots()

	want := map[string]int{"stock": 2, "etf": 3, "crypto": 1}
	if len(got) != len(want) {
		t.Fatalf("slots = %v, want %v", got, want)
	}

	for class, n := range want {
		if got[class] != n {
			t.Errorf("slots[%q] = %d, want %d", class, got[class], n)
		}
	}

	if got := (PortfolioConfig{}).NormalizedAssetClassSlots(); got != nil {
		t.Errorf("slots from an empty config = %v, want nil", got)
	}

	if got := (PortfolioConfig{
		MaxPctPerAssetClass:       map[string]float64{"etf": 0.5},
		MinPositionsPerAssetClass: map[string]int{"etf": 1},
	}).NormalizedAssetClassSlots(); got != nil {
		t.Errorf("slots from an all-no-op config = %v, want nil", got)
	}
}

// Slot counts and class budgets are validated together, but NOT as a pairing
// requirement any more: a class carrying slots and no budget of its own is a
// supported configuration that shares whatever the named budgets left
// unclaimed (see risk.PortfolioGuard.MaxPositionValue). What must still be
// refused is a count that is not a count, and an allocation that claims more
// than the whole book.
func TestAssetClassSlotValidation(t *testing.T) {
	t.Parallel()

	ok := PortfolioConfig{
		MaxPctPerAssetClass:       map[string]float64{" ETF ": 0.30},
		MinPositionsPerAssetClass: map[string]int{"etf": 2},
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("a slot count against its own budget was rejected: %v", err)
	}

	// Formerly rejected as a no-op. It now means "split what the budgeted
	// classes did not take", so refusing it would refuse a real setting.
	noBudget := PortfolioConfig{
		Enabled:                   true,
		MaxPctPerAssetClass:       map[string]float64{"etf": 0.30},
		MinPositionsPerAssetClass: map[string]int{"stock": 2},
	}
	if err := noBudget.Validate(); err != nil {
		t.Errorf("slots for an unbudgeted class were rejected; they share the leftover: %v", err)
	}

	zeroBudget := PortfolioConfig{
		Enabled:                   true,
		MaxPctPerAssetClass:       map[string]float64{"etf": 0},
		MinPositionsPerAssetClass: map[string]int{"etf": 2},
	}
	if err := zeroBudget.Validate(); err != nil {
		t.Errorf("a zero cap is no cap, which is now a leftover share, not an error: %v", err)
	}

	// 0 and 1 are "do not split" and need no budget at all.
	inert := PortfolioConfig{
		Enabled:                   true,
		MinPositionsPerAssetClass: map[string]int{"etf": 1, "stock": 0},
	}
	if err := inert.Validate(); err != nil {
		t.Errorf("an inert slot count was rejected: %v", err)
	}

	// Staged at their off values: config.example.yaml is an entire file
	// written this way, and refusing to start over a block that is inert
	// until it is switched on would be worse than the silence it prevents.
	staged := PortfolioConfig{
		MaxPctPerAssetClass:       map[string]float64{"stock": 0},
		MinPositionsPerAssetClass: map[string]int{"stock": 2},
	}
	if err := staged.Validate(); err != nil {
		t.Errorf("a disabled portfolio block was rejected: %v", err)
	}

	negative := PortfolioConfig{MinPositionsPerAssetClass: map[string]int{"etf": -2}}
	if err := negative.Validate(); err == nil {
		t.Error("a negative position count was accepted")
	}

	empty := PortfolioConfig{MinPositionsPerAssetClass: map[string]int{"  ": 2}}
	if err := empty.Validate(); err == nil {
		t.Error("an empty class name was accepted")
	}
}

// The class budgets are fractions of one book. Over 100% between them, every
// class still computes its own slot from its own share, so the slots total
// more money than exists and whichever order arrives last is refused by a cash
// limit instead of by the allocation the operator wrote down.
func TestAssetClassBudgetsCannotExceedTheWholeBook(t *testing.T) {
	t.Parallel()

	over := PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"stock": 0.7, "etf": 0.3, "unclassified": 0.1},
	}
	if err := over.Validate(); err == nil {
		t.Error("budgets summing to 1.1 were accepted")
	}

	// Exactly the whole book is the normal way to write a full allocation,
	// and 0.7 + 0.3 is not exactly 1 in binary floating point — the check
	// must not refuse it on representation error alone.
	exact := PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"stock": 0.7, "etf": 0.3},
	}
	if err := exact.Validate(); err != nil {
		t.Errorf("budgets summing to exactly 1.0 were rejected: %v", err)
	}

	// Staged off, the same oversubscription must not stop the bot starting.
	staged := PortfolioConfig{
		MaxPctPerAssetClass: map[string]float64{"stock": 0.7, "etf": 0.3, "unclassified": 0.1},
	}
	if err := staged.Validate(); err != nil {
		t.Errorf("a disabled block was rejected for oversubscription: %v", err)
	}
}

// The two tiers of the slot rule live in different config sections, so the
// cross-check has to happen at the top level. Per-class counts are carved out
// of engine.min_positions, so claiming more than it has describes a book that
// cannot exist.
func TestPerClassSlotsCannotExceedGlobalMinPositions(t *testing.T) {
	t.Parallel()

	base := func() Config {
		c := Default()
		c.Portfolio.Enabled = true
		c.Portfolio.MaxPctPerAssetClass = map[string]float64{"stock": 0.7, "etf": 0.3}

		return c
	}

	over := base()
	over.Engine.MinPositions = 3
	over.Portfolio.MinPositionsPerAssetClass = map[string]int{"stock": 2, "etf": 2}

	if err := over.validatePositionSlots(); err == nil {
		t.Error("4 per-class positions against a global 3 were accepted")
	}

	exact := base()
	exact.Engine.MinPositions = 4
	exact.Portfolio.MinPositionsPerAssetClass = map[string]int{"stock": 2, "etf": 2}

	if err := exact.validatePositionSlots(); err != nil {
		t.Errorf("per-class counts exactly filling the global one were rejected: %v", err)
	}

	// min_positions 0 is "no global split": the per-class counts stand alone
	// and nothing is being carved out of anything, which is exactly how a
	// per-class-only configuration is written.
	noGlobal := base()
	noGlobal.Engine.MinPositions = 0
	noGlobal.Portfolio.MinPositionsPerAssetClass = map[string]int{"stock": 2, "etf": 2}

	if err := noGlobal.validatePositionSlots(); err != nil {
		t.Errorf("per-class counts with no global tier were rejected: %v", err)
	}
}

// on_loss rewrites live trading config unsupervised, so a nonsense counter or
// cooldown must be refused at load rather than silently normalised.
func TestAutotuneOnLossValidation(t *testing.T) {
	t.Parallel()

	enabled := func(on AutotuneOnLossConfig) AutotuneConfig {
		return AutotuneConfig{Enabled: true, OnLoss: on}
	}

	if err := enabled(AutotuneOnLossConfig{
		Enabled: true, AfterLosses: -1,
	}).Validate(); err == nil {
		t.Error("a negative after_losses was accepted")
	}

	if err := enabled(AutotuneOnLossConfig{
		Enabled: true, Cooldown: -time.Hour,
	}).Validate(); err == nil {
		t.Error("a negative cooldown was accepted")
	}

	// 0 is the documented "use the default" / "no cooldown" value, not an error.
	if err := enabled(AutotuneOnLossConfig{Enabled: true}).Validate(); err != nil {
		t.Errorf("an all-zero on_loss block was rejected: %v", err)
	}

	// A disabled block is never acted on, so a stray value there is harmless.
	if err := enabled(AutotuneOnLossConfig{
		Enabled: false, AfterLosses: -5,
	}).Validate(); err != nil {
		t.Errorf("a disabled on_loss block was validated: %v", err)
	}
}
