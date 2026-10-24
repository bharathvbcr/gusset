package main

import (
	"os"
	"path/filepath"
	"runtime"
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
	for _, want := range []string{"alias.go:7", "call.go:4", "preamble.go:4"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing violation for %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "preamble.go:2") {
		t.Errorf("free() reported at the comment's first line, not the call:\n%s", got)
	}
	if strings.Contains(got, "clean.go") {
		t.Errorf("comment or string literal flagged:\n%s", got)
	}
	if n := strings.Count(got, "preamble.go"); n != 1 {
		t.Errorf("gusset_buf_free( must not match free(; got %d preamble hits:\n%s", n, got)
	}
}

// free() is not on line 4, and the second line of the block is longer than the
// first, so a position step that uses the wrong line's length lands elsewhere.
// A // preamble is a separate comment per line; its free() is that line, not
// the import "C" line and not the first // line.
func TestR4PreambleFreeReportsItsOwnLine(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "wide.go", `package p
/*
x
this line is longer than the one before it XXXXXXXXXXXXXXXXXXXX
free()
*/
import "C"
`)
	write(t, dir, "slash.go", `package p
// preamble
// not free
// free()
import "C"
`)
	v, err := checkAllocatorSymmetry(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(v, "\n")
	if !strings.Contains(got, "wide.go:5:") {
		t.Fatalf("block-comment free() must be wide.go:5, got:\n%s", got)
	}
	if strings.Contains(got, "wide.go:2:") || strings.Contains(got, "wide.go:4:") {
		t.Fatalf("block-comment free() reported on the wrong line:\n%s", got)
	}
	if !strings.Contains(got, "slash.go:4:") {
		t.Fatalf("// preamble free() must be slash.go:4, got:\n%s", got)
	}
	if strings.Contains(got, "slash.go:2:") || strings.Contains(got, "slash.go:5:") {
		t.Fatalf("// preamble free() reported at the first // line or at import \"C\":\n%s", got)
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
	if v, err := checkDirectives(buildContext(nil), good, exports); err != nil || len(v) != 0 {
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
func f() { C.gusset_cancel() }
`)
	v, err := checkDirectives(buildContext(nil), stray, exports)
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
	if v, err := checkDirectives(buildContext(nil), moved, exports); err == nil {
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

// cmd/cgo merges #cgo directives across the package and only cares about an
// export a file references. A second file whose only #cgo line is LDFLAGS must
// not be reported when the directives live with the reference, and a reference
// in that second file is covered by directives on the first.
func TestR5DirectivesMergeAcrossThePackage(t *testing.T) {
	exports := []string{"gusset_a", "gusset_unreferenced"}

	dir := t.TempDir()
	write(t, dir, "ffi.go", `package ffi
/*
#cgo noescape gusset_a
#cgo nocallback gusset_a
*/
import "C"
func A() { C.gusset_a() }
`)
	write(t, dir, "ldflags.go", `package ffi
/*
#cgo LDFLAGS: -lgusset
*/
import "C"
func Also() { C.gusset_a() }
`)
	if v, err := checkDirectives(buildContext(nil), dir, exports); err != nil || len(v) != 0 {
		t.Fatalf("LDFLAGS file or unreferenced export flagged: %v %v", v, err)
	}

	missing := t.TempDir()
	write(t, missing, "ffi.go", `package ffi
/*
#cgo LDFLAGS: -lgusset
*/
import "C"
func A() { C.gusset_a() }
`)
	v, err := checkDirectives(buildContext(nil), missing, exports)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(v, "\n")
	if len(v) != 2 || !strings.Contains(got, "'#cgo noescape gusset_a'") ||
		!strings.Contains(got, "'#cgo nocallback gusset_a'") ||
		strings.Contains(got, "gusset_unreferenced") {
		t.Fatalf("want only gusset_a's two directives, got:\n%s", got)
	}
}

// otherGOOS is a GOOS this process will not build. windows is not an alias of
// darwin, linux, or ios; linux is not an alias of windows.
func otherGOOS() string {
	if runtime.GOOS == "windows" {
		return "linux"
	}
	return "windows"
}

// cmd/cgo merges directives only across files the default build includes.
// A //go:build ignore file, a file named for another GOOS, and a file whose
// build tag is another GOOS are not in that set: their directives must not
// clear a reference in a file that is.
func TestR5BuildTagsDoNotSupplyDirectives(t *testing.T) {
	exports := []string{"gusset_a", "gusset_unreferenced"}
	ref := `package ffi
/*
#cgo LDFLAGS: -lgusset
*/
import "C"
func Also() { C.gusset_a() }
`
	directives := `package ffi
/*
#cgo noescape gusset_a
#cgo nocallback gusset_a
*/
import "C"
`
	cases := []struct {
		name string
		file string
		src  string
	}{
		{"go:build ignore", "ignored.go", "//go:build ignore\n\n" + directives},
		{"other GOOS filename", "ffi_" + otherGOOS() + ".go", directives},
		{"other GOOS build tag", "tagged.go", "//go:build " + otherGOOS() + "\n\n" + directives},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.file, tc.src)
			write(t, dir, "ldflags.go", ref)
			v, err := checkDirectives(buildContext(nil), dir, exports)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(v, "\n")
			if len(v) != 2 || !strings.Contains(got, "ldflags.go") ||
				!strings.Contains(got, "'#cgo noescape gusset_a'") ||
				!strings.Contains(got, "'#cgo nocallback gusset_a'") ||
				strings.Contains(got, "gusset_unreferenced") ||
				strings.Contains(got, tc.file) {
				t.Fatalf("want exactly ldflags.go's two missing directives, got:\n%s", got)
			}
		})
	}

	// A file the default context does compile must still be able to supply
	// the directives. Excluding every GOOS-suffixed name would fail this.
	dir := t.TempDir()
	write(t, dir, "ffi_"+runtime.GOOS+".go", directives)
	write(t, dir, "ldflags.go", ref)
	if v, err := checkDirectives(buildContext(nil), dir, exports); err != nil || len(v) != 0 {
		t.Fatalf("directives in ffi_%s.go must cover the reference: %v %v", runtime.GOOS, v, err)
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
	v, err := checkWrappers(buildContext(nil), dir, []string{"gusset_a", "gusset_b", "# comment", ""})
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
		// go build's legacy space-separated -tags; read as one tag it would
		// match no file and check the default build instead.
		"-tags with a space": {"-tags", "gusset_pkgconfig other", "x"},
		"-tags with a quote": {"-tags", "'gusset_pkgconfig'", "x"},
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

// writeModule lays out an adopter module with its shim in shim/ and its export
// list beside it, and returns the shim directory and the list.
func writeModule(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	mod := t.TempDir()
	write(t, mod, "go.mod", "module example.com/adopter\n\ngo 1.26\n")
	ffi := filepath.Join(mod, "shim")
	if err := os.MkdirAll(ffi, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		write(t, ffi, name, src)
	}
	write(t, mod, "engine-exports.txt", "myservice_engine_init\n")
	return ffi, filepath.Join(mod, "engine-exports.txt")
}

// Option A gates the adopter's pkg-config shim behind //go:build
// gusset_pkgconfig. gussetvet had no -tags, so it never read that file, and a
// shim missing a directive passed for the build that links with it. -tags
// selects files the way `go build -tags` does, for R5 and R1 alike.
func TestTagsSelectTheFilesR5AndR1Check(t *testing.T) {
	ffi, list := writeModule(t, map[string]string{
		"shim.go": `//go:build !gusset_pkgconfig

package shim

/*
#cgo LDFLAGS: -lgusset
#cgo noescape myservice_engine_init
#cgo nocallback myservice_engine_init
void myservice_engine_init(void);
*/
import "C"

func Init() { C.myservice_engine_init() }
`,
		"shim_pkgconfig.go": `//go:build gusset_pkgconfig

package shim

/*
#cgo pkg-config: gusset
#cgo nocallback myservice_engine_init
void myservice_engine_init(void);
*/
import "C"

func Init() { C.myservice_engine_init() }
`,
	})

	var stdout, stderr strings.Builder
	if code := realMain([]string{"-exports", list, ffi}, &stdout, &stderr); code != 0 {
		t.Fatalf("default build: exit %d, want 0; stderr: %s", code, stderr.String())
	}

	for _, tags := range []string{"gusset_pkgconfig", "other,gusset_pkgconfig"} {
		t.Run(tags, func(t *testing.T) {
			var stdout, stderr strings.Builder
			code := realMain([]string{"-tags", tags, "-exports", list, ffi}, &stdout, &stderr)
			got := stderr.String()
			if code != 1 {
				t.Fatalf("-tags %s: exit %d, want 1 (a violation); stderr: %s", tags, code, got)
			}
			if !strings.Contains(got, "shim_pkgconfig.go: missing '#cgo noescape myservice_engine_init'") {
				t.Errorf("the gated shim's missing noescape is not reported:\n%s", got)
			}
			if n := strings.Count(got, "\n  - "); n != 1 {
				t.Errorf("want exactly the one missing directive, got %d violations:\n%s", n, got)
			}
			if strings.Contains(stdout.String(), "passed") {
				t.Errorf("reported a pass alongside a violation:\n%s", stdout.String())
			}
		})
	}

	// R1 judges the same set: a wrapper only in the gated file counts under
	// the tag and not without it.
	ffi, list = writeModule(t, map[string]string{
		"shim.go": `package shim

/*
#cgo noescape myservice_engine_init
#cgo nocallback myservice_engine_init
void myservice_engine_init(void);
*/
import "C"
`,
		"wrap_pkgconfig.go": `//go:build gusset_pkgconfig

package shim

// void myservice_engine_init(void);
import "C"

func Init() { C.myservice_engine_init() }
`,
	})
	var out1, err1 strings.Builder
	if code := realMain([]string{"-exports", list, ffi}, &out1, &err1); code != 1 ||
		!strings.Contains(err1.String(), "R1: export myservice_engine_init has no Go wrapper") {
		t.Fatalf("default build: want the R1 violation (exit 1), got exit %d: %s", code, err1.String())
	}
	var out2, err2 strings.Builder
	if code := realMain([]string{"-tags", "gusset_pkgconfig", "-exports", list, ffi}, &out2, &err2); code != 0 {
		t.Fatalf("-tags gusset_pkgconfig: exit %d, want 0; stderr: %s", code, err2.String())
	}
}

// R1 asks whether the package wraps each export. A reference in a file the
// build never compiles is not a wrapper: a //go:build ignore file, a file for
// another GOOS, and a _test.go file (cgo refuses import "C" in tests). R5
// already skipped these; R1 walked them, so the two rules judged different
// files and an export wrapped only by a file nobody builds passed R1.
func TestR1CountsOnlyFilesTheBuildCompiles(t *testing.T) {
	shim := `package shim

/*
#cgo noescape myservice_engine_init
#cgo nocallback myservice_engine_init
void myservice_engine_init(void);
*/
import "C"

func Unrelated() {}
`
	ref := `package shim

import "C"

func Init() { C.myservice_engine_init() }
`
	cases := []struct {
		name string
		file string
		src  string
	}{
		{"go:build ignore", "ignored.go", "//go:build ignore\n\n" + ref},
		{"other GOOS filename", "wrap_" + otherGOOS() + ".go", ref},
		{"other GOOS build tag", "tagged.go", "//go:build " + otherGOOS() + "\n\n" + ref},
		{"test file", "wrap_test.go", ref},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ffi, list := writeModule(t, map[string]string{"shim.go": shim, tc.file: tc.src})
			var stdout, stderr strings.Builder
			code := realMain([]string{"-exports", list, ffi}, &stdout, &stderr)
			got := stderr.String()
			if code != 1 {
				t.Fatalf("exit %d, want 1: a reference only in %s satisfied R1\nstdout: %s\nstderr: %s",
					code, tc.file, stdout.String(), got)
			}
			if !strings.Contains(got, "R1: export myservice_engine_init has no Go wrapper") {
				t.Errorf("want the R1 violation, got:\n%s", got)
			}
		})
	}

	// The same reference in a file the build compiles is a wrapper. A matcher
	// that dropped every file would fail this instead of the cases above.
	ffi, list := writeModule(t, map[string]string{"shim.go": shim, "wrap_" + runtime.GOOS + ".go": ref})
	var stdout, stderr strings.Builder
	if code := realMain([]string{"-exports", list, ffi}, &stdout, &stderr); code != 0 {
		t.Fatalf("a wrapper in wrap_%s.go: exit %d, want 0; stderr: %s", runtime.GOOS, code, stderr.String())
	}
}
