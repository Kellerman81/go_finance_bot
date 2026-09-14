package api

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// sessionCandles builds bars in two trading sessions separated by an overnight
// close — the shape of any real exchange-listed instrument.
func sessionCandles(perSession int) []market.Candle {
	var out []market.Candle

	day := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	price := 100.0

	for session := range 2 {
		start := day.AddDate(0, 0, session)

		for i := range perSession {
			price += 0.01

			out = append(out, market.Candle{
				Time:  start.Add(time.Duration(i) * time.Minute),
				Open:  price,
				High:  price + 0.02,
				Low:   price - 0.02,
				Close: price + 0.01,
				// Volume varies so the volume bars are not all one height.
				Volume: float64(100 + i),
			})
		}
	}

	return out
}

// barCentres extracts the x centre of every candle body from the rendered SVG.
func barCentres(t *testing.T, svg string) []float64 {
	t.Helper()

	rect := regexp.MustCompile(`<rect x="([\d.]+)" y="[\d.]+" width="([\d.]+)"`)

	var out []float64

	for _, m := range rect.FindAllStringSubmatch(svg, -1) {
		x, err1 := strconv.ParseFloat(m[1], 64)
		w, err2 := strconv.ParseFloat(m[2], 64)

		if err1 != nil || err2 != nil {
			t.Fatalf("unparseable rect: %v", m)
		}

		out = append(out, x+w/2)
	}

	return out
}

// The x axis must be ordinal, not time-scaled.
//
// This is the bug it guards: with x taken from the timestamp, the overnight
// close between two sessions rendered as a wide empty stretch and squeezed each
// day's bars into a narrow clump — a chart that looked like it was "missing"
// most of its data. Evenly spaced bars are the fix, and the only way to state it
// is to measure the spacing.
func TestChartAxisIsOrdinalNotTimeScaled(t *testing.T) {
	t.Parallel()

	candles := sessionCandles(30)
	view := renderChart(candles, nil)

	// The candle bodies are the only rects with the y/width shape above; volume
	// bars carry a class, so they are excluded by the pattern.
	centres := barCentres(t, string(view.SVG))
	if len(centres) < len(candles) {
		t.Fatalf("found %d candle bodies for %d bars", len(centres), len(candles))
	}

	centres = centres[len(centres)-len(candles):]

	gaps := make([]float64, 0, len(centres)-1)
	for i := 1; i < len(centres); i++ {
		gaps = append(gaps, centres[i]-centres[i-1])
	}

	first := gaps[0]

	for i, g := range gaps {
		if g <= 0 {
			t.Fatalf("bar %d is not to the right of its predecessor (gap %v)", i+1, g)
		}

		// Every gap must match the first. A time-scaled axis would put a gap of
		// roughly a whole session's width where the overnight close falls.
		if diff := g - first; diff > 0.01 || diff < -0.01 {
			t.Fatalf("bar spacing is not uniform: gap %d is %.2f, first gap is %.2f — "+
				"the axis is scaled by time, so closed sessions leave holes", i+1, g, first)
		}
	}
}

// Bars must sit inside the plot, not half-clipped by the frame.
func TestChartBarsStayInsideThePlot(t *testing.T) {
	t.Parallel()

	candles := sessionCandles(20)
	centres := barCentres(t, string(renderChart(candles, nil).SVG))

	for i, c := range centres {
		if c < chartPadL || c > chartW-chartPadR {
			t.Errorf("bar %d is centred at %.2f, outside the plot [%.0f, %.0f]",
				i, c, chartPadL, chartW-chartPadR)
		}
	}
}

// The axis labels live in HTML, not in the SVG: the plot is stretched to the
// panel width and would stretch its own text with it.
func TestChartLabelsAreNotInTheSVG(t *testing.T) {
	t.Parallel()

	view := renderChart(sessionCandles(20), nil)

	if strings.Contains(string(view.SVG), "<text") {
		t.Error(
			"the SVG contains <text>; stretched by preserveAspectRatio=none it renders wide and soft",
		)
	}

	if len(view.PriceTicks) != axisTicks || len(view.TimeTicks) != axisTicks {
		t.Fatalf("got %d price and %d time labels, want %d of each",
			len(view.PriceTicks), len(view.TimeTicks), axisTicks)
	}

	// Highest price first, so the labels line up with the grid top to bottom.
	first, _ := strconv.ParseFloat(strings.ReplaceAll(view.PriceTicks[0], " ", ""), 64)
	last, _ := strconv.ParseFloat(strings.ReplaceAll(view.PriceTicks[axisTicks-1], " ", ""), 64)

	if first <= last {
		t.Errorf("price labels run %v → %v; they must descend", first, last)
	}

	// A window spanning two days has to name them, or the axis is ambiguous.
	if !strings.Contains(view.TimeTicks[0], ".") {
		t.Errorf("time label %q carries no date, but the window spans two sessions",
			view.TimeTicks[0])
	}
}

// A trade is drawn at the bar it happened in.
func TestChartPlacesTradesOnTheirBar(t *testing.T) {
	t.Parallel()

	candles := sessionCandles(25)
	target := candles[40]

	view := renderChart(candles, []chartTrade{{
		Time: target.Time.Add(20 * time.Second), Side: "buy",
		Price: target.Close, Qty: 1, Status: "filled",
	}})

	poly := regexp.MustCompile(`<polygon class="mark [^"]*" points="([\d.]+),`).
		FindStringSubmatch(string(view.SVG))
	if poly == nil {
		t.Fatal("no trade marker rendered")
	}

	got, _ := strconv.ParseFloat(poly[1], 64)

	slot := (chartW - chartPadL - chartPadR) / float64(len(candles))
	want := chartPadL + (40+0.5)*slot

	if diff := got - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("trade marker at x=%.2f, want %.2f (bar 40)", got, want)
	}
}

// A single-session window labels times without a date; two sessions need one.
func TestChartTimeLabelsAddTheDateOnlyWhenNeeded(t *testing.T) {
	t.Parallel()

	oneDay := sessionCandles(30)[:30]
	if got := timeTicks(oneDay)[0]; strings.Contains(got, ".") {
		t.Errorf("single-session label %q carries a redundant date", got)
	}

	if got := timeTicks(sessionCandles(30))[0]; !strings.Contains(got, ".") {
		t.Errorf("multi-session label %q needs a date", got)
	}
}
