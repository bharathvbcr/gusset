// Package benchfile reads committed `go test -bench` output and refuses any
// file that cannot be trusted to describe one quiet machine.
//
// It exists because a contaminated benchmark file and a clean one look
// identical to a parser that only wants numbers. Both parse. Both yield medians
// with tight-looking spreads. docs/img/*.svg and the README table are generated
// from these files, so a lenient parser turns a broken recording run into a
// published performance claim.
//
// The damage this package is written against was observed, not imagined. Two
// recordings ran at once — two working sessions on one checkout, which needs no
// mishap to explain — and both appended to one file through `tee -a`, while a
// third benchmark process competed for the same 18 cores. `go test` prints a
// benchmark's name,
// then runs it, then prints its numbers onto that same line — so the second
// writer spliced itself between the two. 38 of 163 rows carried a metric the
// named benchmark never reports, one row claimed a no-op cgo call took
// 91,916 ns, and the file's own arms disagreed by 20% about how long an
// identical Rust loop takes. The previous parser skipped every malformed line
// and charted the rest.
//
// The checks are ordered cheapest-first and all of them are fatal. A results
// file is a performance claim; there is no such thing as mostly trustworthy.
package benchfile

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// provenanceKey prefixes the header lines bench/record.sh writes.
//
// A comment rather than a `key: value` configuration line, because benchstat
// reads these same files and groups by configuration: a per-run value like CPU
// idle would split one measurement into two columns. benchstat ignores lines it
// cannot parse as configuration, which was verified against the installed
// binary rather than assumed.
const provenanceKey = "# gusset-bench"

// Sample is one benchmark line's metrics, keyed by unit ("ns/op", "peakThreads").
type Sample map[string]float64

// Row is one benchmark line, kept in file order so interleaving is detectable.
type Row struct {
	Name   string // GOMAXPROCS suffix stripped
	Arm    string // shape and transport, without the work size
	Sample Sample
	Line   int // 1-based, for error messages that point at the damage
}

// Env is what bench/record.sh certified about the machine at recording time.
type Env struct {
	Recorded   string
	Host       string
	Uname      string
	IdleBefore float64
	IdleAfter  float64
	MinIdle    float64
	Exclusive  bool
	Raw        map[string]string
}

// File is a parsed, structurally validated results file.
type File struct {
	Path   string
	Env    *Env // nil when the file carries no provenance header
	Rows   []Row
	ByName map[string][]Sample
}

// Arm reduces a benchmark name to the arm that produced it: the shape and
// transport, without the work size. BenchmarkCrossoverSerial/Gusset/1000it-709ns
// becomes BenchmarkCrossoverSerial/Gusset.
func Arm(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) < 2 {
		return parts[0]
	}
	return parts[0] + "/" + parts[1]
}

// Parse reads a results file and runs every structural check on it.
//
// It does not check provenance — call RequireProvenance for that. The split is
// so tests can exercise the structural checks against fixtures captured before
// the header existed, and so a caller can report the two failures differently.
func Parse(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	f := &File{Path: path, ByName: map[string][]Sample{}}
	for i, line := range strings.Split(string(raw), "\n") {
		lineNo := i + 1
		if strings.HasPrefix(line, provenanceKey) {
			f.readProvenance(line)
			continue
		}
		if !strings.HasPrefix(line, "Benchmark") {
			continue
		}
		row, err := parseRow(line, lineNo)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		f.Rows = append(f.Rows, row)
		f.ByName[row.Name] = append(f.ByName[row.Name], row.Sample)
	}

	if len(f.Rows) == 0 {
		return nil, fmt.Errorf("%s: no benchmark lines; run the recording target", path)
	}
	if err := f.checkSingleWriter(); err != nil {
		return nil, err
	}
	if err := f.checkMetricShape(); err != nil {
		return nil, err
	}
	if err := f.checkSampleCounts(); err != nil {
		return nil, err
	}
	return f, nil
}

// parseRow turns one `Benchmark...` line into a Row, rejecting anything that is
// not the shape `go test` emits.
//
// The old parser skipped a line it could not read. That is what let a spliced
// file through: the splice victim — a bare name followed by the next process's
// "goos: darwin" — was silently dropped, and the rows that happened to land
// intact were charted as if nothing had happened.
func parseRow(line string, lineNo int) (Row, error) {
	fields := strings.Fields(line)

	// A line carrying two benchmark names is two processes' output spliced
	// together. There is no way to attribute the numbers, so there is no
	// repairing it.
	names := 0
	for _, fld := range fields {
		if strings.HasPrefix(fld, "Benchmark") {
			names++
		}
	}
	if names > 1 {
		return Row{}, fmt.Errorf("%d benchmark names on one line — two processes wrote this "+
			"file at once, so no number on it can be attributed to a benchmark: %s",
			names, truncate(line))
	}

	if len(fields) < 4 {
		return Row{}, fmt.Errorf("benchmark line has %d fields, want at least 4 "+
			"(name, iterations, value, unit): %s", len(fields), truncate(line))
	}

	// Field 1 is the iteration count. When it is not a number the line was cut
	// mid-write by another writer, which is the splice this catches directly:
	// "BenchmarkCrossoverSerial/RawCgo/noop-18 \t goos: darwin".
	iters, err := strconv.Atoi(fields[1])
	if err != nil {
		return Row{}, fmt.Errorf("iteration count %q is not a number — the line was cut "+
			"mid-write by a second writer: %s", fields[1], truncate(line))
	}
	if iters <= 0 {
		return Row{}, fmt.Errorf("iteration count is %d: %s", iters, truncate(line))
	}

	name := strings.TrimSuffix(fields[0], ":")
	// Strip the trailing -GOMAXPROCS suffix so names are stable across machines.
	if i := strings.LastIndex(name, "-"); i > 0 {
		if _, err := strconv.Atoi(name[i+1:]); err == nil {
			name = name[:i]
		}
	}

	s := Sample{}
	for i := 2; i+1 < len(fields); i += 2 {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return Row{}, fmt.Errorf("metric value %q is not a number: %s",
				fields[i], truncate(line))
		}
		s[fields[i+1]] = v
	}
	if len(s) == 0 {
		return Row{}, fmt.Errorf("no (value, unit) pairs: %s", truncate(line))
	}

	return Row{Name: name, Arm: Arm(name), Sample: s, Line: lineNo}, nil
}

// checkSingleWriter rejects a results file that two benchmark processes wrote
// at once.
//
// `go test` emits its lines sequentially and the recording script runs one arm
// per process, so in an honest file every arm occupies one unbroken run of
// lines. An arm re-entered after a different arm intervened cannot come from
// one producer.
//
// Two concurrent runs also compete for the same cores, so the samples they
// interleave are wrong as well as jumbled: one such file recorded 7.2 µs and
// 46 µs for the same sub-benchmark. Nothing about those numbers looks
// malformed — they would have gone straight into a chart.
func (f *File) checkSingleWriter() error {
	closed := map[string]bool{}
	prev := ""
	for _, row := range f.Rows {
		if row.Arm == prev {
			continue
		}
		if closed[row.Arm] {
			return fmt.Errorf("%s:%d: benchmark arm %q resumes after %q — two benchmark "+
				"processes wrote this file at once, so its timings are contaminated as "+
				"well as out of order. Delete it and re-record with nothing else on the "+
				"machine", f.Path, row.Line, row.Arm, prev)
		}
		if prev != "" {
			closed[prev] = true
		}
		prev = row.Arm
	}
	return nil
}

// checkMetricShape rejects a file in which one benchmark reported different
// metrics on different repetitions.
//
// Which metrics a benchmark reports is fixed in its source: ReportAllocs and
// ReportMetric are called unconditionally or not at all. So repetitions of one
// benchmark must agree on the set of units, and when they do not, the rows were
// assembled from different benchmarks' output.
//
// This is the check that catches a splice the structural checks cannot: a line
// whose name came from one process and whose numbers came from another is
// perfectly well-formed. In the observed file, 38 rows named
// BenchmarkCrossoverSerial/... carried a Δthreads metric that only the Parallel
// arm reports, and 38 RawCgo rows reported allocations for a body that
// allocates nothing.
func (f *File) checkMetricShape() error {
	type shapeRef struct {
		shape string
		line  int
	}
	seen := map[string]shapeRef{}
	for _, row := range f.Rows {
		units := make([]string, 0, len(row.Sample))
		for u := range row.Sample {
			units = append(units, u)
		}
		sort.Strings(units)
		shape := strings.Join(units, ", ")

		first, ok := seen[row.Name]
		if !ok {
			seen[row.Name] = shapeRef{shape: shape, line: row.Line}
			continue
		}
		if first.shape != shape {
			return fmt.Errorf("%s: benchmark %q reports [%s] on line %d but [%s] on line %d. "+
				"A benchmark's metrics are fixed in its source, so these rows came from "+
				"different benchmarks: another process spliced its output into this "+
				"file between a name and its numbers. Re-record it",
				f.Path, row.Name, first.shape, first.line, shape, row.Line)
		}
	}
	return nil
}

// checkSampleCounts rejects a file whose arm was cut short.
//
// Every sub-benchmark in one arm runs the same -count, so they finish with the
// same number of repetitions. A short one means the process died partway —
// which is what an interrupted recording leaves behind, and what makes the
// median of the survivors quietly less reliable than the others.
func (f *File) checkSampleCounts() error {
	type countRef struct {
		name  string
		count int
	}
	perArm := map[string]countRef{}
	counts := map[string]int{}
	for _, row := range f.Rows {
		counts[row.Name]++
	}
	// Sorted, so the error names the same pair on every run.
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		arm := Arm(name)
		ref, ok := perArm[arm]
		if !ok {
			perArm[arm] = countRef{name: name, count: counts[name]}
			continue
		}
		if ref.count != counts[name] {
			return fmt.Errorf("%s: arm %q is uneven — %q has %d repetitions but %q has %d. "+
				"Every sub-benchmark in an arm runs the same -count, so this recording "+
				"was cut short. Re-record it",
				f.Path, arm, ref.name, ref.count, name, counts[name])
		}
	}
	return nil
}

// readProvenance accumulates one `# gusset-bench-...: value` header line.
func (f *File) readProvenance(line string) {
	body := strings.TrimPrefix(line, provenanceKey)
	body = strings.TrimPrefix(body, "-")
	key, value, ok := strings.Cut(body, ":")
	if !ok {
		return
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	if f.Env == nil {
		// -1, not the zero value: an absent reading must not be
		// indistinguishable from a measured 0% idle. That is the same failure
		// this package exists to prevent, one level down — and a header that
		// simply omitted the field would otherwise have read as the worst
		// possible machine, or, had the comparison been the other way, as a
		// perfect one.
		f.Env = &Env{Raw: map[string]string{}, IdleBefore: -1, IdleAfter: -1, MinIdle: -1}
	}
	f.Env.Raw[key] = value

	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return -1
		}
		return v
	}
	switch key {
	case "recorded":
		f.Env.Recorded = value
	case "host":
		f.Env.Host = value
	case "uname":
		f.Env.Uname = value
	case "cpu-idle-before":
		f.Env.IdleBefore = num(value)
	case "cpu-idle-after":
		f.Env.IdleAfter = num(value)
	case "min-idle":
		f.Env.MinIdle = num(value)
	case "exclusive":
		f.Env.Exclusive = value == "yes"
	}
}

// RequireProvenance refuses a file that does not carry proof of the conditions
// it was recorded under.
//
// This is the check the others cannot replace. Every structural check above
// asks whether the file is internally consistent; a single benchmark process
// running beside an unrelated build produces a file that is perfectly
// consistent and completely wrong — every sample in the affected arm is
// uniformly inflated, so the spread stays tight and the median looks
// authoritative.
//
// The only defence is for the recorder to certify what it checked and for the
// consumer to refuse data that carries no certificate. A check that could not
// run must never report the same result as a check that ran and passed.
func (f *File) RequireProvenance() error {
	if f.Env == nil {
		return fmt.Errorf("%s: no provenance header. It was recorded by hand or by a "+
			"version of the recording target that could not certify the machine was "+
			"idle and exclusive, so there is no way to tell whether its numbers describe "+
			"this code or whatever else was running. Re-record it with bench/record.sh "+
			"(via `make bench-crossover` / `make bench-scaling`)", f.Path)
	}
	if !f.Env.Exclusive {
		return fmt.Errorf("%s: recorded without exclusive use of the machine", f.Path)
	}
	floor := f.Env.MinIdle
	if floor <= 0 {
		return fmt.Errorf("%s: provenance header does not say what idle floor was "+
			"enforced, so the recorded idle figures cannot be judged", f.Path)
	}
	for _, c := range []struct {
		when  string
		value float64
	}{
		{"before the run", f.Env.IdleBefore},
		{"after the run", f.Env.IdleAfter},
	} {
		if c.value < 0 {
			return fmt.Errorf("%s: provenance header has no CPU idle reading %s", f.Path, c.when)
		}
		if c.value < floor {
			return fmt.Errorf("%s: CPU was %.1f%% idle %s, below the %.0f%% floor this "+
				"recording claims to enforce; the numbers describe the load, not the code",
				f.Path, c.value, c.when, floor)
		}
	}
	return nil
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 110 {
		return s[:110] + "..."
	}
	return s
}
