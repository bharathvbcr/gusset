package ci_test

import (
	"strings"
	"testing"
)

// The cross job must refuse targets outside linux/macOS, and a Go build with
// cgo off or GOOS=windows, by the named errors. A comment that merely contains
// the phrase is not that check: workflowSteps drops YAML comments between
// steps, and the run body is read with shell comments stripped.

func TestCrossJobRefusesUnsupportedTargetsByNamedError(t *testing.T) {
	lines := workflows(t)["matrix.yml"]
	if len(lines) == 0 {
		t.Fatal("matrix.yml missing")
	}
	for _, p := range unsupportedPlatformProblems(workflowSteps(jobLines(lines, "cross"))) {
		t.Error(p)
	}
}

func TestACommentIsNotAnUnsupportedPlatformCheck(t *testing.T) {
	lines := []string{
		"  cross:",
		"    steps:",
		"      # cargo check --locked -p gusset --target x86_64-unknown-freebsd",
		"      # gusset supports unix targets only pthread_getattr_np E0425",
		"      # CGO_ENABLED=0 go build ./... gusset requires cgo on a unix target docs/platforms.md",
		"      - name: looks like a gate",
		"        run: |",
		"          # if cargo check --locked -p gusset --target x86_64-unknown-freebsd 2>err.txt; then",
		"          # if ! grep -q \"gusset supports unix targets only\" err.txt; then",
		"          # if grep -q \"pthread_getattr_np\" err.txt || grep -q \"E0425\" err.txt; then",
		"          # if cargo check --locked -p gusset --target x86_64-unknown-illumos 2>err.txt; then",
		"          # if CGO_ENABLED=0 go build ./... >err.txt 2>&1; then",
		"          # if GOOS=windows go build ./... >err.txt 2>&1; then",
		"          # grep -q \"gusset requires cgo on a unix target\"",
		"          # grep -q \"docs/platforms.md\"",
		"          # make cross",
		"          # x86_64-pc-windows-msvc",
		"          echo still green",
	}
	problems := unsupportedPlatformProblems(workflowSteps(lines))
	if len(problems) == 0 {
		t.Fatal("a comment containing the phrase was treated as the check")
	}
	for _, want := range []string{"FreeBSD", "illumos", "CGO_ENABLED=0", "make cross", "Windows"} {
		found := false
		for _, p := range problems {
			if strings.Contains(p, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("comment-only workflow did not report %q missing: %v", want, problems)
		}
	}
}

func jobLines(lines []string, name string) []string {
	header := "  " + name + ":"
	var out []string
	in := false
	for _, l := range lines {
		if jobHeader.MatchString(l) {
			in = strings.TrimRight(l, " ") == header
		}
		if in {
			out = append(out, l)
		}
	}
	return out
}

func unsupportedPlatformProblems(steps [][]string) []string {
	var problems []string
	if !hasMakeCross(steps) {
		problems = append(problems, "supported-target step `make cross` is gone")
	}
	if !hasRustNamedRefusal(steps, "x86_64-pc-windows-msvc", false) {
		problems = append(problems, "Windows cargo check no longer fails the job unless the named error is present")
	}
	if !hasRustNamedRefusal(steps, "x86_64-unknown-freebsd", true) {
		problems = append(problems, "FreeBSD cargo check no longer fails the job on success, a missing named error, or E0425/pthread_getattr_np")
	}
	if !hasRustNamedRefusal(steps, "x86_64-unknown-openbsd", true) && !hasRustNamedRefusal(steps, "x86_64-unknown-illumos", true) {
		problems = append(problems, "neither OpenBSD nor illumos cargo check fails the job on success, a missing named error, or E0425/pthread_getattr_np")
	}
	if !hasGoNamedRefusal(steps) {
		problems = append(problems, "CGO_ENABLED=0 or GOOS=windows go build no longer fails the job unless it prints the named cgo error and docs/platforms.md")
	}
	return problems
}

func hasMakeCross(steps [][]string) bool {
	for _, st := range steps {
		for _, line := range strings.Split(stepCode(st), "\n") {
			if strings.TrimSpace(line) == "make cross" {
				return true
			}
		}
	}
	return false
}

func hasRustNamedRefusal(steps [][]string, target string, cascade bool) bool {
	needle := "if cargo check --locked -p gusset --target " + target + " 2>err.txt; then"
	named := `if ! grep -q "gusset supports unix targets only" err.txt; then`
	cascadeLine := `if grep -q "pthread_getattr_np" err.txt || grep -q "E0425" err.txt; then`
	for _, st := range steps {
		code := stepCode(st)
		if !strings.Contains(code, needle) || !strings.Contains(code, named) || !strings.Contains(code, "exit 1") {
			continue
		}
		if cascade && !strings.Contains(code, cascadeLine) {
			continue
		}
		return true
	}
	return false
}

func hasGoNamedRefusal(steps [][]string) bool {
	cgo := "if CGO_ENABLED=0 go build ./... >err.txt 2>&1; then"
	win := "if GOOS=windows go build ./... >err.txt 2>&1; then"
	phrase := `grep -q "gusset requires cgo on a unix target"`
	doc := `grep -q "docs/platforms.md"`
	for _, st := range steps {
		code := stepCode(st)
		if strings.Contains(code, cgo) && strings.Contains(code, win) && strings.Contains(code, phrase) && strings.Contains(code, doc) && strings.Contains(code, "exit 1") {
			return true
		}
	}
	return false
}

func stepCode(step []string) string {
	var b strings.Builder
	for _, line := range strings.Split(stepRunBody(step), "\n") {
		b.WriteString(stripShellComment(line))
		b.WriteByte('\n')
	}
	return b.String()
}
