package ci_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These are the supply-chain rules from the CI hardening audit, held as
// tests so a new workflow step cannot quietly undo them. The parsing is
// line-based on purpose: the workflows are flat enough for it, and a YAML
// dependency would be a new dependency for the sake of a lint.

var (
	usesLine   = regexp.MustCompile(`^\s*(?:- )?uses:\s*(\S+)(.*)$`)
	pinnedUses = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	jobHeader  = regexp.MustCompile(`^  [A-Za-z0-9_-]+:\s*$`)
)

// workflowFiles lists the workflow files in dir: GitHub runs .yml and .yaml
// alike, and a rule that read one extension would never see a workflow added
// under the other.
func workflowFiles(dir string) ([]string, error) {
	var paths []string
	for _, ext := range []string{"yml", "yaml"} {
		found, err := filepath.Glob(filepath.Join(dir, "*."+ext))
		if err != nil {
			return nil, err
		}
		paths = append(paths, found...)
	}
	return paths, nil
}

func workflows(t *testing.T) map[string][]string {
	t.Helper()
	paths, err := workflowFiles("../../.github/workflows")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	out := map[string][]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = strings.Split(string(b), "\n")
	}
	return out
}

func TestEveryActionIsPinnedToACommitWithAVersionComment(t *testing.T) {
	for name, lines := range workflows(t) {
		for i, l := range lines {
			m := usesLine.FindStringSubmatch(l)
			if m == nil || strings.HasPrefix(m[1], "./") {
				continue
			}
			if !pinnedUses.MatchString(m[1]) {
				t.Errorf("%s:%d: %s is not pinned to a full commit SHA", name, i+1, m[1])
			}
			if !regexp.MustCompile(`#\s*v\d`).MatchString(m[2]) {
				t.Errorf("%s:%d: %s has no `# vX` version comment", name, i+1, m[1])
			}
		}
	}
}

// dtolnay/rust-toolchain reads the toolchain from the ref name when none is
// given, and a SHA has no such name, so a pinned use must say which one.
func TestRustToolchainNamesItsToolchain(t *testing.T) {
	for name, lines := range workflows(t) {
		for i, l := range lines {
			if m := usesLine.FindStringSubmatch(l); m == nil || !strings.HasPrefix(m[1], "dtolnay/rust-toolchain@") {
				continue
			}
			end := i + 1
			for end < len(lines) && (strings.TrimSpace(lines[end]) == "" || strings.HasPrefix(lines[end], "          ") || strings.HasSuffix(strings.TrimSpace(lines[end]), "with:")) {
				end++
			}
			if !strings.Contains(strings.Join(lines[i:end], "\n"), "toolchain:") {
				t.Errorf("%s:%d: rust-toolchain pinned by SHA with no `toolchain:` input", name, i+1)
			}
		}
	}
}

func TestNoUnpinnedToolInstalls(t *testing.T) {
	for name, lines := range workflows(t) {
		for i, l := range lines {
			s := strings.TrimSpace(l)
			if strings.HasPrefix(s, "#") {
				continue
			}
			if strings.Contains(s, "@latest") {
				t.Errorf("%s:%d: @latest in a CI step: %s", name, i+1, s)
			}
			if strings.Contains(s, "cargo install") && !strings.Contains(s, "--locked") {
				t.Errorf("%s:%d: cargo install without --locked: %s", name, i+1, s)
			}
		}
	}
}

// A job that checks out and builds code must not hold a token that can write
// issues, and must not persist the token into .git/config.
func TestBuildJobsHoldNoWriteTokenAndPersistNoCredentials(t *testing.T) {
	for name, lines := range workflows(t) {
		var jobs []string
		var header []string
		cur := -1
		inJobs := false
		for _, l := range lines {
			if strings.TrimRight(l, " ") == "jobs:" {
				inJobs = true
				continue
			}
			if !inJobs {
				header = append(header, l)
				continue
			}
			if jobHeader.MatchString(l) {
				jobs = append(jobs, "")
				cur++
			}
			if cur >= 0 {
				jobs[cur] += l + "\n"
			}
		}
		for _, l := range header {
			if regexp.MustCompile(`^\s+[\w-]+:\s*write\s*$`).MatchString(l) {
				t.Errorf("%s: workflow-level write permission reaches every job: %s", name, strings.TrimSpace(l))
			}
		}
		for _, job := range jobs {
			title := strings.TrimSpace(strings.SplitN(job, "\n", 2)[0])
			checkouts := strings.Count(job, "actions/checkout@")
			if checkouts == 0 {
				continue
			}
			if strings.Contains(job, ": write") {
				t.Errorf("%s %s: checks out code and holds a write permission; file the issue from a separate job", name, title)
			}
			if got := strings.Count(job, "persist-credentials: false"); got != checkouts {
				t.Errorf("%s %s: %d checkouts but %d with persist-credentials: false", name, title, checkouts, got)
			}
		}
	}
}
