package api

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/gin-gonic/gin"
)

// The price chart, drawn on the server as SVG.
//
// This replaces about 400 lines of canvas JavaScript. Canvas is imperative and
// therefore had to live in the browser; SVG is markup, so it can be produced
// here like every other view — and it gains things the canvas version had to
// hand-roll: resolution independence by construction, native <title> tooltips
// instead of a crosshair with hit-testing, and reflow without a resize listener.
//
// Colours come from the same CSS custom properties the rest of the page uses, so
// the chart follows the theme without knowing which theme is active.

// The drawing area, in viewBox units. The plot is stretched to whatever width
// the panel gives it, so these are proportions rather than pixels.
const (
	chartW       = 1000.0
	chartH       = 320.0
	chartPadL    = 58.0
	chartPadR    = 12.0
	chartPadT    = 12.0
	chartPadB    = 22.0
	volumeShare  = 0.18 // of the plot height, given to volume bars
	candleMinGap = 1.2  // viewBox units of space between candle bodies
)

// axisTicks is how many labels each axis carries. It is fixed because the
// template pins them with static CSS classes: a variable count would need inline
// styles, which the Content-Security-Policy forbids.
const axisTicks = 5

// fragChart renders the price chart for the selected symbol and timeframe.
func (s *Server) fragChart(c *gin.Context) {
	symbols := s.eng.Watchlist()

	symbol := strings.ToUpper(strings.TrimSpace(c.Query("symbol")))
	if symbol == "" || !contains(symbols, symbol) {
		if len(symbols) == 0 {
			render(c, "chart", map[string]any{
				"Symbols": symbols, "Resolutions": chartResolutions,
				"BarChoices": chartBarChoices, "Res": "1", "Bars": 240,
				"Caption": translator(langOf(c))("chart.noSymbols"),
			})

			return
		}

		symbol = symbols[0]
	}

	res := c.DefaultQuery("res", "1")
	bars := parseIntDefault(c.Query("bars"), 240)

	candles := s.eng.SeriesResampled(symbol, market.Resolution(res))
	if len(candles) > bars {
		candles = candles[len(candles)-bars:]
	}

	trades := s.chartTrades(symbol, candles)
	t := translator(langOf(c))

	data := map[string]any{
		"Symbols": symbols, "Symbol": symbol,
		"Resolutions": chartResolutions, "Res": res,
		"BarChoices": chartBarChoices, "Bars": bars,
	}

	if len(candles) < 2 {
		data["Caption"] = t("chart.notEnough", "symbol", symbol)

		render(c, "chart", data)

		return
	}

	fills := 0

	for _, tr := range trades {
		if tr.Status == "filled" {
			fills++
		}
	}

	view := renderChart(candles, trades)

	data["SVG"] = view.SVG
	data["PriceTicks"] = view.PriceTicks
	data["TimeTicks"] = view.TimeTicks
	data["Caption"] = t("chart.caption",
		"symbol", symbol, "bars", len(candles),
		"decisions", len(trades), "fills", fills)

	render(c, "chart", data)
}

// chartTrades collects this bot's executions inside the visible window.
func (s *Server) chartTrades(symbol string, candles []market.Candle) []chartTrade {
	if len(candles) == 0 {
		return nil
	}

	from := candles[0].Time

	var out []chartTrade

	for _, t := range s.eng.Trades() {
		if t.Symbol != symbol || t.Price <= 0 || t.Time.Before(from) {
			continue
		}

		out = append(out, chartTrade{
			Time: t.Time, Side: string(t.Side), Price: t.Price,
			Qty: t.Qty, Status: t.Status, Reason: t.Reason,
		})
	}

	return out
}

// chartView is a rendered chart: the plot as SVG, plus the axis labels the
// template positions around it in HTML.
//
// The labels are deliberately NOT in the SVG. The plot is stretched to fill its
// panel (preserveAspectRatio="none"), which is right for the geometry — a chart
// should use the width it is given — but it stretches text with it, and the
// price scale was rendering visibly wide and soft. HTML labels at fixed
// positions are crisp at any width and cost nothing.
type chartView struct {
	SVG        template.HTML
	PriceTicks []string // highest first; pinned to the grid lines
	TimeTicks  []string // left to right, at the same fractions
}

// renderChart draws the candles, volume and trade markers.
//
// The x axis is ORDINAL — bar 0, bar 1, bar 2 — not the timestamp. That is what
// makes the chart continuous: an instrument listed on a real exchange has no
// bars overnight or at the weekend, so scaling x by time left the closed
// sessions as wide empty stretches and squeezed each day's trading into a narrow
// clump. Every trading platform plots by bar index for the same reason. The time
// labels under the plot carry what the gaps were trying to say, and say it
// legibly: a label jumping from 17:29 to 09:01 is a session boundary.
func renderChart(candles []market.Candle, trades []chartTrade) chartView {
	lo, hi := priceRange(candles, trades)
	plotBottom := chartH - chartPadB - (chartH-chartPadT-chartPadB)*volumeShare

	slot := (chartW - chartPadL - chartPadR) / float64(len(candles))

	// Bars sit in the middle of equal slots, so the first and last are drawn
	// whole rather than half-clipped by the frame.
	x := func(i int) float64 { return chartPadL + (float64(i)+0.5)*slot }

	y := func(p float64) float64 {
		return plotBottom - ((p-lo)/(hi-lo))*(plotBottom-chartPadT)
	}

	var b strings.Builder

	b.Grow(len(candles)*220 + len(trades)*160 + 1024)

	fmt.Fprintf(&b,
		`<svg viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img">`,
		chartW, chartH)

	drawSVGGrid(&b, y, lo, hi)
	drawSVGVolume(&b, candles, x, slot, plotBottom)
	drawSVGCandles(&b, candles, x, y, slot)
	drawSVGTrades(&b, candles, trades, x, y)

	b.WriteString(`</svg>`)

	return chartView{
		SVG:        template.HTML(b.String()), //nolint:gosec // numeric or escaped throughout
		PriceTicks: priceTicks(lo, hi),
		TimeTicks:  timeTicks(candles),
	}
}

// priceTicks labels the grid lines, highest first.
func priceTicks(lo, hi float64) []string {
	out := make([]string, axisTicks)
	for i := range axisTicks {
		out[i] = formatNum(hi-(hi-lo)*float64(i)/(axisTicks-1), 2)
	}

	return out
}

// timeTicks labels the ordinal axis with the times of evenly spaced bars.
//
// These are not evenly spaced in time, and that is the point: the gap between
// two labels is how much market time the bars between them covered.
func timeTicks(candles []market.Candle) []string {
	out := make([]string, axisTicks)
	last := len(candles) - 1

	// A window that crosses midnight needs the date; for a venue that closes
	// overnight, that is most windows.
	layout := "15:04"
	if candles[0].Time.YearDay() != candles[last].Time.YearDay() {
		layout = "02.01 15:04"
	}

	for i := range axisTicks {
		out[i] = candles[last*i/(axisTicks-1)].Time.Local().Format(layout)
	}

	return out
}

// priceRange spans the candles and every marked trade, so a fill outside the
// visible range is still on screen rather than clipped silently off the top.
func priceRange(candles []market.Candle, trades []chartTrade) (lo, hi float64) {
	lo, hi = candles[0].Low, candles[0].High

	for _, k := range candles {
		low, high := k.Low, k.High
		if low <= 0 {
			low = k.Close
		}

		if high <= 0 {
			high = k.Close
		}

		lo = min(lo, low)
		hi = max(hi, high)
	}

	for _, t := range trades {
		lo = min(lo, t.Price)
		hi = max(hi, t.Price)
	}

	if hi <= lo {
		hi = lo + 1
	}

	// A hair of headroom so the extremes are not drawn on the frame.
	pad := (hi - lo) * 0.04

	return lo - pad, hi + pad
}

// drawSVGGrid draws the horizontal price lines. Their labels are HTML — see
// chartView.
func drawSVGGrid(b *strings.Builder, y func(float64) float64, lo, hi float64) {
	for i := range axisTicks {
		py := y(lo + (hi-lo)*float64(i)/(axisTicks-1))

		fmt.Fprintf(b,
			`<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`,
			chartPadL, py, chartW-chartPadR, py)
	}
}

func drawSVGVolume(
	b *strings.Builder,
	candles []market.Candle,
	x func(int) float64,
	slot, plotBottom float64,
) {
	var peak float64

	for _, k := range candles {
		peak = max(peak, k.Volume)
	}

	if peak <= 0 {
		return
	}

	height := chartH - chartPadB - plotBottom
	width := max(0.5, slot-candleMinGap)

	for i, k := range candles {
		h := k.Volume / peak * height
		if h <= 0 {
			continue
		}

		fmt.Fprintf(b,
			`<rect class="vol %s" x="%.2f" y="%.2f" width="%.2f" height="%.2f"/>`,
			direction(k), x(i)-width/2, chartH-chartPadB-h, width, h)
	}
}

// drawSVGCandles draws bodies and wicks, or a single close line when the candles
// would be too narrow to read.
func drawSVGCandles(
	b *strings.Builder,
	candles []market.Candle,
	x func(int) float64,
	y func(float64) float64,
	slot float64,
) {
	if slot < 3 {
		drawCloseLine(b, candles, x, y)

		return
	}

	width := max(0.8, slot-candleMinGap)

	for i, k := range candles {
		cx := x(i)
		open, closeP := orClose(k.Open, k.Close), k.Close
		high, low := orClose(k.High, closeP), orClose(k.Low, closeP)

		top := y(max(open, closeP))
		// A doji has no body height; give it a hairline rather than nothing.
		bodyH := max(0.8, y(min(open, closeP))-top)

		fmt.Fprintf(b, `<g class="candle %s"><title>%s</title>`, direction(k),
			template.HTMLEscapeString(fmt.Sprintf("%s  O %s  H %s  L %s  C %s  V %s",
				k.Time.Local().Format("2006-01-02 15:04"),
				formatNum(open, 2), formatNum(high, 2), formatNum(low, 2),
				formatNum(closeP, 2), formatNum(k.Volume, 0))))
		fmt.Fprintf(b, `<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f"/>`,
			cx, y(high), cx, y(low))
		fmt.Fprintf(b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f"/></g>`,
			cx-width/2, top, width, bodyH)
	}
}

// drawCloseLine is the fallback for windows too dense to draw candles in: below
// about three units per bar a candle is a vertical smudge that says less than
// the line it replaces.
func drawCloseLine(
	b *strings.Builder,
	candles []market.Candle,
	x func(int) float64,
	y func(float64) float64,
) {
	var path strings.Builder

	for i, k := range candles {
		verb := "L"
		if i == 0 {
			verb = "M"
		}

		fmt.Fprintf(&path, "%s%.2f %.2f", verb, x(i), y(k.Close))
	}

	rising := candles[len(candles)-1].Close >= candles[0].Close

	fmt.Fprintf(b, `<path class="line %s" d="%s"/>`, upDown(rising), path.String())
}

// drawSVGTrades marks this bot's own executions: filled for real fills, hollow
// for dry-run or blocked decisions.
//
// A trade is placed at the bar it happened in. On an ordinal axis there is no
// position "between" bars, and the bar it fell in is where the fill belongs.
func drawSVGTrades(
	b *strings.Builder,
	candles []market.Candle,
	trades []chartTrade,
	x func(int) float64,
	y func(float64) float64,
) {
	for _, t := range trades {
		cx, cy := x(nearestBar(candles, t.Time)), y(t.Price)
		buy := strings.EqualFold(t.Side, "buy")

		points := fmt.Sprintf("%.2f,%.2f %.2f,%.2f %.2f,%.2f",
			cx, cy+9, cx-6, cy-4, cx+6, cy-4)
		if buy {
			points = fmt.Sprintf("%.2f,%.2f %.2f,%.2f %.2f,%.2f",
				cx, cy-9, cx-6, cy+4, cx+6, cy+4)
		}

		state := "pending"
		if t.Status == "filled" {
			state = "filled"
		}

		fmt.Fprintf(b, `<polygon class="mark %s %s" points="%s"><title>%s</title></polygon>`,
			upDown(buy), state, points,
			template.HTMLEscapeString(fmt.Sprintf("%s %s %s @ %s (%s)%s",
				t.Time.Local().Format("2006-01-02 15:04:05"),
				t.Side, formatNum(t.Qty, 4), formatNum(t.Price, 2), t.Status,
				optional(t.Reason))))
	}
}

// nearestBar returns the index of the bar closest in time to ts.
func nearestBar(candles []market.Candle, ts time.Time) int {
	best, bestGap := 0, time.Duration(math.MaxInt64)

	for i, k := range candles {
		gap := ts.Sub(k.Time)
		if gap < 0 {
			gap = -gap
		}

		if gap < bestGap {
			best, bestGap = i, gap
		}
	}

	return best
}

// orClose falls back to the close when a feed left an OHLC field unset.
func orClose(v, closeP float64) float64 {
	if v <= 0 {
		return closeP
	}

	return v
}

// direction classifies a candle for styling.
func direction(k market.Candle) string {
	return upDown(k.Close >= orClose(k.Open, k.Close))
}

func upDown(up bool) string {
	if up {
		return "up"
	}

	return "down"
}

func optional(reason string) string {
	if reason == "" {
		return ""
	}

	return " — " + reason
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}

	return false
}
