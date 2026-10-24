package ci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeDry runs `make -n <goal>` at the repository root with CARGO_TARGET_DIR
// set to dir, or removed when dir is empty, so a developer's global setting
// cannot decide the outcome.
func makeDry(t *testing.T, makePath, goal, dir string, args ...string) (string, error) {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CARGO_TARGET_DIR=") {
			env = append(env, kv)
		}
	}
	if dir != "" {
		env = append(env, "CARGO_TARGET_DIR="+dir)
	}
	cmd := exec.Command(makePath, append([]string{"-n", "-C", "../..", goal}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// With CARGO_TARGET_DIR=/x, `make build` ran `cargo build` into /x and then
// `go generate` and `go build` against ./target, which internal/ffi/ffi.go and
// gen.go hard-code: the link took whatever archive ./target still held (an old
// one, silently) and the hash described that old one. Every target that builds
// the archive and then links Go against it must refuse a redirected target
// directory, naming both archives, and do so under `make -n` as well, which is
// how this test asks without building anything. `install` honours the
// variable and must keep accepting it.
func TestMakefile_RedirectedTargetDirIsRefused(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("no make on PATH; this check did not run")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// make's $(CURDIR) is the physical directory after -C.
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "target", "release", "libgusset.a")
	elsewhere := t.TempDir()
	built := filepath.Join(elsewhere, "release", "libgusset.a")

	for _, goal := range []string{"build", "test", "bench", "bench-crossover", "bench-scaling", "ci-local"} {
		t.Run(goal+"/refused", func(t *testing.T) {
			out, err := makeDry(t, makePath, goal, elsewhere)
			if err == nil {
				t.Fatalf("CARGO_TARGET_DIR=%s make -n %s succeeded; it must refuse:\n%s", elsewhere, goal, out)
			}
			for _, p := range []string{built, linked} {
				if !strings.Contains(out, p) {
					t.Errorf("the refusal does not name %s:\n%s", p, out)
				}
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "cargo ") || strings.HasPrefix(line, "go ") {
					t.Errorf("make printed a command before refusing: %q\n%s", line, out)
				}
			}
		})
	}

	// The same directory spelled three ways is not a redirection.
	for name, dir := range map[string]string{
		"unset":    "",
		"relative": "target",
		"absolute": filepath.Join(root, "target") + "/",
	} {
		t.Run("build/accepted/"+name, func(t *testing.T) {
			out, err := makeDry(t, makePath, "build", dir)
			if err != nil {
				t.Fatalf("CARGO_TARGET_DIR=%q make -n build: %v\n%s", dir, err, out)
			}
			for _, step := range []string{"cargo build --locked --release", "go generate ./internal/ffi", "go build ./..."} {
				if !strings.Contains(out, step) {
					t.Errorf("make -n build does not run %q:\n%s", step, out)
				}
			}
		})
	}

	t.Run("install/accepted", func(t *testing.T) {
		out, err := makeDry(t, makePath, "install", elsewhere, "PREFIX="+t.TempDir())
		if err != nil {
			t.Fatalf("CARGO_TARGET_DIR=%s make -n install: %v\n%s", elsewhere, err, out)
		}
		if !strings.Contains(out, built) {
			t.Errorf("make -n install does not install the archive cargo built at %s:\n%s", built, out)
		}
	})
}
