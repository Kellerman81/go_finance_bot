package api

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// AP3-4: the field optimizer swept the SHARED strategy for every symbol. For a
// symbol with its own strategy_overrides entry — every autotuned one — the
// baseline and each row backtested a strategy that symbol does not trade, and
// Apply then filled the shared form, which never reaches it.
func TestTheFieldSweepUsesTheSymbolsOwnStrategy(t *testing.T) {
	s := newTestServer(t)

	base := s.eng.StrategyConfig()
	own := base
	own.RSIPeriod = base.RSIPeriod + 3

	s.eng.SetSymbolStrategies(map[string]config.StrategyConfig{"AAPL": own})

	if got := s.sweepConfig("AAPL").Strategy.RSIPeriod; got != own.RSIPeriod {
		t.Errorf("the sweep for overridden AAPL used rsi_period %d, want its own %d", got, own.RSIPeriod)
	}

	if got := s.sweepConfig("MSFT").Strategy.RSIPeriod; got != base.RSIPeriod {
		t.Errorf("a non-overridden symbol swept rsi_period %d, want the shared %d", got, base.RSIPeriod)
	}
}
