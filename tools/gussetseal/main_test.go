package main

import (
	"context"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// engineC exports one prefixed function and leaks one helper, the shape of a
// Rust staticlib whose std symbols are still global.
const engineC = `
int helper(int x) { return x * 3; }
int ea_call(int x) { return helper(x) + 1; }
`

func need(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH; this check did not run", tool)
		}
	}
}

// archive compiles src into dir/libengine.a and returns its path; cflags go to
// the compiler.
func archive(t *testing.T, dir, src string, cflags ...string) string {
	t.Helper()
	need(t, "cc", "ar")
	c := filepath.Join(dir, "engine.c")
	o := filepath.Join(dir, "engine_member.o")
	a := filepath.Join(dir, "libengine.a")
	if err := os.WriteFile(c, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{"-c", "-fPIC"}, cflags...), "-o", o, c)
	if err := run(context.Background(), "cc", args...); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), "ar", "rcs", a, o); err != nil {
		t.Fatal(err)
	}
	return a
}

func sealable(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		need(t, "cc")
	case "linux":
		need(t, "ld", "objcopy")
	default:
		t.Skipf("no sealing recipe for %s; this check did not run", runtime.GOOS)
	}
}

func TestSealLeavesOnlyThePrefixGlobal(t *testing.T) {
	sealable(t)
	dir := t.TempDir()
	in := archive(t, dir, engineC)
	out := filepath.Join(dir, "sealed", "libengine.a")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := seal(in, "ea_", out); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("no archive written: %v", err)
	}
	if err := verifyObject(strings.TrimSuffix(out, ".a")+".o", "ea_"); err != nil {
		t.Fatalf("sealed object does not verify: %v", err)
	}
}

func TestVerifyRejectsAnUnsealedObject(t *testing.T) {
	need(t, "cc")
	dir := t.TempDir()
	archive(t, dir, engineC)
	err := verifyObject(filepath.Join(dir, "engine_member.o"), "ea_")
	if err == nil || !strings.Contains(err.Error(), "helper") {
		t.Fatalf("verify of an unsealed object: err = %v, want a refusal naming helper", err)
	}
}

func TestVerifyRejectsAnEngineWithNoExports(t *testing.T) {
	need(t, "cc")
	dir := t.TempDir()
	archive(t, dir, `static int helper(int x) { return x; } int (*keep)(int) = 0;`)
	err := verifyObject(filepath.Join(dir, "engine_member.o"), "ea_")
	if err == nil || !strings.Contains(err.Error(), "export nothing") {
		t.Fatalf("err = %v, want a refusal for exporting nothing", err)
	}
}

func TestPrefixMayNotClaimGussetOrRustNames(t *testing.T) {
	for _, p := range []string{"", "gusset_", "gus", "g", "rust_", "_R", "__rust_x", "_", "ea-", "1ea"} {
		if err := checkPrefix(p); err == nil {
			t.Errorf("checkPrefix(%q) accepted a prefix that must be refused", p)
		}
	}
	for _, p := range []string{"ea_", "mylib_", "Vendor"} {
		if err := checkPrefix(p); err != nil {
			t.Errorf("checkPrefix(%q) = %v, want accepted", p, err)
		}
	}
}

func TestSealRefusesAnOutputThatIsNotAnArchive(t *testing.T) {
	if err := seal("in.a", "ea_", "out.o"); err == nil || !strings.Contains(err.Error(), "must end in .a") {
		t.Fatalf("err = %v, want a refusal of a non-.a output", err)
	}
}

// commonC holds a tentative definition, which -fcommon emits as a common symbol.
// ASan instrumentation emits one too (___asan_globals_registered).
const commonC = engineC + `
int helper_common;
int ea_common(void) { return helper_common; }
`

// A common symbol has no section, so objcopy silently leaves it global, and the
// final link merges same-named commons across sealed engines. On ELF the seal
// renames it to a name only this engine has; on Mach-O, where ld64 leaves it a
// private-external common that still binds across objects, it must refuse.
// Before the fix, ELF wrote an archive with helper_common still shared and
// Mach-O passed it as sealed.
func TestCommonSymbolsDoNotCrossTheSeal(t *testing.T) {
	sealable(t)
	dir := t.TempDir()
	in := archive(t, dir, commonC, "-fcommon")
	out := filepath.Join(dir, "sealed.a")
	err := seal(in, "ea_", out)
	switch runtime.GOOS {
	case "linux":
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		f, ferr := elf.Open(filepath.Join(dir, "sealed.o"))
		if ferr != nil {
			t.Fatal(ferr)
		}
		defer f.Close()
		syms, serr := f.Symbols()
		if serr != nil {
			t.Fatal(serr)
		}
		renamed := false
		for _, sym := range syms {
			if sym.Name == "helper_common" && elf.ST_BIND(sym.Info) != elf.STB_LOCAL {
				t.Fatalf("helper_common is still %v under its shared name after sealing", elf.ST_BIND(sym.Info))
			}
			renamed = renamed || sym.Name == "helper_common.ea_"
		}
		if !renamed {
			t.Fatal("helper_common was not renamed to helper_common.ea_")
		}
	case "darwin":
		if err == nil || !strings.Contains(err.Error(), "helper_common") {
			t.Fatalf("seal: err = %v, want a refusal naming the common helper_common", err)
		}
	}
}

// The seal verified the object and then trusted ar to pack it. An archive whose
// member is not that object, for any reason, must be refused.
func TestArchiveMustHoldTheVerifiedObject(t *testing.T) {
	need(t, "cc", "ar")
	dir := t.TempDir()
	in := archive(t, mkdirAll(t, dir, "in"), engineC)
	member := filepath.Join(dir, "in", "engine_member.o")
	if err := archiveHoldsExactly(in, member); err != nil {
		t.Fatalf("an archive of exactly that object was refused: %v", err)
	}
	archive(t, mkdirAll(t, dir, "other"), commonC)
	if err := archiveHoldsExactly(in, filepath.Join(dir, "other", "engine_member.o")); err == nil {
		t.Fatal("an archive holding a different object was accepted")
	}
}

func mkdirAll(t *testing.T, parent, name string) string {
	t.Helper()
	d := filepath.Join(parent, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}
