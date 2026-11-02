package autotune

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// AT2-8: the search ran on context.Background(), so a SIGTERM let an
// in-flight search finish — guided, then an un-cancellable refine — and then
// write config.yaml and re-wire the live engine after shutdown had begun. A
// cancelled caller must get no search and no proposal.
func TestACancelledCallerGetsNoSearchAndNoProposal(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	eng.SetStrategyConfig(strat)

	data := backtest.Generate([]string{"AAPL"}, 400)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := RunSymbolOptimize(ctx, eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, DefaultObjective, DefaultMinReturnPct)

	if result.Improved {
		t.Error("a cancelled search reported an improvement, which tuneSymbol would apply")
	}

	if result.Best.Result.NumTrades != result.WhatIf.NumTrades ||
		result.Best.Result.TotalReturnPct != result.WhatIf.TotalReturnPct {
		t.Errorf("the search still explored after cancellation: best %+v vs baseline %+v",
			result.Best.Result.NumTrades, result.WhatIf.NumTrades)
	}
}

// AT2-5: a loss request waits in the channel while a scheduled pass runs in
// the same goroutine. If that pass just applied a config to the symbol, the
// request must be dropped by on_loss.cooldown — judged NOW, not only when
// the streak was counted.
func TestALossRequestQueuedBehindAnApplyRespectsTheCooldown(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	cfg := eligibilityCfg(nil, nil)
	cfg.OnLoss.Cooldown = 6 * time.Hour

	eng.RecordAutotuneApply("AAPL") // what the pass just did

	var buf strings.Builder

	runLossPass(context.Background(), eng, rm, pf, cfg,
		filepath.Join(t.TempDir(), "config.yaml"), "AAPL", captureLog(&buf))

	if strings.Contains(buf.String(), "re-tuning after a losing streak") {
		t.Errorf("a symbol applied seconds ago was re-tuned despite a 6h cooldown: %s", buf.String())
	}
}

// AT2-7: the API checked Busy and then started a goroutine that tried for the
// slot itself, answering "started" for a pass that could then lose the slot
// and never run. StartOnce claims it before answering, and gives it back.
func TestStartOnceClaimsTheSlotBeforeAnswering(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)
	cfg := eligibilityCfg(nil, nil)
	path := filepath.Join(t.TempDir(), "config.yaml")

	if !TryAcquire() {
		t.Fatal("slot already held")
	}

	if StartOnce(context.Background(), eng, rm, pf, cfg, path, captureLog(&strings.Builder{})) {
		t.Error("StartOnce reported started while another search held the slot")
	}

	if !Busy() {
		t.Error("Busy() = false while the slot is held")
	}

	Release()

	if Busy() {
		t.Error("Busy() = true on a free slot")
	}

	if !StartOnce(context.Background(), eng, rm, pf, cfg, path, captureLog(&strings.Builder{})) {
		t.Fatal("StartOnce refused a free slot")
	}

	deadline := time.Now().Add(30 * time.Second)
	for Busy() {
		if time.Now().After(deadline) {
			t.Fatal("StartOnce's pass never released the slot")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// AT2-7's other half: Busy used to answer by claiming the slot and releasing
// it, so a scheduled tick that called TryAcquire in that instant lost its
// whole pass to a mere "is anything running?" check (an operator's button,
// polled). Busy must be a pure read: with only Busy calls competing, every
// TryAcquire on a free slot succeeds.
func TestBusyNeverTakesTheSlotFromARealCaller(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			select {
			case <-stop:
				return
			default:
				_ = Busy()
			}
		}
	}()

	lost := 0

	for range 20000 {
		if !TryAcquire() {
			lost++

			continue
		}

		Release()
	}

	close(stop)
	<-done

	if lost > 0 {
		t.Errorf("%d of 20000 TryAcquire calls on a free slot lost to Busy()", lost)
	}
}

// AT3-7: the pass the "Run autotune now" button starts ran on a bare
// goroutine — gin's Recovery covers only the handler, runSupervised only the
// scheduled pass — so a panic in the backtester or optimizer killed the live
// bot. It is recovered, reported, and the run slot is given back. (A nil
// engine is simply the cheapest way to make the pass panic.)
func TestAPanickingManualPassDoesNotKillTheProcess(t *testing.T) {
	cfg := eligibilityCfg(nil, nil)

	if !StartOnce(context.Background(), nil, nil, nil, cfg, "", captureLog(&strings.Builder{})) {
		t.Fatal("StartOnce did not claim an idle slot")
	}

	deadline := time.Now().Add(5 * time.Second)
	for Busy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if Busy() {
		t.Fatal("the panicking pass never released the run slot")
	}

	found := false

	for _, e := range errlog.Recent() {
		if strings.Contains(e.Message, "manual pass panicked") {
			found = true
		}
	}

	if !found {
		t.Error("the recovered panic was not reported to errlog")
	}
}
