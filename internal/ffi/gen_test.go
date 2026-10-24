package ffi

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// generatedHash is the file gen.go writes. It is per machine and gitignored;
// archive_hash.go is the tracked stub beside it.
const generatedHash = "zz_archive_hash.go"

// genTree lays out <root>/internal/ffi with copies of gen.go and the tracked
// stub, so gen.go runs exactly as `go generate` runs it (cwd = the package
// directory) against a target/ that the test controls.
func genTree(t *testing.T) (root, pkg string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; this check did not run")
	}
	root = t.TempDir()
	pkg = filepath.Join(root, "internal", "ffi")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gen.go", "archive_hash.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(pkg, name), data)
	}
	writeFile(t, filepath.Join(root, "go.mod"), []byte("module example.com/gentree\n\ngo 1.26\n"))
	return root, pkg
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// runGen runs gen.go in pkg. CARGO_TARGET_DIR is removed so a developer's
// global setting cannot change what is being tested.
func runGen(t *testing.T, pkg string, extraEnv ...string) (stderr string, err error) {
	t.Helper()
	cmd := exec.Command("go", "run", "gen.go")
	cmd.Dir = pkg
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CARGO_TARGET_DIR=") && !strings.HasPrefix(kv, "PWD=") {
			env = append(env, kv)
		}
	}
	// PWD names pkg as this test spells it, so gen.go's absolute paths match
	// the ones checked below even where the temp dir sits behind a symlink.
	cmd.Env = append(env, append([]string{"PWD=" + pkg, "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local"}, extraEnv...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), err
}

func readOrNil(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// linkerSearchPath is the archive path for each -L in ffi.go's default
// (non-pkg-config) LDFLAGS, in order, relative to the package directory.
func linkerSearchPath(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("ffi.go")
	if err != nil {
		t.Fatal(err)
	}
	const directive = "#cgo !gusset_pkgconfig LDFLAGS:"
	var dirs []string
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(line, directive) {
			continue
		}
		for _, f := range strings.Fields(strings.TrimPrefix(line, directive)) {
			dir, ok := strings.CutPrefix(f, "-L")
			if !ok {
				continue
			}
			rel, ok := strings.CutPrefix(dir, "${SRCDIR}/")
			if !ok {
				t.Fatalf("ffi.go links from %q, which is not under ${SRCDIR}; gen.go cannot hash it", dir)
			}
			dirs = append(dirs, filepath.Join(filepath.FromSlash(rel), "libgusset.a"))
		}
	}
	if len(dirs) == 0 {
		t.Fatalf("no -L in a %q line of ffi.go", directive)
	}
	return dirs
}

// With no archive anywhere cgo links from, gen.go used to write a placeholder
// hash over the tracked file and exit 0: a hash it could not compute reported
// as one it had. It must fail, say where it looked, and write nothing — neither
// the stub nor a previously generated hash may change.
func TestGen_NoArchiveFailsAndWritesNothing(t *testing.T) {
	want := linkerSearchPath(t)
	for _, prior := range []struct {
		name string
		data []byte
		env  []string
	}{
		{"no previous hash", nil, nil},
		{"previous hash kept", []byte("package ffi\n\n// a previous run's output\n"), nil},
		// gen.go does not follow CARGO_TARGET_DIR (cgo does not link from it),
		// but it says that cargo may have built there.
		{"redirected cargo named", nil, []string{"CARGO_TARGET_DIR=/elsewhere/cargo-target"}},
	} {
		t.Run(prior.name, func(t *testing.T) {
			_, pkg := genTree(t)
			if prior.data != nil {
				writeFile(t, filepath.Join(pkg, generatedHash), prior.data)
			}
			stub := readOrNil(t, filepath.Join(pkg, "archive_hash.go"))

			out, err := runGen(t, pkg, prior.env...)
			for _, kv := range prior.env {
				if dir := strings.TrimPrefix(kv, "CARGO_TARGET_DIR="); dir != kv && !strings.Contains(out, dir) {
					t.Errorf("output does not name CARGO_TARGET_DIR %s:\n%s", dir, out)
				}
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("gen.go with no archive: got err=%v, want a non-zero exit\n%s", err, out)
			}
			for _, rel := range want {
				abs, err := filepath.Abs(filepath.Join(pkg, rel))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out, abs) {
					t.Errorf("output does not name the searched path %s:\n%s", abs, out)
				}
			}
			if got := readOrNil(t, filepath.Join(pkg, "archive_hash.go")); !bytes.Equal(got, stub) {
				t.Errorf("archive_hash.go changed:\n--- before\n%s\n--- after\n%s", stub, got)
			}
			if got := readOrNil(t, filepath.Join(pkg, generatedHash)); !bytes.Equal(got, prior.data) {
				t.Errorf("%s changed:\n--- before\n%s\n--- after\n%s", generatedHash, prior.data, got)
			}
		})
	}
}

// gen.go must hash the archive the linker picks: the first of ffi.go's -L
// directories holding libgusset.a. release wins over debug when both exist,
// as it does for the linker, and the hash lands in the ignored file, never in
// the tracked stub.
func TestGen_HashesTheArchiveTheLinkerPicks(t *testing.T) {
	order := linkerSearchPath(t)
	if len(order) < 2 {
		t.Fatalf("expected at least two -L directories in ffi.go, got %v", order)
	}
	_, pkg := genTree(t)
	stub := readOrNil(t, filepath.Join(pkg, "archive_hash.go"))

	// Fill the search path from the back, so each step puts a new archive in
	// front of the last one.
	for i := len(order) - 1; i >= 0; i-- {
		archive := []byte(fmt.Sprintf("archive %d of %v", i, order))
		writeFile(t, filepath.Join(pkg, order[i]), archive)

		if out, err := runGen(t, pkg); err != nil {
			t.Fatalf("gen.go with %s present: %v\n%s", order[i], err, out)
		}
		got := readOrNil(t, filepath.Join(pkg, generatedHash))
		wantHash := fmt.Sprintf("%x", sha256.Sum256(archive))
		if !bytes.Contains(got, []byte(`const ArchiveHash = "`+wantHash+`"`)) {
			t.Fatalf("with %s first on the search path, %s does not hold its hash %s:\n%s", order[i], generatedHash, wantHash, got)
		}
		if formatted, err := format.Source(got); err != nil || !bytes.Equal(formatted, got) {
			t.Fatalf("%s is not gofmt output (err=%v):\n%s", generatedHash, err, got)
		}
		if after := readOrNil(t, filepath.Join(pkg, "archive_hash.go")); !bytes.Equal(after, stub) {
			t.Fatalf("gen.go rewrote the tracked stub archive_hash.go:\n%s", after)
		}
	}

	// An empty archive is not a hash of anything the linker can use.
	writeFile(t, filepath.Join(pkg, order[0]), nil)
	before := readOrNil(t, filepath.Join(pkg, generatedHash))
	out, err := runGen(t, pkg)
	if err == nil {
		t.Fatalf("gen.go accepted an empty %s:\n%s", order[0], out)
	}
	if after := readOrNil(t, filepath.Join(pkg, generatedHash)); !bytes.Equal(after, before) {
		t.Fatalf("a failed run changed %s", generatedHash)
	}
}

// The hash describes one machine's archive. Tracked, it dirtied every checkout
// that ran `go generate` and needed "update archive hash" commits; it is
// generated into an ignored file, and the tracked stub declares nothing that
// file would collide with.
func TestGen_HashFileIsIgnoredAndStubDeclaresNothing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; this check did not run")
	}
	if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").CombinedOutput(); err != nil {
		t.Skipf("not a git work tree (%v: %s); this check did not run", err, out)
	}
	if out, err := exec.Command("git", "check-ignore", "-q", "--no-index", generatedHash).CombinedOutput(); err != nil {
		t.Errorf("internal/ffi/%s is not gitignored (%v) %s", generatedHash, err, out)
	}
	if out, err := exec.Command("git", "ls-files", "--", generatedHash).Output(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Errorf("internal/ffi/%s is tracked (err=%v): %s", generatedHash, err, out)
	}
	if out, err := exec.Command("git", "ls-files", "--error-unmatch", "--", "archive_hash.go").CombinedOutput(); err != nil {
		t.Errorf("the stub archive_hash.go is not tracked (%v): %s", err, out)
	}

	f, err := parser.ParseFile(token.NewFileSet(), "archive_hash.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Decls) != 0 {
		t.Errorf("archive_hash.go declares %d things; the stub must declare nothing, or it collides with %s", len(f.Decls), generatedHash)
	}

	// The placeholder gen.go used to write in place of a hash. Split so this
	// file does not contain it.
	sentinel := "no-archive" + "-found"
	out, err := exec.Command("git", "grep", "-n", "-F", "-e", sentinel, "--", ":/").CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		t.Errorf("the placeholder %q is still in the tree:\n%s", sentinel, out)
	case !errors.As(err, &exit) || exit.ExitCode() != 1:
		t.Errorf("git grep did not run (%v): %s", err, out)
	}
}
