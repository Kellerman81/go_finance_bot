package api

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// The allocation panel.
//
// portfolio.max_pct_per_asset_class refuses buys once a class reaches its share
// of equity, which is invisible from the holdings table: you can see that six
// positions are held without seeing that five of them are ETFs and the sixth
// buy was refused for it. This turns the cap into something an operator can
// read before it bites, rather than something inferred from a blocked trade.
//
// The arithmetic deliberately mirrors risk.PortfolioGuard.checkAssetClass —
// same equity denominator, same "a flat row is not a position" rule — because a
// panel that disagreed with the limit it displays would be worse than no panel.

// applyCap fills in a capped row's ceiling, headroom and at-cap flag.
func (r *allocationRow) applyCap(capFrac, equity float64) {
	r.Capped = true
	r.CapPct = capFrac * 100
	// No equity, no binding cap: the guard allows everything at equity <= 0
	// (it cannot form a share), and "Value >= 0" would mark every capped row
	// at its cap (AP2-7).
	r.AtCap = equity > 0 && r.Value >= capFrac*equity
	r.HeadroomPct = max(r.CapPct-r.PctEquity, 0)
}

// allocationRow is one asset class's share of the book.
type allocationRow struct {
	// Class is the canonical label, or "" for the row holding everything
	// nothing could classify. That row stays keyed on "" rather than on
	// config.UnclassifiedAssetClass so the view can label it in the reader's
	// language; its cap is looked up under the sentinel, via capKey.
	Class     string
	Positions int
	Value     float64
	// PctEquity is Value as a percentage of equity, and CapPct the configured
	// ceiling as a percentage. CapPct is 0 when the class is uncapped.
	PctEquity float64
	CapPct    float64
	// Capped distinguishes "no ceiling" from a ceiling that happens to be far
	// away, and AtCap reports that further buys in this class are refused now.
	Capped bool
	AtCap  bool
	// HeadroomPct is how much more of equity may go into this class, 0 when at
	// or over the cap. Meaningless (and not rendered) when uncapped.
	HeadroomPct float64
}

// allocation summarises the book by asset class: every class actually held,
// plus every class with a cap configured, so an empty budget shows its headroom
// instead of vanishing from the table.
func allocation(
	snap portfolio.Snapshot,
	classes map[string]string,
	caps map[string]float64,
) []allocationRow {
	// Same denominator the guard divides by, including its fallback for a
	// snapshot that filled Cash/Invested without deriving Equity.
	equity := snap.Equity
	if equity <= 0 {
		equity = snap.Cash + snap.Invested
	}

	// The guard canonicalises this map at construction and the caps are keyed
	// lower-cased; the panel used the raw map, so a hand-written
	// asset_classes: entry ("spy: ETF") bucketed one way in the guard and
	// another here — the panel then showed an uncapped "ETF" row holding the
	// position next to a capped "etf" row reading 0% used, while the guard was
	// refusing buys against the cap the panel called absent. Same function,
	// same buckets. See risk.NormalizeClassLabels.
	classes = risk.NormalizeClassLabels(classes)

	rows := make(map[string]*allocationRow, len(caps)+len(snap.Holdings))

	row := func(class string) *allocationRow {
		if r, ok := rows[class]; ok {
			return r
		}

		r := &allocationRow{Class: class}
		rows[class] = r

		return r
	}

	// A class with a cap is always listed, held or not — "0 of 30% used" is
	// exactly what someone deciding what to buy next wants to see. A cap on
	// the unclassified bucket lands on the same row its holdings do.
	for class := range caps {
		if class == config.UnclassifiedAssetClass {
			class = ""
		}

		row(class)
	}

	for sym, h := range snap.Holdings {
		if !h.Held() {
			continue // a flat or dust row is not a position, matching the guard (PF3-4)
		}

		r := row(classes[strings.ToUpper(strings.TrimSpace(sym))])
		r.Positions++
		r.Value += h.MarketValue
	}

	out := make([]allocationRow, 0, len(rows))

	for class, r := range rows {
		if equity > 0 {
			r.PctEquity = r.Value / equity * 100
		}

		if capFrac, ok := caps[capKey(class)]; ok && capFrac > 0 {
			r.applyCap(capFrac, equity)
		}

		out = append(out, *r)
	}

	// Capped classes first (they are the ones that can block a buy), then by
	// how full they are, then by name so the order is stable between renders.
	slices.SortFunc(out, func(a, b allocationRow) int {
		if a.Capped != b.Capped {
			if a.Capped {
				return -1
			}

			return 1
		}

		if c := cmp.Compare(b.PctEquity, a.PctEquity); c != 0 {
			return c
		}

		return cmp.Compare(a.Class, b.Class)
	})

	return out
}

// capKey is the cap a row is judged against: the row's own class, except for the
// unclassified row, whose holdings the guard budgets under the reserved sentinel
// key. The panel has to look it up the same way, or it would report "no limit"
// on the one bucket that does have one.
func capKey(class string) string {
	if class == "" {
		return config.UnclassifiedAssetClass
	}

	return class
}
