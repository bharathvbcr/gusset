package main

// The benchmark gallery: every certified results file under bench/results/,
// drawn, so that docs/benchmarks.md can show a shape where it used to show a
// column of numbers.
//
// Same contract as the rest of benchplot. Each chart is read out of committed
// raw output, every file goes through mustLoad (so one without a provenance
// header is fatal, not charted), and `-check` fails when an SVG drifts.
//
// What is deliberately absent: the linux-amd64-vm files with no header
// (inline-*, ring-*, waitchan-*, seed-micro, simd*). They were run by hand, so
// nothing certifies the machine was idle, and charting them would present an
// unverified run as a measurement. docs/benchmarks.md names them instead.
//
// Also absent: the VM's transport (crossover) recording. It has a header, but
// checkCalibrationAgreement refuses it: the arms disagree by 11% about how long
// the same Rust loop takes, over the 10% tolerance. The check is not loosened
// to get a chart; the file needs re-recording on that host.

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
)

const (
	darwinResultsDir = "bench/results"
	vmResultsDir     = "bench/results/linux-amd64-vm"

	// A "before" arm is drawn in grey, in the same slot its "after" arm takes
	// in blue, so the reader compares a pair rather than decoding two hues.
	colBefore = "#8c959f"
)

func nan() float64 { return math.NaN() }

// ---------------------------------------------------------------------------
// Bar chart primitive
// ---------------------------------------------------------------------------

// barSeries is one colour of bar. A NaN value leaves its slot empty, which is
// how a group that has no "before" measurement stays honest about it.
type barSeries struct {
	label string
	col   string
	vals  []float64
}

// barChart draws grouped bars. A log axis is the default because the suites
// span 56 ns to 45 µs on one chart; linear is for counts, where a log axis
// would make 255 threads against 21 look like a modest gap.
func barChart(title, sub, yLabel string, groups []string, ss []barSeries, logY bool, vf func(float64) string) string {
	var lo, hi float64
	maxV, minV := 0.0, math.Inf(1)
	for _, s := range ss {
		for _, v := range s.vals {
			if math.IsNaN(v) {
				continue
			}
			maxV = math.Max(maxV, v)
			minV = math.Min(minV, v)
		}
	}
	if maxV <= 0 {
		fatal(fmt.Errorf("bar chart %q has no positive values to draw", title))
	}
	var frac func(float64) float64
	var ticks []float64
	if logY {
		// Headroom of at least 0.35 decades so the value label above the
		// tallest bar stays inside the plot.
		lo = math.Pow(10, math.Floor(math.Log10(minV)))
		hi = math.Pow(10, math.Ceil(math.Log10(maxV)+0.35))
		a, b := math.Log10(lo), math.Log10(hi)
		frac = func(v float64) float64 { return (math.Log10(v) - a) / (b - a) }
		for t := lo; t <= hi*1.0001; t *= 10 {
			ticks = append(ticks, t)
		}
	} else {
		step := niceStep(maxV * 1.15 / 4)
		hi = step * math.Ceil(maxV*1.15/step)
		frac = func(v float64) float64 { return v / hi }
		for t := 0.0; t <= hi*1.0001; t += step {
			ticks = append(ticks, t)
		}
	}

	w := 130*len(groups) + 112
	// Wide enough for the title and a three-entry key to share the header strip,
	// and for three value labels to sit side by side without touching.
	if w < 880 {
		w = 880
	}
	c := newChart(w, 400, title, sub)
	for _, t := range ticks {
		c.gridY(frac(math.Max(t, lo)), vf(t), false)
	}
	c.axes("", yLabel)
	c.legendRow(ss)

	slot := float64(c.plotW()) / float64(len(groups))
	// Capped so a lone series does not become a wall, but wide enough that three
	// adjacent value labels ("362.8 ms") do not run together.
	bw := math.Min(slot*0.8/float64(len(ss)), 56)
	for g, name := range groups {
		cx := float64(c.padL) + (float64(g)+0.5)*slot
		_, yAxis := c.xy(0, 0)
		fmt.Fprintf(&c.b, `<text x="%.1f" y="%.1f" font-size="11" text-anchor="middle" fill="%s">%s</text>`,
			cx, yAxis+19, colMuted, esc(name))
		for i, s := range ss {
			v := s.vals[g]
			if math.IsNaN(v) {
				continue
			}
			x := cx - bw*float64(len(ss))/2 + bw*float64(i)
			_, y := c.xy(0, frac(v))
			fmt.Fprintf(&c.b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" rx="2"/>`,
				x+1, y, bw-2, yAxis-y, s.col)
			fmt.Fprintf(&c.b, `<text x="%.1f" y="%.1f" font-size="10.5" text-anchor="middle" fill="%s">%s</text>`,
				x+bw/2, y-5, colText, esc(vf(v)))
		}
	}
	return c.done()
}

// niceStep rounds a raw step up to 1, 2 or 5 times a power of ten.
func niceStep(raw float64) float64 {
	p := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if raw <= m*p {
			return m * p
		}
	}
	return 10 * p
}

// legendRow puts a horizontal key at the right of the title strip. A box inside
// the plot would sit on top of the tallest bars; there is nothing in this strip.
func (c *chart) legendRow(ss []barSeries) {
	if len(ss) < 2 {
		return
	}
	x := float64(c.w - c.padR)
	for i := len(ss) - 1; i >= 0; i-- {
		tw := float64(len(ss[i].label)) * 6.4
		x -= tw
		fmt.Fprintf(&c.b, `<text x="%.1f" y="24" font-size="11.5" fill="%s">%s</text>`, x, colText, esc(ss[i].label))
		x -= 17
		fmt.Fprintf(&c.b, `<rect x="%.1f" y="15" width="11" height="11" fill="%s" rx="2"/>`, x, ss[i].col)
		x -= 16
	}
}

// ---------------------------------------------------------------------------
// Suites
// ---------------------------------------------------------------------------

// suiteGroups are Gusset's own benchmarks, in the order bench/bench_test.go
// declares them, with the label a reader would use.
var suiteGroups = []struct{ bench, label string }{
	{"BenchmarkGussetCallNoop", "Call, no-op"},
	{"BenchmarkGussetCallParallel", "Call, parallel"},
	{"BenchmarkGussetSubmitWait", "Submit + Wait"},
	{"BenchmarkGussetBufferLarge", "64 KiB, copy"},
	{"BenchmarkGussetBufferLargeZeroCopy", "64 KiB, zero-copy"},
	{"BenchmarkChannelHop", "Go channel hop (floor)"},
}

func suiteLabels() []string {
	out := make([]string, len(suiteGroups))
	for i, g := range suiteGroups {
		out[i] = g.label
	}
	return out
}

func suiteVals(m map[string][]benchfile.Sample) []float64 {
	out := make([]float64, len(suiteGroups))
	for i, g := range suiteGroups {
		out[i] = mustMedian(m, g.bench, "ns/op")
	}
	return out
}

func darwinSuiteChart(m map[string][]benchfile.Sample) string {
	return barChart(
		"Gusset's own suite: cost per operation",
		"Median of the committed samples. Log scale. The channel hop is the pure-Go floor, not a Gusset call.",
		"time per operation",
		suiteLabels(),
		[]barSeries{{"Gusset", colGus, suiteVals(m)}},
		true, fmtDur)
}

func vmSuiteChart(before, after map[string][]benchfile.Sample) string {
	return barChart(
		"Gusset's own suite on a 4 vCPU Linux VM: before and after",
		"Same benchmark file, same host. Before = original main; after = spin-then-park completion path. Log scale.",
		"time per operation",
		suiteLabels(),
		[]barSeries{{"before", colBefore, suiteVals(before)}, {"after", colGus, suiteVals(after)}},
		true, fmtDur)
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// scalingTimeChart plots wall-clock for one batch of N in-flight calls. The
// claim it supports is "the bound costs a few percent", so it needs the raw
// times side by side, not a derived percentage that hides the scale.
func scalingTimeChart(raw, before, after map[string][]benchfile.Sample, concs []int) string {
	groups := make([]string, len(concs))
	var rv, bv, av []float64
	for i, n := range concs {
		groups[i] = strconv.Itoa(n) + " in flight"
		rv = append(rv, mustMedian(raw, fmt.Sprintf("BenchmarkThreadScaling/RawCgo/conc%d", n), "ns/op"))
		bv = append(bv, mustMedian(before, fmt.Sprintf("BenchmarkThreadScaling/Gusset/conc%d", n), "ns/op"))
		av = append(av, mustMedian(after, fmt.Sprintf("BenchmarkThreadScaling/Gusset/conc%d", n), "ns/op"))
	}
	return barChart(
		"Wall time for a batch of concurrent calls (Linux VM)",
		"Each call does one 10M-iteration Rust loop. The thread bound should cost little wall time; before and after should match.",
		"wall time per batch",
		groups,
		[]barSeries{{"blocking cgo", colRaw, rv}, {"Gusset, before", colBefore, bv}, {"Gusset, after", colGus, av}},
		true, fmtDur)
}

// scalingThreadsChart plots peak OS threads per concurrency level for the VM,
// with both Gusset recordings. Bars rather than lines: before and after differ
// by about one thread, and two near-coincident lines would hide one of them
// and collide their labels.
func scalingThreadsChart(raw, before, after map[string][]benchfile.Sample, concs []int) string {
	groups := make([]string, len(concs))
	var rv, bv, av []float64
	for i, n := range concs {
		groups[i] = strconv.Itoa(n) + " in flight"
		rv = append(rv, mustMedian(raw, fmt.Sprintf("BenchmarkThreadScaling/RawCgo/conc%d", n), "peakThreads"))
		bv = append(bv, mustMedian(before, fmt.Sprintf("BenchmarkThreadScaling/Gusset/conc%d", n), "peakThreads"))
		av = append(av, mustMedian(after, fmt.Sprintf("BenchmarkThreadScaling/Gusset/conc%d", n), "peakThreads"))
	}
	return barChart(
		"OS threads under concurrent in-flight calls (Linux VM)",
		"Each call does one 10M-iteration Rust loop. One transport per process — Go never destroys a thread.",
		"peak OS threads",
		groups,
		[]barSeries{{"blocking cgo", colRaw, rv}, {"Gusset, before", colBefore, bv}, {"Gusset, after", colGus, av}},
		false, func(v float64) string { return fmt.Sprintf("%.0f", v) })
}

// pressureChart compares peak OS threads at 512 in-flight calls, one group per
// recorded platform. Linear, because the point is 255 against 21.
func pressureChart(darwinRaw, darwinGus, vmRaw, vmBefore, vmAfter map[string][]benchfile.Sample) string {
	peak := func(m map[string][]benchfile.Sample, transport string) float64 {
		name, ok := pressureName(m, transport)
		if !ok {
			fatal(fmt.Errorf("no ThreadPressure/%s benchmark in the committed results; run `make bench-crossover`", transport))
		}
		return mustMedian(m, name, "peakThreads")
	}
	return barChart(
		"Peak OS threads with 512 calls in flight",
		"ThreadPressure: 512 concurrent calls, one 10M-iteration Rust loop each. A single run per bar, so a gap of a few threads is not resolved.",
		"peak OS threads",
		[]string{"darwin arm64, 18 cores", "linux amd64 VM, 4 vCPU"},
		[]barSeries{
			{"blocking cgo", colRaw, []float64{peak(darwinRaw, "RawCgo"), peak(vmRaw, "RawCgo")}},
			{"Gusset, before", colBefore, []float64{nan(), peak(vmBefore, "Gusset")}},
			{"Gusset", colGus, []float64{peak(darwinGus, "Gusset"), peak(vmAfter, "Gusset")}},
		},
		false, func(v float64) string { return fmt.Sprintf("%.0f", v) })
}

// pressureName finds the ThreadPressure benchmark for a transport. The name
// carries a calibrated duration ("10000000it-7180us") that differs per host,
// so it is discovered rather than spelled out.
func pressureName(m map[string][]benchfile.Sample, transport string) (string, bool) {
	for name := range m {
		if strings.HasPrefix(name, "BenchmarkThreadPressure/10000000it-") && strings.Contains(name, "/"+transport+"/") {
			return name, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------

// galleryCharts loads every certified file the gallery draws from and returns
// the SVGs by file name.
func galleryCharts() map[string]string {
	darwin := mustLoad(mustOne(darwinResultsDir, "darwin-*.txt"))
	dRaw := mustLoad(mustOne(resultsDir, "threads-rawcgo-*.txt"))
	dGus := mustLoad(mustOne(resultsDir, "threads-gusset-*.txt"))

	vm := func(name string) map[string][]benchfile.Sample { return mustLoad(filepath.Join(vmResultsDir, name)) }
	mainBefore, mainAfter := vm("main-before.txt"), vm("main-after.txt")
	scRaw, scBefore, scAfter := vm("scaling-rawcgo.txt"), vm("scaling-gusset-before.txt"), vm("scaling-gusset-after.txt")
	thRaw, thBefore, thAfter := vm("threads-rawcgo.txt"), vm("threads-gusset-before.txt"), vm("threads-gusset-after.txt")

	concs := discoverConcurrencies(scRaw)
	return map[string]string{
		"darwin-suite.svg":    darwinSuiteChart(darwin),
		"vm-suite.svg":        vmSuiteChart(mainBefore, mainAfter),
		"vm-scaling-time.svg": scalingTimeChart(scRaw, scBefore, scAfter, concs),
		"vm-threads.svg":      scalingThreadsChart(scRaw, scBefore, scAfter, concs),
		"vm-memory.svg": memChart(scRaw, scAfter, concs,
			"Peak resident memory under the same load (4 vCPU Linux VM)",
			"Measured with ps(1), not derived from a stack size. The thread cost is mostly not a memory cost."),
		"thread-pressure.svg": pressureChart(dRaw, dGus, thRaw, thBefore, thAfter),
	}
}
