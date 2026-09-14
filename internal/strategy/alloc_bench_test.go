package strategy

import (
	"fmt"
	"strings"
	"testing"
)

// A/B benchmarks for the allocation work.
//
// Each pairs the current implementation against the one it replaced, so the
// claim "this is cheaper" stays checkable instead of living in a commit message.
// Run with:
//
//	go test ./internal/strategy/ -run XXX -bench Alloc -benchmem

// BenchmarkSeriesAllocUnpooled is the old path: one fresh column buffer per
// evaluation.
func BenchmarkSeriesAllocUnpooled(b *testing.B) {
	candles := poolCandles(1000, 1)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		s := toSeries(candles)
		_ = s.close[len(s.close)-1]
	}
}

// BenchmarkSeriesAllocPooled is the current path.
func BenchmarkSeriesAllocPooled(b *testing.B) {
	candles := poolCandles(1000, 1)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		s := acquireSeries(candles)
		_ = s.close[len(s.close)-1]

		releaseSeries(s)
	}
}

// formatVotesLegacy is the previous reason formatter: a Sprintf per vote into a
// []string, then a Join and a final Sprintf.
func formatVotesLegacy(votes []detectorVote, result Action, mode CombineMode) string {
	if len(votes) == 1 {
		return votes[0].reason
	}

	parts := make([]string, len(votes))
	for i, v := range votes {
		parts[i] = fmt.Sprintf("%s:%s(%.2f)", v.name, v.action, v.strength)
	}

	return fmt.Sprintf("%s => %s by %s", strings.Join(parts, ", "), result, mode)
}

func benchVotes() []detectorVote {
	names := []string{
		"ema_cross", "rsi", "macd", "bollinger", "adx",
		"stochastic", "supertrend", "donchian", "trend",
	}
	out := make([]detectorVote, len(names))

	for i, n := range names {
		out[i] = detectorVote{n, Buy, 0.5 + float64(i)/100, n + ": reason text"}
	}

	return out
}

func BenchmarkFormatVotesLegacy(b *testing.B) {
	votes := benchVotes()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = formatVotesLegacy(votes, Buy, Weighted)
	}
}

func BenchmarkFormatVotes(b *testing.B) {
	votes := benchVotes()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = formatVotes(votes, Buy, Weighted)
	}
}

// BenchmarkVotesAllocPooled and its unpooled twin measure the per-side tally.
//
// Both write to a package-level sink. Without it escape analysis keeps the
// unpooled slice on the stack and the comparison flatters it — in Evaluate the
// tally is returned from tallyDetectors and does escape, which is the case worth
// measuring.
var voteSink []detectorVote

func fillVotes(v []detectorVote) []detectorVote {
	for i := range 9 {
		v = append(v, detectorVote{"d", Buy, float64(i), "r"})
	}

	return v
}

func BenchmarkVotesAllocUnpooled(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		voteSink = fillVotes(make([]detectorVote, 0, 9))
	}
}

func BenchmarkVotesAllocPooled(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		voteSink = fillVotes(acquireVotes(9))

		releaseVotes(voteSink)
	}
}
