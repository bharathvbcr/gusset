package ci_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Rules that keep the prose in step with what it describes. Each one failed on
// main when it was written: the decision log had a blank line that made GitHub
// render most of its rows as plain text, and the testing table in AGENTS.md
// named neither the hammer, the floor jobs nor the fifth libFuzzer target.

// decisionsTableProblems reports every line between a Markdown table's first and
// last row that is not itself a row. A blank line there ends the table, and
// GitHub renders the rest as running text.
func decisionsTableProblems(src string) []string {
	lines := strings.Split(src, "\n")
	first, last := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "|") {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return []string{"no table rows found"}
	}
	var problems []string
	for i := first; i <= last; i++ {
		if !strings.HasPrefix(lines[i], "|") {
			problems = append(problems, "line "+strconv.Itoa(i+1)+" is inside the table but is not a row ("+quoteShort(lines[i])+"); the rows after it stop rendering as a table")
		}
	}
	return problems
}

func quoteShort(s string) string {
	if s == "" {
		return "blank"
	}
	if len(s) > 40 {
		s = s[:40] + "..."
	}
	return `"` + s + `"`
}

func TestDecisionsLogRendersAsOneTable(t *testing.T) {
	b, err := os.ReadFile("../../DECISIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range decisionsTableProblems(string(b)) {
		t.Error(p)
	}
}

func TestDecisionsTableRuleOnFixtures(t *testing.T) {
	const whole = "# Decisions\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |\n"
	if got := decisionsTableProblems(whole); len(got) != 0 {
		t.Fatalf("an unbroken table was refused: %v", got)
	}
	const planted = "# Decisions\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\n| 3 | 4 |\n| 5 | 6 |\n"
	got := decisionsTableProblems(planted)
	if len(got) != 1 || !strings.Contains(got[0], "line 6 ") {
		t.Fatalf("a planted blank line (line 6) was not reported exactly once: %v", got)
	}
	const prose = "| A | B |\n| --- | --- |\n| 1 | 2 |\nnot a row\n| 3 | 4 |\n"
	if got := decisionsTableProblems(prose); len(got) != 1 {
		t.Fatalf("a prose line inside the table was not reported: %v", got)
	}
	if got := decisionsTableProblems("no table here"); len(got) != 1 {
		t.Fatalf("a file with no table must be reported, got %v", got)
	}
}

// jobIDs returns the job keys under `jobs:` in a workflow file.
func jobIDs(lines []string) []string {
	var ids []string
	in := false
	for _, l := range lines {
		if strings.TrimRight(l, " ") == "jobs:" {
			in = true
			continue
		}
		if in && len(l) > 0 && l[0] != ' ' && l[0] != '#' {
			in = false
		}
		if in && jobHeader.MatchString(l) {
			ids = append(ids, strings.TrimSuffix(strings.TrimSpace(l), ":"))
		}
	}
	return ids
}

var backticked = regexp.MustCompile("`([^`\n]+)`")

// sectionTokens returns the backticked tokens in the Markdown section that
// starts at the heading and runs to the next heading of the same level.
func sectionTokens(src, heading string) map[string]bool {
	start := strings.Index(src, "\n"+heading+"\n")
	if start < 0 {
		return nil
	}
	body := src[start+len(heading)+2:]
	if end := strings.Index(body, "\n## "); end >= 0 {
		body = body[:end]
	}
	out := map[string]bool{}
	for _, m := range backticked.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

// testingPlanGaps names the CI jobs and libFuzzer targets that the Testing
// plan section does not mention. `report` is the issue-filing job of nightly
// and tip, not a test.
func testingPlanGaps(agents string, jobs, fuzzTargets []string) []string {
	tokens := sectionTokens(agents, "## Testing plan")
	if tokens == nil {
		return []string{"AGENTS.md has no `## Testing plan` section"}
	}
	var gaps []string
	for _, id := range jobs {
		if id == "report" {
			continue
		}
		if !tokens[id] {
			gaps = append(gaps, "CI job `"+id+"` has no mention in the Testing plan")
		}
	}
	for _, f := range fuzzTargets {
		if !tokens[f] {
			gaps = append(gaps, "libFuzzer target `"+f+"` (fuzz/fuzz_targets) has no mention in the Testing plan")
		}
	}
	sort.Strings(gaps)
	return gaps
}

func TestTestingPlanNamesEveryCIJobAndFuzzTarget(t *testing.T) {
	agents, err := os.ReadFile("../../AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var jobs []string
	for _, lines := range workflows(t) {
		for _, id := range jobIDs(lines) {
			if !seen[id] {
				seen[id] = true
				jobs = append(jobs, id)
			}
		}
	}
	if len(jobs) < 20 {
		t.Fatalf("found %d job ids across the workflows; the parser no longer matches their layout: %v", len(jobs), jobs)
	}
	files, err := filepath.Glob("../../fuzz/fuzz_targets/*.rs")
	if err != nil || len(files) == 0 {
		t.Fatalf("no libFuzzer targets found: %v", err)
	}
	var targets []string
	for _, f := range files {
		targets = append(targets, strings.TrimSuffix(filepath.Base(f), ".rs"))
	}
	for _, g := range testingPlanGaps(string(agents), jobs, targets) {
		t.Error(g)
	}
}

func TestTestingPlanRuleOnFixtures(t *testing.T) {
	const doc = "# T\n\n## Testing plan\n\n| `unit` | `hammer` | `fuzz_a` |\n\n## Other\n\n`planted` is outside the section.\n"
	if got := testingPlanGaps(doc, []string{"unit", "hammer", "report"}, []string{"fuzz_a"}); len(got) != 0 {
		t.Fatalf("a complete section was refused: %v", got)
	}
	got := testingPlanGaps(doc, []string{"unit", "planted", "go-floor"}, []string{"fuzz_a", "fuzz_b"})
	if len(got) != 3 {
		t.Fatalf("want the 2 missing jobs and 1 missing target reported (a token outside the section does not count), got %v", got)
	}
	if got := testingPlanGaps("# T\n", nil, nil); len(got) != 1 {
		t.Fatalf("a missing section must be reported, got %v", got)
	}
}

func TestJobIDsReadsOnlyTheJobsBlock(t *testing.T) {
	lines := strings.Split("on:\n  push:\n    branches: [main]\njobs:\n  unit:\n    runs-on: x\n  go-floor:\n    runs-on: y\n", "\n")
	if got := strings.Join(jobIDs(lines), ","); got != "unit,go-floor" {
		t.Fatalf("job ids %q, want unit,go-floor (not the `push` trigger)", got)
	}
}
