// Command benchgate holds the two CI benchmark gates that used to be a grep.
//
//	benchgate allocs -max Name=N [-max Name=N ...] results.txt
//	benchgate regress -threshold PCT [-min-samples N] [-allow-one-sided Name]... benchstat.csv
//
// allocs reads raw `go test -bench` output and fails when any sample of a named
// benchmark reports more allocs/op than its bound, or when a named benchmark has
// no sample at all. The step it replaced ran `benchstat | grep ChannelHop`, which
// matched the benchmark's name and never looked at a number, so it passed at any
// allocation count. A benchmark that did not run is a failure, not a pass.
//
// regress reads `benchstat -format csv old new` and fails on a sec/op row that
// is slower by more than the threshold AND whose confidence intervals do not
// overlap. benchstat's own significance test (the "~" it prints for a row it
// cannot distinguish from noise) is honoured first. The interval test is on top
// of it: a +14.55% row with ±9% and ±7% dispersion was significant by p-value
// alone and failed a commit that only touched tools/. The geomean summary row is
// not a benchmark and is never gated. A row present on only one side (benchstat
// emits 3 fields for base-only and 5 for new-only, against 7 for a comparison)
// is judged by which side it is on. Base-only means a benchmark was dropped or
// renamed, so its replacement is not guarded: it fails unless its name was
// passed to -allow-one-sided (the default list is empty). New-only is a
// benchmark the change adds, which the base cannot have: it is reported as a
// ::notice and not compared. Failing it turned every push that added a
// benchmark red, with no allow path a push could use (Matrix 39270656716). A
// rename is a base-only row plus a new-only one, so it still fails. A package
// only one file has rows for gets a table of its own that names just that
// file, and its rows have 3 fields on either side; the table's header says the
// side, placed against the first header that names both files. A table that
// cannot be placed fails as base-only. Allow-list
// names match with or without the runner's -N GOMAXPROCS suffix. A one-sided
// row does not end the sec/op table: a later comparison is still gated. A
// renamed benchmark used to clear the table and a later regression passed.
// -min-samples (default 6, the least benchstat can give an interval for) fails
// any compared row whose smaller side has fewer samples: at n=1 every row is
// "~" and the gate used to print "no regression" having compared nothing.
package main

import (
	"bufio"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: benchgate allocs|regress ...")
		return 2
	}
	var err error
	switch args[0] {
	case "allocs":
		err = runAllocs(args[1:], stdout)
	case "regress":
		err = runRegress(args[1:], stdout)
	default:
		fmt.Fprintf(stderr, "benchgate: unknown subcommand %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "benchgate: %v\n", err)
		return 1
	}
	return 0
}

type bounds map[string]float64

func (b bounds) String() string { return fmt.Sprint(map[string]float64(b)) }

func (b bounds) Set(v string) error {
	name, num, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("-max %q: want Name=N", v)
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("-max %q: bound must be a non-negative number", v)
	}
	b[name] = n
	return nil
}

func runAllocs(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("allocs", flag.ContinueOnError)
	limits := bounds{}
	fs.Var(limits, "max", "Name=N: the most allocs/op any sample of Name may report (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || len(limits) == 0 {
		return errors.New("usage: benchgate allocs -max Name=N [...] results.txt")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	return checkAllocs(f, limits, stdout)
}

// benchLine matches one result row. The trailing -N is GOMAXPROCS.
var benchLine = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+.*?([0-9.]+)\s+allocs/op`)

func checkAllocs(r io.Reader, limits bounds, stdout io.Writer) error {
	worst := map[string]float64{}
	samples := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		m := benchLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		name := strings.TrimPrefix(m[1], "Benchmark")
		if _, tracked := limits[name]; !tracked {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return fmt.Errorf("%s: unreadable allocs/op %q", name, m[2])
		}
		samples[name]++
		if v > worst[name] {
			worst[name] = v
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	var failures []string
	for name, bound := range limits {
		switch {
		case samples[name] == 0:
			failures = append(failures, fmt.Sprintf("%s: no allocs/op sample (not run, or run without -benchmem/ReportAllocs); a gate that did not run is not a pass", name))
		case worst[name] > bound:
			failures = append(failures, fmt.Sprintf("%s: %v allocs/op exceeds the bound of %v", name, worst[name], bound))
		default:
			fmt.Fprintf(stdout, "ok   %s: worst %v allocs/op over %d samples (bound %v)\n", name, worst[name], samples[name], bound)
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(sorted(failures), "\n"))
	}
	return nil
}

// nameSet is the repeatable -allow-one-sided list. Empty is fail closed:
// a benchmark on only one side of the comparison is a failure.
type nameSet map[string]struct{}

func (s nameSet) String() string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func (s nameSet) Set(v string) error {
	if v == "" {
		return errors.New("empty benchmark name")
	}
	s[v] = struct{}{}
	return nil
}

func runRegress(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("regress", flag.ContinueOnError)
	threshold := fs.Float64("threshold", 10, "percent slower, beyond which a significant row fails")
	minSamples := fs.Int("min-samples", 6, "fewest samples either side of a compared row may have; below it the gate fails (benchstat cannot give a confidence interval under 6)")
	allow := nameSet{}
	fs.Var(allow, "allow-one-sided", "benchmark present on only one side; repeatable, default none (a renamed benchmark fails)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: benchgate regress -threshold PCT [-min-samples N] [-allow-one-sided Name]... benchstat.csv")
	}
	if *minSamples < 1 {
		return fmt.Errorf("-min-samples %d: must be at least 1", *minSamples)
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	return checkRegress(f, *threshold, *minSamples, allow, stdout)
}

func parsePercent(s string) (float64, bool) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	v, err := strconv.ParseFloat(strings.TrimPrefix(s, "+"), 64)
	return v, err == nil
}

func checkRegress(r io.Reader, threshold float64, minSamples int, allowOneSided map[string]struct{}, stdout io.Writer) error {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	records, err := cr.ReadAll()
	if err != nil {
		return fmt.Errorf("benchstat csv: %w", err)
	}
	base, head, placed := fileSides(records)
	inTable := false
	// tableSide is set by a table header that names one file: benchstat prints
	// a package that only one file has rows for as a table of its own, whose
	// rows have 3 fields on either side. It says which side that table is on.
	tableSide := sideUnknown
	rows := 0
	var failures []string
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		if rec[0] == "" {
			// A header row: the first column is blank. Only the sec/op table
			// is gated; B/op and allocs/op follow it in the same file. A short
			// header is still a header, not a one-sided benchmark.
			if names, ok := fileHeader(rec); ok {
				tableSide = sideUnknown
				if len(names) == 1 && placed {
					switch names[0] {
					case head:
						tableSide = sideHead
					case base:
						tableSide = sideBase
					}
				}
			}
			inTable = len(rec) > 1 && rec[1] == "sec/op"
			continue
		}
		if len(rec) == 1 && strings.Contains(rec[0], ": ") {
			// A configuration line between tables (pkg:, goos:, cpu:). It is
			// not a benchmark row: one of those has at least 3 fields. Without
			// -benchmem the next package's pkg: line follows a sec/op table
			// directly and was read as a base-only benchmark.
			inTable = false
			continue
		}
		if !inTable || rec[0] == "geomean" {
			continue
		}
		// A comparison row has 7 fields. Fewer is a benchmark on only one
		// side. It must not clear inTable: that used to end the sec/op table
		// so a later regression was never read, while rows > 0 still passed.
		if len(rec) < 7 {
			switch {
			case allowed(allowOneSided, rec[0]):
			case tableSide == sideHead || (tableSide != sideBase && newOnly(rec)):
				fmt.Fprintf(stdout, "::notice title=New benchmark::%s has no base sample, so it was not compared\n", rec[0])
			default:
				failures = append(failures, fmt.Sprintf("%s: base-only sec/op row (%d fields); a renamed or dropped benchmark is not a pass (allow it with -allow-one-sided)", rec[0], len(rec)))
			}
			continue
		}
		rows++
		// Checked before the "~" skip: with one sample per side benchstat
		// calls every row "~", which is exactly the case this guards.
		if lo, ok := minSampleCount(rec[6]); !ok {
			failures = append(failures, fmt.Sprintf("%s: no sample count in %q; cannot show the comparison had %d samples per side", rec[0], rec[6], minSamples))
			continue
		} else if lo < minSamples {
			failures = append(failures, fmt.Sprintf("%s: %d sample(s) on the smaller side, fewer than -min-samples %d; a comparison that thin reports \"~\" for everything and is not a pass", rec[0], lo, minSamples))
			continue
		}
		delta := strings.TrimSpace(rec[5])
		if delta == "~" || delta == "" {
			continue
		}
		pct, ok := parsePercent(delta)
		if !ok {
			return fmt.Errorf("%s: unreadable delta %q", rec[0], delta)
		}
		if pct <= threshold {
			continue
		}
		oldC, e1 := strconv.ParseFloat(rec[1], 64)
		newC, e2 := strconv.ParseFloat(rec[3], 64)
		oldCI, ok1 := parsePercent(rec[2])
		newCI, ok2 := parsePercent(rec[4])
		if e1 != nil || e2 != nil || !ok1 || !ok2 {
			return fmt.Errorf("%s: slower by %s but its confidence interval is unreadable (%q, %q); not enough samples to call it noise or a regression", rec[0], delta, rec[2], rec[4])
		}
		oldHigh := oldC * (1 + oldCI/100)
		newLow := newC * (1 - newCI/100)
		if newLow <= oldHigh {
			fmt.Fprintf(stdout, "noise %s: %s but intervals overlap (old %v ± %v%%, new %v ± %v%%)\n", rec[0], delta, oldC, oldCI, newC, newCI)
			continue
		}
		failures = append(failures, fmt.Sprintf("%s: %s slower with non-overlapping intervals (old %v ± %v%%, new %v ± %v%%), beyond %v%%", rec[0], delta, oldC, oldCI, newC, newCI, threshold))
	}
	// One-sided rows first. A file whose only sec/op data is one-sided still
	// names that benchmark; "no sec/op rows" stays the result when nothing
	// was classified as a failure and nothing was compared.
	if len(failures) > 0 {
		return errors.New(strings.Join(sorted(failures), "\n"))
	}
	if rows == 0 {
		return errors.New("no sec/op rows in the benchstat output; the comparison did not happen")
	}
	fmt.Fprintf(stdout, "no regression beyond %v%% across %d benchmarks\n", threshold, rows)
	return nil
}

// sampleCount matches benchstat's "n=10" (equal sides) or "n=8+7" (unequal).
var sampleCount = regexp.MustCompile(`\bn=(\d+)(?:\+(\d+))?`)

// minSampleCount returns the smaller side's sample count from a P column such
// as "p=0.001 n=8+7".
func minSampleCount(p string) (int, bool) {
	m := sampleCount.FindStringSubmatch(p)
	if m == nil {
		return 0, false
	}
	lo, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	if m[2] != "" {
		hi, err := strconv.Atoi(m[2])
		if err != nil {
			return 0, false
		}
		lo = min(lo, hi)
	}
	return lo, true
}

// newOnly reports a one-sided row whose base columns are empty: benchstat's
// shape for a benchmark the new side has and the base does not.
// Which file a single-file benchstat table belongs to.
const (
	sideUnknown = iota
	sideBase
	sideHead
)

// fileHeader reports whether rec is the row of a benchstat table that names its
// files, and returns those names. It is the header row above the unit row: a
// blank first column and file names, where the unit row carries "CI" cells.
func fileHeader(rec []string) ([]string, bool) {
	if len(rec) < 2 || rec[0] != "" {
		return nil, false
	}
	var names []string
	for _, c := range rec[1:] {
		c = strings.TrimSpace(c)
		if c == "CI" {
			return nil, false
		}
		if c != "" {
			names = append(names, c)
		}
	}
	return names, len(names) > 0
}

// fileSides takes the base and head file names from the first table header
// that names two files, wherever it is: benchstat's table order is not the
// order the files were written in, so a single-file table can come first.
// With no such header nothing places a single-file table, and its rows are
// judged by their shape alone, which fails closed.
func fileSides(records [][]string) (base, head string, ok bool) {
	for _, rec := range records {
		if names, isHeader := fileHeader(rec); isHeader && len(names) == 2 {
			return names[0], names[1], true
		}
	}
	return "", "", false
}

func newOnly(rec []string) bool {
	return len(rec) >= 4 && strings.TrimSpace(rec[1]) == "" && strings.TrimSpace(rec[2]) == "" &&
		strings.TrimSpace(rec[3]) != ""
}

// procsSuffix is the -N GOMAXPROCS suffix go test appends to a benchmark name.
var procsSuffix = regexp.MustCompile(`-\d+$`)

// allowed matches name against the allow list with and without its -N suffix,
// so an allow entry does not depend on the runner's core count.
func allowed(allow map[string]struct{}, name string) bool {
	if _, ok := allow[name]; ok {
		return true
	}
	_, ok := allow[procsSuffix.ReplaceAllString(name, "")]
	return ok
}

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}
