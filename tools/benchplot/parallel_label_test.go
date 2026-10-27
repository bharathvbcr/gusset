package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
)

// The parallel arm of the crossover benchmark is b.RunParallel with the default
// parallelism, so it runs GOMAXPROCS goroutines, and `go test` writes that
// GOMAXPROCS onto every benchmark name as the -N suffix. The generated tables
// used to say "18 goroutines" because the committed recording was made on an
// 18-core host; a recording from any other host would have been labelled with
// the wrong count and still passed `make docs-check`, which only compares the
// tables with themselves.

// recording writes a transport-shaped results file whose parallel rows carry
// the suffixes procs gives them (one per repetition, cycled), and parses it. The
// serial rows carry suffix 18 throughout: only the parallel arm's count is a
// label.
func recording(t *testing.T, procs ...string) *benchfile.File {
	t.Helper()
	var b strings.Builder
	for _, arm := range []string{
		"CrossoverSerial/RawCgo", "CrossoverSerial/Gusset",
		"CrossoverParallel/RawCgo", "CrossoverParallel/Gusset",
	} {
		for _, work := range []string{"noop", "1000it-718ns"} {
			for rep := 0; rep < 2; rep++ {
				suffix, extra := "18", ""
				if strings.HasPrefix(arm, "CrossoverParallel") {
					suffix = procs[rep%len(procs)]
					extra = "\t         0 Δthreads"
				}
				name := "Benchmark" + arm + "/" + work
				if suffix != "" {
					name += "-" + suffix
				}
				b.WriteString(fmt.Sprintf("%s\t1000\t100.0 ns/op%s\t       0 B/op\t       0 allocs/op\n", name, extra))
			}
		}
	}
	path := filepath.Join(t.TempDir(), "transport-test.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := benchfile.Parse(path)
	if err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	return f
}

// A recording from an 8-core host is labelled 8 goroutines in both generated
// tables, and the committed host's 18 appears nowhere.
func TestParallelLabelFollowsTheRecordedGOMAXPROCS(t *testing.T) {
	f := recording(t, "8")
	label, err := parallelGoroutines(f)
	if err != nil {
		t.Fatal(err)
	}
	if label != "8 goroutines" {
		t.Fatalf("label = %q, want %q", label, "8 goroutines")
	}

	works := discoverWorks(f.ByName)
	cost := renderCostTable(f.ByName, works, label)
	lo, hi := repetitions(f.ByName, works)
	crossover := renderCrossover(crossoverArms(f.ByName, works), lo, hi, label)
	for _, want := range []string{"cgo, 8 goroutines | Gusset, 8 goroutines", "| 8 goroutines |"} {
		if !strings.Contains(cost+crossover, want) {
			t.Errorf("tables do not contain %q:\n%s%s", want, cost, crossover)
		}
	}
	if strings.Contains(cost+crossover, "18 goroutines") {
		t.Errorf("tables still name the committed host's 18 goroutines:\n%s%s", cost, crossover)
	}
}

// One recording, two GOMAXPROCS values: the parallel arm did not run at one
// concurrency, so no single label describes it.
func TestParallelLabelRefusesMixedGOMAXPROCS(t *testing.T) {
	f := recording(t, "8", "16")
	label, err := parallelGoroutines(f)
	if err == nil {
		t.Fatalf("mixed -8 and -16 suffixes produced the label %q", label)
	}
	for _, want := range []string{"BenchmarkCrossoverParallel", "8", "16", "more than one GOMAXPROCS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// `go test` omits the suffix at GOMAXPROCS 1, and a hand-edited file has none
// either; the two cannot be told apart, so neither is guessed at.
func TestParallelLabelRefusesRowsWithoutASuffix(t *testing.T) {
	f := recording(t, "")
	label, err := parallelGoroutines(f)
	if err == nil {
		t.Fatalf("rows with no -N suffix produced the label %q", label)
	}
	if !strings.Contains(err.Error(), "-GOMAXPROCS suffix") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

// A recording with no parallel arm at all has nothing to label.
func TestParallelLabelRefusesARecordingWithoutTheParallelArm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport-serial-only.txt")
	row := "BenchmarkCrossoverSerial/RawCgo/noop-18\t1000\t16.0 ns/op\n"
	if err := os.WriteFile(path, []byte(row+row), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := benchfile.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if label, err := parallelGoroutines(f); err == nil {
		t.Fatalf("a serial-only recording produced the label %q", label)
	}
}

// The committed recording was made at GOMAXPROCS 18, and the rendered docs say
// so: this is the byte-identity `make docs-check` holds the generated block to,
// asserted on the label alone so that a failure points here and not at a diff.
func TestParallelLabelOfTheCommittedRecordingIs18(t *testing.T) {
	hits, err := filepath.Glob(filepath.Join("..", "..", resultsDir, "transport-*.txt"))
	if err != nil || len(hits) != 1 {
		t.Fatalf("want exactly one committed transport recording, got %v (err %v)", hits, err)
	}
	f, err := benchfile.Parse(hits[0])
	if err != nil {
		t.Fatal(err)
	}
	label, err := parallelGoroutines(f)
	if err != nil {
		t.Fatal(err)
	}
	if label != "18 goroutines" {
		t.Fatalf("label = %q, want %q", label, "18 goroutines")
	}
}
