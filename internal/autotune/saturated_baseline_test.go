package autotune

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
)

// AT2-6: two winning closes since the last apply put the recent record at a
// 100% win rate, which a strict > can never beat — so even a candidate that
// also wins every trade, at five times the expectancy, was refused until the
// symbol happened to lose. A saturated win rate can only be matched.
func TestASaturatedWinRateCanBeMatchedNotOnlyBeaten(t *testing.T) {
	t.Parallel()

	baseTrial := backtest.Trial{Score: 1}
	perfect := engine.SymbolStats{Trades: 2, WinRatePct: 100, ExpectancyPct: 0.5}
	better := backtest.Trial{Score: 2, Result: backtest.Result{WinRatePct: 100, ExpectancyPct: 2.5}}

	if !beatsBothBaselines(better, baseTrial, false, engine.SymbolStats{}, DefaultMinReturnPct, true, perfect) {
		t.Error("a 100%-win, higher-expectancy candidate could not replace a 100%-win recent record")
	}

	if !beatsBothBaselines(better, baseTrial, true, perfect, DefaultMinReturnPct, false, engine.SymbolStats{}) {
		t.Error("a 100%-win real record made the symbol un-tunable")
	}

	// Expectancy still has to improve: matching both numbers is not a win.
	same := backtest.Trial{Score: 2, Result: backtest.Result{WinRatePct: 100, ExpectancyPct: 0.5}}
	if beatsBothBaselines(same, baseTrial, true, perfect, DefaultMinReturnPct, false, engine.SymbolStats{}) {
		t.Error("a candidate merely equal to the real record was accepted")
	}
}

// AT3-5: the AT2-6 fix still asked a candidate to MATCH a saturated record,
// and one winning close since the last apply is a 100% record — so a
// 20-trade, 90%-win candidate at five times the expectancy was refused, and
// the symbol stayed locked until a loss arrived. The record's win rate is
// read smoothed by its size: one win is a low hurdle, not an impassable one.
func TestOneWinningCloseDoesNotLockTheSymbol(t *testing.T) {
	t.Parallel()

	baseTrial := backtest.Trial{Score: 1}
	oneWin := engine.SymbolStats{Trades: 1, Wins: 1, WinRatePct: 100, ExpectancyPct: 0.5}
	good := backtest.Trial{Score: 2, Result: backtest.Result{WinRatePct: 90, ExpectancyPct: 2.5}}

	if !beatsBothBaselines(good, baseTrial, false, engine.SymbolStats{}, DefaultMinReturnPct, true, oneWin) {
		t.Error("a 90%-win candidate at 5x the expectancy was refused against a single winning close")
	}

	// A long winning record stays a high hurdle.
	streak := engine.SymbolStats{Trades: 30, Wins: 30, WinRatePct: 100, ExpectancyPct: 0.5}
	if beatsBothBaselines(good, baseTrial, true, streak, DefaultMinReturnPct, false, engine.SymbolStats{}) {
		t.Error("a 90%-win candidate replaced a 30-for-30 real record")
	}
}
