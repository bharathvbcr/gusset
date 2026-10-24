package ci_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A red nightly is worth something only when it leaves evidence: the input that
// broke a fuzz target, and an issue that names the job. Both were missing.
// cargo-fuzz kept no crash input, so the ffi_exports OOM input was lost every
// night. A job cancelled at its timeout opened no issue: Miri, 2026-10-15..17.
// As in workflow_supplychain_test.go, each rule is a pure function over lines,
// run over the real workflows and over a planted violation.

var (
	cargoFuzzRun = regexp.MustCompile(`\bcargo\b.*\bfuzz run\b`)
	goFuzzRun    = regexp.MustCompile(`\bmake fuzz\b|\bgo test\b.*\s-fuzz[= ]`)
)

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

// fuzzArtifactViolations reports every job that fuzzes without an upload-artifact
// step that runs on failure and covers where the fuzzer writes its inputs:
// fuzz/artifacts/ for cargo-fuzz, a testdata/ tree for Go's fuzzer.
func fuzzArtifactViolations(name string, lines []string) []string {
	var out []string
	for _, job := range splitJobs(lines) {
		jl := strings.Split(job, "\n")
		var cargo, gofuzz bool
		var uploads []string
		for _, st := range workflowSteps(jl) {
			for _, l := range strings.Split(stepRunBody(st), "\n") {
				l = stripShellComment(l)
				cargo = cargo || cargoFuzzRun.MatchString(l)
				gofuzz = gofuzz || goFuzzRun.MatchString(l)
			}
			text := strings.Join(st, "\n")
			if !strings.Contains(text, "actions/upload-artifact@") {
				continue
			}
			if guard, ok := stepIfGuard(st); ok && (strings.Contains(guard, "failure()") || strings.Contains(guard, "always()")) {
				uploads = append(uploads, text)
			}
		}
		check := func(fuzzer, dir string) {
			for _, u := range uploads {
				if strings.Contains(u, dir) {
					return
				}
			}
			out = append(out, name+" "+jobName(jl)+": runs "+fuzzer+" with no on-failure upload-artifact step covering "+dir)
		}
		if cargo {
			check("cargo fuzz", "fuzz/artifacts/")
		}
		if gofuzz {
			check("Go fuzzing", "testdata/")
		}
	}
	return out
}

func TestEveryFuzzJobUploadsItsInputsOnFailure(t *testing.T) {
	jobs := 0
	for name, lines := range workflows(t) {
		for _, v := range fuzzArtifactViolations(name, lines) {
			t.Error(v)
		}
		for _, job := range splitJobs(lines) {
			if cargoFuzzRun.MatchString(job) || goFuzzRun.MatchString(job) {
				jobs++
			}
		}
	}
	// matrix fuzz, nightly cargo-fuzz and go-fuzz. Fewer means the detector
	// stopped seeing a job, which would make this test pass on nothing.
	if jobs < 3 {
		t.Fatalf("found %d fuzz jobs across the workflows; want at least 3", jobs)
	}
}

const fuzzJobHead = `jobs:
  cargo-fuzz:
    runs-on: ubuntu-latest
    steps:
      - name: Fuzz each target
        working-directory: fuzz
        run: |
          for t in a b; do
            cargo "+$RUST_NIGHTLY" fuzz run -O "$t" -- -max_total_time=300
          done
`

func TestPlantedFuzzJobWithoutAnArtifactIsCaught(t *testing.T) {
	cases := []struct {
		name, tail string
		bad        bool
	}{
		{"no upload (nightly before 2026-10-23)", "", true},
		{"upload with no if", `
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        with:
          path: fuzz/artifacts/
`, true},
		{"upload only on success", `
      - if: success()
        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        with:
          path: fuzz/artifacts/
`, true},
		{"upload of the wrong tree", `
      - name: Keep crashers
        if: failure()
        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        with:
          path: testdata/fuzz/
`, true},
		{"on-failure upload of fuzz/artifacts/", `
      - name: Keep crashers
        if: failure()
        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        with:
          path: fuzz/artifacts/
`, false},
	}
	for _, c := range cases {
		got := fuzzArtifactViolations("planted.yml", strings.Split(fuzzJobHead+c.tail, "\n"))
		if c.bad && len(got) == 0 {
			t.Errorf("%s: not caught", c.name)
		}
		if !c.bad && len(got) != 0 {
			t.Errorf("%s: reported %v", c.name, got)
		}
	}
	// A Go fuzz job is held to the same rule.
	goJob := "jobs:\n  fuzz:\n    steps:\n      - name: Fuzz\n        run: make fuzz FUZZTIME=45s\n"
	if got := fuzzArtifactViolations("planted.yml", strings.Split(goJob, "\n")); len(got) != 1 {
		t.Errorf("go fuzz job with no upload: got %v, want one violation", got)
	}
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
