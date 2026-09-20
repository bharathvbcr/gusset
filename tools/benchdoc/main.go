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
)

const (
	readmePath = "README.md"
	resultsDir = "bench/results"

	// The generated block is delimited so the surrounding prose stays hand-written.
	beginMarker = "<!-- BENCHDOC:BEGIN -->"
	endMarker   = "<!-- BENCHDOC:END -->"
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
	"GussetCallNoop":     "Gusset Call No-op",
	"GussetCallParallel": "Gusset Call Parallel",
	"GussetSubmitWait":   "Gusset Submit & Wait",
	"GussetBufferLarge":  "Large Buffer (64 KiB)",
	"ChannelHop":         "Channel Hop Baseline",
}

func main() {
	check := flag.Bool("check", false, "verify README.md is up to date instead of rewriting it")
	flag.Parse()

	resultsFile, err := latestResults()
	if err != nil {
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

	updated, err := replaceBlock(string(readme), table)
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

// latestResults picks the raw results file to summarise.
//
// It fails rather than guessing when there is more than one and none matches this
// platform: summarising a linux run into a README that claims darwin numbers is
// exactly the kind of quiet wrongness this tool exists to stop.
func latestResults() (string, error) {
	entries, err := filepath.Glob(filepath.Join(resultsDir, "*.txt"))
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no benchmark results in %s; run `make bench` first", resultsDir)
	}
	sort.Strings(entries)
	if len(entries) == 1 {
		return entries[0], nil
	}
	return "", fmt.Errorf("found %d results files in %s; pass one explicitly rather than "+
		"letting the tool choose which platform the README describes", len(entries), resultsDir)
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

		for _, k := range []string{"goos:", "goarch:", "pkg:", "cpu:"} {
			if strings.HasPrefix(trimmed, k) {
				meta = append(meta, trimmed)
			}
		}

		// Section headers name the unit being reported.
		switch {
		case strings.Contains(trimmed, "sec/op"):
			section = "sec"
			continue
		case strings.Contains(trimmed, "B/op"):
			section = "bytes"
			continue
		case strings.Contains(trimmed, "allocs/op"):
			section = "allocs"
			continue
		}

		// Skip table borders, footnotes and the geomean summary.
		if strings.HasPrefix(trimmed, "│") || strings.HasPrefix(trimmed, "¹") ||
			strings.HasPrefix(trimmed, "²") || strings.HasPrefix(trimmed, "geomean") ||
			section == "" {
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		if !strings.HasPrefix(name, "Gusset") && !strings.HasPrefix(name, "Channel") {
			continue
		}
		// Drop the trailing -N parallelism suffix, which is machine-specific.
		base := name
		if i := strings.LastIndex(base, "-"); i > 0 {
			base = base[:i]
		}

		// benchstat renders "13.43µ ± 6%", or "21.55µ ± ∞ ¹" when the sample was
		// too small for an interval. Keep the whole summary: the dispersion is the
		// part that says whether the point estimate means anything, and dropping
		// it is how a table comes to look more certain than the data behind it.
		value := strings.TrimSpace(strings.Join(fields[1:], " "))
		value = strings.TrimSpace(strings.TrimRight(value, "¹²³"))

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

func renderTable(rows []row, meta []string, resultsFile string) string {
	var b strings.Builder

	b.WriteString(beginMarker)
	b.WriteString("\n")
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

	b.WriteString("\n")
	b.WriteString(endMarker)
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func replaceBlock(readme, table string) (string, error) {
	start := strings.Index(readme, beginMarker)
	end := strings.Index(readme, endMarker)
	if start < 0 || end < 0 {
		return "", fmt.Errorf("README.md has no %s / %s block; add one around the "+
			"benchmark table so this tool has somewhere to write", beginMarker, endMarker)
	}
	if end < start {
		return "", fmt.Errorf("README.md markers are out of order")
	}
	return readme[:start] + table + readme[end+len(endMarker):], nil
}
