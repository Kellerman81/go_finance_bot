package indicators

import (
	"math"
	"math/rand"
	"testing"
)

// randSeries builds pseudo-random OHLCV data for equivalence tests.
func randSeries(n int) (high, low, close, volume []float64) {
	r := rand.New(rand.NewSource(42))
	high = make([]float64, n)
	low = make([]float64, n)
	close = make([]float64, n)
	volume = make([]float64, n)
	price := 100.0
	for i := 0; i < n; i++ {
		price += r.NormFloat64()
		if price < 1 {
			price = 1
		}
		spread := 0.5 + r.Float64()
		high[i] = price + spread
		low[i] = price - spread
		close[i] = price
		volume[i] = 1000 + r.Float64()*5000
	}
	return high, low, close, volume
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.12f, want %.12f (Last of full series)", name, got, want)
	}
}

// The *Last helpers must return exactly what Last(FullSeries(...)) returns.
func TestLastVariantsMatchFullSeries(t *testing.T) {
	high, low, close, volume := randSeries(300)

	if got, ok := EMALast(close, 26); !ok {
		t.Fatal("EMALast not ok")
	} else {
		approx(t, "EMALast", got, lastVal(t, EMA(close, 26)))
	}
	if got, ok := RSILast(close, 14); !ok {
		t.Fatal("RSILast not ok")
	} else {
		approx(t, "RSILast", got, lastVal(t, RSI(close, 14)))
	}
	if got, ok := ATRLast(high, low, close, 14); !ok {
		t.Fatal("ATRLast not ok")
	} else {
		approx(t, "ATRLast", got, lastVal(t, ATR(high, low, close, 14)))
	}
	if m, u, l, ok := BollingerLast(close, 20, 2); !ok {
		t.Fatal("BollingerLast not ok")
	} else {
		mid, up, lo := BollingerBands(close, 20, 2)
		approx(t, "BollingerLast mid", m, lastVal(t, mid))
		approx(t, "BollingerLast upper", u, lastVal(t, up))
		approx(t, "BollingerLast lower", l, lastVal(t, lo))
	}
	if got, ok := VWAPLast(high, low, close, volume, 20); !ok {
		t.Fatal("VWAPLast not ok")
	} else {
		approx(t, "VWAPLast", got, lastVal(t, VWAP(high, low, close, volume, 20)))
	}
	if got, ok := RVOLLast(volume, 20); !ok {
		t.Fatal("RVOLLast not ok")
	} else {
		approx(t, "RVOLLast", got, lastVal(t, RVOL(volume, 20)))
	}
}

// MACDLast must return exactly what Last() of each MACD(...) series returns,
// including when fast > slow and when there are too few bars.
func TestMACDLastMatchesFullSeries(t *testing.T) {
	_, _, close, _ := randSeries(300)
	sets := [][3]int{{12, 26, 9}, {5, 35, 5}, {8, 21, 5}, {26, 12, 9}}
	for _, s := range sets {
		m, sig, hist, ok := MACDLast(close, s[0], s[1], s[2])
		if !ok {
			t.Fatalf("MACDLast(%v) not ok", s)
		}
		full := MACD(close, s[0], s[1], s[2])
		approx(t, "MACDLast macd", m, lastVal(t, full.MACD))
		approx(t, "MACDLast signal", sig, lastVal(t, full.Signal))
		approx(t, "MACDLast hist", hist, lastVal(t, full.Histogram))
	}
	// 12/26/9 needs 25+9=34 bars for a defined histogram.
	if _, _, _, ok := MACDLast(close[:33], 12, 26, 9); ok {
		t.Error("MACDLast ok with 33 bars, want undefined (needs 34)")
	}
	if _, _, _, ok := MACDLast(close[:34], 12, 26, 9); !ok {
		t.Error("MACDLast not ok with 34 bars, want defined")
	}
}

func TestADXTrendStrength(t *testing.T) {
	n := 120
	high := make([]float64, n)
	low := make([]float64, n)
	cl := make([]float64, n)
	for i := 0; i < n; i++ {
		cl[i] = 100 + float64(i) // clean, steady uptrend
		high[i] = cl[i] + 0.5
		low[i] = cl[i] - 0.5
	}
	adx, plus, minus, ok := ADXLast(high, low, cl, 14)
	if !ok {
		t.Fatal("ADXLast not ok")
	}
	if adx < 40 {
		t.Errorf("clean uptrend ADX = %.1f, want strong (>40)", adx)
	}
	if plus <= minus {
		t.Errorf("uptrend +DI %.1f should exceed -DI %.1f", plus, minus)
	}
}

func TestStochasticExtremes(t *testing.T) {
	n := 30
	high := make([]float64, n)
	low := make([]float64, n)
	cl := make([]float64, n)
	for i := 0; i < n; i++ {
		high[i], low[i] = 110, 90
		cl[i] = 100
	}
	cl[n-1] = 110 // closes at the top of the range => %K ~ 100
	k, _, ok := StochasticLast(high, low, cl, 14, 3)
	if !ok {
		t.Fatal("StochasticLast not ok")
	}
	if k < 99 {
		t.Errorf("close at range high => %%K = %.1f, want ~100", k)
	}
}

func TestDonchianBreakout(t *testing.T) {
	n := 25
	high := make([]float64, n)
	low := make([]float64, n)
	for i := 0; i < n; i++ {
		high[i], low[i] = 105, 95
	}
	high[n-1] = 120 // last bar irrelevant to prior channel
	up, lo, ok := DonchianLast(high, low, 20)
	if !ok {
		t.Fatal("DonchianLast not ok")
	}
	if math.Abs(up-105) > 1e-9 || math.Abs(lo-95) > 1e-9 {
		t.Errorf("prior channel = [%.2f,%.2f], want [95,105] (excludes last bar)", lo, up)
	}
}

func TestSupertrendDirection(t *testing.T) {
	n := 60
	high := make([]float64, n)
	low := make([]float64, n)
	cl := make([]float64, n)
	for i := 0; i < n; i++ {
		cl[i] = 100 + 2*float64(i)
		high[i] = cl[i] + 1
		low[i] = cl[i] - 1
	}
	dir, level, ok := SupertrendLast(high, low, cl, 14, 3)
	if !ok {
		t.Fatal("SupertrendLast not ok")
	}
	if dir != 1 {
		t.Errorf("steady uptrend Supertrend dir = %d, want +1", dir)
	}
	if level >= cl[n-1] {
		t.Errorf("up-trend level %.2f should sit below price %.2f", level, cl[n-1])
	}
}
