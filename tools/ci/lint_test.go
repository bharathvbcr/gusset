// Package ci holds the tests for the shell pipeline CI and `make lint` share.
package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The forbidden patterns, split so this file does not spell them: the audit greps
// every tracked Go source, this one included, and would fail on its own fixtures.
const (
	linkname      = "//go:" + "link" + "name"
	asmTrampoline = "asmcgo" + "call"
	cgocheck      = "cgocheck" + "=0"
	invalidptr    = "invalidptr" + "=0"
	nolint        = "//" + "nolint"
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
		"a.go": "package a\n\n// Not a cgo-free trampoline (like purego or\n// " + asmTrampoline + "), and not an IPC bus.\nfunc F() {}\n",
		"b.rs": "// " + asmTrampoline + " is refused\nfn main() {}\n",
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
		"trampoline call": "package a\nfunc F() { " + asmTrampoline + "(nil, nil) }\n",
		"trailing":        "package a\nvar x = 1 // " + asmTrampoline + "\nfunc G() { " + asmTrampoline + "() }\n",
		"linkname":        "package a\n" + linkname + " f runtime.f\nfunc f()\n",
		"cgocheck":        "package a\nconst e = \"GODEBUG=" + cgocheck + "\"\n",
		"invalidptr":      "package a\nconst e = \"" + invalidptr + "\"\n",
		"nolint":          "package a\nfunc F() {} " + nolint + ":all\n",
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
	out, err := lint(t, map[string]string{"a.go": "package a\n" + linkname + " f runtime.f\nfunc f()\n"}, []string{"PATH=/usr/bin:/bin"},
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

// A parse error is gofmt writing to stderr and an empty stdout. The old
// pipeline's status was grep's, so that exited 0.
func TestGofmtParseErrorFailsTheStep(t *testing.T) {
	out, err := lint(t, map[string]string{
		"bad.go": "package p\nfunc (\n",
	}, nil, "gofmt")
	if err == nil || !strings.Contains(out, "gofmt FAIL") || !strings.Contains(out, "bad.go") {
		t.Fatalf("a parse error passed: %v\n%s", err, out)
	}
}

// The path stays one argv. If the NUL split were lost, gofmt would be asked
// for the two halves and this formatted file would fail.
func TestGofmtKeepsANewlineInTheFilenameWhole(t *testing.T) {
	const name = "dir/has\nnewline.go"
	out, err := lint(t, map[string]string{
		name: "package dir\n\nfunc F() {}\n",
	}, nil, "gofmt")
	if err != nil {
		t.Fatalf("a formatted file whose name contains a newline failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "no such file") || strings.Contains(out, "gofmt FAIL") {
		t.Fatalf("the newline was treated as a separator:\n%s", out)
	}

	out, err = lint(t, map[string]string{
		name: "package dir\nfunc bad( ) {}\n",
	}, nil, "gofmt")
	if err == nil || !strings.Contains(out, "gofmt FAIL") || !strings.Contains(out, "dir/has\nnewline.go") {
		t.Fatalf("unformatted newline path was split or passed: %v\n%s", err, out)
	}
	if strings.Contains(out, "no such file") {
		t.Fatalf("gofmt was handed a fragment of the path:\n%s", out)
	}

	// A newline in a directory name is the same constraint: one argv, not two.
	const dirName = "dir\nname/file.go"
	out, err = lint(t, map[string]string{
		dirName: "package p\n\nfunc F() {}\n",
	}, nil, "gofmt")
	if err != nil || strings.Contains(out, "no such file") || strings.Contains(out, "gofmt FAIL") {
		t.Fatalf("a formatted file under a directory whose name contains a newline failed: %v\n%s", err, out)
	}
}

// gofmt's flag parser takes a leading dash as an option. A clean -ok.go must
// pass; an unformatted one must fail naming that path, not "no such file".
func TestGofmtPathThatLooksLikeAFlag(t *testing.T) {
	out, err := lint(t, map[string]string{
		"-ok.go": "package ok\n\nfunc F() {}\n",
	}, nil, "gofmt")
	if err != nil || strings.Contains(out, "flag provided but not defined") || strings.Contains(out, "gofmt FAIL") {
		t.Fatalf("a formatted -ok.go was parsed as a flag or otherwise failed: %v\n%s", err, out)
	}

	out, err = lint(t, map[string]string{
		"-ok.go": "package ok\nfunc bad( ) {}\n",
	}, nil, "gofmt")
	if err == nil || !strings.Contains(out, "gofmt FAIL") || !strings.Contains(out, "-ok.go") {
		t.Fatalf("unformatted -ok.go passed or was not named: %v\n%s", err, out)
	}
	if strings.Contains(out, "no such file") || strings.Contains(out, "flag provided but not defined") {
		t.Fatalf("gofmt did not take -ok.go as a path:\n%s", out)
	}
}

// bench/seed is excluded the way `grep -v '^bench/seed/'` excluded it, including
// a file gofmt cannot parse. A sibling outside that tree is still gated.
func TestGofmtStillExcludesBenchSeed(t *testing.T) {
	out, err := lint(t, map[string]string{
		"bench/seed/bad.go":  "package seed\nfunc (\n",
		"bench/seed/ugly.go": "package seed\nfunc bad( ) {}\n",
		"ok.go":              "package ok\n\nfunc F() {}\n",
	}, nil, "gofmt")
	if err != nil {
		t.Fatalf("bench/seed was not excluded: %v\n%s", err, out)
	}
	out, err = lint(t, map[string]string{
		"bench/seed/ugly.go": "package seed\nfunc bad( ) {}\n",
		"ugly.go":            "package ok\nfunc bad( ) {}\n",
	}, nil, "gofmt")
	if err == nil || !strings.Contains(out, "ugly.go") || strings.Contains(out, "bench/seed/") {
		t.Fatalf("exclusion or the real file is wrong: %v\n%s", err, out)
	}
}
