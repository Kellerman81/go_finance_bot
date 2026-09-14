package screener

import (
	"context"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// fakeProber returns a canned result per symbol and counts how many candidates
// actually reached it — the cutoff is the whole point of the stage.
type fakeProber struct {
	results map[string]Probe
	calls   []string
}

func (f *fakeProber) Probe(_ context.Context, symbol string, _ []market.Candle) (Probe, bool) {
	f.calls = append(f.calls, symbol)

	p, ok := f.results[symbol]

	return p, ok
}

func probeCfg(p config.ScreenerProbeConfig, universe []string) config.ScreenerConfig {
	return config.ScreenerConfig{
		Enabled: true, Universe: universe,
		Filters: []string{"history"}, Probe: p,
	}
}

// The probe's whole purpose: an instrument that passes every tradeability
// filter but loses money when actually traded must not be adopted.
func TestProbeRejectsALosingCandidate(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{
		"WINNER": trending(300, 100, 0.05, 0.01, 50000),
		"LOSER":  trending(300, 100, 0.05, 0.01, 50000),
	}

	prober := &fakeProber{results: map[string]Probe{
		"WINNER": {Source: "backtest", Trades: 40, ExpectancyPct: 0.004, ReturnPct: 12, Score: 12},
		"LOSER":  {Source: "backtest", Trades: 40, ExpectancyPct: -0.003, ReturnPct: -8, Score: -8},
	}}

	s := New(probeCfg(config.ScreenerProbeConfig{
		Enabled: true, Backtest: true, MinExpectancyPct: 0,
	}, []string{"WINNER", "LOSER"}), fakeHistory{series: series}, market.Res1Min,
		Options{Prober: prober})

	res := s.Screen(context.Background(), Inputs{})

	if len(res.Selected) != 1 || res.Selected[0] != "WINNER" {
		t.Fatalf("selected %v, want only the profitable candidate", res.Selected)
	}

	// The rejected candidate keeps a verdict carrying its probe, so an
	// operator can see it was measured rather than silently vanishing.
	for _, v := range res.Verdicts {
		if v.Symbol != "LOSER" {
			continue
		}

		if v.Passed {
			t.Error("LOSER still reads as passed")
		}

		if v.Probe == nil {
			t.Fatal("LOSER lost its probe result")
		}

		if !strings.Contains(v.Reason, "expectancy") {
			t.Errorf("LOSER reason %q does not explain the probe rejection", v.Reason)
		}
	}
}

// A measured result outranks an indicator proxy: the cheap ranks are guesses at
// "would this have traded well", and the probe is the answer.
func TestProbeScoreDrivesTheRanking(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{
		// BIGVOL wins every volume ranking ...
		"BIGVOL": trending(300, 100, 0.05, 0.01, 9_000_000),
		"SMALL":  trending(300, 100, 0.05, 0.01, 1000),
	}

	// ... but trades far worse.
	prober := &fakeProber{results: map[string]Probe{
		"BIGVOL": {Source: "backtest", Trades: 30, ExpectancyPct: 0.0001, Score: 1},
		"SMALL":  {Source: "backtest", Trades: 30, ExpectancyPct: 0.02, Score: 99},
	}}

	cfg := probeCfg(config.ScreenerProbeConfig{
		Enabled: true, Backtest: true, MinExpectancyPct: -1,
	}, []string{"BIGVOL", "SMALL"})
	cfg.Rank = "volume"
	cfg.MaxSymbols = 1

	s := New(cfg, fakeHistory{series: series}, market.Res1Min, Options{Prober: prober})

	res := s.Screen(context.Background(), Inputs{})

	if len(res.Selected) != 1 || res.Selected[0] != "SMALL" {
		t.Fatalf("selected %v, want the better-trading candidate despite lower volume", res.Selected)
	}
}

// A score built on three lucky fills says nothing. The candidate keeps its
// cheap-filter ranking rather than being judged on it — but the probe is still
// recorded, so the operator can see why it was not used.
func TestProbeIgnoresAThinSample(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{"THIN": trending(300, 100, 0.05, 0.01, 50000)}

	prober := &fakeProber{results: map[string]Probe{
		// Terrible expectancy, but only two trades behind it.
		"THIN": {Source: "backtest", Trades: 2, ExpectancyPct: -0.5, Score: -99},
	}}

	s := New(probeCfg(config.ScreenerProbeConfig{
		Enabled: true, Backtest: true, MinTrades: 5, MinExpectancyPct: 0,
	}, []string{"THIN"}), fakeHistory{series: series}, market.Res1Min,
		Options{Prober: prober})

	res := s.Screen(context.Background(), Inputs{})

	if len(res.Selected) != 1 {
		t.Fatalf("selected %v, want the candidate kept on its cheap ranking", res.Selected)
	}

	if res.Verdicts[0].Probe == nil {
		t.Error("the ignored probe was not recorded")
	}

	if !strings.Contains(res.Verdicts[0].Reason, "ignored") {
		t.Errorf("reason %q does not say the probe was ignored", res.Verdicts[0].Reason)
	}
}

// Probing is the expensive stage, so only the best few per class reach it — and
// a candidate beyond the cutoff is kept, not dropped. Not probed is not the
// same as probed badly.
func TestProbeRespectsTheCandidateCutoff(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{}
	results := map[string]Probe{}

	for _, sym := range []string{"A", "B", "C", "D", "E"} {
		// Descending volume, so the cheap ranking order is A > B > C > D > E.
		vol := float64(1_000_000 - 1000*len(series))

		series[sym] = trending(300, 100, 0.05, 0.01, vol)
		results[sym] = Probe{Source: "backtest", Trades: 30, ExpectancyPct: 0.01, Score: 5}
	}

	prober := &fakeProber{results: results}

	cfg := probeCfg(config.ScreenerProbeConfig{
		Enabled: true, Backtest: true, MaxCandidates: 2, MinExpectancyPct: -1,
	}, keysOf(series))
	cfg.Rank = "volume"

	s := New(cfg, fakeHistory{series: series}, market.Res1Min, Options{Prober: prober})

	res := s.Screen(context.Background(), Inputs{})

	if len(prober.calls) != 2 {
		t.Errorf("probed %v, want exactly the top 2", prober.calls)
	}

	if len(res.Selected) != 5 {
		t.Errorf("selected %v, want all five kept (unprobed is not rejected)", res.Selected)
	}
}

// A probe can only ever demote or reject. An instrument that cannot clear its
// own spread does not become tradeable because a backtest looked good.
func TestProbeCannotRescueAFilteredCandidate(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{"CHEAP": synth(300, 1, 0.01, 50000)}

	prober := &fakeProber{results: map[string]Probe{
		"CHEAP": {Source: "backtest", Trades: 99, ExpectancyPct: 0.5, Score: 999},
	}}

	cfg := probeCfg(config.ScreenerProbeConfig{
		Enabled: true, Backtest: true, MinExpectancyPct: 0,
	}, []string{"CHEAP"})
	cfg.Filters = []string{"history", "price"}
	cfg.MinPrice = 50 // CHEAP trades at 1

	s := New(cfg, fakeHistory{series: series}, market.Res1Min, Options{Prober: prober})

	res := s.Screen(context.Background(), Inputs{})

	if len(res.Selected) != 0 {
		t.Errorf("selected %v, want a filtered candidate to stay rejected", res.Selected)
	}

	if len(prober.calls) != 0 {
		t.Errorf("probed %v, want no probe for a candidate that failed the filters", prober.calls)
	}
}

// Probing off, or no prober installed, must leave the pass exactly as it was.
func TestProbeIsInertWhenOffOrAbsent(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{"X": trending(300, 100, 0.05, 0.01, 50000)}
	prober := &fakeProber{results: map[string]Probe{
		"X": {Source: "backtest", Trades: 40, ExpectancyPct: -1, Score: -1},
	}}

	// Configured on, but no prober wired in.
	noProber := New(probeCfg(config.ScreenerProbeConfig{
		Enabled: true, Backtest: true,
	}, []string{"X"}), fakeHistory{series: series}, market.Res1Min, Options{})

	if got := noProber.Screen(context.Background(), Inputs{}).Selected; len(got) != 1 {
		t.Errorf("selected %v with no prober installed, want the candidate kept", got)
	}

	// Prober wired in, but probing switched off.
	off := New(probeCfg(config.ScreenerProbeConfig{Enabled: false}, []string{"X"}),
		fakeHistory{series: series}, market.Res1Min, Options{Prober: prober})

	if got := off.Screen(context.Background(), Inputs{}).Selected; len(got) != 1 {
		t.Errorf("selected %v with probing off, want the candidate kept", got)
	}

	if len(prober.calls) != 0 {
		t.Errorf("probed %v with probing off", prober.calls)
	}
}
