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

// versionTree is the five files version-parity reads, all saying 1.2.3, laid
// out the way the real ones are: [Unreleased] headings above the newest
// release, a SECURITY.md table with one supported row, a v-prefixed default
// ref and a git dependency pinned by tag.
func versionTree() map[string]string {
	return map[string]string{
		"Cargo.toml":          "[workspace]\nmembers = [\"crates/x\"]\n\n[workspace.package]\nversion = \"1.2.3\"\nedition = \"2021\"\n\n[profile.release]\nopt-level = 3\n",
		"crates/x/Cargo.toml": "[package]\nname = \"x\"\nversion = \"9.9.9\"\n",
		"CHANGELOG.md": "# Changelog\n\n## [Unreleased] - 2026-10-25 · Something new\n\n- a change\n\n" +
			"## [1.2.3] - 2026-10-24\n\n### Security\n- a note\n\n## [1.2.2] - 2026-09-26\n",
		"SECURITY.md": "## Supported Versions\n\n| Version | Supported |\n| :--- | :--- |\n" +
			"| `v1.2.3` | :white_check_mark: |\n| `< v1.2.3` | :x: |\n",
		"install.sh":       "#!/usr/bin/env bash\nset -euo pipefail\n\nGUSSET_DEFAULT_REF=\"v1.2.3\"\nGUSSET_UPSTREAM_URL=\"https://example.invalid/x.git\"\n",
		"docs/adoption.md": "```toml\n[dependencies]\n# pin a release tag\ngusset_core = { package = \"gusset\", git = \"https://github.com/bharathvbcr/gusset\", tag = \"v1.2.3\" }\n```\n",
	}
}

func TestVersionParityPassesWhenEveryInputAgrees(t *testing.T) {
	out, err := lint(t, versionTree(), nil, "version-parity")
	if err != nil || !strings.Contains(out, "version-parity PASS") {
		t.Fatalf("five agreeing inputs did not pass: %v\n%s", err, out)
	}
	// A crate's own [package] version is not the workspace's.
	if strings.Contains(out, "9.9.9") {
		t.Errorf("read a version outside [workspace.package]:\n%s", out)
	}
}

// One planted disagreement per input, and each way an input can fail to say
// anything: a check that could not read an input must not pass.
func TestVersionParityFailsOnEachMismatch(t *testing.T) {
	type edit struct{ file, from, to string }
	for name, c := range map[string]struct {
		edits []edit
		drop  string
		want  string // the input the failure must name
	}{
		"cargo bumped alone":          {edits: []edit{{"Cargo.toml", `version = "1.2.3"`, `version = "1.2.4"`}}, want: "CHANGELOG.md"},
		"changelog heading":           {edits: []edit{{"CHANGELOG.md", "## [1.2.3]", "## [1.2.4]"}}, want: "CHANGELOG.md"},
		"changelog only unreleased":   {edits: []edit{{"CHANGELOG.md", "## [1.2.3] - 2026-10-24", "## [Unreleased] - 2026-10-24"}, {"CHANGELOG.md", "## [1.2.2] - 2026-09-26", "## [Unreleased] - 2026-09-26"}}, want: "CHANGELOG.md"},
		"security supported row":      {edits: []edit{{"SECURITY.md", "| `v1.2.3` |", "| `v1.2.2` |"}}, want: "SECURITY.md"},
		"security two supported rows": {edits: []edit{{"SECURITY.md", "| `< v1.2.3` | :x: |", "| `v1.2.2` | :white_check_mark: |\n| `< v1.2.2` | :x: |"}}, want: "SECURITY.md"},
		"security unsupported row":    {edits: []edit{{"SECURITY.md", "| `< v1.2.3` |", "| `< v1.2.2` |"}}, want: "SECURITY.md"},
		"install.sh default ref":      {edits: []edit{{"install.sh", `GUSSET_DEFAULT_REF="v1.2.3"`, `GUSSET_DEFAULT_REF="v1.2.2"`}}, want: "install.sh"},
		"install.sh ref without v":    {edits: []edit{{"install.sh", `GUSSET_DEFAULT_REF="v1.2.3"`, `GUSSET_DEFAULT_REF="1.2.3"`}}, want: "install.sh"},
		"adoption.md tag":             {edits: []edit{{"docs/adoption.md", `tag = "v1.2.3"`, `tag = "v1.2.2"`}}, want: "docs/adoption.md"},
		"adoption.md rev, not a tag":  {edits: []edit{{"docs/adoption.md", `tag = "v1.2.3"`, `rev = "abc123"`}}, want: "docs/adoption.md"},
		"cargo version not x.y.z":     {edits: []edit{{"Cargo.toml", `version = "1.2.3"`, `version = "1.2"`}}, want: "Cargo.toml"},
		"cargo version outside table": {edits: []edit{{"Cargo.toml", "[workspace.package]\nversion = \"1.2.3\"\n", "[workspace.package]\n"}}, want: "Cargo.toml"},
		"missing SECURITY.md":         {drop: "SECURITY.md", want: "SECURITY.md"},
		"missing docs/adoption.md":    {drop: "docs/adoption.md", want: "docs/adoption.md"},
	} {
		files := versionTree()
		for _, e := range c.edits {
			body := files[e.file]
			if !strings.Contains(body, e.from) {
				t.Fatalf("%s: fixture %s has no %q", name, e.file, e.from)
			}
			files[e.file] = strings.Replace(body, e.from, e.to, 1)
		}
		if c.drop != "" {
			delete(files, c.drop)
		}
		out, err := lint(t, files, nil, "version-parity")
		if err == nil || !strings.Contains(out, "version-parity FAIL") {
			t.Errorf("%s: the mismatch passed: %v\n%s", name, err, out)
			continue
		}
		if !strings.Contains(out, "::error::version-parity: "+c.want) {
			t.Errorf("%s: the failure does not name %s:\n%s", name, c.want, out)
		}
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
