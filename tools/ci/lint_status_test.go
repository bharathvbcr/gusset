package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plantedLint copies lint.sh and versions.env into a temp dir, replaces the body
// of one step function, and runs the copy. A step's exit code is the tool's to
// define: govulncheck v1.9.0 exits 3 when it finds vulnerabilities, so a status
// that lint.sh read as "not verified" would turn a found vulnerability green.
func plantedLint(t *testing.T, stepFn, body string, steps ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	src, err := os.ReadFile("lint.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, stepFn+"() {")
	if start < 0 {
		t.Fatalf("lint.sh has no %s", stepFn)
	}
	// The planted body replaces the whole function; the next top-level line
	// that starts with "}" ends it, or a one-liner ends at its own "}".
	end := strings.Index(text[start:], "\n}\n")
	oneLine := strings.Index(text[start:], "\n")
	if strings.HasSuffix(strings.TrimSpace(text[start:start+oneLine]), "}") && !strings.HasSuffix(strings.TrimSpace(text[start:start+oneLine]), "{") {
		end = oneLine - 2
	}
	text = text[:start] + stepFn + "() { " + body + "; }" + text[start+end+2:]
	script := filepath.Join(dir, "lint.sh")
	if err := os.WriteFile(script, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile("versions.env")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "versions.env"), env, 0o644); err != nil {
		t.Fatal(err)
	}
	return lintScriptWith(t, script, versionTree(), nil, tagFixture(t, tagLocal, false, "", false), steps...)
}

// Any non-zero status from a step other than version-parity is FAIL and fails the
// run, whatever its value. 3 is the one the NOT VERIFIED mapping once honoured.
func TestNonZeroStepStatusIsAlwaysFail(t *testing.T) {
	for _, code := range []string{"2", "3", "255"} {
		for _, step := range []struct{ fn, name string }{
			{"step_go_vet", "go-vet"},
			{"step_govulncheck", "govulncheck"},
		} {
			out, err := plantedLint(t, step.fn, "return "+code, step.name)
			if err == nil {
				t.Errorf("%s returning %s did not fail the run:\n%s", step.name, code, out)
			}
			if !strings.Contains(out, step.name+" FAIL") || strings.Contains(out, "NOT VERIFIED") || strings.Contains(out, step.name+" PASS") {
				t.Errorf("%s returning %s was not summarised FAIL:\n%s", step.name, code, out)
			}
		}
	}
}

// version-parity itself: a status of 3 from it is a FAIL too. Only the explicit
// release-window signal is NOT VERIFIED.
func TestVersionParityExitCodeIsNotTheNotVerifiedSignal(t *testing.T) {
	out, err := plantedLint(t, "step_version_parity", "return 3", "version-parity")
	if err == nil || !strings.Contains(out, "version-parity FAIL") || strings.Contains(out, "NOT VERIFIED") {
		t.Errorf("version-parity returning 3 was not a FAIL:\n%v\n%s", err, out)
	}
}

// A step that claims the marker without being version-parity is not honoured.
func TestOnlyVersionParityMayBeNotVerified(t *testing.T) {
	out, err := plantedLint(t, "step_go_vet", "lint_not_verified=version-parity; return 0", "go-vet")
	if err != nil || !strings.Contains(out, "go-vet PASS") || strings.Contains(out, "NOT VERIFIED") {
		t.Errorf("another step's marker changed its summary:\n%v\n%s", err, out)
	}
	// And a failing step that sets it is still a FAIL.
	out, err = plantedLint(t, "step_go_vet", "lint_not_verified=version-parity; return 3", "go-vet")
	if err == nil || !strings.Contains(out, "go-vet FAIL") {
		t.Errorf("a failing step with the marker was not FAIL:\n%v\n%s", err, out)
	}
}
