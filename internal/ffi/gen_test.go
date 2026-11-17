package ffi

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset/internal/ltostrip"
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
	// gen.go imports internal/ltostrip, so the tree carries the real package
	// (not a test double) under the repository's own module path.
	strip, err := os.ReadFile(filepath.Join("..", "ltostrip", "ltostrip.go"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "internal", "ltostrip", "ltostrip.go"), strip)
	writeFile(t, filepath.Join(root, "go.mod"), []byte("module github.com/bharathvbcr/gusset\n\ngo 1.27\n"))
	return root, pkg
}

// Where cargo leaves libgusset.a, release first, relative to the package
// directory. gen.go's contract: the archive it stages is the first that exists.
var builtPaths = []string{
	filepath.Join("..", "..", "target", "release", "libgusset.a"),
	filepath.Join("..", "..", "target", "debug", "libgusset.a"),
}

// ltoArchive builds a real archive defining symbol, with the LTO section a Rust
// staticlib carries, using the host's cc and ar as cgo does.
func ltoArchive(t *testing.T, path, symbol string) {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Fatalf("no cc on PATH: cgo cannot work here either, so this test cannot run: %v", err)
	}
	ar, err := exec.LookPath("ar")
	if err != nil {
		t.Fatalf("no ar on PATH: %v", err)
	}
	asm := ".section .llvmbc,\\\"\\\",%progbits\\n.byte 1,2,3,4\\n.text\\n"
	if runtime.GOOS == "darwin" {
		asm = ".section __LLVM,__bitcode\\n.byte 1,2,3,4\\n.text\\n"
	}
	dir := t.TempDir()
	src := filepath.Join(dir, symbol+".c")
	writeFile(t, src, []byte("int "+symbol+" = 1;\n__asm__(\""+asm+"\");\n"))
	obj := filepath.Join(dir, symbol+".o")
	if out, err := exec.Command(cc, "-c", "-o", obj, src).CombinedOutput(); err != nil {
		t.Fatalf("cc -c: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ar, "rcs", path, obj).CombinedOutput(); err != nil {
		t.Fatalf("ar rcs: %v\n%s", err, out)
	}
	if found, err := ltostrip.Scan(path); err != nil || len(found) == 0 {
		t.Fatalf("setup: %s should carry an LTO section: findings=%v err=%v", path, found, err)
	}
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

// linkDirs is each -L in ffi.go's default (non-pkg-config) LDFLAGS, relative to
// the package directory.
func linkDirs(t *testing.T) []string {
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
				t.Fatalf("ffi.go links from %q, which is not under ${SRCDIR}; gen.go cannot stage into it", dir)
			}
			dirs = append(dirs, filepath.FromSlash(rel))
		}
	}
	if len(dirs) == 0 {
		t.Fatalf("no -L in a %q line of ffi.go", directive)
	}
	return dirs
}

// goLibFile is the go command's own filter (cmd/go/internal/work/security.go,
// reLinkerLibFile): every file it validates when it walks a -L directory.
var goLibFile = regexp.MustCompile(`(\.(a|o|obj|dll|dylib|lib|so|tbd)$|\.so\.)`)

// The go command walks every local -L directory whole and refuses any library
// in it that carries LTO IR (GO-2026-6853, Go 1.26.11 and 1.27.4). This asserts
// the property itself, on the directories ffi.go really links from, rather than
// the symptom of one Go patch release: it fails on a tree that links from
// cargo's output directories, whatever Go version runs it.
func TestLinkDirsCarryNoLTOSections(t *testing.T) {
	var walked int
	for _, dir := range linkDirs(t) {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !goLibFile.MatchString(strings.ToLower(d.Name())) {
				return nil
			}
			walked++
			found, err := ltostrip.Scan(path)
			if err != nil {
				t.Errorf("%s cannot be inspected: %v", path, err)
			}
			for _, f := range found {
				t.Errorf("%s carries %s: the go command would refuse it. Run `go generate ./internal/ffi` after the cargo build", path, f)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if walked == 0 {
		t.Skip("no library under ffi.go's -L directories: run `cargo build --locked --release && go generate ./internal/ffi` first; nothing was checked")
	}
}

// ffi.go links from exactly one directory, the one gen.go fills.
func TestGen_FFILinksOnlyFromTheStagedDirectory(t *testing.T) {
	dirs := linkDirs(t)
	if len(dirs) != 1 {
		t.Fatalf("ffi.go has %d -L directories %v; the go command walks all of them, so only the staged one may be named", len(dirs), dirs)
	}
	_, pkg := genTree(t)
	ltoArchive(t, filepath.Join(pkg, builtPaths[0]), "link_dir_probe")
	if out, err := runGen(t, pkg); err != nil {
		t.Fatalf("gen.go: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(pkg, dirs[0], "libgusset.a")); err != nil {
		t.Errorf("gen.go did not stage libgusset.a into ffi.go's -L directory %s: %v", dirs[0], err)
	}
}

// With no archive anywhere cargo writes one, gen.go used to write a placeholder
// hash over the tracked file and exit 0: a hash it could not compute reported
// as one it had. It must fail, say where it looked, and write nothing — neither
// the stub, a previously generated hash, nor anything the linker could pick up.
func TestGen_NoArchiveFailsAndWritesNothing(t *testing.T) {
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
			for _, rel := range builtPaths {
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
			if _, err := os.Stat(filepath.Join(pkg, "..", "..", "target", "gusset", "libgusset.a")); err == nil {
				t.Error("a failed run left a staged archive the linker could pick up")
			}
		})
	}
}

// gen.go stages the archive cargo built, with its LTO sections removed, and
// hashes the staged file: the one the linker reads. release wins over debug
// when both exist, as it did when the linker chose between them. The built
// archive is left alone, and the hash lands in the ignored file, never in the
// tracked stub.
func TestGen_StagesAStrippedArchiveAndHashesIt(t *testing.T) {
	_, pkg := genTree(t)
	stub := readOrNil(t, filepath.Join(pkg, "archive_hash.go"))
	staged := filepath.Join(pkg, "..", "..", "target", "gusset", "libgusset.a")

	// Fill the search path from the back, so each step puts a new archive in
	// front of the last one.
	for i := len(builtPaths) - 1; i >= 0; i-- {
		built := filepath.Join(pkg, builtPaths[i])
		symbol := fmt.Sprintf("built_%d", i)
		ltoArchive(t, built, symbol)
		builtBytes := readOrNil(t, built)

		if out, err := runGen(t, pkg); err != nil {
			t.Fatalf("gen.go with %s present: %v\n%s", builtPaths[i], err, out)
		}
		data := readOrNil(t, staged)
		if !bytes.Contains(data, []byte(symbol)) {
			t.Fatalf("with %s first on the search path, the staged archive does not hold its symbol %s", builtPaths[i], symbol)
		}
		if found, err := ltostrip.Scan(staged); err != nil || len(found) != 0 {
			t.Fatalf("the staged archive still carries LTO sections: %v (err=%v)", found, err)
		}
		if !bytes.Equal(readOrNil(t, built), builtBytes) {
			t.Fatalf("gen.go modified the archive cargo built, %s", builtPaths[i])
		}
		got := readOrNil(t, filepath.Join(pkg, generatedHash))
		wantHash := fmt.Sprintf("%x", sha256.Sum256(data))
		if !bytes.Contains(got, []byte(`const ArchiveHash = "`+wantHash+`"`)) {
			t.Fatalf("with %s first on the search path, %s does not hold the staged file's hash %s:\n%s", builtPaths[i], generatedHash, wantHash, got)
		}
		if formatted, err := format.Source(got); err != nil || !bytes.Equal(formatted, got) {
			t.Fatalf("%s is not gofmt output (err=%v):\n%s", generatedHash, err, got)
		}
		if after := readOrNil(t, filepath.Join(pkg, "archive_hash.go")); !bytes.Equal(after, stub) {
			t.Fatalf("gen.go rewrote the tracked stub archive_hash.go:\n%s", after)
		}
	}

	// An empty archive is not a hash of anything the linker can use.
	writeFile(t, filepath.Join(pkg, builtPaths[0]), nil)
	before := readOrNil(t, filepath.Join(pkg, generatedHash))
	out, err := runGen(t, pkg)
	if err == nil {
		t.Fatalf("gen.go accepted an empty %s:\n%s", builtPaths[0], out)
	}
	if after := readOrNil(t, filepath.Join(pkg, generatedHash)); !bytes.Equal(after, before) {
		t.Fatalf("a failed run changed %s", generatedHash)
	}
}

// A strip that fails must not leave the previous build's archive to be linked
// under a failed generate: that is old Rust code behind an error nobody reads.
func TestGen_FailedStagingLeavesNothingToLink(t *testing.T) {
	_, pkg := genTree(t)
	staged := filepath.Join(pkg, "..", "..", "target", "gusset", "libgusset.a")
	ltoArchive(t, filepath.Join(pkg, builtPaths[0]), "fresh_build")
	writeFile(t, staged, []byte("the previous build's archive"))
	prior := []byte("package ffi\n\n// a previous run's output\n")
	writeFile(t, filepath.Join(pkg, generatedHash), prior)

	out, err := runGen(t, pkg, "OBJCOPY="+filepath.Join(pkg, "no-such-objcopy"))
	if err == nil {
		t.Fatalf("gen.go succeeded with an objcopy that does not exist:\n%s", out)
	}
	if !strings.Contains(out, "no-such-objcopy") {
		t.Errorf("the error does not name the tool that failed:\n%s", out)
	}
	if _, err := os.Stat(staged); err == nil {
		t.Error("the previous build's archive is still staged after a failed generate")
	}
	if got := readOrNil(t, filepath.Join(pkg, generatedHash)); !bytes.Equal(got, prior) {
		t.Errorf("a failed run changed %s", generatedHash)
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
