package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/screener"
	"github.com/gin-gonic/gin"
)

func TestScreenerTableRendersNewColumns(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/v2/f/watch", nil)

	render(c, "watchlist", map[string]any{
		"Symbols": []watchRow{}, "Quotes": nil, "CanSearch": false,
		"ScreenedAt": time.Now(),
		"Verdicts": []screener.Verdict{{
			Symbol: "IWDA.AS", Passed: true, Reason: "passed",
			AssetClass: "etf", Sector: "technology", Resolution: "D",
			Price: 92.5, AvgVol: 123456, ATRPct: 1.25, SpreadPct: 0.0012,
			TrendR2: 0.87, MaxDrawdownPct: 0.14, CostMultiple: 4.2,
			Probe: &screener.Probe{
				Source: "backtest", Trades: 31, ReturnPct: 12.5,
				WinRatePct: 58, ExpectancyPct: 0.004,
			},
		}, {
			Symbol: "MYSTERY", Passed: false, Reason: "only 12 bars, need 100",
		}},
	})

	body := w.Body.String()

	for _, want := range []string{
		"etf", "technology", "0.87", "14.00 %", "4.20×",
		"backtest", "31 trades", "12.50 %", "58.00 % win",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}

	// An unmeasured verdict must render as an em-dash, never as a bare 0.00
	// that reads like a real measurement of zero.
	if strings.Count(body, "—") < 6 {
		t.Errorf("unmeasured fields did not render as em-dashes:\n%s", body)
	}

	for _, bad := range []string{"{source}", "{win}", "{exp}", "{trades}"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains an unrendered placeholder %q", bad)
		}
	}
}

// The watchlist row carries the buckets a cap would refuse an order on, and
// the loss streak counting toward an automatic re-tune.
func TestWatchlistRowTags(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/v2/f/watch", nil)

	render(c, "watchlist", map[string]any{
		"Quotes": nil, "CanSearch": false, "Verdicts": nil,
		"Symbols": []watchRow{
			{Symbol: "IWDA.AS", AssetClass: "etf", Sector: "technology",
				LossStreak: 2, LossStreakOf: 3},
			// Trigger off: a streak must not be advertised as counting toward
			// something that will never fire.
			{Symbol: "QUIET", AssetClass: "stock", LossStreak: 5, LossStreakOf: 0},
		},
	})

	body := w.Body.String()

	for _, want := range []string{"etf", "technology", "2/3 losses", "stock"} {
		if !strings.Contains(body, want) {
			t.Errorf("watchlist row is missing %q", want)
		}
	}

	if strings.Contains(body, "5/0") || strings.Contains(body, "5/") {
		t.Error("a streak was advertised with the on_loss trigger off")
	}

	for _, bad := range []string{"{n}", "{of}"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains an unrendered placeholder %q", bad)
		}
	}
}
