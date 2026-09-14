package engine

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// lossStore records what the loss-streak counters were last persisted as.
type lossStore struct {
	nopStore

	streaks map[string]int
	writes  int
}

func (s *lossStore) SaveLossStreaks(streaks map[string]int) error {
	s.streaks = streaks
	s.writes++

	return nil
}

func (s *lossStore) LoadLossStreaks() (map[string]int, error) { return s.streaks, nil }

// lossEngine builds an engine wired for the loss-streak path only: a store, a
// silent log and the request channel. It deliberately does not install
// protections — the counter must work whether or not they are configured.
func lossEngine(st Store, cfg config.AutotuneConfig) *Engine {
	return &Engine{
		watchlist:       make(map[string]struct{}),
		pinned:          make(map[string]struct{}),
		openPos:         make(map[string]*OpenPosition),
		peaks:           make(map[string]float64),
		autotuneApplies: make(map[string]time.Time),
		lossStreaks:     make(map[string]int),
		lossTune:        make(chan string, lossTuneQueue),
		autotuneCfg:     cfg,
		store:           st,
		provider:        quietProvider{},
		barsStore:       newBarStore(maxBars, time.Minute),
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// onLossCfg enables the trigger with an explicit counter.
func onLossCfg(after int) config.AutotuneConfig {
	return config.AutotuneConfig{
		Enabled: true,
		OnLoss:  config.AutotuneOnLossConfig{Enabled: true, AfterLosses: after},
	}
}

// closed builds a realized closing sell with the given P&L.
func closed(symbol string, pnl float64) TradeRecord {
	return TradeRecord{Symbol: symbol, Side: broker.Sell, PnL: &pnl, Status: "filled"}
}

// requested drains one pending tune request, or reports that none is queued.
func requested(e *Engine) (string, bool) {
	select {
	case sym := <-e.lossTune:
		return sym, true
	default:
		return "", false
	}
}

// The counter fires exactly on the configured value, not before, and then
// every further N in a row — the next re-tune needs a fresh run of losses
// rather than one more loss.
//
// The streak itself keeps climbing rather than resetting on a trigger. That is
// what lets one honest consecutive-loss count serve both on_loss and
// protections.drop_after_losses: a counter reset by the re-tune could never
// reach the give-up threshold, and would make the number mean "losses since we
// last did something" rather than "losses in a row".
func TestLossStreakFiresEveryNLosses(t *testing.T) {
	t.Parallel()

	e := lossEngine(&lossStore{}, onLossCfg(3))

	for range 2 {
		e.noteRealizedClose(closed("SAP.DE", -10))

		if sym, ok := requested(e); ok {
			t.Fatalf("tune requested for %s before the counter was reached", sym)
		}
	}

	e.noteRealizedClose(closed("SAP.DE", -10))

	sym, ok := requested(e)
	if !ok {
		t.Fatal("no tune requested after the third consecutive loss")
	}

	if sym != "SAP.DE" {
		t.Errorf("requested %q, want SAP.DE", sym)
	}

	if got := e.LossStreaks()["SAP.DE"]; got != 3 {
		t.Errorf("streak = %d after firing, want it to keep counting to 3", got)
	}

	// The next two losses must NOT fire: holding the counter at the threshold
	// would turn one bad regime into a re-tune per trade.
	for range 2 {
		e.noteRealizedClose(closed("SAP.DE", -10))

		if sym, ok := requested(e); ok {
			t.Errorf("a single further loss re-fired a tune for %s", sym)
		}
	}

	// The sixth in a row does, and the streak reads as six.
	e.noteRealizedClose(closed("SAP.DE", -10))

	if _, ok := requested(e); !ok {
		t.Error("no tune requested on the sixth consecutive loss")
	}

	if got := e.LossStreaks()["SAP.DE"]; got != 6 {
		t.Errorf("streak = %d, want 6 consecutive losses counted honestly", got)
	}
}

// A win resets the streak — this counts consecutive losses, so a symbol
// alternating wins and losses is working as intended and is never tuned.
func TestLossStreakResetsOnAWin(t *testing.T) {
	t.Parallel()

	e := lossEngine(&lossStore{}, onLossCfg(3))

	for range 10 {
		e.noteRealizedClose(closed("AAPL", -5))
		e.noteRealizedClose(closed("AAPL", 5))
	}

	if sym, ok := requested(e); ok {
		t.Errorf("alternating wins and losses requested a tune for %s", sym)
	}

	if got := e.LossStreaks()["AAPL"]; got != 0 {
		t.Errorf("streak = %d, want 0 after a winning close", got)
	}

	// A breakeven close counts as "still working" too, not as a loss.
	e.noteRealizedClose(closed("AAPL", -5))
	e.noteRealizedClose(closed("AAPL", 0))

	if got := e.LossStreaks()["AAPL"]; got != 0 {
		t.Errorf("streak = %d, want a breakeven close to reset it", got)
	}
}

// Each symbol counts on its own: one symbol's losses must never tune another.
func TestLossStreakIsPerSymbol(t *testing.T) {
	t.Parallel()

	e := lossEngine(&lossStore{}, onLossCfg(2))

	e.noteRealizedClose(closed("AAPL", -1))
	e.noteRealizedClose(closed("MSFT", -1))

	if sym, ok := requested(e); ok {
		t.Fatalf("one loss each on two symbols fired a tune for %s", sym)
	}

	e.noteRealizedClose(closed("MSFT", -1))

	sym, ok := requested(e)
	if !ok || sym != "MSFT" {
		t.Fatalf("requested (%q, %v), want MSFT", sym, ok)
	}

	if got := e.LossStreaks()["AAPL"]; got != 1 {
		t.Errorf("AAPL streak = %d, want its own count of 1 to be untouched", got)
	}
}

// 0 means the documented default, not "tune on every loss".
func TestLossStreakDefaultCounter(t *testing.T) {
	t.Parallel()

	e := lossEngine(&lossStore{}, onLossCfg(0))

	for range defaultAfterLosses - 1 {
		e.noteRealizedClose(closed("NVDA", -1))
	}

	if sym, ok := requested(e); ok {
		t.Fatalf("fired early for %s with after_losses unset", sym)
	}

	e.noteRealizedClose(closed("NVDA", -1))

	if _, ok := requested(e); !ok {
		t.Errorf("no tune after %d losses with after_losses unset", defaultAfterLosses)
	}
}

// "autotune disabled means nothing is written to strategy_overrides
// automatically" must hold however on_loss is configured. Counting while
// disabled would also mean re-enabling fires a burst from a stale streak.
func TestLossStreakGatedOffWhenDisabled(t *testing.T) {
	t.Parallel()

	cases := map[string]config.AutotuneConfig{
		"autotune off": {
			Enabled: false,
			OnLoss:  config.AutotuneOnLossConfig{Enabled: true, AfterLosses: 2},
		},
		"on_loss off": {
			Enabled: true,
			OnLoss:  config.AutotuneOnLossConfig{Enabled: false, AfterLosses: 2},
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			e := lossEngine(&lossStore{}, cfg)

			for range 5 {
				e.noteRealizedClose(closed("SAP.DE", -10))
			}

			if sym, ok := requested(e); ok {
				t.Errorf("a disabled trigger requested a tune for %s", sym)
			}

			if got := e.LossStreaks()["SAP.DE"]; got != 0 {
				t.Errorf("streak = %d while disabled, want no accumulation", got)
			}
		})
	}
}

// Only a realized closing sell is an outcome. A buy, a blocked decision or a
// record with no P&L says nothing about whether the config is working.
func TestLossStreakIgnoresNonOutcomes(t *testing.T) {
	t.Parallel()

	e := lossEngine(&lossStore{}, onLossCfg(2))

	pnl := -10.0
	for _, rec := range []TradeRecord{
		{Symbol: "X", Side: broker.Buy, PnL: &pnl, Status: "filled"},
		{Symbol: "X", Side: broker.Sell, PnL: &pnl, Status: "blocked"},
		{Symbol: "X", Side: broker.Sell, PnL: nil, Status: "filled"},
	} {
		e.noteRealizedClose(rec)
	}

	if got := e.LossStreaks()["X"]; got != 0 {
		t.Errorf("streak = %d, want non-outcome records to be ignored", got)
	}
}

// Test-mode closes count, matching how autotune's existing gating already
// judges symbols on simulated trades — otherwise the reactive trigger would be
// the one part of autotune that goes blind during the training phase.
func TestLossStreakCountsSimulatedCloses(t *testing.T) {
	t.Parallel()

	e := lossEngine(&lossStore{}, onLossCfg(2))

	for range 2 {
		rec := closed("SIM", -10)
		rec.Simulated = true

		e.noteRealizedClose(rec)
	}

	if _, ok := requested(e); !ok {
		t.Error("simulated closes did not count toward the streak")
	}
}

// The cooldown bounds how often one persistently losing symbol can rewrite its
// own config, and a streak arriving inside it is dropped rather than queued.
func TestLossStreakCooldown(t *testing.T) {
	t.Parallel()

	cfg := onLossCfg(2)
	cfg.OnLoss.Cooldown = time.Hour

	e := lossEngine(&lossStore{}, cfg)
	e.autotuneApplies["SAP.DE"] = time.Now() // just tuned

	for range 2 {
		e.noteRealizedClose(closed("SAP.DE", -10))
	}

	if sym, ok := requested(e); ok {
		t.Errorf("a tune was requested for %s inside its cooldown", sym)
	}

	// The counter keeps climbing through a dropped request: it counts losses,
	// not requests, which is what lets drop_after_losses still be reached.
	if got := e.LossStreaks()["SAP.DE"]; got != 2 {
		t.Errorf("streak = %d, want the losing run still counted at 2", got)
	}

	// Outside the cooldown the same streak fires.
	e.autotuneApplies["SAP.DE"] = time.Now().Add(-2 * time.Hour)

	for range 2 {
		e.noteRealizedClose(closed("SAP.DE", -10))
	}

	if _, ok := requested(e); !ok {
		t.Error("no tune requested after the cooldown had elapsed")
	}
}

// A symbol autotune has never touched has no cooldown to serve.
func TestLossStreakCooldownAllowsANeverTunedSymbol(t *testing.T) {
	t.Parallel()

	cfg := onLossCfg(2)
	cfg.OnLoss.Cooldown = 24 * time.Hour

	e := lossEngine(&lossStore{}, cfg)

	for range 2 {
		e.noteRealizedClose(closed("FRESH", -10))
	}

	if _, ok := requested(e); !ok {
		t.Error("a symbol with no recorded apply was held back by the cooldown")
	}
}

// Losing three in a row across a restart is still losing three in a row.
func TestLossStreaksSurviveARestart(t *testing.T) {
	t.Parallel()

	st := &lossStore{}
	first := lossEngine(st, onLossCfg(3))

	first.noteRealizedClose(closed("SAP.DE", -10))
	first.noteRealizedClose(closed("SAP.DE", -10))

	if st.writes == 0 {
		t.Fatal("the counter was not persisted")
	}

	second := lossEngine(st, onLossCfg(3))
	second.restoreLossStreaks()

	if got := second.LossStreaks()["SAP.DE"]; got != 2 {
		t.Fatalf("restored streak = %d, want 2", got)
	}

	second.noteRealizedClose(closed("SAP.DE", -10))

	if _, ok := requested(second); !ok {
		t.Error("the restored streak did not carry through to a tune request")
	}
}

// A full request queue must never block the trading loop — dropping costs one
// re-tune opportunity, blocking would stall every decision behind it.
func TestLossStreakDropsWhenTheQueueIsFull(t *testing.T) {
	t.Parallel()

	e := lossEngine(&lossStore{}, onLossCfg(1))

	for i := range lossTuneQueue + 10 {
		done := make(chan struct{})

		go func() {
			defer close(done)

			e.noteRealizedClose(closed("SAP.DE", -1))
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("noteRealizedClose blocked on a full queue at request %d", i)
		}
	}
}
