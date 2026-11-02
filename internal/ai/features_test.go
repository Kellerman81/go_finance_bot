package ai

import (
	"strings"
	"testing"
)

// series builds n bars of a steadily rising instrument, enough to define every
// indicator in the snapshot.
func series(n int) (high, low, closes, volume []float64) {
	price := 100.0

	for range n {
		price *= 1.002

		high = append(high, price*1.003)
		low = append(low, price*0.997)
		closes = append(closes, price)
		volume = append(volume, 1000)
	}

	return high, low, closes, volume
}

// TestBuildFeaturesPopulatesReadings checks a full-history snapshot defines the
// readings the prompt relies on.
func TestBuildFeaturesPopulatesReadings(t *testing.T) {
	t.Parallel()

	h, l, c, v := series(300)

	f, ok := BuildFeatures("AAPL", h, l, c, v, Params{}.WithDefaults())
	if !ok {
		t.Fatal("BuildFeatures: not ok on 300 bars")
	}

	if f.Symbol != "AAPL" || f.Price != c[len(c)-1] {
		t.Errorf("symbol/price = %q/%v", f.Symbol, f.Price)
	}

	for name, defined := range map[string]bool{
		"ema": f.emaOK, "rsi": f.rsiOK, "macd": f.macdOK, "bb": f.bbOK,
		"atr": f.atrOK, "adx": f.adxOK, "stoch": f.stochOK, "rvol": f.rvolOK,
		"range": f.rangeOK, "trend": f.trendOK,
	} {
		if !defined {
			t.Errorf("%s reading undefined on 300 bars", name)
		}
	}

	if f.Ret5 <= 0 || f.EMASpreadPct <= 0 || f.TrendPct <= 0 {
		t.Errorf("a steadily rising series should show positive returns/spread/trend: %+v", f)
	}
}

// TestBuildFeaturesRejectsEmpty checks the caller is told not to consult the
// model when there is nothing to describe.
func TestBuildFeaturesRejectsEmpty(t *testing.T) {
	t.Parallel()

	p := Params{}.WithDefaults()

	if _, ok := BuildFeatures("X", nil, nil, nil, nil, p); ok {
		t.Error("BuildFeatures(empty) = ok, want not ok")
	}

	if _, ok := BuildFeatures("X", []float64{1}, []float64{1}, []float64{0}, []float64{1}, p); ok {
		t.Error("BuildFeatures(zero price) = ok, want not ok")
	}
}

// TestPromptOmitsUndefinedReadings checks a short history produces a prompt
// without the readings that could not be computed — a model must never be shown
// a zero standing in for missing data.
func TestPromptOmitsUndefinedReadings(t *testing.T) {
	t.Parallel()

	h, l, c, v := series(5)

	f, ok := BuildFeatures("AAPL", h, l, c, v, Params{}.WithDefaults())
	if !ok {
		t.Fatal("BuildFeatures: not ok")
	}

	prompt := f.Prompt()
	if strings.Contains(prompt, "adx") || strings.Contains(prompt, "rsi") {
		t.Errorf("prompt shows undefined readings:\n%s", prompt)
	}

	if !strings.Contains(prompt, "symbol: AAPL") || !strings.Contains(prompt, "price:") {
		t.Errorf("prompt is missing the basics:\n%s", prompt)
	}
}

// TestPromptIsLabelledLines checks the prompt is the compact key: value form the
// system prompt tells the model to expect.
func TestPromptIsLabelledLines(t *testing.T) {
	t.Parallel()

	h, l, c, v := series(300)
	f, _ := BuildFeatures("AAPL", h, l, c, v, Params{}.WithDefaults())

	for _, line := range strings.Split(strings.TrimSpace(f.Prompt()), "\n") {
		if !strings.Contains(line, ": ") {
			t.Errorf("line %q is not a key: value pair", line)
		}
	}
}

// TestFingerprintTracksMeaningfulChange checks the refresh gate: an unchanged
// picture reuses the cached verdict, a materially different one does not.
func TestFingerprintTracksMeaningfulChange(t *testing.T) {
	t.Parallel()

	base := Features{Ret5: 1.0, RSI: 55, EMASpreadPct: 0.5, BBPos: 0.5, ADX: 30, RangePos: 0.5}

	same := base
	same.RSI = 55.4 // within the rounding step

	if base.Fingerprint() != same.Fingerprint() {
		t.Error("a negligible RSI change should not force a new model call")
	}

	moved := base
	moved.RSI = 72

	if base.Fingerprint() == moved.Fingerprint() {
		t.Error("a large RSI change should force a new model call")
	}
}

// TestParamsWithDefaults checks a zero Params is usable, and that explicit
// values survive.
func TestParamsWithDefaults(t *testing.T) {
	t.Parallel()

	p := Params{}.WithDefaults()
	if p.RSIPeriod == 0 || p.SlowMA == 0 || p.BBStdDev == 0 {
		t.Errorf("zero Params not filled in: %+v", p)
	}

	if got := (Params{RSIPeriod: 7}).WithDefaults().RSIPeriod; got != 7 {
		t.Errorf("RSIPeriod = %d, want the configured 7", got)
	}

	if p.WarmupBars() < p.SlowMA {
		t.Errorf("WarmupBars %d < slow MA %d", p.WarmupBars(), p.SlowMA)
	}
}

// TestTrendFeatureOfAConvexRallyIsPositive is ST2-2's copy in the feature
// vector the ml and ai detectors share: a fitted intercept far below zero was
// accepted as the denominator, flipping the sign, so a long flat stretch and a
// steep breakout fed the model a large NEGATIVE TrendPct.
func TestTrendFeatureOfAConvexRallyIsPositive(t *testing.T) {
	t.Parallel()

	p := Params{}.WithDefaults()

	n := p.WarmupBars() + 100
	closes := make([]float64, n)

	for i := range closes {
		closes[i] = 1
		if i >= n-20 {
			closes[i] = 1 + float64(i-(n-20)+1)
		}
	}

	high, low, vol := make([]float64, n), make([]float64, n), make([]float64, n)
	for i, c := range closes {
		high[i], low[i], vol[i] = c*1.001, c*0.999, 1000
	}

	p.TrendPeriod = n

	f, ok := BuildFeatures("X", high, low, closes, vol, p)
	if !ok || !f.trendOK {
		t.Fatal("trend feature undefined")
	}

	if f.TrendPct <= 0 {
		t.Errorf("TrendPct = %.2f for a rally from 1 to 21, want positive", f.TrendPct)
	}
}
