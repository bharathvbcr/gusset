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

// tagWhere says where the fixture's v1.2.3 tag lives after setup.
type tagWhere int

const (
	tagLocal   tagWhere = iota // in the checkout
	tagOrigin                  // only on origin, as in a CI checkout without tags
	tagNowhere                 // not yet created: the release window
)

// tagFixture commits the tree, tags it v1.2.3 (annotated when annotated) where
// the tag says, and rewrites install.sh's baked commit to bake, or to the tag's
// commit when bake is empty. The tag's own tree cannot name the tag's commit, so
// the rewrite happens after tagging, in the working tree the step reads.
// unreachable points origin at a path that does not exist.
func tagFixture(t *testing.T, where tagWhere, annotated bool, bake string, unreachable bool) func(root string) {
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
		tagged := git("rev-parse", "HEAD")
		if where != tagNowhere {
			if annotated {
				git("tag", "-a", "-m", "v1.2.3", "v1.2.3")
			} else {
				git("tag", "v1.2.3")
			}
		}
		if where == tagOrigin {
			git("push", "-q", "origin", "refs/tags/v1.2.3")
			git("tag", "-d", "v1.2.3")
		}
		if unreachable {
			git("remote", "set-url", "origin", filepath.Join(root, "no-such-origin"))
		}
		if bake == "" {
			bake = tagged
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

// The baked commit is held to the tag it belongs to, wherever the checkout can
// read the tag from. A moved tag, or a bump of the tag without the commit, is the
// disagreement that lets a default install build a tree nobody reviewed.
func TestVersionParityHoldsBakedCommitToTheTag(t *testing.T) {
	other := strings.Repeat("ab", 20)
	for name, c := range map[string]struct {
		where     tagWhere
		annotated bool
		bake      string
		wantFail  bool
		want      string
	}{
		"local lightweight matches":     {tagLocal, false, "", false, "(= v1.2.3, from this checkout)"},
		"local annotated matches":       {tagLocal, true, "", false, "(= v1.2.3, from this checkout)"},
		"local tag moved":               {tagLocal, false, other, true, "tag v1.2.3 is "},
		"local annotated moved":         {tagLocal, true, other, true, "tag v1.2.3 is "},
		"origin only lightweight match": {tagOrigin, false, "", false, "from origin)"},
		"origin only annotated match":   {tagOrigin, true, "", false, "from origin)"},
		"origin only lightweight moved": {tagOrigin, false, other, true, "(origin)"},
		"origin only annotated moved":   {tagOrigin, true, other, true, "(origin)"},
	} {
		out, err := lintWith(t, versionTree(), nil, tagFixture(t, c.where, c.annotated, c.bake, false), "version-parity")
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

// Neither here nor on origin: origin answered and has no such tag, which is the
// release window. The run still exits 0 (DECISIONS.md 2026-11-06), but the
// summary must not call it a PASS: it says NOT VERIFIED (release window), and
// the closing line says no step failed yet one is not verified.
func TestVersionParityReportsNotVerifiedInTheReleaseWindow(t *testing.T) {
	out, err := lintWith(t, versionTree(), nil, tagFixture(t, tagNowhere, false, "", false), "version-parity")
	if err != nil {
		t.Fatalf("the release window must not fail the run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "version-parity NOT VERIFIED (release window)") {
		t.Errorf("the summary lacks the NOT VERIFIED status:\n%s", out)
	}
	if strings.Contains(out, "version-parity PASS") || strings.Contains(out, "every step passed") {
		t.Errorf("a step that verified nothing was summarised as a pass:\n%s", out)
	}
	if !strings.Contains(out, "::warning::version-parity:") || !strings.Contains(out, "is not a PASS") {
		t.Errorf("the warning or the closing line is missing:\n%s", out)
	}
}

// NOT VERIFIED is an exception to the exit rule for that one case, not a way to
// hide a real failure: a version that disagrees in the same run is still FAIL
// and still exits non-zero.
func TestVersionParityNotVerifiedDoesNotHideAMismatch(t *testing.T) {
	files := versionTree()
	files["SECURITY.md"] = strings.ReplaceAll(files["SECURITY.md"], "1.2.3", "1.2.2")
	out, err := lintWith(t, files, nil, tagFixture(t, tagNowhere, false, "", false), "version-parity")
	if err == nil || !strings.Contains(out, "version-parity FAIL") {
		t.Errorf("a mismatch next to an unverified commit did not fail:\n%v\n%s", err, out)
	}
	if strings.Contains(out, "version-parity NOT VERIFIED (release window)") {
		t.Errorf("a failing step was summarised as NOT VERIFIED:\n%s", out)
	}
}

// A checkout without the tag whose origin cannot be asked verified nothing, and
// must not exit like a pass. Both an unreachable origin and no origin at all.
func TestVersionParityFailsWhenOriginCannotBeAsked(t *testing.T) {
	noOrigin := func(root string) {
		cmd := exec.Command("git", "remote", "remove", "origin")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git remote remove: %v\n%s", err, out)
		}
	}
	for name, setup := range map[string]func(string){
		"origin unreachable": tagFixture(t, tagNowhere, false, "", true),
		"no origin":          noOrigin,
	} {
		out, err := lintWith(t, versionTree(), nil, setup, "version-parity")
		if err == nil || !strings.Contains(out, "version-parity FAIL") {
			t.Errorf("%s: an unverifiable baked commit passed: %v\n%s", name, err, out)
			continue
		}
		if !strings.Contains(out, "::error::version-parity: install.sh GUSSET_DEFAULT_COMMIT") || !strings.Contains(out, "was not verified") {
			t.Errorf("%s: the failure does not say the commit was not verified:\n%s", name, out)
		}
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
