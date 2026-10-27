package main

import (
	"fmt"
	"strings"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
)

// The crossover summary in docs/choosing.md's generated block.
//
// The cost-per-call table above it gives every ratio; this answers the question
// an adopter actually asks of it, "from what work size does Gusset stop being a
// problem", without leaving them to eyeball a column. It is derived from the
// same medians, in the same block, so `make docs-check` holds it to the data.
//
// Two limits are stated, not one: within 2x is where the overhead stops being
// the dominant cost, within 1.10x is where it stops being worth measuring on
// the call. Neither is a target Gusset was built to hit: it caps overhead, it
// does not promise a ratio to raw cgo, so these report where the measured line
// falls and do not assert that it clears one.
const (
	crossoverLoose = 2.0
	crossoverTight = 1.10
)

// parallelGoroutines names the parallel arm's row ("18 goroutines") from the
// recording, not from the host that made the committed one.
//
// BenchmarkCrossoverParallel is b.RunParallel with the default parallelism
// (bench/seed/go/crossover_test.go never calls SetParallelism), which starts
// GOMAXPROCS goroutines, and `go test` writes that GOMAXPROCS on every
// benchmark name. A recording that cannot say so unambiguously is refused.
func parallelGoroutines(f *benchfile.File) (string, error) {
	procs, err := f.ProcsOf("BenchmarkCrossoverParallel/")
	if err != nil {
		return "", err
	}
	if procs == 1 {
		return "1 goroutine", nil
	}
	return fmt.Sprintf("%d goroutines", procs), nil
}

// renderCostTable is the cost-per-call table: every work size, serial and
// parallel, with the ratio of each pair. parallel names the parallel columns'
// goroutine count.
func renderCostTable(tm map[string][]benchfile.Sample, works []workPoint, parallel string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("| work per call | cgo, serial | Gusset, serial | ratio | cgo, %s | Gusset, %s | ratio |\n",
		parallel, parallel))
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
	return b.String()
}

// crossoverArm is one work size's Gusset ÷ raw-cgo ratio, serial and parallel.
type crossoverArm struct {
	label    string
	serial   float64
	parallel float64
}

// crossoverArms reads the ratios out of the committed transport recording, in
// the order of works (noop first, then ascending iteration count).
func crossoverArms(tm map[string][]benchfile.Sample, works []workPoint) []crossoverArm {
	arms := make([]crossoverArm, 0, len(works))
	for _, w := range works {
		sr := medianByIters(tm, "BenchmarkCrossoverSerial/RawCgo/", w.iters, "ns/op")
		sg := medianByIters(tm, "BenchmarkCrossoverSerial/Gusset/", w.iters, "ns/op")
		pr := medianByIters(tm, "BenchmarkCrossoverParallel/RawCgo/", w.iters, "ns/op")
		pg := medianByIters(tm, "BenchmarkCrossoverParallel/Gusset/", w.iters, "ns/op")
		arms = append(arms, crossoverArm{label: w.label, serial: sg / sr, parallel: pg / pr})
	}
	return arms
}

// repetitions is the fewest and most samples any arm of the transport
// recording has, so the block can say how many runs each median stands on.
func repetitions(tm map[string][]benchfile.Sample, works []workPoint) (lo, hi int) {
	for _, w := range works {
		for _, prefix := range []string{
			"BenchmarkCrossoverSerial/RawCgo/", "BenchmarkCrossoverSerial/Gusset/",
			"BenchmarkCrossoverParallel/RawCgo/", "BenchmarkCrossoverParallel/Gusset/",
		} {
			n := len(tm[findByIters(tm, prefix, w.iters)])
			if lo == 0 || n < lo {
				lo = n
			}
			if n > hi {
				hi = n
			}
		}
	}
	return lo, hi
}

// holdsFrom is the smallest arm index from which every larger arm is within
// limit, or len(arms) when even the largest arm is not.
//
// "From an arm upward" rather than "the first arm under the limit": a noisy
// dip at one size must not read as a crossover that the next size contradicts.
func holdsFrom(arms []crossoverArm, limit float64, ratio func(crossoverArm) float64) int {
	from := len(arms)
	for i := len(arms) - 1; i >= 0; i-- {
		if ratio(arms[i]) > limit {
			break
		}
		from = i
	}
	return from
}

// withinCell renders one cell of the crossover table.
func withinCell(arms []crossoverArm, limit float64, ratio func(crossoverArm) float64) string {
	from := holdsFrom(arms, limit, ratio)
	switch {
	case from == len(arms):
		last := arms[len(arms)-1]
		return fmt.Sprintf("no arm; the largest, %s, is %s", last.label, tableRatio(ratio(last)))
	case from == 0:
		return fmt.Sprintf("every arm, down to %s (%s)", arms[0].label, tableRatio(ratio(arms[0])))
	}
	return fmt.Sprintf("from %s (%s); not at %s (%s)",
		arms[from].label, tableRatio(ratio(arms[from])),
		arms[from-1].label, tableRatio(ratio(arms[from-1])))
}

// aheadCell lists the arms where Gusset's median beat raw cgo's.
func aheadCell(arms []crossoverArm, ratio func(crossoverArm) float64) string {
	var ahead []string
	for _, a := range arms {
		if ratio(a) < 1 {
			ahead = append(ahead, fmt.Sprintf("%s (%s)", a.label, tableRatio(ratio(a))))
		}
	}
	if len(ahead) == 0 {
		return fmt.Sprintf("none of the %d arms", len(arms))
	}
	return strings.Join(ahead, ", ")
}

// renderCrossover is the generated crossover summary. repsLo and repsHi are the
// fewest and most repetitions behind any median; parallel names the parallel
// row ("18 goroutines"), which parallelGoroutines reads off the recording.
func renderCrossover(arms []crossoverArm, repsLo, repsHi int, parallel string) string {
	serialRatio := func(a crossoverArm) float64 { return a.serial }
	parallelRatio := func(a crossoverArm) float64 { return a.parallel }

	labels := make([]string, len(arms))
	for i, a := range arms {
		labels[i] = a.label
	}
	reps := fmt.Sprintf("%d", repsLo)
	if repsLo != repsHi {
		reps = fmt.Sprintf("%d to %d", repsLo, repsHi)
	}

	var b strings.Builder
	b.WriteString("\n**Where the crossover is.** The ratio is Gusset's median over raw cgo's median on the same work, " +
		"the ratio column above. A limit holds *from* an arm when that arm and every larger one is within it, " +
		"so one lucky size does not count as a crossover.\n\n")
	b.WriteString(fmt.Sprintf("| | within %g× of raw cgo | within %g× of raw cgo | Gusset ahead of raw cgo |\n",
		crossoverLoose, crossoverTight))
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, row := range []struct {
		name  string
		ratio func(crossoverArm) float64
	}{{"serial", serialRatio}, {parallel, parallelRatio}} {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", row.name,
			withinCell(arms, crossoverLoose, row.ratio),
			withinCell(arms, crossoverTight, row.ratio),
			aheadCell(arms, row.ratio)))
	}
	b.WriteString(fmt.Sprintf("\nThe arms recorded are %s, %s repetitions each. "+
		"A crossover is bracketed by those arms, not interpolated between them.\n",
		joinAnd(labels), reps))
	return b.String()
}

// joinAnd renders "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
