package config

import "testing"

// CM3-6: alpaca.paper was read only by the startup log line, so a live
// base_url with paper: true logged "paper=true" while real orders went out.
// And use_sim ("no Alpaca calls") was silently ignored once broker: alpaca
// was set explicitly: paper is refused at load, use_sim reported at startup.
func TestAlpacaSwitchesThatPromiseSafetyMustHoldIt(t *testing.T) {
	t.Parallel()

	live := Default()
	live.Broker = "alpaca"
	live.Alpaca.BaseURL = "https://api.alpaca.markets"
	live.Alpaca.Paper = true
	live.Alpaca.UseSim = false

	if err := live.Validate(); err == nil {
		t.Error("paper: true over the live endpoint was accepted")
	}

	sim := Default()
	sim.Broker = "alpaca"
	sim.Alpaca.BaseURL = "https://paper-api.alpaca.markets"
	sim.Alpaca.Paper = true
	sim.Alpaca.UseSim = true // Default()'s own value

	// use_sim with broker: alpaca is reported, not refused: Default() sets it,
	// so refusing broke every broker: alpaca file that never wrote it.
	if err := sim.Validate(); err != nil {
		t.Errorf("the default use_sim refused a broker: alpaca config: %v", err)
	}

	if !sim.AlpacaUseSimIgnored() {
		t.Error("use_sim with broker: alpaca is not reported as ignored")
	}

	ok := Default()
	ok.Broker = "alpaca"
	ok.Alpaca.BaseURL = "https://paper-api.alpaca.markets"
	ok.Alpaca.Paper = true
	ok.Alpaca.UseSim = false

	if err := ok.Validate(); err != nil {
		t.Errorf("a consistent paper setup was refused: %v", err)
	}
}
