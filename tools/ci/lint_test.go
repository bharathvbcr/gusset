// Package ci holds the tests for the shell pipeline CI and `make lint` share.
package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lint runs tools/ci/lint.sh against a throwaway repository holding files, so
// the steps that read tracked sources are exercised on known inputs.
func lint(t *testing.T, files map[string]string, env []string, steps ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("lint.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cmd := exec.Command("sh", append([]string{script}, steps...)...)
	cmd.Env = append(os.Environ(), append([]string{"LINT_ROOT=" + root}, env...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gusset.go's package doc says what Gusset is not and names asmcgocall. The
// audit grepped it as a use, so it failed on every run that got far enough to
// reach it -- which was none, because staticcheck broke first and skipped it.
func TestForbiddenAuditIgnoresProseAboutTheRule(t *testing.T) {
	out, err := lint(t, map[string]string{
		"a.go": "package a\n\n// Not a cgo-free trampoline (like purego or\n// asmcgocall), and not an IPC bus.\nfunc F() {}\n",
		"b.rs": "// asmcgocall is refused\nfn main() {}\n",
	}, nil, "forbidden-patterns")
	if err != nil {
		t.Fatalf("a comment naming the rule failed the audit: %v\n%s", err, out)
	}
	if !strings.Contains(out, "forbidden-patterns PASS") {
		t.Errorf("no PASS line in:\n%s", out)
	}
}

func TestForbiddenAuditCatchesRealUses(t *testing.T) {
	for name, src := range map[string]string{
		"asmcgocall call": "package a\nfunc F() { asmcgocall(nil, nil) }\n",
		"trailing":        "package a\nvar x = 1 // asmcgocall\nfunc G() { asmcgocall() }\n",
		"linkname":        "package a\n//go:linkname f runtime.f\nfunc f()\n",
		"cgocheck":        "package a\nconst e = \"GODEBUG=cgocheck=0\"\n",
		"invalidptr":      "package a\nconst e = \"invalidptr=0\"\n",
		"nolint":          "package a\nfunc F() {} //nolint:all\n",
	} {
		out, err := lint(t, map[string]string{"a.go": src}, nil, "forbidden-patterns")
		if err == nil {
			t.Errorf("%s: the audit passed:\n%s", name, out)
		}
		if !strings.Contains(out, "forbidden-patterns FAIL") {
			t.Errorf("%s: no FAIL line in:\n%s", name, out)
		}
	}
}

// A step that cannot run must not look like a step that passed.
func TestSkippedAndUnknownStepsAreNeverClean(t *testing.T) {
	out, err := lint(t, map[string]string{"a.go": "package a\n"}, []string{"LINT_SKIP=forbidden-patterns"}, "forbidden-patterns")
	if err == nil || !strings.Contains(out, "SKIPPED (explicit)") {
		t.Errorf("an explicitly skipped step exited clean: %v\n%s", err, out)
	}
	out, err = lint(t, map[string]string{"a.go": "package a\n"}, nil, "no-such-step")
	if err == nil || !strings.Contains(out, "unknown step") {
		t.Errorf("an unknown step passed: %v\n%s", err, out)
	}
}

func TestMissingToolFailsLoudly(t *testing.T) {
	// An empty PATH with only /usr/bin and /bin: no staticcheck, no cargo-deny.
	out, err := lint(t, map[string]string{"a.go": "package a\n"}, []string{"PATH=/usr/bin:/bin"}, "cargo-deny")
	if err == nil || !strings.Contains(out, "is not installed") || !strings.Contains(out, "cargo-deny FAIL") {
		t.Errorf("a missing tool did not fail loudly: %v\n%s", err, out)
	}
}

// One broken tool must not skip the others: every requested step reports.
func TestEveryStepReportsEvenAfterAFailure(t *testing.T) {
	out, err := lint(t, map[string]string{"a.go": "package a\n//go:linkname f runtime.f\nfunc f()\n"}, []string{"PATH=/usr/bin:/bin"},
		"cargo-deny", "forbidden-patterns", "cargo-deny")
	if err == nil {
		t.Fatalf("expected failure:\n%s", out)
	}
	for _, want := range []string{"cargo-deny FAIL", "forbidden-patterns FAIL"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}
