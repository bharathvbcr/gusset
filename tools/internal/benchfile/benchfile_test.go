package benchfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A clean recording: two arms, each written by its own process, each cycling
// through its sub-benchmarks once per -count repetition. Sub-benchmark names
// repeat non-contiguously inside an arm and that is normal, which is why the
// single-writer check groups by arm rather than by full name.
//
// Every sub-benchmark appears the same number of times, because `-count=N`
// gives every sub-benchmark N repetitions — verified against a real run, where
// all five work sizes recorded exactly 10 rows.
const cleanTransport = `goos: darwin
goarch: arm64
BenchmarkCrossoverSerial/RawCgo/noop-18   	10000000	       118.0 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/RawCgo/1000it-18 	 1000000	      1180 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/RawCgo/noop-18   	10000000	       119.0 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/RawCgo/1000it-18 	 1000000	      1190 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/Gusset/noop-18   	  100000	     15600 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/Gusset/1000it-18 	  100000	     15700 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/Gusset/noop-18   	  100000	     15800 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/Gusset/1000it-18 	  100000	     15900 ns/op	       0 B/op	       0 allocs/op
PASS
`

const goodHeader = `# gusset-bench: v1
# gusset-bench-recorded: 2026-09-20T21:10:03Z
# gusset-bench-host: testhost
# gusset-bench-cpu-idle-before: 80.1
# gusset-bench-cpu-idle-after: 79.4
# gusset-bench-min-idle: 60
# gusset-bench-exclusive: yes
`

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "results.txt")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustReject(t *testing.T, body string, want ...string) error {
	t.Helper()
	_, err := Parse(writeTemp(t, body))
	if err == nil {
		t.Fatal("contaminated recording accepted; it would have been charted")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error does not mention %q: %v", w, err)
		}
	}
	return err
}

func TestParseAcceptsCleanRecording(t *testing.T) {
	f, err := Parse(writeTemp(t, cleanTransport))
	if err != nil {
		t.Fatalf("clean recording rejected: %v", err)
	}
	// Repetitions must still accumulate, otherwise a median has nothing to
	// filter an outlier out of.
	if n := len(f.ByName["BenchmarkCrossoverSerial/RawCgo/noop"]); n != 2 {
		t.Errorf("noop repetitions = %d, want 2", n)
	}
	if got := f.ByName["BenchmarkCrossoverSerial/Gusset/noop"][0]["ns/op"]; got != 15600 {
		t.Errorf("first Gusset noop sample = %v, want 15600", got)
	}
}

// The file that actually happened: two `make bench-crossover` runs teeing into
// one results file while a third ran beside them. The previous parser read it
// without complaint — it skipped every line it could not understand and charted
// what was left.
func TestParseRejectsTheRealSplicedRecording(t *testing.T) {
	_, err := Parse(filepath.Join("testdata", "spliced-real.txt"))
	if err == nil {
		t.Fatal("the real spliced recording was accepted; this is the file that " +
			"produced the committed charts")
	}
	t.Logf("rejected with: %v", err)
}

// Two `make bench-crossover` runs teeing into one file, trimmed to the arms
// that reveal it.
func TestParseRejectsInterleavedArms(t *testing.T) {
	_, err := Parse(filepath.Join("testdata", "interleaved.txt"))
	if err == nil {
		t.Fatal("interleaved recording accepted; concurrent writers went undetected")
	}
	for _, want := range []string{
		"BenchmarkCrossoverSerial/Gusset",
		"BenchmarkCrossoverParallel/RawCgo",
		"two benchmark",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A single arm can never trip the single-writer check, however its
// sub-benchmarks are ordered: the scaling and thread-pressure targets record one
// transport per file, and a false positive there would fail `make docs-check` on
// honest data.
func TestParseAcceptsSingleArmInAnyOrder(t *testing.T) {
	body := `BenchmarkThreadScaling/RawCgo/2048-18 	       3	 989000000 ns/op	     310 peakThreads
BenchmarkThreadScaling/RawCgo/32-18   	       3	  33000000 ns/op	      33 peakThreads
BenchmarkThreadScaling/RawCgo/2048-18 	       3	 952000000 ns/op	     309 peakThreads
BenchmarkThreadScaling/RawCgo/32-18   	       3	  34000000 ns/op	      33 peakThreads
`
	if _, err := Parse(writeTemp(t, body)); err != nil {
		t.Fatalf("single-arm recording rejected: %v", err)
	}
}

// The splice no structural check can see: one arm throughout, well-formed
// lines, correct field count — and a row whose numbers came from a different
// benchmark. Only the metric shape gives it away, because the serial arm never
// calls ReportMetric and so can never emit Δthreads.
//
// 38 of the 163 rows in the observed file were this exact shape.
func TestParseRejectsMetricShapeSplice(t *testing.T) {
	body := `BenchmarkCrossoverSerial/RawCgo/noop-18 	10000000	      16.22 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/RawCgo/noop-18 	10000000	      16.13 ns/op	       0 B/op	       0 allocs/op
BenchmarkCrossoverSerial/RawCgo/noop-18 	  250299	      4573 ns/op	       0 Δthreads	     165 B/op	       2 allocs/op
`
	mustReject(t, body, "Δthreads", "different benchmarks", "spliced")
}

// The other half of the same splice: `go test` had printed the name and not yet
// the numbers when the second process wrote a whole line of its own. The
// previous parser dropped this line silently, which is why a file full of them
// still produced a chart.
func TestParseRejectsLineCutMidWrite(t *testing.T) {
	body := `BenchmarkCrossoverSerial/RawCgo/noop-18         	goos: darwin
BenchmarkCrossoverSerial/RawCgo/noop-18 	10000000	      16.13 ns/op
`
	mustReject(t, body, "at least 4")
}

func TestParseRejectsTwoNamesOnOneLine(t *testing.T) {
	body := `BenchmarkCrossoverSerial/RawCgo/noop-18 	BenchmarkCrossoverParallel/Gusset/noop-18 	  261812	      4594 ns/op
`
	mustReject(t, body, "2 benchmark names on one line", "two processes")
}

func TestParseRejectsNonNumericIterationCount(t *testing.T) {
	body := `BenchmarkCrossoverSerial/RawCgo/noop-18 	notanumber	      16.13 ns/op	       0 B/op
`
	mustReject(t, body, "is not a number", "cut mid-write")
}

// An arm cut short partway through its repetitions, which is what an
// interrupted recording leaves behind. The survivors' median is quietly built
// on fewer samples than the arm it is compared against.
func TestParseRejectsUnevenArm(t *testing.T) {
	body := `BenchmarkCrossoverSerial/RawCgo/noop-18   	10000000	       118.0 ns/op
BenchmarkCrossoverSerial/RawCgo/1000it-18 	 1000000	      1180 ns/op
BenchmarkCrossoverSerial/RawCgo/noop-18   	10000000	       119.0 ns/op
`
	mustReject(t, body, "uneven", "cut short")
}

func TestParseRejectsEmptyFile(t *testing.T) {
	mustReject(t, "goos: darwin\nPASS\n", "no benchmark lines")
}

// RequireProvenance is the check the structural ones cannot replace: a single
// benchmark process running beside an unrelated build produces a perfectly
// consistent file in which every sample is uniformly wrong.
func TestRequireProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "absent",
			header: "",
			want:   "no provenance header",
		},
		{
			name: "not exclusive",
			header: `# gusset-bench-cpu-idle-before: 80.1
# gusset-bench-cpu-idle-after: 79.4
# gusset-bench-min-idle: 60
# gusset-bench-exclusive: no
`,
			want: "without exclusive use",
		},
		{
			name: "idle below the floor it claims to enforce",
			header: `# gusset-bench-cpu-idle-before: 80.1
# gusset-bench-cpu-idle-after: 0.9
# gusset-bench-min-idle: 60
# gusset-bench-exclusive: yes
`,
			want: "0.9% idle after the run",
		},
		{
			name: "no floor recorded",
			header: `# gusset-bench-cpu-idle-before: 80.1
# gusset-bench-cpu-idle-after: 79.4
# gusset-bench-exclusive: yes
`,
			want: "does not say what idle floor",
		},
		{
			name: "no idle reading",
			header: `# gusset-bench-min-idle: 60
# gusset-bench-exclusive: yes
`,
			want: "no CPU idle reading",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse(writeTemp(t, tc.header+cleanTransport))
			if err != nil {
				t.Fatalf("structural parse failed: %v", err)
			}
			err = f.RequireProvenance()
			if err == nil {
				t.Fatal("accepted a recording with no proof the machine was quiet")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %q: %v", tc.want, err)
			}
		})
	}
}

func TestRequireProvenanceAcceptsACertifiedRecording(t *testing.T) {
	f, err := Parse(writeTemp(t, goodHeader+cleanTransport))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if err := f.RequireProvenance(); err != nil {
		t.Fatalf("certified recording rejected: %v", err)
	}
	if f.Env.Host != "testhost" {
		t.Errorf("host = %q, want testhost", f.Env.Host)
	}
	if f.Env.IdleBefore != 80.1 || f.Env.IdleAfter != 79.4 {
		t.Errorf("idle = %v/%v, want 80.1/79.4", f.Env.IdleBefore, f.Env.IdleAfter)
	}
}

// benchstat reads these same files. A `key: value` line would be parsed as
// configuration and would split one measurement into separate columns, so the
// header has to be something benchstat ignores — which is why it is a comment.
// This pins the shape so nobody "tidies" it into a config line.
func TestProvenanceIsACommentBenchstatIgnores(t *testing.T) {
	for _, line := range strings.Split(strings.TrimSpace(goodHeader), "\n") {
		if !strings.HasPrefix(line, "#") {
			t.Errorf("provenance line is not a comment, benchstat will group by it: %q", line)
		}
	}
}

func TestArm(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"BenchmarkCrossoverSerial/Gusset/1000it-709ns", "BenchmarkCrossoverSerial/Gusset"},
		{"BenchmarkCrossoverParallel/RawCgo/noop", "BenchmarkCrossoverParallel/RawCgo"},
		{"BenchmarkThreadScaling/Gusset/512", "BenchmarkThreadScaling/Gusset"},
		{"BenchmarkThreadPressure/10000000it/Gusset", "BenchmarkThreadPressure/10000000it"},
		{"BenchmarkSomethingFlat", "BenchmarkSomethingFlat"},
	} {
		if got := Arm(tc.name); got != tc.want {
			t.Errorf("Arm(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
