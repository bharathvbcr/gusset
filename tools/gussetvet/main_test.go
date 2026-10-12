package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The line scan this replaced missed a C.free that is referenced but not called
// on the same line, and matched the rule's own text inside string literals.
func TestR4CatchesEveryReferenceToCFree(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "alias.go", `package p
/*
#include <stdlib.h>
*/
import "C"
import "unsafe"
func f(p unsafe.Pointer) { free := C.free; free(p) }
`)
	write(t, dir, "call.go", `package p
import "C"
import "unsafe"
func g(p unsafe.Pointer) { C.free(p) }
`)
	write(t, dir, "preamble.go", `package p
/*
#include <stdlib.h>
static void drop_it(void* p) { free(p); }
static void fine(void* p) { gusset_buf_free(0, 0, 0); }
*/
import "C"
`)
	write(t, dir, "clean.go", `package p
// C.free( in a comment is documentation, not a call.
var s = "C.free( in a string is not a call either // really"
`)
	v, err := checkAllocatorSymmetry(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(v, "\n")
	for _, want := range []string{"alias.go:7", "call.go:4", "preamble.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing violation for %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "clean.go") {
		t.Errorf("comment or string literal flagged:\n%s", got)
	}
	if n := strings.Count(got, "preamble.go"); n != 1 {
		t.Errorf("gusset_buf_free( must not match free(; got %d preamble hits:\n%s", n, got)
	}
}

// R5 reads the preamble only. A doc comment that names `#cgo noescape` used
// to fail every export; directives in a comment cgo never reads must not count.
func TestR5DirectivesComeFromThePreambleOnly(t *testing.T) {
	exports := []string{"gusset_cancel", "gusset_cancel_all"}

	good := t.TempDir()
	write(t, good, "ffi.go", `package ffi
/*
#cgo noescape gusset_cancel
#cgo nocallback gusset_cancel
#cgo noescape gusset_cancel_all
#cgo nocallback gusset_cancel_all
*/
import "C"

// A helper whose doc names the #cgo noescape lines is documentation.
func f() { C.gusset_cancel(); C.gusset_cancel_all() }
`)
	if v, err := checkDirectives(good, exports); err != nil || len(v) != 0 {
		t.Fatalf("doc comment naming a directive flagged: %v %v", v, err)
	}

	stray := t.TempDir()
	write(t, stray, "ffi.go", `package ffi
/*
#cgo noescape gusset_cancel_all
#cgo nocallback gusset_cancel_all
*/
import "C"

// #cgo noescape gusset_cancel
// #cgo nocallback gusset_cancel
func f() {}
`)
	v, err := checkDirectives(stray, exports)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(v, "\n")
	if len(v) != 2 || !strings.Contains(got, "'#cgo noescape gusset_cancel'") ||
		!strings.Contains(got, "'#cgo nocallback gusset_cancel'") {
		t.Fatalf("want gusset_cancel's two directives missing (prefix of gusset_cancel_all, stray comment ignored), got:\n%s", got)
	}

	// Every directive present, none of them where cgo reads them. The old scan
	// passed this.
	moved := t.TempDir()
	write(t, moved, "ffi.go", `package ffi
/*
#include <stdint.h>
*/
import "C"

// #cgo noescape gusset_cancel
// #cgo nocallback gusset_cancel
// #cgo noescape gusset_cancel_all
// #cgo nocallback gusset_cancel_all
func f() { C.gusset_cancel(); C.gusset_cancel_all() }
`)
	if v, err := checkDirectives(moved, exports); err == nil {
		t.Fatalf("directives outside the preamble must not pass R5, got %v", v)
	}
}

// The R4 and R5-callback scans used to walk ".", not the tree targetDir belongs
// to. install.sh runs `gussetvet "$SCRIPT_DIR/internal/ffi"` from wherever the
// installer was started, so both scans checked an unrelated directory and
// passed. Run from an empty directory against a module with one violation of
// each, and the run must fail on both.
func TestScansTheTargetsModuleFromAnyDirectory(t *testing.T) {
	mod := t.TempDir()
	write(t, mod, "go.mod", "module example.com/adopter\n\ngo 1.26\n")
	ffi := filepath.Join(mod, "internal", "ffi")
	if err := os.MkdirAll(ffi, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, ffi, "exports.txt", "gusset_a\n")
	write(t, ffi, "ffi.go", `package ffi
/*
#cgo noescape gusset_a
#cgo nocallback gusset_a
*/
import "C"
func A() { C.gusset_a() }
`)
	write(t, mod, "leak.go", `package adopter
import "C"
import "unsafe"
func g(p unsafe.Pointer) { C.free(p) }
//export goCallback
func goCallback() {}
`)

	t.Chdir(t.TempDir())
	var stdout, stderr strings.Builder
	if code := run(ffi, &stdout, &stderr); code == 0 {
		t.Fatalf("passed while run from outside the module:\n%s", stdout.String())
	}
	got := stderr.String()
	for _, want := range []string{"leak.go:4: R4", "leak.go:5: R5"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// With no go.mod above targetDir there is no tree to scan; reporting R4 as
// passed having walked nothing is the failure this tool exists to prevent.
func TestRefusesATargetOutsideAnyModule(t *testing.T) {
	ffi := t.TempDir()
	write(t, ffi, "exports.txt", "gusset_a\n")
	write(t, ffi, "ffi.go", `package ffi
/*
#cgo noescape gusset_a
#cgo nocallback gusset_a
*/
import "C"
func A() { C.gusset_a() }
`)
	t.Chdir(t.TempDir())
	var stdout, stderr strings.Builder
	if code := run(ffi, &stdout, &stderr); code == 0 {
		t.Fatalf("passed with no module to scan:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "go.mod") {
		t.Errorf("error does not say what was missing: %s", stderr.String())
	}
}

func TestR5RefusesExportedGoCallbacks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cb.go", `package p
import "C"
//export goCallback
func goCallback() {}
`)
	v, err := checkNoCallbacks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || !strings.Contains(v[0], "cb.go:3") {
		t.Fatalf("want one violation at cb.go:3, got %v", v)
	}
}

func TestR1RequiresAWrapperForEveryExport(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ffi.go", `package ffi
import "C"
func A() { C.gusset_a() }
`)
	v, err := checkWrappers(dir, []string{"gusset_a", "gusset_b", "# comment", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || !strings.Contains(v[0], "gusset_b") {
		t.Fatalf("want exactly gusset_b reported, got %v", v)
	}
}

// writeAdopterFFI lays out a minimal compliant ffi directory holding one
// export, the shape an adopter's own cgo shim has.
func writeAdopterFFI(t *testing.T, exportsBody string) string {
	t.Helper()
	mod := t.TempDir()
	write(t, mod, "go.mod", "module example.com/adopter\n\ngo 1.26\n")
	ffi := filepath.Join(mod, "shim")
	if err := os.MkdirAll(ffi, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, ffi, "shim.go", `package shim
/*
#cgo noescape myservice_engine_init
#cgo nocallback myservice_engine_init
void myservice_engine_init(void);
*/
import "C"
func Init() { C.myservice_engine_init() }
`)
	if exportsBody != "" {
		write(t, ffi, "exports.txt", exportsBody)
	}
	return ffi
}

// An export list with nothing in it checked zero exports, and the run still
// printed "checks passed". A check that could not run must not look like one
// that passed.
func TestRefusesAnEmptyExportList(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         " ",
		"comments only": "# nothing here\n\n   \n# still nothing\n",
	} {
		t.Run(name, func(t *testing.T) {
			ffi := writeAdopterFFI(t, body)
			var stdout, stderr strings.Builder
			if code := run(ffi, &stdout, &stderr); code == 0 {
				t.Fatalf("passed with no exports to check:\n%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), "no exports") {
				t.Errorf("error does not say the list was empty: %s", stderr.String())
			}
			if strings.Contains(stdout.String(), "passed") {
				t.Errorf("reported a pass for a check that did not run:\n%s", stdout.String())
			}
		})
	}
}

func TestRefusesAMissingExportList(t *testing.T) {
	ffi := writeAdopterFFI(t, "")
	var stdout, stderr strings.Builder
	if code := run(ffi, &stdout, &stderr); code == 0 {
		t.Fatalf("passed with no exports.txt:\n%s", stdout.String())
	}
}

// -h used to be taken as the directory to check.
func TestHelpExitsZeroWithoutChecking(t *testing.T) {
	for _, flagName := range []string{"-h", "--help", "-help"} {
		t.Run(flagName, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := realMain([]string{flagName}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d, want 0; stderr: %s", code, stderr.String())
			}
			out := stdout.String() + stderr.String()
			if !strings.Contains(out, "Usage") || !strings.Contains(out, "-exports") {
				t.Errorf("usage text missing:\n%s", out)
			}
			if strings.Contains(out, "failed to read") || strings.Contains(out, "passed") {
				t.Errorf("-h ran a check:\n%s", out)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":    {"-nope"},
		"two directories": {"a", "b"},
		"empty -exports":  {"-exports", "", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := realMain(args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2; stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

// An adopter keeps its export list wherever it likes, not inside the checked
// directory, and has no Gusset exports.txt to copy.
func TestExportsFlagTakesAListOutsideTheDirectory(t *testing.T) {
	ffi := writeAdopterFFI(t, "")
	list := filepath.Join(t.TempDir(), "mine.txt")
	if err := os.WriteFile(list, []byte("# adopter exports\nmyservice_engine_init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := realMain([]string{"-exports", list, ffi}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr.String())
	}

	var out2, err2 strings.Builder
	missing := filepath.Join(t.TempDir(), "absent.txt")
	if code := realMain([]string{"-exports", missing, ffi}, &out2, &err2); code == 0 {
		t.Fatalf("a missing -exports file passed:\n%s", out2.String())
	}

	// A list naming an export the shim never wraps is still caught.
	bad := filepath.Join(t.TempDir(), "bad.txt")
	if err := os.WriteFile(bad, []byte("myservice_engine_init\nmyservice_other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out3, err3 strings.Builder
	if code := realMain([]string{"-exports", bad, ffi}, &out3, &err3); code == 0 ||
		!strings.Contains(err3.String(), "myservice_other") {
		t.Fatalf("unwrapped export not reported (exit %d): %s", code, err3.String())
	}
}
