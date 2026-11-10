package ci_test

import (
	"strconv"
	"strings"
	"testing"
)

// The workflow rules share one hand-rolled walker (splitJobs, jobName, jobKey).
// Its failure mode is silence: a job it cannot parse is skipped, so every rule
// that walks jobs passes over it with no verdict (card ft-006f614c). These tests
// count the jobs a second, deliberately simple way and require the walker to
// agree, so a form the walker does not understand is an error, not a skip.

// independentJobNames lists the job ids under the top-level `jobs:` mapping:
// the 2-space-indented lines that end in `:` once a ` #` comment, the `\r` of
// CRLF and quotes are stripped, up to the next column-0 key. It shares no code
// with splitJobs on purpose.
func independentJobNames(lines []string) []string {
	var names []string
	in := false
	for _, raw := range lines {
		l := strings.TrimRight(raw, "\r")
		if l != "" && l[0] != ' ' && l[0] != '#' && l[0] != '\t' {
			in = strings.HasPrefix(l, "jobs:") || strings.HasPrefix(l, `"jobs":`) || strings.HasPrefix(l, `'jobs':`)
			continue
		}
		if !in || !strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "   ") {
			continue
		}
		if i := strings.Index(l, " #"); i >= 0 {
			l = l[:i]
		}
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' || !strings.HasSuffix(l, ":") {
			continue
		}
		names = append(names, strings.Trim(strings.TrimSpace(strings.TrimSuffix(l, ":")), `"'`))
	}
	return names
}

// scannerProblems reports every way splitJobs disagrees with the independent
// count for one workflow, and every job-level line jobKey cannot read.
func scannerProblems(name string, lines []string) []string {
	var out []string
	want := independentJobNames(lines)
	jobs := splitJobs(lines)
	if len(jobs) != len(want) {
		out = append(out, name+": splitJobs found "+itoa(len(jobs))+" jobs, an independent count found "+itoa(len(want)))
	}
	for i, j := range jobs {
		jl := strings.Split(j, "\n")
		if i < len(want) && jobName(jl) != want[i] {
			out = append(out, name+": job "+itoa(i)+" is named "+jobName(jl)+" by jobName, "+want[i]+" by the independent count")
		}
		out = append(out, jobKeyProblems(name+" "+jobName(jl), jl)...)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// jobKeyProblems reports job-body lines in a form jobKey cannot read: a tab in
// the indentation (YAML forbids it, and jobKey would skip the key), or a block
// scalar indicator it would return as the value.
func jobKeyProblems(where string, job []string) []string {
	var out []string
	for n, l := range job {
		l = strings.TrimRight(l, "\r")
		lead := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if strings.Contains(lead, "\t") {
			out = append(out, where+": line "+itoa(n+1)+" is indented with a tab, which jobKey does not read")
			continue
		}
		if indentOf(l) != 4 {
			continue
		}
		if _, v, ok := strings.Cut(l, ":"); ok {
			v = strings.TrimSpace(v)
			if v != "" && (v[0] == '>' || v[0] == '|') && !blockScalarHeader.MatchString(v) {
				out = append(out, where+": line "+itoa(n+1)+" has block scalar indicator "+v+", which jobKey does not read")
			}
		}
	}
	return out
}

func TestScannerSeesEveryJob(t *testing.T) {
	total := 0
	for name, lines := range workflows(t) {
		total += len(independentJobNames(lines))
		for _, p := range scannerProblems(name, lines) {
			t.Error(p)
		}
	}
	if total == 0 {
		t.Error("the independent count found no job in any workflow: the check proves nothing")
	}
}

func plant(s string) []string { return strings.Split(s, "\n") }

const plantedBase = "name: x\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    if: >\n      a\n      b\n    steps:\n      - run: true\n  report:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n"

// Forms the walker tolerates: each must yield the same jobs as the plain file.
func TestScannerToleratesBenignForms(t *testing.T) {
	cases := map[string]string{
		"plain":             plantedBase,
		"CRLF":              strings.ReplaceAll(plantedBase, "\n", "\r\n"),
		"jobs comment":      strings.Replace(plantedBase, "jobs:", "jobs: # c", 1),
		"jobs trailing sp":  strings.Replace(plantedBase, "jobs:", "jobs:  ", 1),
		"header comment":    strings.Replace(plantedBase, "  report:", "  report: # c", 1),
		"double quoted id":  strings.Replace(plantedBase, "  report:", `  "report":`, 1),
		"single quoted id":  strings.Replace(plantedBase, "  report:", `  'report':`, 1),
		"space before :":    strings.Replace(plantedBase, "  report:", "  report :", 1),
		"quoted id comment": strings.Replace(plantedBase, "  report:", `  "report": # c`, 1),
		"if > comment":      strings.Replace(plantedBase, "if: >\n", "if: > # note\n", 1),
		"if >+":             strings.Replace(plantedBase, "if: >\n", "if: >+\n", 1),
		"if >2":             strings.Replace(plantedBase, "if: >\n", "if: >2\n", 1),
		"if >-2":            strings.Replace(plantedBase, "if: >\n", "if: >-2\n", 1),
		"if |2-":            strings.Replace(plantedBase, "if: >\n", "if: |2-\n", 1),
		"quoted key":        strings.Replace(plantedBase, "    if: >", "    'if': >", 1),
	}
	for name, src := range cases {
		lines := plant(src)
		if ps := scannerProblems(name, lines); len(ps) != 0 {
			t.Errorf("%s: a benign form is not read: %v", name, ps)
		}
		jobs := splitJobs(lines)
		if len(jobs) != 2 {
			t.Errorf("%s: want 2 jobs, got %d", name, len(jobs))
			continue
		}
		if got := jobName(strings.Split(jobs[1], "\n")); got != "report" {
			t.Errorf("%s: job 2 is %q, want report", name, got)
		}
		v, ok := jobKey(strings.Split(jobs[0], "\n"), "if")
		if !ok || strings.TrimSpace(v) != "a b" {
			t.Errorf("%s: jobKey(if) = %q, %v; want %q", name, v, ok, "a b")
		}
	}
}

// Forms the walker does not read: the check must say so rather than skip.
func TestScannerRefusesFormsItCannotRead(t *testing.T) {
	cases := map[string]string{
		"tab-indented key":   strings.Replace(plantedBase, "    if: >", "\tif: >", 1),
		"tab inside indent":  strings.Replace(plantedBase, "    if: >", "  \t  if: >", 1),
		"unknown indicator":  strings.Replace(plantedBase, "if: >\n", "if: >x\n", 1),
		"indicator twice":    strings.Replace(plantedBase, "if: >\n", "if: >+-\n", 1),
		"id with a dot":      strings.Replace(plantedBase, "  report:", "  re.port:", 1),
		"id with a space":    strings.Replace(plantedBase, "  report:", "  re port:", 1),
		"quoted id with sp.": strings.Replace(plantedBase, "  report:", `  "re port":`, 1),
	}
	for name, src := range cases {
		if ps := scannerProblems(name, plant(src)); len(ps) == 0 {
			t.Errorf("%s: the scanner check reported nothing for a form it cannot read", name)
		}
	}
}
