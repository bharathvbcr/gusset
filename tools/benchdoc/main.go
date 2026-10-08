// Command benchdoc regenerates the README benchmark table from benchstat output.
//
// R15 requires that every performance number in the documentation be generated
// from a benchstat run over committed raw data. It was not being met: `make docs`
// printed "Documentation up to date" and regenerated nothing, and the README's
// figures did not match what benchstat reports for the committed results file —
// the parallel row read 5.07 µs where benchstat's median for that same data is
// 5.237 µs. Hand-transcribed numbers drift silently, and a performance claim
// nobody can reproduce is worse than no claim.
//
// Two modes:
//
//	benchdoc            rewrite the table in README.md
//	benchdoc -check     exit 1 if README.md does not match the generated table
//
// Either takes an optional results file, `benchdoc [-check] <results-file>`,
// which is required when bench/results holds more than one.
//
// `-check` is what CI runs, so a change to the benchmarks that is not reflected in
// the README fails the build instead of quietly making the README wrong.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bharathvbcr/gusset/tools/internal/benchfile"
	"github.com/bharathvbcr/gusset/tools/internal/docblock"
)

const (
	readmePath = "README.md"
	resultsDir = "bench/results"

	// The generated block is delimited so the surrounding prose stays
	// hand-written. tools/internal/docblock owns the marker mechanics, shared
	// with tools/benchplot.
	blockName = "BENCHDOC"
)

// row is one benchmark's summary across the three benchstat sections.
type row struct {
	name    string
	secOp   string
	bytesOp string
	allocs  string
}

// friendly maps a benchmark's Go name onto the label used in the README.
//
// A benchmark with no entry here still appears, under its raw name: an unmapped
// benchmark must show up as an ugly row rather than be silently dropped from the
// table, which is how a regression hides.
var friendly = map[string]string{
	"GussetCallNoop":            "Gusset Call No-op",
	"GussetCallParallel":        "Gusset Call Parallel",
	"GussetSubmitWait":          "Gusset Submit & Wait",
	"GussetBufferLarge":         "Large Buffer (64 KiB, Copy)",
	"GussetBufferLargeZeroCopy": "Large Buffer (64 KiB, Zero-Copy)",
	"ChannelHop":                "Channel Hop Baseline",
}

func main() {
	check := flag.Bool("check", false, "verify README.md is up to date instead of rewriting it")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: benchdoc [-check] [results-file]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	resultsFile, err := resultsFile(resultsDir, flag.Args())
	if err != nil {
		fatal(err)
	}

	// benchstat will summarise anything that parses, including a file two
	// benchmark processes wrote at once and a file recorded on a machine that
	// was busy doing something else. It reports a median and a percentage
	// either way, and the README prints them as this project's performance.
	// Refuse the file before it becomes a published claim.
	f, err := benchfile.Parse(resultsFile)
	if err != nil {
		fatal(err)
	}
	if err := f.RequireProvenance(); err != nil {
		fatal(err)
	}

	out, err := runBenchstat(resultsFile)
	if err != nil {
		fatal(err)
	}

	rows, meta, err := parseBenchstat(out)
	if err != nil {
		fatal(err)
	}
	if len(rows) == 0 {
		fatal(fmt.Errorf("benchstat produced no rows from %s; refusing to write an "+
			"empty table over real numbers", resultsFile))
	}

	table := renderTable(rows, meta, resultsFile)

	readme, err := os.ReadFile(readmePath)
	if err != nil {
		fatal(err)
	}

	updated, err := docblock.Replace(string(readme), readmePath, blockName, table)
	if err != nil {
		fatal(err)
	}

	if *check {
		if updated != string(readme) {
			fmt.Fprintf(os.Stderr,
				"benchdoc: README.md is out of date with %s.\nRun `make docs` and commit the result.\n",
				resultsFile)
			os.Exit(1)
		}
		fmt.Printf("benchdoc: README.md matches benchstat over %s\n", resultsFile)
		return
	}

	if updated == string(readme) {
		fmt.Printf("benchdoc: README.md already matches benchstat over %s\n", resultsFile)
		return
	}
	if err := os.WriteFile(readmePath, []byte(updated), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("benchdoc: regenerated the README table from %s\n", resultsFile)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "benchdoc: %v\n", err)
	os.Exit(1)
}

// resultsFile picks the raw results file to summarise: the one named in args,
// or else the only .txt in dir.
//
// It fails rather than guessing when dir holds more than one and none is named:
// summarising a linux run into a README that claims darwin numbers is exactly
// the kind of quiet wrongness this tool exists to stop. That error used to ask
// for a file to be passed explicitly when the tool read no arguments at all.
func resultsFile(dir string, args []string) (string, error) {
	switch len(args) {
	case 0:
	case 1:
		path := filepath.Clean(args[0])
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("results file: %w", err)
		}
		return path, nil
	default:
		return "", fmt.Errorf("got %d results files %q; the README describes exactly one", len(args), args)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no benchmark results in %s; run `make bench` first", dir)
	}
	sort.Strings(entries)
	if len(entries) == 1 {
		return entries[0], nil
	}
	return "", fmt.Errorf("found %d results files in %s; name one with "+
		"`benchdoc [-check] <results-file>` rather than letting the tool choose which "+
		"platform the README describes", len(entries), dir)
}

func runBenchstat(path string) (string, error) {
	cmd := exec.Command("benchstat", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("benchstat failed: %v\n%s\n"+
			"install it with: go install golang.org/x/perf/cmd/benchstat@latest", err, stderr.String())
	}
	return stdout.String(), nil
}

// parseBenchstat reads benchstat's three sections into rows plus the goos/goarch
// header lines.
//
// Every row in a known section becomes a row of the table, whatever its name.
// This used to keep only names starting with Gusset or Channel, which dropped
// any other benchmark without a word — and was also, by accident, the only
// thing stopping the pkg:/cpu: lines benchstat repeats per package from
// parsing as rows. Those are now recognised as metadata explicitly.
func parseBenchstat(out string) ([]row, []string, error) {
	var (
		rows    []row
		index   = map[string]int{}
		meta    []string
		section string
	)

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if isMeta(trimmed) {
			meta = append(meta, trimmed)
			continue
		}

		// Each section opens with two │-bordered lines: the file name, then the
		// unit. Every such line sets the section, so a unit this table has no
		// column for (a custom b.ReportMetric) reads as "" and its rows are
		// skipped, instead of inheriting the previous section and overwriting
		// that column. Every benchmark reports sec/op, so skipping a custom
		// metric's section never drops a benchmark from the table.
		if strings.HasPrefix(trimmed, "│") {
			section = sectionFor(trimmed)
			continue
		}

		// Skip footnotes, the geomean summary, and anything outside a section.
		if strings.ContainsRune(footnoteMarks, []rune(trimmed)[0]) ||
			strings.HasPrefix(trimmed, "geomean") || section == "" {
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		base := stripProcs(strings.TrimSuffix(fields[0], ":"))

		// benchstat renders "13.43µ ± 6%", or "21.55µ ± ∞ ¹" when the sample was
		// too small for an interval. Keep the whole summary: the dispersion is the
		// part that says whether the point estimate means anything, and dropping
		// it is how a table comes to look more certain than the data behind it.
		value := strings.TrimSpace(strings.Join(fields[1:], " "))
		value = strings.TrimSpace(strings.TrimRight(value, footnoteMarks))

		i, ok := index[base]
		if !ok {
			rows = append(rows, row{name: base})
			i = len(rows) - 1
			index[base] = i
		}
		switch section {
		case "sec":
			rows[i].secOp = value
		case "bytes":
			rows[i].bytesOp = value
		case "allocs":
			rows[i].allocs = value
		}
	}

	for _, r := range rows {
		if r.secOp == "" {
			return nil, nil, fmt.Errorf("benchmark %s has no sec/op summary; the benchstat "+
				"output format has changed and this parser would silently emit a blank cell", r.name)
		}
	}
	return rows, meta, nil
}

// footnoteMarks are the superscripts benchstat attaches to a summary and then
// explains on a line of their own.
const footnoteMarks = "¹²³⁴⁵⁶⁷⁸⁹"

func isMeta(line string) bool {
	for _, k := range []string{"goos:", "goarch:", "pkg:", "cpu:"} {
		if strings.HasPrefix(line, k) {
			return true
		}
	}
	return false
}

// sectionFor names the column a │-bordered header line's unit fills, or "" for
// the file-name line and for units the table has no column for.
func sectionFor(header string) string {
	switch strings.TrimSpace(strings.Trim(header, "│ ")) {
	case "sec/op":
		return "sec"
	case "B/op":
		return "bytes"
	case "allocs/op":
		return "allocs"
	}
	return ""
}

// stripProcs drops the trailing -N GOMAXPROCS suffix, which is machine-specific.
// Only a numeric suffix: a sub-benchmark named "size-large" is a name, and
// cutting at the last hyphen regardless turned it into "size".
func stripProcs(name string) string {
	i := strings.LastIndex(name, "-")
	if i <= 0 || i == len(name)-1 {
		return name
	}
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return name
		}
	}
	return name[:i]
}

func renderTable(rows []row, meta []string, resultsFile string) string {
	var b strings.Builder

	b.WriteString("<!-- Generated by `make docs` (tools/benchdoc). Do not edit by hand. -->\n\n")

	if len(meta) > 0 {
		b.WriteString("Summarised by `benchstat` over `")
		b.WriteString(resultsFile)
		b.WriteString("`:\n\n")
		b.WriteString("```\n")
		for _, m := range meta {
			b.WriteString(m)
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}

	b.WriteString("| Benchmark | sec/op | B/op | allocs/op |\n")
	b.WriteString("| :--- | ---: | ---: | ---: |\n")
	for _, r := range rows {
		label := friendly[r.name]
		if label == "" {
			label = r.name
		}
		b.WriteString(fmt.Sprintf("| **%s** | %s | %s | %s |\n",
			label, dash(r.secOp), dash(r.bytesOp), dash(r.allocs)))
	}

	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
