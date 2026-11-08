package ci_test

import (
	"regexp"
	"strings"
	"testing"
)

// A job that hits its timeout-minutes concludes `cancelled`, and failure() is
// false for that. So a report job whose `if:` names failure but not cancelled
// opens no issue for the very hang it exists to report: the Miri job's 6 h
// cancels on 2026-10-15..17 (nightly) and tip's 90 minute gotip timeout (R13).
// The rule is a pure function over lines, like the other workflow rules.

var (
	// failure() or the 'failure' result string, as `contains(needs.*.result, 'failure')`
	// or `needs.x.result == 'failure'` spell it.
	failureCond = regexp.MustCompile(`\bfailure\(\)|'failure'`)
	// A status function that makes the job run when a needed job was cancelled
	// (or timed out): always() or cancelled(), not under `!`. Without one,
	// GitHub applies the implicit success() and skips the job as soon as a
	// needed job did not succeed, so a failure-only report job never reports.
	runsOnCancelFn = regexp.MustCompile(`(?:^|[^!\w.])(?:always|cancelled)\(\)`)
	// cancelled() as the status function is itself the match.
	cancelledFn = regexp.MustCompile(`(?:^|[^!\w.])cancelled\(\)`)
	// 'cancelled' counts only as a needs result tested positively:
	// contains(needs.*.result, 'cancelled') or needs.x.result == 'cancelled',
	// not under `!` and not `!=`. An unrelated `github.event.x == 'cancelled'`
	// does not match.
	cancelledResult = regexp.MustCompile(`(?:^|[^!\w])contains\(\s*needs\.\*\.result\s*,\s*'cancelled'\s*\)|(?:^|[^!\w.])needs\.[\w-]+\.result\s*==\s*'cancelled'`)
	trailingComment = regexp.MustCompile(`\s#.*$`)
)

// reportCondOK says whether a job-level `if:` that matches on failure also
// runs, and matches, on cancelled. A `# ...` comment is not part of the
// expression, so it is stripped first and cannot satisfy the rule.
func reportCondOK(cond string) bool {
	cond = trailingComment.ReplaceAllString(cond, "")
	if cancelledFn.MatchString(cond) {
		return true
	}
	return runsOnCancelFn.MatchString(cond) && cancelledResult.MatchString(cond)
}

// failureWithoutCancelledViolations reports every job whose job-level `if:`
// matches on failure and does not run on cancelled (see reportCondOK).
func failureWithoutCancelledViolations(name string, lines []string) []string {
	var out []string
	for _, job := range splitJobs(lines) {
		jl := strings.Split(job, "\n")
		cond, ok := jobKey(jl, "if")
		if !ok || !failureCond.MatchString(trailingComment.ReplaceAllString(cond, "")) || reportCondOK(cond) {
			continue
		}
		out = append(out, name+": job "+jobName(jl)+" runs on failure but not on cancelled, so a timeout opens nothing: if: "+cond)
	}
	return out
}

func TestEveryFailureReportJobAlsoRunsOnCancelled(t *testing.T) {
	matched := 0
	for name, lines := range workflows(t) {
		for _, job := range splitJobs(lines) {
			if cond, ok := jobKey(strings.Split(job, "\n"), "if"); ok && failureCond.MatchString(cond) {
				matched++
			}
		}
		for _, v := range failureWithoutCancelledViolations(name, lines) {
			t.Error(v)
		}
	}
	if matched < 2 {
		t.Fatalf("found %d job(s) with a failure condition; nightly's report and tip's report are two, so the scan is broken", matched)
	}
}

func TestPlantedFailureWithoutCancelledIsCaught(t *testing.T) {
	job := func(cond string) []string {
		return strings.Split("jobs:\n  report:\n    needs: [a]\n    if: "+cond+"\n    timeout-minutes: 10\n    steps:\n      - run: true\n", "\n")
	}
	bad := []string{
		"always() && contains(needs.*.result, 'failure') && !contains(needs.*.result, 'cancelled')",
		"always() && needs.a.result == 'failure' && needs.a.result != 'cancelled'",
		"failure() # cancelled()",
		"failure() || github.event.x == 'cancelled'",
		"contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled')",
		"failure()",
		"${{ failure() }}",
		"always() && contains(needs.*.result, 'failure')",
		"always() && (contains(needs.*.result, 'failure') || !cancelled())",
		"always() && needs.a.result == 'failure'",
	}
	good := []string{
		"failure() || cancelled()",
		"always() && (contains(needs.*.result, 'failure') || needs.a.result == 'cancelled')",
		"always() && (contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled')) # also on cancel",
		"always() && (contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled'))",
		"always() && github.ref == 'refs/heads/main' && (contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled') || contains(needs.*.result, 'skipped'))",
		"success()",
	}
	for _, c := range bad {
		if len(failureWithoutCancelledViolations("planted.yml", job(c))) != 1 {
			t.Errorf("not caught: if: %s", c)
		}
	}
	for _, c := range good {
		if v := failureWithoutCancelledViolations("planted.yml", job(c)); len(v) != 0 {
			t.Errorf("compliant form flagged: if: %s: %v", c, v)
		}
	}
	// A folded block, as nightly writes it.
	folded := strings.Split("jobs:\n  report:\n    if: >-\n      always() &&\n      contains(needs.*.result, 'failure')\n    steps:\n      - run: true\n", "\n")
	if len(failureWithoutCancelledViolations("planted.yml", folded)) != 1 {
		t.Error("a folded failure-only condition was not caught")
	}
}
