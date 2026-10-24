package ci_test

import (
	"strconv"
	"strings"
	"testing"
)

// A red nightly is worth something only when it leaves evidence: an issue that
// names the job. A job cancelled at its timeout opened no issue: Miri,
// 2026-10-15..17. As in workflow_supplychain_test.go, each rule is a pure
// function over lines, run over the real workflows and over a planted
// violation.

func jobName(job []string) string { return strings.TrimSuffix(strings.TrimSpace(job[0]), ":") }

// jobKey returns a job-level key's value, with the lines of a folded or literal
// block joined by spaces.
func jobKey(job []string, key string) (string, bool) {
	prefix := "    " + key + ":"
	for i, l := range job {
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(l, prefix))
		switch v {
		case ">", ">-", "|", "|-":
		default:
			return v, true
		}
		var parts []string
		for _, c := range job[i+1:] {
			if strings.TrimSpace(c) != "" && indentOf(c) <= 4 {
				break
			}
			if s := strings.TrimSpace(c); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " "), true
	}
	return "", false
}

// nightlyReportViolations checks the job that files the nightly issue. It must
// list every other job in needs, run whenever one of them did not succeed
// (cancelled and timed out included), and name every result other than success.
// The Miri job must be time-bounded at 4 h, well under GitHub's 6 h default.
func nightlyReportViolations(lines []string) []string {
	var out []string
	var report []string
	var others []string
	miriBound := false
	for _, job := range splitJobs(lines) {
		jl := strings.Split(job, "\n")
		switch name := jobName(jl); name {
		case "report":
			report = jl
		case "miri":
			v, ok := jobKey(jl, "timeout-minutes")
			n, err := strconv.Atoi(v)
			if !ok || err != nil || n <= 0 || n > 240 {
				out = append(out, "miri: timeout-minutes is "+strconv.Quote(v)+"; want 1..240")
			}
			miriBound = true
			others = append(others, name)
		default:
			others = append(others, name)
		}
	}
	if !miriBound {
		out = append(out, "no miri job")
	}
	if report == nil {
		return append(out, "no report job")
	}
	needs, _ := jobKey(report, "needs")
	listed := map[string]bool{}
	for _, n := range strings.Split(strings.Trim(needs, "[]"), ",") {
		listed[strings.TrimSpace(n)] = true
	}
	for _, o := range others {
		if !listed[o] {
			out = append(out, "report: needs does not list "+o+", so its failure files nothing")
		}
	}
	guard, _ := jobKey(report, "if")
	if !strings.Contains(guard, "always()") || !strings.Contains(guard, "'cancelled'") || !strings.Contains(guard, "'failure'") {
		out = append(out, "report: if: "+strconv.Quote(guard)+" does not run on a cancelled or timed-out job; want always() and both 'failure' and 'cancelled' results")
	}
	script := strings.Join(report, "\n")
	if strings.Contains(script, "=== 'failure'") || !strings.Contains(script, "!== 'success'") {
		out = append(out, "report: the issue body lists only failed jobs; want every result other than success")
	}
	if !strings.Contains(script, "cancelled:") {
		out = append(out, "report: the issue body does not name cancelled jobs")
	}
	return out
}

func TestNightlyReportNamesEveryUnsuccessfulJob(t *testing.T) {
	for _, v := range nightlyReportViolations(workflows(t)["nightly.yml"]) {
		t.Error(v)
	}
}

// The report and Miri jobs as they were before 2026-10-23.
const plantedNightly = `jobs:
  miri:
    name: Miri Undefined Behavior Check
    runs-on: ubuntu-latest
    steps:
      - run: cargo miri test -p gusset --lib
  cargo-fuzz:
    runs-on: ubuntu-latest
    timeout-minutes: 40
    steps:
      - run: true
  report:
    name: Open an issue on failure
    needs: [miri, cargo-fuzz]
    if: failure() && github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3 # v9.0.0
        with:
          script: |
            const failed = Object.entries(${{ toJSON(needs) }})
              .filter(([, job]) => job.result === 'failure')
              .map(([name]) => name);
`

func TestPlantedNightlyReportGapsAreCaught(t *testing.T) {
	got := strings.Join(nightlyReportViolations(strings.Split(plantedNightly, "\n")), "\n")
	for _, want := range []string{
		"miri: timeout-minutes",
		"does not run on a cancelled or timed-out job",
		"lists only failed jobs",
		"does not name cancelled jobs",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("planted pre-2026-10-23 nightly: %q not reported; got:\n%s", want, got)
		}
	}
	dropped := strings.Replace(plantedNightly, "needs: [miri, cargo-fuzz]", "needs: [miri]", 1)
	if got := strings.Join(nightlyReportViolations(strings.Split(dropped, "\n")), "\n"); !strings.Contains(got, "does not list cargo-fuzz") {
		t.Errorf("a job missing from needs was not reported; got:\n%s", got)
	}
	unbounded := strings.Replace(plantedNightly, "    runs-on: ubuntu-latest\n    steps:\n      - run: cargo miri", "    runs-on: ubuntu-latest\n    timeout-minutes: 360\n    steps:\n      - run: cargo miri", 1)
	if got := strings.Join(nightlyReportViolations(strings.Split(unbounded, "\n")), "\n"); !strings.Contains(got, `timeout-minutes is "360"`) {
		t.Errorf("a 6 h Miri bound was not reported; got:\n%s", got)
	}
}
