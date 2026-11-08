package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shimDir writes executable scripts named after their keys into a fresh
// directory and returns a PATH that puts it first.
func shimDir(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// The audit's grep exits 2 when it could not search; 1 is the ordinary "no
// match". The old xargs pipeline folded both into one status and the audit
// ignored it, so a grep that failed to read anything reported a clean tree.
func TestForbiddenAuditFailsWhenGrepCannotRun(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	path := shimDir(t, map[string]string{
		// Both ways the audit could search: `git grep` and a bare grep.
		"git":  `if [ "$1" = grep ]; then echo "simulated: cannot read" >&2; exit 2; fi` + "\n" + `exec ` + realGit + ` "$@"`,
		"grep": `echo "simulated: cannot read" >&2; exit 2`,
	})
	out, err := lint(t, map[string]string{"a.go": "package a\n"}, []string{path}, "forbidden-patterns")
	if err == nil || !strings.Contains(out, "forbidden-patterns FAIL") {
		t.Fatalf("an audit whose grep exited 2 reported clean: %v\n%s", err, out)
	}
	if !strings.Contains(out, "did not run") {
		t.Errorf("the failure does not say the audit did not run:\n%s", out)
	}
}

// A tree with no forbidden pattern is grep exit 1 on every pattern, and that
// must stay a PASS: the guard above is for errors, not for no match.
func TestForbiddenAuditStillPassesACleanTree(t *testing.T) {
	out, err := lint(t, map[string]string{"a.go": "package a\n", "b.rs": "fn main() {}\n"}, nil, "forbidden-patterns")
	if err != nil || !strings.Contains(out, "forbidden-patterns PASS") {
		t.Fatalf("a clean tree failed the audit: %v\n%s", err, out)
	}
}

// cargo-deny runs over the root workspace and over fuzz/, and a failure in the
// first must not hide the second's verdict.
func TestCargoDenyRunsBothWorkspacesWhateverTheFirstSays(t *testing.T) {
	versions, err := os.ReadFile("versions.env")
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, l := range strings.Split(string(versions), "\n") {
		if v, ok := strings.CutPrefix(l, "CARGO_DENY_VERSION="); ok {
			want = v
		}
	}
	if want == "" {
		t.Fatal("versions.env pins no CARGO_DENY_VERSION")
	}
	for name, c := range map[string]struct {
		failRoot, failFuzz bool
	}{
		"root fails":  {true, false},
		"fuzz fails":  {false, true},
		"both fail":   {true, true},
		"both passes": {false, false},
	} {
		log := filepath.Join(t.TempDir(), "calls")
		script := `case "$*" in *--manifest-path*) rc=` + b2s(c.failFuzz) + `;; *) rc=` + b2s(c.failRoot) + `;; esac
echo "$*" >> "` + log + `"
exit $rc`
		path := shimDir(t, map[string]string{
			"cargo-deny": `echo "cargo-deny ` + want + `"`,
			"cargo":      script,
		})
		out, runErr := lint(t, map[string]string{"a.go": "package a\n"}, []string{path}, "cargo-deny")
		calls, _ := os.ReadFile(log)
		if got := strings.Count(string(calls), "\n"); got != 2 {
			t.Errorf("%s: cargo deny ran %d time(s), want 2 (root and fuzz):\n%s\n%s", name, got, calls, out)
		}
		failed := c.failRoot || c.failFuzz
		if failed && (runErr == nil || !strings.Contains(out, "cargo-deny FAIL")) {
			t.Errorf("%s: a failing cargo deny exited clean: %v\n%s", name, runErr, out)
		}
		if !failed && (runErr != nil || !strings.Contains(out, "cargo-deny PASS")) {
			t.Errorf("%s: two passing cargo deny runs did not pass: %v\n%s", name, runErr, out)
		}
	}
}

func b2s(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
