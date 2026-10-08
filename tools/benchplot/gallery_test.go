package main

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func fmtCount(v float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmtDur(v), " ns"), ".00")
}

// A chart that is not well-formed XML renders as a broken image in the README
// and nothing else notices, because the file is only ever compared to itself.
func wellFormed(t *testing.T, svg string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(svg))
	for {
		if _, err := d.Token(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("not well-formed XML: %v", err)
		}
	}
}

func countBars(svg string) int {
	// Bars are the only rects with rounded corners of 2; the card is rx=6 and
	// the legend swatches are rx=2 too, so subtract those separately.
	return strings.Count(svg, `rx="2"/>`)
}

func TestBarChartDrawsOneBarPerPresentValue(t *testing.T) {
	groups := []string{"a", "b", "c"}
	ss := []barSeries{
		{"before", colBefore, []float64{100, 200, nan()}},
		{"after", colGus, []float64{10, 20, 30}},
	}
	svg := barChart("t", "s", "y", groups, ss, true, fmtDur)
	wellFormed(t, svg)
	// 5 bars + 2 legend swatches. The NaN slot must be empty, not a zero-height
	// bar and not a bar at the axis: a missing "before" is not a measurement.
	if got, want := countBars(svg), 5+2; got != want {
		t.Errorf("rx=2 rects = %d, want %d (5 bars + 2 legend swatches)", got, want)
	}
	if strings.Contains(svg, "NaN") {
		t.Error("a NaN leaked into the SVG")
	}
}

func TestBarChartLinearAxisStartsAtZero(t *testing.T) {
	svg := barChart("t", "s", "y", []string{"a", "b"},
		[]barSeries{{"x", colRaw, []float64{157, 41}}}, false, func(v float64) string { return fmtCount(v) })
	wellFormed(t, svg)
	// A count axis that does not start at zero exaggerates the gap it is there
	// to show; the bottom tick must be the zero label.
	if !strings.Contains(svg, ">0</text>") && !strings.Contains(svg, ">0.00</text>") {
		t.Error("linear axis has no zero tick")
	}
}

func TestNiceStep(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{1, 1}, {1.1, 2}, {3.5, 5}, {5.1, 10}, {42, 50}, {0.07, 0.1},
	} {
		if got := niceStep(tc.in); got != tc.want {
			t.Errorf("niceStep(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
