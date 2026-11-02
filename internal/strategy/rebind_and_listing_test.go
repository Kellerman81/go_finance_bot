package strategy

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
)

// modelStrategy builds a Combined with an ai and an ml detector in every set
// that can hold one — buy, sell and higher-timeframe — so a rebind that
// skipped a set leaves a detector pointing at the live advisor/predictor.
func modelStrategy(t *testing.T) *Combined {
	t.Helper()

	cfg := config.Default().Strategy
	cfg.Detectors = []string{"ema_cross", "ai", "ml"}
	cfg.DetectorsSell = []string{"rsi", "llm", "classifier"}
	cfg.HTFMultiple = 5
	cfg.HTFDetectors = []string{"ollama", "model"}

	c, ok := New(cfg).(*Combined)
	if !ok {
		t.Fatal("New did not return a *Combined")
	}

	if c.htf == nil {
		t.Fatal("setup: no higher-timeframe set")
	}

	return c
}

// everyModelDetector lists the ai and ml detectors across all three sets.
func everyModelDetector(c *Combined) (ais []*aiDetector, mls []*mlDetector) {
	for _, set := range [][]Detector{c.detectors, c.sellDetectors, c.htf.detectors} {
		for _, d := range set {
			switch x := d.(type) {
			case *aiDetector:
				ais = append(ais, x)
			case *mlDetector:
				mls = append(mls, x)
			}
		}
	}

	return ais, mls
}

// TestUseAdvisorAndPredictorRebindEverySet guards ST3-1 and ML2-3: a replay
// rebinds its strategy's ai and ml detectors to its own Fresh advisor and
// predictor. One left on the live default reads TODAY's verdict on a
// historical bar (lookahead) or overwrites the model the live loop trades on —
// and the sell and higher-timeframe sets are the ones easy to forget. A nil
// argument, or a strategy that is not a *Combined, must be a no-op.
func TestUseAdvisorAndPredictorRebindEverySet(t *testing.T) {
	t.Parallel()

	c := modelStrategy(t)

	adv, err := ai.New(config.AIConfig{Enabled: true, Provider: "ollama", Model: "test", BaseURL: "http://127.0.0.1:1"})
	if err != nil || adv == nil {
		t.Fatalf("setup: ai.New = %v, %v", adv, err)
	}

	pred := ml.New(config.MLConfig{Enabled: true}, ai.Params{})

	UseAdvisor(c, adv)
	UsePredictor(c, pred)

	ais, mls := everyModelDetector(c)
	if len(ais) != 3 || len(mls) != 3 {
		t.Fatalf("setup: %d ai and %d ml detectors, want 3 of each", len(ais), len(mls))
	}

	for i, d := range ais {
		if got, _ := d.adv.(*ai.Advisor); got != adv {
			t.Errorf("ai detector %d not rebound to the replay's advisor", i)
		}
	}

	for i, d := range mls {
		if d.pred != pred {
			t.Errorf("ml detector %d not rebound to the replay's predictor", i)
		}
	}

	// nil keeps the binding rather than unhooking the detector.
	UseAdvisor(c, nil)
	UsePredictor(c, nil)

	if got, _ := ais[0].adv.(*ai.Advisor); got != adv || mls[0].pred != pred {
		t.Error("a nil rebind changed the binding")
	}

	// A non-Combined strategy is ignored, not a panic.
	UseAdvisor(nil, adv)
	UsePredictor(nil, pred)
}

// TestCombinedNameDescribesBothSides guards the strategy label the UI,
// trade records and autotune reports carry: when the sell side differs in
// detectors or merge mode the name must say so, or two different strategies
// are reported under one label.
func TestCombinedNameDescribesBothSides(t *testing.T) {
	t.Parallel()

	same := New(config.StrategyConfig{Detectors: []string{"ema_cross", "rsi"}, Combine: "majority"})
	if got := same.Name(); got != "combined[ema_cross,rsi|majority]" {
		t.Errorf("same sides: Name = %q", got)
	}

	split := New(config.StrategyConfig{
		Detectors: []string{"EMA", "rsi"}, DetectorsSell: []string{"macd"},
		Combine: "weighted", CombineSell: "unanimous",
	})

	want := "combined[buy:ema_cross,rsi/sell:macd|buy:weighted/sell:unanimous]"
	if got := split.Name(); got != want {
		t.Errorf("split sides: Name = %q, want %q", got, want)
	}
}

// TestEvaluateAllListsEveryDetector guards the UI's detector matrix: every
// registered detector appears once under its canonical name, the Enabled /
// EnabledSell flags follow the configured sets even through aliases, an
// empty sell set means "same as buy", a short history reports warming up
// rather than voting, and the model detectors abstain without a symbol.
func TestEvaluateAllListsEveryDetector(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Strategy
	cfg.Detectors = []string{"EMA", " bb "}

	res := EvaluateAll(cfg, "", poolCandles(400, 7))

	if len(res) != len(AvailableDetectors()) {
		t.Fatalf("EvaluateAll returned %d results, want %d", len(res), len(AvailableDetectors()))
	}

	byName := map[string]DetectorResult{}
	for _, r := range res {
		if _, dup := byName[r.Name]; dup {
			t.Errorf("detector %q listed twice", r.Name)
		}

		byName[r.Name] = r
	}

	for _, name := range []string{"ema_cross", "bollinger"} {
		if r := byName[name]; !r.Enabled || !r.EnabledSell {
			t.Errorf("%s: Enabled=%v EnabledSell=%v, want both (alias in the buy set, no sell set)",
				name, r.Enabled, r.EnabledSell)
		}
	}

	if r := byName["rsi"]; r.Enabled || r.EnabledSell {
		t.Errorf("rsi flagged enabled but is in neither set: %+v", r)
	}

	for _, name := range []string{"ai", "ml"} {
		if r := byName[name]; r.Action != Hold {
			t.Errorf("%s with no symbol voted %v, want HOLD", name, r.Action)
		}
	}

	cfg.DetectorsSell = []string{"rsi"}

	for _, r := range EvaluateAll(cfg, "", poolCandles(400, 7)) {
		if want := r.Name == "rsi"; r.EnabledSell != want {
			t.Errorf("explicit sell set: %s EnabledSell=%v, want %v", r.Name, r.EnabledSell, want)
		}
	}

	// Too few bars: every detector reports warming up, none votes.
	for _, r := range EvaluateAll(cfg, "", poolCandles(3, 7)) {
		if r.Action != Hold || !strings.HasPrefix(r.Reason, "warming up") {
			t.Errorf("%s on 3 bars: %v %q, want HOLD warming up", r.Name, r.Action, r.Reason)
		}
	}

	// No candles at all must not panic (the series is never acquired).
	if got := EvaluateAll(cfg, "", nil); len(got) != len(AvailableDetectors()) {
		t.Errorf("EvaluateAll(nil candles) returned %d results", len(got))
	}
}

// TestNormalizeWeightsKeysByCanonicalName guards the weighted merge: votes
// are looked up by a detector's canonical Name, so a weight written under
// an alias ("bb", "EMA") must be re-keyed to it — otherwise it is never read
// and the detector silently votes at weight 1.
func TestNormalizeWeightsKeysByCanonicalName(t *testing.T) {
	t.Parallel()

	got := normalizeWeights(map[string]float64{"bb": 2, " EMA ": 0.5, "Mystery": 3}, config.StrategyConfig{})

	want := map[string]float64{"bollinger": 2, "ema_cross": 0.5, "mystery": 3}
	if len(got) != len(want) {
		t.Fatalf("normalizeWeights = %v, want %v", got, want)
	}

	for k, v := range want {
		if got[k] != v {
			t.Errorf("weight[%q] = %v, want %v", k, got[k], v)
		}
	}

	if normalizeWeights(nil, config.StrategyConfig{}) != nil {
		t.Error("no weights did not normalise to nil")
	}
}

// TestModelDetectorsAbstainWithoutContext guards the plain Detect path of
// the two model detectors: it has no symbol and no bar times, so it must
// vote HOLD rather than guess, and both must report a lookback covering
// their whole warmup so the backtester's window does not truncate them.
func TestModelDetectorsAbstainWithoutContext(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Strategy
	cfg.TrendPeriod = 5000

	ad := newAIDetector(cfg)
	if ad.MaxLookback() != 5000 {
		t.Errorf("ai MaxLookback = %d, want the 5000-bar trend window", ad.MaxLookback())
	}

	cfg.TrendPeriod = 2

	if ad2 := newAIDetector(cfg); ad2.MaxLookback() != ad2.WarmupBars() {
		t.Errorf("ai MaxLookback = %d, want the warmup %d when it is longer",
			ad2.MaxLookback(), ad2.WarmupBars())
	}

	md := newMLDetector(config.Default().Strategy)
	if md.Name() != "ml" {
		t.Errorf("ml Name = %q", md.Name())
	}

	if md.WarmupBars() <= 0 || md.MaxLookback() != md.WarmupBars() {
		t.Errorf("ml WarmupBars=%d MaxLookback=%d, want equal and positive", md.WarmupBars(), md.MaxLookback())
	}

	s := acquireSeries(poolCandles(50, 1))
	defer releaseSeries(s)

	if a, _, why := md.Detect(s); a != Hold || !strings.Contains(why, "no symbol") {
		t.Errorf("ml Detect = %v %q, want HOLD no symbol context", a, why)
	}
}
