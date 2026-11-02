package engine

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/protect"
)

// protPeakSaveEvery throttles saves that only raise the equity peak, which can
// move on every sync while the account climbs.
const protPeakSaveEvery = 5 * time.Minute

// persistProtectionsIfChanged saves the protection state when it has moved
// since the last save.
//
// The state was only ever saved when a halt tripped or on Resume, so any
// restart without one reset the drawdown baselines: the peak and the day's
// starting equity came back as the current, already lower equity, and the
// realized give-back counter as zero. With a 20% limit, a 100k peak and 85k
// equity, a restart and a further fall to 75k — 25% below the real peak —
// halted nothing (PR2-1).
//
// A new UTC day, a halt, a change to the realized counters or the day's
// starting equity is rare and saved at once. A peak that only rose is
// throttled to one save per protPeakSaveEvery, and Run saves again on
// shutdown, so the evaluation path does not write the DB every 30 s.
func (e *Engine) persistProtectionsIfChanged(force bool) {
	p := e.Protections()
	if p == nil {
		return
	}

	cur := p.ExportState()

	e.protSaveMu.Lock()

	last, lastAt := e.protSaved, e.protSavedAt

	e.protSaveMu.Unlock()

	if cur == last {
		return
	}

	if !force && !materialChange(cur, last) && time.Since(lastAt) < protPeakSaveEvery {
		return
	}

	e.saveProtectionState(cur)
}

// materialChange reports whether the state moved in a way that must be saved
// at once: anything but a peak that only rose.
func materialChange(cur, last protect.State) bool {
	return cur.Halted != last.Halted || cur.Day != last.Day ||
		cur.DayEquity != last.DayEquity || cur.RealizedPnL != last.RealizedPnL ||
		cur.RealizedPeak != last.RealizedPeak || cur.PeakEquity < last.PeakEquity
}

// saveProtectionState writes s and remembers it as the last state saved.
func (e *Engine) saveProtectionState(s protect.State) {
	if err := e.store.SaveProtectionState(s); err != nil {
		e.log.Warn("persist protection state failed", "err", err)

		return
	}

	e.protSaveMu.Lock()

	e.protSaved, e.protSavedAt = s, time.Now()

	e.protSaveMu.Unlock()
}
