package ci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// install.sh bakes the commit its default tag must resolve to and checks it
// without GUSSET_COMMIT (ft-415ddf1f, audit B1-7). These tests pipe the real
// script into bash against a local fixture repository whose tag is then moved,
// with cargo, go and make replaced by shims, so no network is used and nothing
// is built: the failure they look for is the refusal before any build step.

var (
	defaultRefLine    = regexp.MustCompile(`(?m)^GUSSET_DEFAULT_REF="([^"]+)"$`)
	defaultCommitLine = regexp.MustCompile(`(?m)^GUSSET_DEFAULT_COMMIT="[^"]*"$`)
)

type installFixture struct {
	repo, ref, tagged, moved string // moved is the commit the tag points at after the move
	script                   []byte
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newInstallFixture makes a repository with a Makefile whose install recipe
// honours DESTDIR, tags the default ref on its first commit and bakes that
// commit into a copy of install.sh. With moveTag the tag is then forced onto a
// second commit, as a compromised push credential would.
func newInstallFixture(t *testing.T, moveTag bool) installFixture {
	t.Helper()
	real, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := defaultRefLine.FindSubmatch(real)
	if m == nil {
		t.Fatal("install.sh has no GUSSET_DEFAULT_REF line")
	}
	f := installFixture{repo: t.TempDir(), ref: string(m[1])}
	gitIn(t, f.repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(f.repo, "Makefile"), []byte("install:\n\t@echo $(DESTDIR)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "commit", "-q", "-m", "release")
	f.tagged = gitIn(t, f.repo, "rev-parse", "HEAD")
	gitIn(t, f.repo, "tag", "-a", "-m", f.ref, f.ref)
	f.moved = f.tagged
	if moveTag {
		if err := os.WriteFile(filepath.Join(f.repo, "evil.txt"), []byte("not reviewed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, f.repo, "add", "-A")
		gitIn(t, f.repo, "commit", "-q", "-m", "attacker")
		f.moved = gitIn(t, f.repo, "rev-parse", "HEAD")
		gitIn(t, f.repo, "tag", "-f", "-a", "-m", f.ref, f.ref)
	}
	f.script = defaultCommitLine.ReplaceAll(real, []byte(`GUSSET_DEFAULT_COMMIT="`+f.tagged+`"`))
	if !strings.Contains(string(f.script), f.tagged) {
		t.Fatal("install.sh has no GUSSET_DEFAULT_COMMIT line to bake the fixture commit into")
	}
	return f
}

// run pipes the script into bash and reports its output, exit error and
// whether the make shim ran.
func (f installFixture) run(t *testing.T, env ...string) (out string, built bool, err error) {
	t.Helper()
	shims := t.TempDir()
	log := filepath.Join(t.TempDir(), "make.log")
	prefix, dest := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{
		"cargo": "exit 0\n",
		"go":    "exit 0\n",
		"make":  `echo make >>"` + log + `"; mkdir -p "$DESTDIR$GUSSET_PREFIX/bin"; printf '#!/bin/sh\n' >"$DESTDIR$GUSSET_PREFIX/bin/gussetvet"; chmod +x "$DESTDIR$GUSSET_PREFIX/bin/gussetvet"` + "\n",
	} {
		if err := os.WriteFile(filepath.Join(shims, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash")
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader(string(f.script))
	cmd.Env = append([]string{
		"PATH=" + shims + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GUSSET_REPO_URL=file://" + f.repo,
		"GUSSET_PREFIX=" + prefix,
		"DESTDIR=" + dest,
	}, env...)
	b, err := cmd.CombinedOutput()
	_, statErr := os.Stat(log)
	return string(b), statErr == nil, err
}

// A moved default tag is refused, before make, with no GUSSET_COMMIT set. This
// is the case the audit named: `curl .../install.sh | bash` of a moved tag.
func TestInstallRefusesAMovedDefaultTag(t *testing.T) {
	f := newInstallFixture(t, true)
	for name, env := range map[string][]string{
		"GUSSET_REF unset":          nil,
		"GUSSET_REF set to default": {"GUSSET_REF=" + f.ref},
	} {
		out, built, err := f.run(t, env...)
		if err == nil {
			t.Errorf("%s: a moved default tag installed:\n%s", name, out)
		}
		if built {
			t.Errorf("%s: make ran before the refusal:\n%s", name, out)
		}
		if !strings.Contains(out, "Nothing was built") || !strings.Contains(out, f.tagged) || !strings.Contains(out, f.moved) {
			t.Errorf("%s: the refusal does not name both commits and say nothing was built:\n%s", name, out)
		}
	}
}

// The control: the tag still at its baked commit installs by default, so the
// refusal above is the check and not a fixture that cannot install.
func TestInstallDefaultTagAtItsBakedCommitInstalls(t *testing.T) {
	f := newInstallFixture(t, false)
	out, built, err := f.run(t)
	if err != nil || !built {
		t.Fatalf("an unmoved default tag did not install (err=%v, make ran=%v):\n%s", err, built, out)
	}
}

// An explicit GUSSET_COMMIT is the reviewer's decision and wins over the baked
// one, in both directions.
func TestInstallExplicitCommitOverridesTheBakedOne(t *testing.T) {
	f := newInstallFixture(t, true)
	if out, built, err := f.run(t, "GUSSET_COMMIT="+f.moved); err != nil || !built {
		t.Errorf("a reviewed GUSSET_COMMIT for the moved tag did not install (err=%v, make ran=%v):\n%s", err, built, out)
	}
	out, built, err := f.run(t, "GUSSET_COMMIT="+f.tagged)
	if err == nil || built || !strings.Contains(out, "not the GUSSET_COMMIT") {
		t.Errorf("a GUSSET_COMMIT the tag no longer resolves to was not refused by that variable (err=%v, make ran=%v):\n%s", err, built, out)
	}
}

// A GUSSET_REF other than the default keeps its behaviour: cloned unchecked
// unless GUSSET_COMMIT is given. The baked commit belongs to the default tag.
func TestInstallCustomRefIsNotHeldToTheBakedCommit(t *testing.T) {
	f := newInstallFixture(t, true)
	gitIn(t, f.repo, "branch", "feature")
	out, built, err := f.run(t, "GUSSET_REF=feature")
	if err != nil || !built {
		t.Fatalf("a non-default GUSSET_REF was refused (err=%v, make ran=%v):\n%s", err, built, out)
	}
}
