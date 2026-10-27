package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
)

// The numbers the crossover table prints are compared against bars: Gusset is
// ahead of raw cgo below 1, and the two limits the table reports are
// crossoverTight and crossoverLoose. A printed ratio that sits on the other side
// of one of those bars than the value it stands for contradicts the sentence
// around it, so the property under test is that rounding never crosses or lands
// on a bar unless the value itself does.

// printedValue reads the number back out of a rendered ratio ("1.002×").
func printedValue(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "×"), 64)
	if err != nil {
		t.Fatalf("%q is not a rendered ratio: %v", s, err)
	}
	return v
}

func side(v, bar float64) int {
	switch {
	case v < bar:
		return -1
	case v > bar:
		return 1
	}
	return 0
}

func TestTableRatioNeverContradictsTheBarsItIsComparedWith(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.998, "0.998×"}, // a win; "1.00×" would print it as a tie
		{1.0, "1.00×"},
		{1.002, "1.002×"}, // a loss; "1.00×" would print it as a tie
		{1.099, "1.099×"}, // within the 1.10 bar; "1.10×" hides that it is under
		{1.1, "1.10×"},    // exactly on the bar, and within it
		{1.104, "1.104×"}, // over the bar; "1.10×" printed it as on the bar
		{1.996, "1.996×"}, // within the 2× bar
		{2.0, "2.00×"},
		{2.004, "2.004×"}, // over the 2× bar
		{3.14159, "3.14×"},
		{12.34, "12.3×"},
		{978.74, "979×"},
	} {
		got := tableRatio(tc.in)
		if got != tc.want {
			t.Errorf("tableRatio(%v) = %q, want %q", tc.in, got, tc.want)
		}
		pv := printedValue(t, got)
		for _, bar := range []float64{1, crossoverTight, crossoverLoose} {
			if side(pv, bar) != side(tc.in, bar) {
				t.Errorf("tableRatio(%v) = %q is on the other side of %v from the value", tc.in, got, bar)
			}
		}
	}
}

// ratioLabel is the chart annotation. It prints a percentage near break-even,
// so a loss of 0.2% must not read as "+0%", a win of 0.2% must not read as
// "−0%", and the 10% a value under the tight bar rounds to must not read as the
// bar itself.
func TestRatioLabelNeverContradictsTheBarsItIsComparedWith(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.5, "−50%"},
		{0.998, "−0.2%"},
		{1.0, "+0%"},
		{1.002, "+0.2%"},
		{1.04, "+4%"},
		{1.099, "+9.9%"},
		{1.1, "1.10×"},
		{1.104, "1.104×"},
		{2.5, "2.50×"},
	} {
		if got := ratioLabel(tc.in); got != tc.want {
			t.Errorf("ratioLabel(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAheadCellDoesNotListATieAsAWin(t *testing.T) {
	arms := ratioArms([]float64{80, 3, 1.2, 1.1, 0.998}, []float64{80, 3, 1.2, 1.1, 1.002})
	serial := func(a crossoverArm) float64 { return a.serial }
	parallel := func(a crossoverArm) float64 { return a.parallel }
	if got := aheadCell(arms, serial); got != "720 µs (0.998×)" {
		t.Errorf("serial ahead cell = %q, want the win printed as under 1", got)
	}
	if got := aheadCell(arms, parallel); got != "none of the 5 arms" {
		t.Errorf("parallel ahead cell = %q; 1.002 is a loss", got)
	}
}

func TestWithinCellDoesNotPrintAnOverBarArmAsTheBar(t *testing.T) {
	arms := ratioArms([]float64{80, 9, 5, 1.104, 1.05}, []float64{80, 9, 5, 1.104, 1.05})
	cell := withinCell(arms, crossoverTight, func(a crossoverArm) float64 { return a.serial })
	// 720 µs holds; 72 µs, at 1.104, is the arm that fails the bar.
	want := "from 720 µs (1.05×); not at 72 µs (1.104×)"
	if cell != want {
		t.Errorf("cell = %q, want %q", cell, want)
	}
}

const serialRawPrefix = "BenchmarkCrossoverSerial/RawCgo/"

func rawArm(suffix string, ns_ float64) func() (string, []benchfile.Sample) {
	return func() (string, []benchfile.Sample) { return ns(serialRawPrefix+suffix, ns_, ns_, ns_) }
}

// Every arm of a sweep is found by its iteration count, and a count whose
// digits begin another's (1000 and 10000, 1400 and 14000) must not resolve to
// the wrong arm: the match is on "<iters>it-", not on the digits.
func TestWorksAndLookupsAcceptTheOneAndTenMicrosecondArms(t *testing.T) {
	m := dataset(
		rawArm("noop", 10),
		rawArm("1000it-708ns", 1000),
		rawArm("1400it-990ns", 1400),
		rawArm("10000it-7us", 10000),
		rawArm("14000it-10us", 14000),
		rawArm("100000it-70us", 100000),
	)
	var got []string
	for _, w := range discoverWorks(m) {
		got = append(got, strconv.Itoa(w.iters)+"="+w.label)
	}
	want := "0=noop 1000=708 ns 1400=990 ns 10000=7 µs 14000=10 µs 100000=70 µs"
	if strings.Join(got, " ") != want {
		t.Fatalf("works = %q, want %q", strings.Join(got, " "), want)
	}
	for iters, wantNs := range map[int]float64{0: 10, 1000: 1000, 1400: 1400, 10000: 10000, 14000: 14000, 100000: 100000} {
		if got := medianByIters(m, serialRawPrefix, iters, "ns/op"); got != wantNs {
			t.Errorf("medianByIters(%d) = %v, want %v: resolved the wrong arm", iters, got, wantNs)
		}
	}
}

// A recording with the 1 µs and 10 µs arms charts and tabulates all seven sizes,
// each from its own medians.
func TestChartAndCrossoverBlockRenderTheOneAndTenMicrosecondArms(t *testing.T) {
	arms := []struct {
		suffix string
		iters  int
		label  string
	}{
		{"noop", 0, "noop"},
		{"1000it-708ns", 1000, "708 ns"},
		{"1400it-990ns", 1400, "990 ns"},
		{"10000it-7us", 10000, "7 µs"},
		{"14000it-10us", 14000, "10 µs"},
		{"100000it-70us", 100000, "70 µs"},
		{"1000000it-709us", 1000000, "709 µs"},
	}
	var rows []func() (string, []benchfile.Sample)
	var works []workPoint
	for _, a := range arms {
		works = append(works, workPoint{iters: a.iters, label: a.label})
		for _, p := range []string{"Serial", "Parallel"} {
			base := float64(100 + a.iters)
			rows = append(rows,
				func() (string, []benchfile.Sample) {
					return ns("BenchmarkCrossover"+p+"/RawCgo/"+a.suffix, base, base, base)
				},
				func() (string, []benchfile.Sample) {
					return ns("BenchmarkCrossover"+p+"/Gusset/"+a.suffix, base*1.05, base*1.05, base*1.05)
				})
		}
	}
	m := dataset(rows...)
	if got := discoverWorks(m); len(got) != len(arms) {
		t.Fatalf("discoverWorks found %d arms, want %d", len(got), len(arms))
	}
	svg := crossoverChart(m, works)
	for _, a := range arms {
		if !strings.Contains(svg, ">"+a.label+"<") {
			t.Errorf("chart has no tick for %q", a.label)
		}
	}
	lo, hi := repetitions(m, works)
	out := renderCrossover(crossoverArms(m, works), lo, hi, "18 goroutines")
	if !strings.Contains(out, "noop, 708 ns, 990 ns, 7 µs, 10 µs, 70 µs and 709 µs, 3 repetitions each.") {
		t.Errorf("block does not name all seven arms:\n%s", out)
	}
}

// A recording made before the 1 µs and 10 µs arms existed has only the original
// five sizes and must still resolve every one of them, and only those.
func TestWorksFromARecordingWithoutTheNewArms(t *testing.T) {
	m := dataset(
		rawArm("noop", 10),
		rawArm("1000it-708ns", 708),
		rawArm("10000it-7us", 7000),
		rawArm("100000it-70us", 70000),
		rawArm("1000000it-709us", 709000),
	)
	if works := discoverWorks(m); len(works) != 5 {
		t.Fatalf("works = %+v, want the 5 recorded sizes", works)
	}
	for _, iters := range []int{1400, 14000} {
		if name := findByIters(m, serialRawPrefix, iters); name != "" {
			t.Errorf("findByIters(%d) = %q in a recording that lacks it", iters, name)
		}
	}
}

// benchplot's only flag is -check, so the old advice to "pass one explicitly"
// named something that cannot be done. The remedy is in the directory.
func TestResolveOneNamesARemedyThatExists(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"transport-a.txt", "transport-b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := resolveOne(dir, "transport-*.txt")
	if err == nil {
		t.Fatal("two matching files were accepted")
	}
	msg := err.Error()
	if strings.Contains(msg, "pass one explicitly") {
		t.Errorf("message still names a flag that does not exist: %v", msg)
	}
	for _, want := range []string{
		"found 2 files matching transport-*.txt",
		"transport-a.txt", "transport-b.txt",
		"benchplot takes no file argument; its only flag is -check",
		"move the recording you do not want charted out of bench/results/ entirely",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not contain %q:\n%s", want, msg)
		}
	}

	one, err := resolveOne(dir, "transport-a.txt")
	if err != nil || filepath.Base(one) != "transport-a.txt" {
		t.Errorf("resolveOne of a single match = %q, %v", one, err)
	}
	if _, err := resolveOne(dir, "scaling-*.txt"); err == nil || !strings.Contains(err.Error(), "no scaling-*.txt in "+dir) {
		t.Errorf("resolveOne of no match: %v", err)
	}
}
