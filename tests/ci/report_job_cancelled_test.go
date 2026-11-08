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
	// a cancelled that makes the job run: cancelled() not under `!`, or the
	// 'cancelled' result string. `!cancelled()` means the opposite.
	cancelledCond = regexp.MustCompile(`(?:^|[^!\w])cancelled\(\)|'cancelled'`)
)

// failureWithoutCancelledViolations reports every job whose job-level `if:`
// matches on failure and not on cancelled.
func failureWithoutCancelledViolations(name string, lines []string) []string {
	var out []string
	for _, job := range splitJobs(lines) {
		jl := strings.Split(job, "\n")
		cond, ok := jobKey(jl, "if")
		if !ok || !failureCond.MatchString(cond) || cancelledCond.MatchString(cond) {
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
		"failure()",
		"${{ failure() }}",
		"always() && contains(needs.*.result, 'failure')",
		"always() && (contains(needs.*.result, 'failure') || !cancelled())",
		"always() && needs.a.result == 'failure'",
	}
	good := []string{
		"failure() || cancelled()",
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
