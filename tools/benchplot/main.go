// Command benchplot renders the adoption charts in docs/ from committed
// benchmark output.
//
// Same contract as tools/benchdoc, for the same reason: a chart is a
// performance claim, and a hand-drawn one is a claim with nothing behind it.
// Every number here is read out of bench/results/, so a chart cannot drift from
// the data it describes, and `-check` fails CI when the committed SVGs no
// longer match the committed measurements.
//
//	go run ./tools/benchplot          # regenerate docs/img/*.svg
//	go run ./tools/benchplot -check   # fail if they are stale
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
	"github.com/bharathvbcr/gusset/tools/internal/docblock"
)

const (
	resultsDir = "bench/results/crossover"
	outDir     = "docs/img"

	// The adoption guide's measured tables, generated from the same medians as
	// the charts that sit beside them. tools/internal/docblock owns the marker
	// mechanics, shared with tools/benchdoc.
	docPath      = "docs/choosing.md"
	docBlockName = "BENCHPLOT"
)

// median of one metric across a benchmark's repetitions.
//
// Median, not mean: these runs include thread-creation and scheduler effects
// with long tails, and one unlucky repetition should not move the chart.
func median(samples []benchfile.Sample, unit string) (float64, bool) {
	var vs []float64
	for _, s := range samples {
		if v, ok := s[unit]; ok {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return 0, false
	}
	sort.Float64s(vs)
	n := len(vs)
	if n%2 == 1 {
		return vs[n/2], true
	}
	return (vs[n/2-1] + vs[n/2]) / 2, true
}

func mustMedian(m map[string][]benchfile.Sample, name, unit string) float64 {
	s, ok := m[name]
	if !ok {
		fatal(fmt.Errorf("benchmark %q not present in the committed results; "+
			"run `make bench-crossover` and `make bench-scaling`", name))
	}
	v, ok := median(s, unit)
	if !ok {
		fatal(fmt.Errorf("benchmark %q has no %q metric", name, unit))
	}
	return v
}

// ---------------------------------------------------------------------------
// Minimal SVG chart primitives.
//
// Hand-rolled rather than pulling in a plotting library: this is three charts,
// and R-whatever-number-we-are-on says a new dependency needs a reason. An
// explicit light card background rather than a transparent one, because these
// render in Markdown on both light and dark themes and a transparent chart with
// dark axes disappears on one of them.
// ---------------------------------------------------------------------------

type chart struct {
	w, h                   int
	padL, padR, padT, padB int
	b                      strings.Builder
}

const (
	colText  = "#1f2328"
	colMuted = "#57606a"
	colGrid  = "#d8dee4"
	// Transport colours, used consistently wherever a chart compares the two.
	colRaw = "#cf222e" // blocking cgo
	colGus = "#0969da" // Gusset
	// Load-shape colours, for the crossover chart. Deliberately *not* the
	// transport colours: both of its lines are Gusset-over-cgo ratios, so
	// reusing red and blue there would have the same two colours meaning
	// "transport" on one chart and "load shape" on another.
	colSerial = "#bc4c00"
	colPar    = "#1a7f37"
	colMark   = "#8250df"
)

func newChart(w, h int, title, sub string) *chart {
	c := &chart{w: w, h: h, padL: 78, padR: 34, padT: 58, padB: 52}
	fmt.Fprintf(&c.b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="-apple-system,Segoe UI,Helvetica,Arial,sans-serif">`, w, h, w, h)
	fmt.Fprintf(&c.b, `<rect width="%d" height="%d" fill="#ffffff" rx="6"/>`, w, h)
	fmt.Fprintf(&c.b, `<text x="%d" y="24" font-size="15" font-weight="600" fill="%s">%s</text>`, c.padL-48, colText, esc(title))
	fmt.Fprintf(&c.b, `<text x="%d" y="42" font-size="11.5" fill="%s">%s</text>`, c.padL-48, colMuted, esc(sub))
	return c
}

func (c *chart) plotW() int { return c.w - c.padL - c.padR }
func (c *chart) plotH() int { return c.h - c.padT - c.padB }

// xy maps a unit-square point (0..1) into plot pixels, y inverted.
func (c *chart) xy(fx, fy float64) (float64, float64) {
	return float64(c.padL) + fx*float64(c.plotW()),
		float64(c.padT) + (1-fy)*float64(c.plotH())
}

func (c *chart) gridY(fy float64, label string, emphasis bool) {
	x0, y := c.xy(0, fy)
	x1, _ := c.xy(1, fy)
	stroke, dash := colGrid, `stroke-dasharray="3 3"`
	if emphasis {
		stroke, dash = colMark, `stroke-dasharray="6 4"`
	}
	fmt.Fprintf(&c.b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1" %s/>`, x0, y, x1, y, stroke, dash)
	fill := colMuted
	if emphasis {
		fill = colMark
	}
	fmt.Fprintf(&c.b, `<text x="%.1f" y="%.1f" font-size="11" text-anchor="end" fill="%s">%s</text>`, x0-8, y+4, fill, esc(label))
}

func (c *chart) xTick(fx float64, label string) {
	x, y := c.xy(fx, 0)
	fmt.Fprintf(&c.b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x, y, x, y+5, colGrid)
	fmt.Fprintf(&c.b, `<text x="%.1f" y="%.1f" font-size="11" text-anchor="middle" fill="%s">%s</text>`, x, y+19, colMuted, esc(label))
}

func (c *chart) axes(xLabel, yLabel string) {
	x0, y0 := c.xy(0, 0)
	x1, _ := c.xy(1, 0)
	_, y1 := c.xy(0, 1)
	fmt.Fprintf(&c.b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.3"/>`, x0, y0, x1, y0, colMuted)
	fmt.Fprintf(&c.b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.3"/>`, x0, y0, x0, y1, colMuted)
	fmt.Fprintf(&c.b, `<text x="%.1f" y="%d" font-size="11.5" text-anchor="middle" fill="%s">%s</text>`,
		(x0+x1)/2, c.h-8, colMuted, esc(xLabel))
	fmt.Fprintf(&c.b, `<text transform="translate(16,%.1f) rotate(-90)" font-size="11.5" text-anchor="middle" fill="%s">%s</text>`,
		(y0+y1)/2, colMuted, esc(yLabel))
}

func (c *chart) series(pts [][2]float64, col string) {
	var d strings.Builder
	for i, p := range pts {
		x, y := c.xy(p[0], p[1])
		if i == 0 {
			fmt.Fprintf(&d, "M%.1f %.1f", x, y)
		} else {
			fmt.Fprintf(&d, " L%.1f %.1f", x, y)
		}
	}
	fmt.Fprintf(&c.b, `<path d="%s" fill="none" stroke="%s" stroke-width="2.4" stroke-linejoin="round"/>`, d.String(), col)
	for _, p := range pts {
		x, y := c.xy(p[0], p[1])
		fmt.Fprintf(&c.b, `<circle cx="%.1f" cy="%.1f" r="3.6" fill="%s"/>`, x, y, col)
	}
}

// legend draws a key inside the top-right of the plot area.
//
// Replaces labelling the last point of each line: at the right-hand edge the
// two series converge, so end labels landed on top of each other exactly where
// the chart is most interesting.
func (c *chart) legend(entries [][2]string) {
	x, y := c.xy(1, 1)
	boxW := 0.0
	for _, e := range entries {
		if w := 13 + float64(len(e[1]))*6.4; w > boxW {
			boxW = w
		}
	}
	boxH := float64(len(entries))*18 + 10
	bx, by := x-boxW-10, y+6
	fmt.Fprintf(&c.b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="#ffffff" fill-opacity="0.92" stroke="%s" rx="4"/>`,
		bx, by, boxW, boxH, colGrid)
	for i, e := range entries {
		ly := by + 17 + float64(i)*18
		fmt.Fprintf(&c.b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="2.6"/>`,
			bx+7, ly-4, bx+21, ly-4, e[0])
		fmt.Fprintf(&c.b, `<text x="%.1f" y="%.1f" font-size="11.5" fill="%s">%s</text>`, bx+27, ly, colText, esc(e[1]))
	}
}

// note writes an annotation at unit-square coordinates.
func (c *chart) note(fx, fy float64, text, col string) {
	x, y := c.xy(fx, fy)
	fmt.Fprintf(&c.b, `<text x="%.1f" y="%.1f" font-size="11" fill="%s">%s</text>`, x, y, col, esc(text))
}

func (c *chart) pointLabel(fx, fy float64, text, col string, dy float64) {
	x, y := c.xy(fx, fy)
	fmt.Fprintf(&c.b, `<text x="%.1f" y="%.1f" font-size="10.5" text-anchor="middle" fill="%s">%s</text>`, x, y+dy, col, esc(text))
}

func (c *chart) done() string { c.b.WriteString("</svg>"); return c.b.String() }

func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// Charts
// ---------------------------------------------------------------------------

// workPoint is one work size in the crossover sweep.
//
// Identified by iteration count, not by the name fragment. Each sub-benchmark
// calibrates its own wall-clock label on the machine that runs it, so the
// serial and parallel arms of the same work size carry different durations in
// their names ("1000it-708ns" against "1000it-750ns"). The iteration count is
// the thing that is actually equal on both sides.
type workPoint struct {
	iters int
	label string
}

// findByIters resolves the benchmark under prefix that ran this iteration
// count, whatever duration its name was calibrated to.
func findByIters(m map[string][]benchfile.Sample, prefix string, iters int) string {
	if iters == 0 {
		if _, ok := m[prefix+"noop"]; ok {
			return prefix + "noop"
		}
		return ""
	}
	want := strconv.Itoa(iters) + "it-"
	for name := range m {
		if strings.HasPrefix(name, prefix) && strings.HasPrefix(strings.TrimPrefix(name, prefix), want) {
			return name
		}
	}
	return ""
}

func medianByIters(m map[string][]benchfile.Sample, prefix string, iters int, unit string) float64 {
	name := findByIters(m, prefix, iters)
	if name == "" {
		fatal(fmt.Errorf("no benchmark under %q for %d iterations in the committed results; "+
			"run `make bench-crossover`", prefix, iters))
	}
	return mustMedian(m, name, unit)
}

// serialFloor is how far below 1.0 the serial Gusset ÷ blocking-cgo ratio may
// fall before the run is treated as contaminated rather than surprising.
//
// Serial Gusset runs the identical Rust loop *plus* a semaphore, a submit, a
// worker dispatch, a pipe write and a netpoller wake, so the ratio is bounded
// below by roughly 1. Some slack is real — the Rust worker and the parked caller
// can land on differently boosted cores — but a serial ratio of 0.26 is not a
// discovery about the transport.
const serialFloor = 0.8

// checkTransportOrdering refuses to chart a run in which serial blocking cgo
// came out slower than serial Gusset on the same work.
//
// The arms run as separate processes, which is what makes the thread numbers
// honest and also what lets an unrelated load spike land on one arm and not the
// other. That failure is invisible in the raw file: every sample in the affected
// arm is uniformly wrong, so the spread stays tight and the median looks
// authoritative. A spread check cannot catch it; comparing the arms against an
// ordering the transport guarantees can.
//
// Observed, which is why this exists: a regeneration run that overlapped an
// unrelated fuzzer reported blocking cgo at 2.98 ms against Gusset's 764 µs for
// the same loop — a chart showing Gusset 3.9x *faster* than the call it wraps.
func checkTransportOrdering(m map[string][]benchfile.Sample, works []workPoint) error {
	for _, w := range works {
		sr := medianByIters(m, "BenchmarkCrossoverSerial/RawCgo/", w.iters, "ns/op")
		sg := medianByIters(m, "BenchmarkCrossoverSerial/Gusset/", w.iters, "ns/op")
		ratio := sg / sr
		if ratio < serialFloor {
			return fmt.Errorf(
				"serial Gusset measured %.2fx blocking cgo at %s (%.0f ns vs %.0f ns), which the "+
					"transport cannot do: Gusset runs the same Rust loop plus the boundary. The "+
					"blocking-cgo arm was measured under load the Gusset arm did not see; re-run "+
					"`make bench-crossover` on an idle machine rather than charting this",
				ratio, w.label, sg, sr)
		}
	}
	return nil
}

// calibrationTolerance is how far two arms' calibrations of the identical Rust
// loop may differ before the run is treated as contaminated.
//
// Each benchmark name carries a live measurement: bench/seed/go's calibrate()
// times rs_spin at that iteration count and puts the result in the name, so
// BenchmarkCrossoverSerial/RawCgo/1000000it-709us is an arm reporting that a
// million iterations took 709 µs on the machine it ran on. It takes the *minimum
// of five* attempts, which is what makes the signal strong: transient noise is
// discarded by construction, so a minimum that is 20% high means the machine was
// saturated through all five.
//
// 10% is wide enough for thermal drift between arms recorded minutes apart and
// narrow enough to catch what was observed: a clean machine calibrated 728 µs
// where a contaminated arm called the same loop 875 µs.
const calibrationTolerance = 0.10

// checkCalibrationAgreement refuses a dataset whose arms disagree about how long
// an identical, deterministic Rust loop takes.
//
// This is the check that was staring at the previous version of this file and
// got worked around instead of read. findByIters and medianByIters exist
// precisely because the arms' names disagreed — they resolve a benchmark by
// iteration count "whatever duration its name was calibrated to". That
// divergence was not an inconvenience to route around. It was the contamination
// announcing itself in the data's own filenames.
func checkCalibrationAgreement(m map[string][]benchfile.Sample) error {
	// iters -> arm -> the interval the name pins the calibration to.
	byIters := map[int]map[string]calibration{}
	for name := range m {
		iters, c, ok := calibratedFromName(name)
		if !ok || iters == 0 {
			continue
		}
		if byIters[iters] == nil {
			byIters[iters] = map[string]calibration{}
		}
		byIters[iters][benchfile.Arm(name)] = c
	}

	iterList := make([]int, 0, len(byIters))
	for it := range byIters {
		iterList = append(iterList, it)
	}
	sort.Ints(iterList)

	for _, it := range iterList {
		arms := byIters[it]
		if len(arms) < 2 {
			continue
		}
		names := make([]string, 0, len(arms))
		for a := range arms {
			names = append(names, a)
		}
		sort.Strings(names)

		// Each name pins its arm's calibration to an interval, not a point:
		// label() formats with Duration.Microseconds(), which truncates, so
		// "10000it-6us" means [6, 7) µs. The arms are consistent when their
		// intervals share any value — and at small sizes they often differ by a
		// whole unit for no reason but rounding. Comparing the printed numbers
		// instead of the intervals makes an identical 6.9 µs loop look like a
		// 17% disagreement, which is a check that fails honest data.
		loArm, hiArm := names[0], names[0]
		for _, a := range names {
			if arms[a].lo > arms[loArm].lo {
				loArm = a // highest lower bound
			}
			if arms[a].hi < arms[hiArm].hi {
				hiArm = a // lowest upper bound
			}
		}
		loMax, hiMin := arms[loArm].lo, arms[hiArm].hi
		if loMax <= hiMin || hiMin <= 0 {
			continue // the intervals overlap: no arm contradicts another
		}
		if spread := loMax/hiMin - 1; spread > calibrationTolerance {
			return fmt.Errorf(
				"arms disagree by %.0f%% about how long %d iterations of the same Rust loop "+
					"take: %s calibrated %s, %s calibrated %s. The loop is deterministic, so "+
					"the arms did not run on the same machine conditions and their timings "+
					"cannot be compared. Re-record with `make bench-crossover`",
				spread*100, it,
				hiArm, humanNanos(arms[hiArm].lo), loArm, humanNanos(arms[loArm].lo))
		}
	}
	return nil
}

// calibration is the interval a benchmark name pins its arm's measurement to.
// The name carries a truncated value, so "705us" means [705, 706) µs.
type calibration struct{ lo, hi float64 }

// calibratedFromName pulls the iteration count and the calibrated duration back
// out of a name like "BenchmarkCrossoverSerial/RawCgo/1000000it-709us".
func calibratedFromName(name string) (iters int, c calibration, ok bool) {
	frag := name[strings.LastIndex(name, "/")+1:]
	countStr, durStr, found := strings.Cut(frag, "it-")
	if !found {
		return 0, calibration{}, false
	}
	iters, err := strconv.Atoi(countStr)
	if err != nil {
		return 0, calibration{}, false
	}
	var unit float64
	switch {
	case strings.HasSuffix(durStr, "ns"):
		unit = 1
		durStr = strings.TrimSuffix(durStr, "ns")
	case strings.HasSuffix(durStr, "us"):
		unit = 1000
		durStr = strings.TrimSuffix(durStr, "us")
	default:
		return 0, calibration{}, false
	}
	v, err := strconv.ParseFloat(durStr, 64)
	if err != nil {
		return 0, calibration{}, false
	}
	// [v, v+1) in the printed unit, because label() truncates.
	return iters, calibration{lo: v * unit, hi: (v + 1) * unit}, true
}

func humanNanos(ns float64) string {
	if ns >= 1000 {
		return fmt.Sprintf("%.0fµs", ns/1000)
	}
	return fmt.Sprintf("%.0fns", ns)
}

// mustLoad parses a results file and refuses it unless it carries proof of the
// conditions it was recorded under.
func mustLoad(path string) map[string][]benchfile.Sample {
	f, err := benchfile.Parse(path)
	if err != nil {
		fatal(err)
	}
	if err := f.RequireProvenance(); err != nil {
		fatal(err)
	}
	return f.ByName
}

// crossoverChart plots Gusset's cost as a multiple of blocking cgo's, against
// how much work each call does.
//
// The ratio is the useful quantity, not either absolute time: an adopter is
// deciding what the coordination costs them, and that is a percentage of their
// own workload. The break-even line is drawn because crossing it is the whole
// point — below it, the safer transport is also the faster one.
func crossoverChart(m map[string][]benchfile.Sample, works []workPoint) string {
	c := newChart(780, 430,
		"What Gusset's coordination costs, against how much work a call does",
		"Gusset ÷ one blocking cgo call, same Rust loop on both sides. Lower is better; 1× is break-even.")

	// Ceiling above the largest measured ratio, so the noop point — which is
	// the whole point of the left-hand end — is on the chart rather than
	// clipped off the top of it.
	const lo, hi = 0.5, 4000.0
	logRatio := func(r float64) float64 {
		a, b := math.Log10(lo), math.Log10(hi)
		return (math.Log10(r) - a) / (b - a)
	}

	for _, g := range []float64{0.5, 1, 10, 100, 1000} {
		c.gridY(logRatio(g), fmt.Sprintf("%g×", g), g == 1)
	}
	c.axes("work per call", "Gusset ÷ blocking cgo")

	var serial, parallel [][2]float64
	for i, w := range works {
		fx := float64(i) / float64(len(works)-1)
		c.xTick(fx, w.label)

		sr := medianByIters(m, "BenchmarkCrossoverSerial/RawCgo/", w.iters, "ns/op")
		sg := medianByIters(m, "BenchmarkCrossoverSerial/Gusset/", w.iters, "ns/op")
		pr := medianByIters(m, "BenchmarkCrossoverParallel/RawCgo/", w.iters, "ns/op")
		pg := medianByIters(m, "BenchmarkCrossoverParallel/Gusset/", w.iters, "ns/op")

		sRatio, pRatio := sg/sr, pg/pr
		serial = append(serial, [2]float64{fx, logRatio(sRatio)})
		parallel = append(parallel, [2]float64{fx, logRatio(pRatio)})

		// Label the last two work sizes: that is the crossover region, and it
		// is where a reader needs the number rather than the shape.
		if i >= len(works)-2 {
			c.pointLabel(fx, logRatio(sRatio), ratioLabel(sRatio), colSerial, -11)
			c.pointLabel(fx, logRatio(pRatio), ratioLabel(pRatio), colPar, 19)
		}
	}

	c.series(serial, colSerial)
	c.series(parallel, colPar)
	c.legend([][2]string{
		{colSerial, "one call at a time"},
		{colPar, "all cores busy"},
	})
	c.note(0.015, logRatio(0.66), "Gusset faster below this line", colMark)
	return c.done()
}

// ratioLabel renders a ratio the way a reader would say it out loud.
func ratioLabel(r float64) string {
	switch {
	case r >= 1.1:
		return fmt.Sprintf("%.2f×", r)
	case r >= 1:
		return fmt.Sprintf("+%.0f%%", (r-1)*100)
	default:
		return fmt.Sprintf("−%.0f%%", (1-r)*100)
	}
}

// threadChart plots peak OS threads against in-flight call count.
//
// This is the failure the library exists to prevent, and it is a *shape*: one
// line tracks concurrency, the other tracks the pool. A single concurrency
// level cannot tell those apart, which is why the sweep exists.
func threadChart(raw, gus map[string][]benchfile.Sample, concs []int) string {
	c := newChart(760, 420,
		"OS threads under concurrent in-flight calls",
		"Each call does ~7 ms of Rust work. One transport per process — Go never destroys a thread.")

	maxY := 0.0
	for _, n := range concs {
		for _, m := range []map[string][]benchfile.Sample{raw, gus} {
			for _, pre := range []string{"BenchmarkThreadScaling/RawCgo/conc", "BenchmarkThreadScaling/Gusset/conc"} {
				if s, ok := m[fmt.Sprintf("%s%d", pre, n)]; ok {
					if v, ok := median(s, "peakThreads"); ok && v > maxY {
						maxY = v
					}
				}
			}
		}
	}
	maxY = math.Ceil(maxY/100) * 100

	for v := 0.0; v <= maxY; v += maxY / 4 {
		c.gridY(v/maxY, fmt.Sprintf("%.0f", v), false)
	}
	c.axes("concurrent in-flight calls", "peak OS threads")

	var rawPts, gusPts [][2]float64
	for i, n := range concs {
		fx := float64(i) / float64(len(concs)-1)
		c.xTick(fx, strconv.Itoa(n))
		rv := mustMedian(raw, fmt.Sprintf("BenchmarkThreadScaling/RawCgo/conc%d", n), "peakThreads")
		gv := mustMedian(gus, fmt.Sprintf("BenchmarkThreadScaling/Gusset/conc%d", n), "peakThreads")
		rawPts = append(rawPts, [2]float64{fx, rv / maxY})
		gusPts = append(gusPts, [2]float64{fx, gv / maxY})
		c.pointLabel(fx, rv/maxY, fmt.Sprintf("%.0f", rv), colRaw, -10)
		c.pointLabel(fx, gv/maxY, fmt.Sprintf("%.0f", gv), colGus, 18)
	}
	c.series(rawPts, colRaw)
	c.series(gusPts, colGus)
	c.legend([][2]string{{colRaw, "blocking cgo"}, {colGus, "Gusset"}})
	return c.done()
}

// memChart plots peak resident memory against in-flight call count.
//
// Included precisely because it is the *unimpressive* chart. Thread explosion
// is usually sold as a memory problem, and measured here it is not much of one:
// the resident gap at 2048 in-flight calls is tens of megabytes, not gigabytes.
// The cost is the thread count itself — scheduler pressure, creation latency,
// and Go's 10,000-thread ceiling — so showing the modest memory curve next to
// the steep thread curve is what keeps the claim honest.
func memChart(raw, gus map[string][]benchfile.Sample, concs []int) string {
	c := newChart(760, 400,
		"Peak resident memory under the same load",
		"Measured with ps(1), not derived from a stack size. The thread cost is mostly not a memory cost.")

	maxY := 0.0
	for _, n := range concs {
		for _, p := range []struct {
			m   map[string][]benchfile.Sample
			pre string
		}{{raw, "RawCgo"}, {gus, "Gusset"}} {
			if s, ok := p.m[fmt.Sprintf("BenchmarkThreadScaling/%s/conc%d", p.pre, n)]; ok {
				if v, ok := median(s, "peakRSSmib"); ok && v > maxY {
					maxY = v
				}
			}
		}
	}
	maxY = math.Ceil(maxY/10) * 10

	for v := 0.0; v <= maxY; v += maxY / 4 {
		c.gridY(v/maxY, fmt.Sprintf("%.0f MiB", v), false)
	}
	c.axes("concurrent in-flight calls", "peak RSS")

	var rawPts, gusPts [][2]float64
	for i, n := range concs {
		fx := float64(i) / float64(len(concs)-1)
		c.xTick(fx, strconv.Itoa(n))
		rv := mustMedian(raw, fmt.Sprintf("BenchmarkThreadScaling/RawCgo/conc%d", n), "peakRSSmib")
		gv := mustMedian(gus, fmt.Sprintf("BenchmarkThreadScaling/Gusset/conc%d", n), "peakRSSmib")
		rawPts = append(rawPts, [2]float64{fx, rv / maxY})
		gusPts = append(gusPts, [2]float64{fx, gv / maxY})
	}
	c.series(rawPts, colRaw)
	c.series(gusPts, colGus)
	c.legend([][2]string{{colRaw, "blocking cgo"}, {colGus, "Gusset"}})
	return c.done()
}

// ---------------------------------------------------------------------------

func main() {
	check := flag.Bool("check", false, "fail if the committed charts are stale")
	flag.Parse()

	transport := mustOne(resultsDir, "transport-*.txt")
	scalingRaw := mustOne(resultsDir, "scaling-rawcgo-*.txt")
	scalingGus := mustOne(resultsDir, "scaling-gusset-*.txt")

	tm := mustLoad(transport)
	rm := mustLoad(scalingRaw)
	gm := mustLoad(scalingGus)

	// The work-size fragments are read back out of the committed benchmark
	// names rather than hardcoded: the duration in each name is calibrated on
	// the machine that ran it, so hardcoding would break on the next host.
	works := discoverWorks(tm)
	concs := discoverConcurrencies(rm)

	// Before anything is drawn: a chart is a claim, and these ask whether the
	// data behind it came off one quiet machine. benchfile.Parse catches a file
	// two processes wrote and rows whose numbers belong to another benchmark;
	// RequireProvenance catches a recording nobody certified; these two catch
	// one process measured beside somebody else's load. All of it parses
	// cleanly and none of it shows up as spread.
	if err := checkCalibrationAgreement(tm); err != nil {
		fatal(err)
	}
	if err := checkTransportOrdering(tm, works); err != nil {
		fatal(err)
	}

	files := map[string]string{
		"crossover.svg": crossoverChart(tm, works),
		"threads.svg":   threadChart(rm, gm, concs),
		"memory.svg":    memChart(rm, gm, concs),
	}

	// The adoption guide's tables, from the same medians as the charts beside
	// them. They used to be typed in by hand: three recordings apart from the
	// data they described, quoting thread counts of 31/72/173/322 against a
	// measured 33/90/224/368 and a wall-clock ordering that had since reversed.
	// A reader cannot tell a stale hand-typed number from a fresh one, and
	// neither could `make docs-check`.
	stale := writeDocTables(tm, rm, gm, works, concs, *check)
	for name, body := range files {
		path := filepath.Join(outDir, name)
		old, err := os.ReadFile(path)
		same := err == nil && string(old) == body
		if *check {
			if !same {
				fmt.Fprintf(os.Stderr, "benchplot: %s is stale; run `go run ./tools/benchplot`\n", path)
				stale = true
			}
			continue
		}
		if same {
			fmt.Printf("benchplot: %s already current\n", path)
			continue
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("benchplot: wrote %s\n", path)
	}
	if stale {
		os.Exit(1)
	}
	if *check {
		fmt.Printf("benchplot: docs/img matches %s\n", resultsDir)
	}
}

// discoverWorks pulls the serial sweep's work sizes out of the benchmark names,
// in the order the sweep ran them (noop first, then ascending iteration count).
func discoverWorks(m map[string][]benchfile.Sample) []workPoint {
	const pre = "BenchmarkCrossoverSerial/RawCgo/"
	var out []workPoint
	for name := range m {
		if !strings.HasPrefix(name, pre) {
			continue
		}
		frag := strings.TrimPrefix(name, pre)
		if frag == "noop" {
			out = append(out, workPoint{iters: 0, label: "noop"})
			continue
		}
		// frag is "<iters>it-<duration><unit>". The label comes from the serial
		// arm: it is the uncontended measurement, so it is the honest answer to
		// "how long does this work take".
		parts := strings.SplitN(frag, "it-", 2)
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		out = append(out, workPoint{iters: n, label: humanDuration(parts[1])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].iters < out[j].iters })
	if len(out) < 2 {
		fatal(fmt.Errorf("found %d work sizes in the committed results; need at least 2", len(out)))
	}
	return out
}

// humanDuration turns the benchmark name's "708ns" / "70us" into a label.
func humanDuration(s string) string {
	switch {
	case strings.HasSuffix(s, "ns"):
		return strings.TrimSuffix(s, "ns") + " ns"
	case strings.HasSuffix(s, "us"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "us"))
		if err == nil && n >= 1000 {
			return fmt.Sprintf("%.1f ms", float64(n)/1000)
		}
		return strings.TrimSuffix(s, "us") + " µs"
	}
	return s
}

func discoverConcurrencies(m map[string][]benchfile.Sample) []int {
	const pre = "BenchmarkThreadScaling/RawCgo/conc"
	var out []int
	for name := range m {
		if !strings.HasPrefix(name, pre) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(name, pre)); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	if len(out) < 2 {
		fatal(fmt.Errorf("found %d concurrency levels in the committed results; need at least 2; "+
			"run `make bench-scaling`", len(out)))
	}
	return out
}

// mustOne resolves exactly one results file, refusing to guess between several.
// tools/benchdoc takes the same line, for the same reason: picking one silently
// means the docs describe a platform nobody chose.
func mustOne(dir, glob string) string {
	hits, err := filepath.Glob(filepath.Join(dir, glob))
	if err != nil {
		fatal(err)
	}
	switch len(hits) {
	case 1:
		return hits[0]
	case 0:
		fatal(fmt.Errorf("no %s in %s; run `make bench-crossover` and `make bench-scaling`", glob, dir))
	default:
		fatal(fmt.Errorf("found %d files matching %s in %s; pass one explicitly rather than "+
			"letting the tool choose which platform the docs describe", len(hits), glob, dir))
	}
	return ""
}

// writeDocTables regenerates the measured tables in docs/choosing.md, or in
// -check mode reports whether they are stale. It returns true when stale.
func writeDocTables(tm, rm, gm map[string][]benchfile.Sample, works []workPoint, concs []int, check bool) bool {
	body := renderDocTables(tm, rm, gm, works, concs)

	doc, err := os.ReadFile(docPath)
	if err != nil {
		fatal(err)
	}
	updated, err := docblock.Replace(string(doc), docPath, docBlockName, body)
	if err != nil {
		fatal(err)
	}
	if updated == string(doc) {
		fmt.Printf("benchplot: %s tables already current\n", docPath)
		return false
	}
	if check {
		fmt.Fprintf(os.Stderr, "benchplot: %s tables are stale; run `go run ./tools/benchplot`\n", docPath)
		return true
	}
	if err := os.WriteFile(docPath, []byte(updated), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("benchplot: rewrote %s tables\n", docPath)
	return false
}

func renderDocTables(tm, rm, gm map[string][]benchfile.Sample, works []workPoint, concs []int) string {
	var b strings.Builder
	b.WriteString("<!-- Generated by `make docs` (tools/benchplot). Do not edit by hand. -->\n")

	b.WriteString("\n**Cost per call, by how much work the call does.** " +
		"Both transports run a byte-identical Rust loop, so the difference is transport alone.\n\n")
	b.WriteString("| work per call | cgo, serial | Gusset, serial | ratio | cgo, 18 goroutines | Gusset, 18 goroutines | ratio |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, w := range works {
		sr := medianByIters(tm, "BenchmarkCrossoverSerial/RawCgo/", w.iters, "ns/op")
		sg := medianByIters(tm, "BenchmarkCrossoverSerial/Gusset/", w.iters, "ns/op")
		pr := medianByIters(tm, "BenchmarkCrossoverParallel/RawCgo/", w.iters, "ns/op")
		pg := medianByIters(tm, "BenchmarkCrossoverParallel/Gusset/", w.iters, "ns/op")
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s |\n",
			w.label, fmtDur(sr), fmtDur(sg), tableRatio(sg/sr),
			fmtDur(pr), fmtDur(pg), tableRatio(pg/pr)))
	}

	b.WriteString("\n**Cost of concurrency.** Medians over the committed sweep, each transport in its own process.\n\n")
	b.WriteString("| in-flight calls | peak threads, cgo | peak threads, Gusset | peak RSS, cgo | peak RSS, Gusset | wall clock, cgo | wall clock, Gusset |\n")
	b.WriteString("| ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	name := func(transport string, c int) string {
		return fmt.Sprintf("BenchmarkThreadScaling/%s/conc%d", transport, c)
	}
	for _, c := range concs {
		b.WriteString(fmt.Sprintf("| %d | %.0f | %.0f | %.1f MiB | %.1f MiB | %s | %s |\n", c,
			mustMedian(rm, name("RawCgo", c), "peakThreads"), mustMedian(gm, name("Gusset", c), "peakThreads"),
			mustMedian(rm, name("RawCgo", c), "peakRSSmib"), mustMedian(gm, name("Gusset", c), "peakRSSmib"),
			fmtDur(mustMedian(rm, name("RawCgo", c), "ns/op")), fmtDur(mustMedian(gm, name("Gusset", c), "ns/op"))))
	}

	// The address-space figures, derived rather than quoted, because this is the
	// number the write-ups get wrong. "threads × 8 MiB" is the usual arithmetic
	// and it does not survive being measured: both transports here cost the same
	// per thread, and it is not 8 MiB. Generating the division means the doc
	// cannot claim a per-thread cost the data disagrees with.
	lo, hi := concs[0], concs[len(concs)-1]
	b.WriteString(fmt.Sprintf(
		"\nAcross that sweep (%d → %d in-flight calls) reserved *virtual* address space grew by "+
			"**%s** for blocking cgo and **%s** for Gusset, over %.0f and %.0f extra OS threads — "+
			"**%.0f MiB and %.0f MiB per thread** respectively.\n",
		lo, hi,
		fmtMiB(mustMedian(rm, name("RawCgo", hi), "peakVSZmib")-mustMedian(rm, name("RawCgo", lo), "peakVSZmib")),
		fmtMiB(mustMedian(gm, name("Gusset", hi), "peakVSZmib")-mustMedian(gm, name("Gusset", lo), "peakVSZmib")),
		mustMedian(rm, name("RawCgo", hi), "peakThreads")-mustMedian(rm, name("RawCgo", lo), "peakThreads"),
		mustMedian(gm, name("Gusset", hi), "peakThreads")-mustMedian(gm, name("Gusset", lo), "peakThreads"),
		perThread(rm, name("RawCgo", lo), name("RawCgo", hi)),
		perThread(gm, name("Gusset", lo), name("Gusset", hi)),
	))
	return b.String()
}

// perThread is the virtual address space one extra OS thread costs, in MiB.
func perThread(m map[string][]benchfile.Sample, lo, hi string) float64 {
	dThreads := mustMedian(m, hi, "peakThreads") - mustMedian(m, lo, "peakThreads")
	if dThreads <= 0 {
		return 0
	}
	return (mustMedian(m, hi, "peakVSZmib") - mustMedian(m, lo, "peakVSZmib")) / dThreads
}

func fmtMiB(mib float64) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.1f GiB", mib/1024)
	}
	return fmt.Sprintf("%.0f MiB", mib)
}

// tableRatio renders a ratio for a table column, where every cell has to be
// read against the ones above it.
//
// Not ratioLabel, which switches to "+2%" near break-even: that reads well as a
// single annotation on a chart and badly as a column, where it puts "978.74×"
// three rows above "+2%" and leaves the reader converting between two scales to
// see that one number is the continuation of the other.
func tableRatio(r float64) string {
	switch {
	case r >= 100:
		return fmt.Sprintf("%.0f×", r)
	case r >= 10:
		return fmt.Sprintf("%.1f×", r)
	default:
		return fmt.Sprintf("%.2f×", r)
	}
}

// fmtDur renders nanoseconds at a readable scale.
func fmtDur(ns float64) string {
	switch {
	case ns >= 1e9:
		return fmt.Sprintf("%.2f s", ns/1e9)
	case ns >= 1e6:
		return fmt.Sprintf("%.1f ms", ns/1e6)
	case ns >= 1e3:
		return fmt.Sprintf("%.2f µs", ns/1e3)
	default:
		return fmt.Sprintf("%.2f ns", ns)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "benchplot: %v\n", err)
	os.Exit(1)
}
