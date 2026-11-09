package ci_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A behavioural check of the mechanism the test-uniffi-link recipe relies on,
// with no cargo and no Gusset: a toy C static archive linked by a tiny cgo
// program, built with `go build -o` onto an existing output file (the cached-
// link form; a plain `go test -count=1` relinks every run and never reuses a
// link). The recipe's mechanism is the digest: sha256 of the archive, passed in
// `-ldflags "-X <pkg>.umbrellaSHA256=<digest>"`, which the link's cache key
// includes.
//
//	control: without the digest, the existing output is kept after the archive
//	         changes (the measured go1.27.3 behaviour), so the old code runs;
//	recipe:  with the digest, the same sequence relinks and the new code runs.
//
// If cgo, a C compiler or ar is unavailable the test SKIPs with the reason; it
// does not pass silently.

const (
	toyMain = `package main

/*
#cgo LDFLAGS: -ltoy
int toy_value(void);
*/
import "C"

import "fmt"

var umbrellaSHA256 string

func main() { fmt.Println(C.toy_value()) }
`
	toyMod = "module toy\n\ngo 1.21\n"
)

// toyEnv is the environment of one go command: cgo on, the archive directory on
// the link line the way the recipe passes it (CGO_LDFLAGS), no workspace.
func toyEnv(archiveDir string) []string {
	return append(os.Environ(), "CGO_ENABLED=1", "GOWORK=off", "GOFLAGS=", "CGO_LDFLAGS=-L"+archiveDir)
}

func runBounded(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// toyArchive compiles `int toy_value(void) { return n; }` into dir/libtoy.a and
// returns its sha256, the digest the recipe computes with sha256sum.
func toyArchive(t *testing.T, cc []string, ar, dir string, n int) string {
	t.Helper()
	src := filepath.Join(dir, "toy.c")
	if err := os.WriteFile(src, []byte(fmt.Sprintf("int toy_value(void) { return %d; }\n", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	obj := filepath.Join(dir, "toy.o")
	runBounded(t, dir, os.Environ(), cc[0], append(append([]string{}, cc[1:]...), "-c", "-o", obj, src)...)
	lib := filepath.Join(dir, "libtoy.a")
	_ = os.Remove(lib)
	runBounded(t, dir, os.Environ(), ar, "rcs", lib, obj)
	b, err := os.ReadFile(lib)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestDigestInLdflagsRelinksOnAChangedArchive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ccOut, err := exec.CommandContext(ctx, "go", "env", "CC", "CGO_ENABLED").Output()
	if err != nil {
		t.Skipf("cannot ask go for CC: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(string(ccOut)), "\n")
	if len(fields) < 2 || strings.TrimSpace(fields[1]) != "1" {
		t.Skip("cgo is disabled (go env CGO_ENABLED is not 1); the link-cache behaviour cannot be exercised")
	}
	cc := strings.Fields(fields[0])
	if len(cc) == 0 {
		t.Skip("go env CC is empty")
	}
	if _, err := exec.LookPath(cc[0]); err != nil {
		t.Skipf("no C compiler %q on PATH: %v", cc[0], err)
	}
	ar, err := exec.LookPath("ar")
	if err != nil {
		t.Skip("no ar on PATH; cannot build the toy static archive")
	}

	prog := t.TempDir()
	for name, body := range map[string]string{"main.go": toyMain, "go.mod": toyMod} {
		if err := os.WriteFile(filepath.Join(prog, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ldflags := func(digest string) []string {
		return []string{"-ldflags", "-X main.umbrellaSHA256=" + digest}
	}
	build := func(out, archiveDir string, extra ...string) string {
		args := append([]string{"build", "-o", out}, extra...)
		runBounded(t, prog, toyEnv(archiveDir), "go", args...)
		return runBounded(t, prog, nil, out)
	}

	// Control: no digest. The second build onto the existing output keeps the
	// first binary, so a changed archive is not picked up.
	ctl := t.TempDir()
	d1 := toyArchive(t, cc, ar, ctl, 1)
	outCtl := filepath.Join(t.TempDir(), "toy-control")
	if got := build(outCtl, ctl); got != "1" {
		t.Fatalf("control, first build: got %q, want 1", got)
	}
	d2 := toyArchive(t, cc, ar, ctl, 2)
	if d1 == d2 {
		t.Fatal("the two toy archives hash alike; the fixture does not change the archive")
	}
	// The control documents why the digest exists; it is not the contract. A
	// toolchain that relinks here is correct, not broken, so this logs rather
	// than fails: failing would turn a Go fix into a red build while the
	// recipe assertions below still hold.
	if got := build(outCtl, ctl); got != "1" {
		t.Logf("control did not reproduce: without the digest, a rebuild onto an existing output printed %q, not the stale 1. "+
			"The measured go1.27.3 behaviour (the linker is skipped) does not hold on this toolchain (%s); "+
			"the recipe's digest is then not needed here, but the assertions below still hold",
			got, runBounded(t, prog, nil, "go", "version"))
	}

	// The recipe's mechanism: the archive's sha256 in -ldflags. The same
	// sequence now relinks, and the new code runs.
	rec := t.TempDir()
	r1 := toyArchive(t, cc, ar, rec, 1)
	outRec := filepath.Join(t.TempDir(), "toy-recipe")
	if got := build(outRec, rec, ldflags(r1)...); got != "1" {
		t.Fatalf("recipe, first build: got %q, want 1", got)
	}
	r2 := toyArchive(t, cc, ar, rec, 2)
	if got := build(outRec, rec, ldflags(r2)...); got != "2" {
		t.Errorf("recipe: with the changed archive's digest in -ldflags the binary printed %q, want 2 (stale link)", got)
	}
	// An unchanged archive and digest is still correct.
	if got := build(outRec, rec, ldflags(r2)...); got != "2" {
		t.Errorf("recipe, repeated build: got %q, want 2", got)
	}
}
