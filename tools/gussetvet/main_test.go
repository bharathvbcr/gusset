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
