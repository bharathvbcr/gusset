package main

import (
	"context"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// COMDAT groups are ELF-only, so this file is too: on darwin the test does not
// exist rather than reporting a skip.

// personalityC adds what every Rust staticlib carries on ELF: a hidden weak
// DW.ref.rust_eh_personality in a COMDAT group of the same name.
const personalityC = engineC + `
__asm__(".section .data.DW.ref.rust_eh_personality,\"awG\",@progbits,DW.ref.rust_eh_personality,comdat\n"
        ".weak DW.ref.rust_eh_personality\n"
        ".hidden DW.ref.rust_eh_personality\n"
        "DW.ref.rust_eh_personality:\n"
        ".quad 0\n"
        ".text\n");
`

// The seal that only localized symbols left the personality COMDAT group under
// its shared name, and the second engine's first panic crashed the process
// (bench/r14 "nocomdat"). verify must refuse that object, and seal must not
// produce it.
func TestPersonalityComdatIsRenamedPerEngine(t *testing.T) {
	need(t, "ld", "objcopy")
	dir := t.TempDir()
	in := archive(t, dir, personalityC)

	localizedOnly := filepath.Join(dir, "localized.o")
	if err := run(context.Background(), "ld", "-r", "--whole-archive", in, "-o", localizedOnly); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), "objcopy", "--wildcard", "--keep-global-symbol=ea_*", localizedOnly); err != nil {
		t.Fatal(err)
	}
	err := verifyObject(localizedOnly, "ea_")
	if err == nil || !strings.Contains(err.Error(), "DW.ref.rust_eh_personality") {
		t.Fatalf("verify of a localize-only seal: err = %v, want a refusal naming the shared COMDAT group", err)
	}

	out := filepath.Join(dir, "sealed.a")
	if err := seal(in, "ea_", out); err != nil {
		t.Fatalf("seal: %v", err)
	}
	f, err := elf.Open(filepath.Join(dir, "sealed.o"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sigs, err := groupSignatures(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 1 || sigs[0] != "DW.ref.rust_eh_personality.ea_" {
		t.Fatalf("group signatures after seal = %v, want [DW.ref.rust_eh_personality.ea_]", sigs)
	}
}

// flagC stands in for an ASan-instrumented engine: the runtime's per-image flag
// as the instrumentation emits it (a hidden common), and an export returning its
// address. ENGINE is the export prefix.
const flagC = `
__attribute__((visibility("hidden"))) long ___asan_globals_registered;
long *ENGINE_flag(void) { return &___asan_globals_registered; }
`

// The seal that gave every common private storage gave each sealed engine its
// own ___asan_globals_registered, so each re-registered every instrumented global
// in the binary and ASan aborted with an odr-violation on Gusset's own
// LIVE_BYTES (bench/r14 asan-rust). Two sealed engines and an unsealed object
// (Gusset's place in the link) must all resolve to one flag.
func TestASanRegistrationFlagStaysOnePerImage(t *testing.T) {
	need(t, "ld", "objcopy", "cc")
	dir := t.TempDir()
	var libs []string
	for _, p := range []string{"ea_", "eb_"} {
		d := filepath.Join(dir, p)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		in := archive(t, d, strings.ReplaceAll(flagC, "ENGINE_", p), "-fcommon")
		out := filepath.Join(d, "libsealed.a")
		if err := seal(in, p, out); err != nil {
			t.Fatalf("seal %s: %v", p, err)
		}
		libs = append(libs, out)
	}
	g := filepath.Join(dir, "unsealed")
	if err := os.MkdirAll(g, 0o755); err != nil {
		t.Fatal(err)
	}
	libs = append(libs, archive(t, g, strings.ReplaceAll(flagC, "ENGINE_", "g_"), "-fcommon"))
	main := filepath.Join(dir, "main.c")
	if err := os.WriteFile(main, []byte(`
long *ea_flag(void); long *eb_flag(void); long *g_flag(void);
int main(void) { return ea_flag() == eb_flag() && eb_flag() == g_flag() ? 0 : 1; }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "main")
	if err := run(context.Background(), "cc", append([]string{"-o", bin, main}, libs...)...); err != nil {
		t.Fatalf("link two sealed engines: %v", err)
	}
	if err := exec.Command(bin).Run(); err != nil {
		t.Fatalf("the two sealed engines see different ___asan_globals_registered flags: %v", err)
	}
}
