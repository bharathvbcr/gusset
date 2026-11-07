package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bakedCommit is the commit versionTree's install.sh names. No tag points at it,
// so the tests that need the tag to exist plant one.
const bakedCommit = "0123456789abcdef0123456789abcdef01234567"

// tagFixture commits the tree, tags it v1.2.3 (annotated when annotated), and
// rewrites install.sh's baked commit to bake, or to the tag's commit when bake
// is empty. The tag's own tree cannot name the tag's commit, so the rewrite
// happens after tagging, in the working tree the step reads.
func tagFixture(t *testing.T, annotated bool, bake string) func(root string) {
	return func(root string) {
		t.Helper()
		git := func(args ...string) string {
			cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		git("commit", "-q", "-m", "release")
		if annotated {
			git("tag", "-a", "-m", "v1.2.3", "v1.2.3")
		} else {
			git("tag", "v1.2.3")
		}
		if bake == "" {
			bake = git("rev-parse", "v1.2.3^{commit}")
		}
		p := filepath.Join(root, "install.sh")
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(body), bakedCommit, bake, 1)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// The baked commit is held to the tag it belongs to. A moved tag, or a bump of
// the tag without the commit, is the disagreement that lets a default install
// build a tree nobody reviewed.
func TestVersionParityHoldsBakedCommitToTheTag(t *testing.T) {
	other := strings.Repeat("ab", 20)
	for name, c := range map[string]struct {
		annotated bool
		bake      string
		wantFail  bool
		want      string
	}{
		"lightweight tag matches":  {false, "", false, "(= v1.2.3)"},
		"annotated tag matches":    {true, "", false, "(= v1.2.3)"},
		"tag moved off the commit": {false, other, true, "tag v1.2.3 is "},
		"annotated tag moved":      {true, other, true, "tag v1.2.3 is "},
	} {
		out, err := lintWith(t, versionTree(), nil, tagFixture(t, c.annotated, c.bake), "version-parity")
		switch {
		case c.wantFail && (err == nil || !strings.Contains(out, "::error::version-parity: install.sh GUSSET_DEFAULT_COMMIT")):
			t.Errorf("%s: a baked commit that is not the tag's passed: %v\n%s", name, err, out)
		case !c.wantFail && (err != nil || !strings.Contains(out, "version-parity PASS")):
			t.Errorf("%s: an agreeing baked commit failed: %v\n%s", name, err, out)
		case !strings.Contains(out, c.want):
			t.Errorf("%s: output lacks %q:\n%s", name, c.want, out)
		}
	}
}

// With no tag in the checkout nothing can be compared. That passes the step but
// says so on the line: it must not read like a verified match.
func TestVersionParityReportsAnUncheckableBakedCommit(t *testing.T) {
	out, err := lint(t, versionTree(), nil, "version-parity")
	if err != nil || !strings.Contains(out, "NOT VERIFIED") {
		t.Fatalf("a checkout without the tag did not say the commit was unverified: %v\n%s", err, out)
	}
}

// A baked commit that is not exactly one full SHA is a FAIL whether or not the
// tag is present: a short or abbreviated value cannot be compared to HEAD.
func TestVersionParityRejectsAMalformedBakedCommit(t *testing.T) {
	for name, line := range map[string]string{
		"short":    `GUSSET_DEFAULT_COMMIT="ffae8aa"`,
		"upper":    `GUSSET_DEFAULT_COMMIT="` + strings.ToUpper(bakedCommit) + `"`,
		"empty":    `GUSSET_DEFAULT_COMMIT=""`,
		"missing":  ``,
		"doubled":  `GUSSET_DEFAULT_COMMIT="` + bakedCommit + "\"\nGUSSET_DEFAULT_COMMIT=\"" + bakedCommit + `"`,
		"a branch": `GUSSET_DEFAULT_COMMIT="main"`,
	} {
		files := versionTree()
		files["install.sh"] = strings.Replace(files["install.sh"], `GUSSET_DEFAULT_COMMIT="`+bakedCommit+`"`, line, 1)
		out, err := lint(t, files, nil, "version-parity")
		if err == nil || !strings.Contains(out, "::error::version-parity: install.sh GUSSET_DEFAULT_COMMIT") {
			t.Errorf("%s: a malformed baked commit passed: %v\n%s", name, err, out)
		}
	}
}
