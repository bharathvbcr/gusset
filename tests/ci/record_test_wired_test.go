package ci_test

import (
	"os"
	"strings"
	"testing"
)

// bench/record_test.sh is the only proof that bench/record.sh's guards work
// (the lock and its takeover, signal cleanup, the zero-row and COUNT refusals,
// the cargo arm). It sat in the tree for weeks with nothing invoking it, so a
// regression in the recorder could not turn any check red. This holds the wiring
// as a test: the Matrix `unit` job, which runs on ubuntu, ubuntu-arm and macOS,
// must have a step that runs it, so the Linux /proc/stat branch of the recorder
// executes in CI and not only on the macOS machine it was written on.
//
// A step inside an existing job, never a new job: main has name-pinned required
// checks, and a new job name is a check nobody requires.

const recorderTestCommand = "sh bench/record_test.sh"

// jobRunsRecorderTest reports whether the named job of a workflow has a step
// whose run is the recorder's self-test, and whether that step is guarded so a
// red step before it does not skip it.
func jobRunsRecorderTest(lines []string, job string) (runs bool, guarded bool) {
	in := false
	var step []string
	flush := func() {
		if len(step) == 0 {
			return
		}
		block := strings.Join(step, "\n")
		if strings.Contains(block, "run: "+recorderTestCommand) {
			runs = true
			guarded = strings.Contains(block, "if: success() || failure()")
		}
		step = nil
	}
	for _, l := range lines {
		if jobHeader.MatchString(l) {
			flush()
			in = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(l), ":")) == job
			continue
		}
		if !in {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(l), "- ") && strings.HasPrefix(l, "      - ") {
			flush()
		}
		step = append(step, l)
	}
	flush()
	return runs, guarded
}

func TestRecorderSelfTestRunsInTheUnitJob(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/matrix.yml")
	if err != nil {
		t.Fatal(err)
	}
	runs, guarded := jobRunsRecorderTest(strings.Split(string(b), "\n"), "unit")
	if !runs {
		t.Fatalf("the Matrix unit job has no step running %q: bench/record_test.sh runs nowhere", recorderTestCommand)
	}
	if !guarded {
		t.Errorf("the recorder self-test step is not guarded with `if: success() || failure()`: a red step before it would skip it")
	}
}

// The check above has to be able to fail, or it proves nothing about the file
// it reads.
func TestRecorderSelfTestWiringCheckCanFail(t *testing.T) {
	const good = `jobs:
  unit:
    name: Unit
    steps:
      - name: Tests
        run: go test ./...

      - name: Benchmark recorder self-test
        if: success() || failure()
        run: sh bench/record_test.sh
  other:
    name: Other
    steps:
      - name: Tests
        run: go vet ./...
`
	cases := map[string]struct {
		yaml        string
		job         string
		runs, guard bool
	}{
		"wired and guarded": {good, "unit", true, true},
		"step dropped": {
			strings.Replace(good, "        run: sh bench/record_test.sh\n", "        run: true\n", 1),
			"unit", false, false,
		},
		"guard dropped": {
			strings.Replace(good, "        if: success() || failure()\n", "", 1),
			"unit", true, false,
		},
		"in another job only": {good, "other", false, false},
	}
	for name, c := range cases {
		runs, guarded := jobRunsRecorderTest(strings.Split(c.yaml, "\n"), c.job)
		if runs != c.runs || guarded != c.guard {
			t.Errorf("%s: got runs=%v guarded=%v, want runs=%v guarded=%v", name, runs, guarded, c.runs, c.guard)
		}
	}
}
