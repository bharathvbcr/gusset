package ci_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// R14's harness runs each scenario's binary with its stderr in a file beside it,
// and a CI runner is gone when the job ends. The asan-rust run failed once on
// PR #24 (Matrix run 40383974940) and left only the path of a file nobody could
// read; the same job passed on the rerun, so the cause is unknowable. Two rules
// close the blind spot, in the style of workflow_failure_evidence_test.go: a job
// that runs bench/r14/run.sh keeps the stderr files on failure, and run.sh's
// expect_pass prints their tail into the job log itself.

const r14Harness = "bench/r14/run.sh"

// r14ArtifactViolations reports every job that runs the R14 harness without an
// upload-artifact step after it that runs on failure, covers
// bench/r14/target/**/*.stderr, and, in a matrix job, names its artifact by the
// matrix leg so the legs do not collide.
func r14ArtifactViolations(name string, lines []string) []string {
	var out []string
	for _, job := range splitJobs(lines) {
		jl := strings.Split(job, "\n")
		steps := workflowSteps(jl)
		lastRun := -1
		for i, st := range steps {
			if strings.Contains(stepRunBody(st), r14Harness) {
				lastRun = i
			}
		}
		if lastRun < 0 {
			continue
		}
		where := name + " " + jobName(jl) + ": "
		found := false
		for _, st := range steps[lastRun+1:] {
			text := strings.Join(st, "\n")
			if !strings.Contains(text, "actions/upload-artifact@") {
				continue
			}
			guard, ok := stepIfGuard(st)
			if !ok || !(strings.Contains(guard, "failure()") || strings.Contains(guard, "always()")) {
				continue
			}
			if !strings.Contains(text, "bench/r14/target/") || !strings.Contains(text, ".stderr") {
				continue
			}
			found = true
			artifact := ""
			for _, l := range st {
				if t := strings.TrimSpace(l); indentOf(l) == 10 && strings.HasPrefix(t, "name:") {
					artifact = strings.TrimSpace(strings.TrimPrefix(t, "name:"))
				}
			}
			if artifact == "" {
				out = append(out, where+"the stderr upload names no artifact")
			}
			if strings.Contains(job, "\n      matrix:") && !strings.Contains(artifact, "${{ matrix.") {
				out = append(out, where+"artifact "+strconv.Quote(artifact)+" does not name its matrix leg, so the legs collide")
			}
		}
		if !found {
			out = append(out, where+"runs "+r14Harness+" with no later on-failure upload-artifact step covering bench/r14/target/**/*.stderr")
		}
	}
	return out
}

func TestEveryR14JobUploadsItsStderrOnFailure(t *testing.T) {
	jobs := 0
	for name, lines := range workflows(t) {
		for _, v := range r14ArtifactViolations(name, lines) {
			t.Error(v)
		}
		for _, job := range splitJobs(lines) {
			if strings.Contains(job, r14Harness) {
				jobs++
			}
		}
	}
	// r14 and r14-musl. Fewer means the detector stopped seeing a job, which
	// would make this test pass on nothing.
	if jobs < 2 {
		t.Fatalf("found %d jobs that run %s; want at least 2", jobs, r14Harness)
	}
}

const (
	r14Pin     = "actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2"
	r14JobHead = `jobs:
  r14:
    name: R14 sealed staticlibs (${{ matrix.os }})
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    steps:
`
	r14Harness1 = `      - name: R14 harness
        run: |
          set -o pipefail
          bench/r14/run.sh | tee r14.txt
`
)

// r14Upload is an upload-artifact step shaped like the real ones: a name, an
// optional if, the pinned action, and its inputs.
func r14Upload(guard, artifact, path string) string {
	if guard != "" {
		guard = "        if: " + guard + "\n"
	}
	return `      - name: Keep the R14 harness stderr
` + guard + `        uses: ` + r14Pin + `
        with:
          name: ` + artifact + `
          path: ` + path + `
`
}

func TestPlantedR14JobWithoutItsStderrIsCaught(t *testing.T) {
	cases := []struct {
		name, src string
		bad       bool
	}{
		{"no upload (matrix before 2026-10-26)", r14JobHead + r14Harness1, true},
		{"upload with no if", r14JobHead + r14Harness1 + r14Upload("", "r14-${{ matrix.os }}", "bench/r14/target/**/*.stderr"), true},
		{"upload only on success", r14JobHead + r14Harness1 + r14Upload("success()", "r14-${{ matrix.os }}", "bench/r14/target/**/*.stderr"), true},
		{"upload of the wrong files", r14JobHead + r14Harness1 + r14Upload("failure()", "r14-${{ matrix.os }}", "r14.txt"), true},
		{"one artifact name for every leg", r14JobHead + r14Harness1 + r14Upload("failure()", "r14-stderr", "bench/r14/target/**/*.stderr"), true},
		{"upload before the harness", r14JobHead + r14Upload("failure()", "r14-${{ matrix.os }}", "bench/r14/target/**/*.stderr") + r14Harness1, true},
		{"on-failure upload named by leg", r14JobHead + r14Harness1 + r14Upload("failure()", "r14-stderr-${{ matrix.os }}", "bench/r14/target/**/*.stderr"), false},
		{"always() upload named by leg", r14JobHead + r14Harness1 + r14Upload("always()", "r14-stderr-${{ matrix.os }}", "bench/r14/target/**/*.stderr"), false},
	}
	for _, c := range cases {
		got := r14ArtifactViolations("planted.yml", strings.Split(c.src, "\n"))
		if c.bad && len(got) == 0 {
			t.Errorf("%s: not caught", c.name)
		}
		if !c.bad && len(got) != 0 {
			t.Errorf("%s: reported %v", c.name, got)
		}
	}
	// A job without a matrix needs no leg in the artifact name.
	plain := "jobs:\n  r14-musl:\n    runs-on: ubuntu-latest\n    steps:\n" + r14Harness1 +
		r14Upload("failure()", "r14-musl-stderr", "bench/r14/target/**/*.stderr")
	if got := r14ArtifactViolations("planted.yml", strings.Split(plain, "\n")); len(got) != 0 {
		t.Errorf("a matrix-free job was held to the matrix rule: %v", got)
	}
}

// shellFunc returns the definition of the top-level function name in a shell
// script, from its `name() {` line to the `}` that closes it in column 0.
func shellFunc(t *testing.T, lines []string, name string) string {
	t.Helper()
	for i, l := range lines {
		if !strings.HasPrefix(l, name+"() {") {
			continue
		}
		if strings.HasSuffix(l, "}") {
			return l
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "}" {
				return strings.Join(lines[i:j+1], "\n")
			}
		}
	}
	t.Fatalf("%s has no top-level function %s", r14Harness, name)
	return ""
}

// runExpectPass runs run.sh's own fail and expect_pass, extracted as written,
// under `sh -eu` as the script runs, against a stub binary made from body. It
// returns the combined output and the exit status.
func runExpectPass(t *testing.T, body string) (string, int) {
	t.Helper()
	src, err := os.ReadFile("../../bench/r14/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	bin := filepath.Join(t.TempDir(), "host")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -eu\n" + shellFunc(t, lines, "fail") + "\n" + shellFunc(t, lines, "expect_pass") +
		"\nexpect_pass \"$1\" \"stub run\"\necho reached-after-expect_pass\n"
	out, err := exec.Command("sh", "-c", script, "sh", bin).CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	default:
		t.Fatal(err)
		return "", 0
	}
}

func TestR14ExpectPassPrintsTheStderrOfAFailedRun(t *testing.T) {
	out, status := runExpectPass(t, "echo 'AddressSanitizer: heap-use-after-free on address 0xdead' >&2\necho stdout-line\nexit 3\n")
	if status != 1 {
		t.Errorf("exit status %d, want 1 (the FAIL line's)", status)
	}
	for _, want := range []string{
		"R14-STDERR: AddressSanitizer: heap-use-after-free on address 0xdead",
		"exited with status 3",
		"FAIL: stub run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "reached-after-expect_pass") {
		t.Errorf("a failed run did not stop the script:\n%s", out)
	}
}

func TestR14ExpectPassKeepsAPassingRunQuiet(t *testing.T) {
	out, status := runExpectPass(t, "echo 'noise on stderr' >&2\nexit 0\n")
	if status != 0 || strings.Contains(out, "R14-STDERR") || !strings.Contains(out, "reached-after-expect_pass") {
		t.Errorf("a passing run: status %d, output:\n%s", status, out)
	}
}

func TestR14ExpectPassBoundsWhatItPrints(t *testing.T) {
	// 500 lines, the first of them 2000 bytes wide, then one 1000 bytes wide:
	// only the last 200 may appear, each cut to 500 bytes. A crash that spews a
	// megabyte must not flood the job log.
	body := `i=1
while [ $i -le 500 ]; do
  if [ $i -eq 1 ]; then printf 'first-%02000d\n' 0 >&2; else echo "line $i" >&2; fi
  i=$((i+1))
done
printf 'wide-%01000d\n' 0 >&2
exit 1
`
	out, status := runExpectPass(t, body)
	if status != 1 {
		t.Fatalf("exit status %d, want 1:\n%.2000s", status, out)
	}
	if strings.Contains(out, "first-") || strings.Contains(out, "R14-STDERR: line 300\n") {
		t.Errorf("lines before the last 200 were printed")
	}
	if !strings.Contains(out, "R14-STDERR: line 500\n") {
		t.Errorf("the last lines were not printed")
	}
	wide := false
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "R14-STDERR: wide-") {
			wide = true
			if want := len("R14-STDERR: ") + 500; len(l) != want {
				t.Errorf("a wide line is %d bytes, want it cut to %d", len(l), want)
			}
		}
	}
	if !wide {
		t.Errorf("the wide line was not printed")
	}
}

func TestR14ExpectPassSaysWhenThereIsNoStderr(t *testing.T) {
	out, status := runExpectPass(t, "kill -SEGV $$\n")
	if status != 1 || !strings.Contains(out, "stderr was empty") || !strings.Contains(out, "exited with status 139") {
		t.Errorf("a silent crash: status %d, output:\n%s", status, out)
	}
}
