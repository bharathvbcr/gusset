package ci_test

import (
	"strconv"
	"strings"
	"testing"
)

// A job with no timeout-minutes runs to GitHub's 360 minute default. A Rust
// test that deadlocks then holds a required check pending for six hours, and
// strict branch protection blocks every merge behind it. go test -timeout
// bounds only Go; cargo test, bench/r14/run.sh and the bench job's interleaved
// runs have no bound of their own. As in workflow_supplychain_test.go, the rule
// is a pure function over lines, run over the real workflows and over a planted
// violation, and it reuses splitJobs and jobKey rather than parsing YAML again.

// defaultJobTimeoutMinutes is GitHub's limit when timeout-minutes is absent. A
// declared value above it bounds nothing.
const defaultJobTimeoutMinutes = 360

// timeoutViolations reports every job without a positive integer
// timeout-minutes of at most the default. A job that calls a reusable workflow
// (`uses:` at job level) is skipped: GitHub rejects timeout-minutes beside it,
// and the called workflow's own jobs carry the bound.
func timeoutViolations(name string, lines []string) []string {
	var out []string
	for _, job := range splitJobs(lines) {
		jl := strings.Split(job, "\n")
		if _, reusable := jobKey(jl, "uses"); reusable {
			continue
		}
		v, ok := jobKey(jl, "timeout-minutes")
		if !ok {
			out = append(out, name+": job "+jobName(jl)+" has no timeout-minutes, so it runs to the 360 minute default")
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > defaultJobTimeoutMinutes {
			out = append(out, name+": job "+jobName(jl)+" has timeout-minutes "+strconv.Quote(v)+"; want an integer from 1 to 360")
		}
	}
	return out
}

func TestEveryJobHasATimeout(t *testing.T) {
	jobs := 0
	for name, lines := range workflows(t) {
		jobs += len(splitJobs(lines))
		for _, v := range timeoutViolations(name, lines) {
			t.Error(v)
		}
	}
	if jobs == 0 {
		t.Fatal("no jobs found in any workflow; the walker matched nothing")
	}
}

func TestPlantedJobTimeoutGapsAreCaught(t *testing.T) {
	const bounded = `jobs:
  ok:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - run: true
`
	cases := []struct {
		name string
		yaml string
		want int
	}{
		{"bounded job passes", bounded, 0},
		{"missing timeout", bounded + "  bad:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n", 1},
		{"timeout only in a step does not count", bounded + "  bad:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n        timeout-minutes: 5\n", 1},
		{"zero is no bound", strings.Replace(bounded, "timeout-minutes: 30", "timeout-minutes: 0", 1), 1},
		{"above the default is no bound", strings.Replace(bounded, "timeout-minutes: 30", "timeout-minutes: 720", 1), 1},
		{"non-numeric", strings.Replace(bounded, "timeout-minutes: 30", "timeout-minutes: soon", 1), 1},
		{"reusable-workflow call job is exempt", bounded + "  call:\n    uses: ./.github/workflows/other.yml\n", 0},
	}
	for _, c := range cases {
		got := timeoutViolations("planted.yml", strings.Split(c.yaml, "\n"))
		if len(got) != c.want {
			t.Errorf("%s: got %d violations %v, want %d", c.name, len(got), got, c.want)
		}
	}
}
