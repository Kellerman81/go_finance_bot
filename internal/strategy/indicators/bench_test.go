package indicators

import "testing"

// Benchmarks contrasting the O(period) tail-only helpers against computing the
// full O(n·period) series just to read its last value, over a realistic backtest
// window.
func BenchmarkBollingerFullVsLast(b *testing.B) {
	_, _, close, _ := randSeries(1200)
	b.Run("full+Last", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, up, _ := BollingerBands(close, 20, 2)
			_, _ = Last(up)
		}
	})
	b.Run("Last", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _, _, _ = BollingerLast(close, 20, 2)
		}
	})
}

func BenchmarkMACDFullVsLast(b *testing.B) {
	_, _, close, _ := randSeries(1200)
	b.Run("full+Last", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = Last(MACD(close, 12, 26, 9).Histogram)
		}
	})
	b.Run("Last", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _, _, _ = MACDLast(close, 12, 26, 9)
		}
	})
}

func BenchmarkVWAPFullVsLast(b *testing.B) {
	high, low, close, vol := randSeries(1200)
	b.Run("full+Last", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = Last(VWAP(high, low, close, vol, 20))
		}
	})
	b.Run("Last", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = VWAPLast(high, low, close, vol, 20)
		}
	})
}
