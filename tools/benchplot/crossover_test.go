package main

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
)

func ratioArms(serial, parallel []float64) []crossoverArm {
	labels := []string{"noop", "718 ns", "7 µs", "72 µs", "720 µs"}
	arms := make([]crossoverArm, len(serial))
	for i := range serial {
		arms[i] = crossoverArm{label: labels[i], serial: serial[i], parallel: parallel[i]}
	}
	return arms
}

// A limit holds from an arm upward. A single size under the limit with a larger
// one over it is noise, and calling it the crossover would send an adopter below
// the line the data actually supports.
func TestHoldsFromIgnoresADipBelowALargerFailure(t *testing.T) {
	arms := ratioArms(
		[]float64{80, 1.5, 3.0, 1.5, 1.2},
		[]float64{80, 1.5, 3.0, 1.5, 1.2},
	)
	got := holdsFrom(arms, 2, func(a crossoverArm) float64 { return a.serial })
	if got != 3 {
		t.Fatalf("holdsFrom = %d, want 3 (72 µs): the dip at 718 ns is below a failure at 7 µs", got)
	}
}

func TestHoldsFromWhenNoArmQualifies(t *testing.T) {
	arms := ratioArms([]float64{80, 9, 5, 4, 3}, []float64{80, 9, 5, 4, 3})
	ratio := func(a crossoverArm) float64 { return a.serial }
	if got := holdsFrom(arms, 2, ratio); got != len(arms) {
		t.Fatalf("holdsFrom = %d, want %d (none)", got, len(arms))
	}
	cell := withinCell(arms, 2, ratio)
	for _, want := range []string{"no arm", "720 µs", "3.00×"} {
		if !strings.Contains(cell, want) {
			t.Errorf("cell %q does not mention %q", cell, want)
		}
	}
}

func TestWithinCellWhenEveryArmQualifies(t *testing.T) {
	arms := ratioArms([]float64{1.05, 1.04, 1.03, 1.02, 1.01}, []float64{1, 1, 1, 1, 1})
	cell := withinCell(arms, 1.10, func(a crossoverArm) float64 { return a.serial })
	if !strings.Contains(cell, "every arm") || !strings.Contains(cell, "noop") {
		t.Fatalf("cell %q should say every arm, down to noop", cell)
	}
}

func TestAheadCellNamesTheArmsGussetWon(t *testing.T) {
	arms := ratioArms([]float64{80, 3, 1.2, 1.1, 0.97}, []float64{80, 3, 1.2, 1.1, 1.0})
	serial := func(a crossoverArm) float64 { return a.serial }
	parallel := func(a crossoverArm) float64 { return a.parallel }
	if got := aheadCell(arms, serial); got != "720 µs (0.97×)" {
		t.Errorf("serial ahead cell = %q", got)
	}
	if got := aheadCell(arms, parallel); got != "none of the 5 arms" {
		t.Errorf("parallel ahead cell = %q (a tie at 1.00× is not a win)", got)
	}
}

// The generated block is built from the transport recording's names and
// medians, ratios read per iteration count rather than per name.
func TestCrossoverArmsAndBlockFromARecording(t *testing.T) {
	m := dataset(
		func() (string, []benchfile.Sample) { return ns("BenchmarkCrossoverSerial/RawCgo/noop", 10, 10, 10) },
		func() (string, []benchfile.Sample) { return ns("BenchmarkCrossoverSerial/Gusset/noop", 100, 100, 100) },
		func() (string, []benchfile.Sample) { return ns("BenchmarkCrossoverParallel/RawCgo/noop", 1, 1, 1) },
		func() (string, []benchfile.Sample) { return ns("BenchmarkCrossoverParallel/Gusset/noop", 50, 50, 50) },
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/RawCgo/10000it-7us", 7000, 7000, 7000, 7000)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverSerial/Gusset/10000it-7us", 8400, 8400, 8400, 8400)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverParallel/RawCgo/10000it-6us", 500, 500, 500, 500)
		},
		func() (string, []benchfile.Sample) {
			return ns("BenchmarkCrossoverParallel/Gusset/10000it-6us", 3000, 3000, 3000, 3000)
		},
	)
	works := []workPoint{{iters: 0, label: "noop"}, {iters: 10000, label: "7 µs"}}
	arms := crossoverArms(m, works)
	if len(arms) != 2 || arms[0].serial != 10 || arms[1].serial != 1.2 || arms[1].parallel != 6 {
		t.Fatalf("arms = %+v", arms)
	}
	lo, hi := repetitions(m, works)
	if lo != 3 || hi != 4 {
		t.Fatalf("repetitions = %d..%d, want 3..4", lo, hi)
	}
	out := renderCrossover(arms, lo, hi, "18 goroutines")
	for _, want := range []string{
		"| serial | from 7 µs (1.20×); not at noop (10.0×) |",
		"| 18 goroutines | no arm; the largest, 7 µs, is 6.00× |",
		"none of the 2 arms",
		"The arms recorded are noop and 7 µs, 3 to 4 repetitions each.",
		"within 2× of raw cgo", "within 1.1× of raw cgo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block does not contain %q:\n%s", want, out)
		}
	}
}
