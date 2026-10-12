package main

import (
	"bytes"
	"context"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// Apple's ar pads a member's data with '\n' to a multiple of 8 inside the size
// it declares, so an object whose length is not already a multiple of 8 comes
// back from the archive longer than it went in. Compiler output is aligned;
// ld -r output need not be, and the seal refused and deleted every such
// archive. GNU ar pads outside the size, so on Linux this passes either way.
func TestArchiveOfAnUnalignedObjectIsAccepted(t *testing.T) {
	need(t, "cc", "ar")
	dir := t.TempDir()
	archive(t, dir, engineC)
	obj, err := os.ReadFile(filepath.Join(dir, "engine_member.o"))
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []int{1, 3, 7} {
		t.Run(strconv.Itoa(extra), func(t *testing.T) {
			sub := t.TempDir()
			o := filepath.Join(sub, "odd.o")
			a := filepath.Join(sub, "odd.a")
			odd := append(append([]byte(nil), obj...), make([]byte, roundup8(len(obj))+extra-len(obj))...)
			if err := os.WriteFile(o, odd, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := run(context.Background(), "ar", "rcs", a, o); err != nil {
				t.Fatal(err)
			}
			if err := archiveHoldsExactly(a, o); err != nil {
				t.Fatalf("an archive of exactly that %d-byte object was refused: %v", len(odd), err)
			}
		})
	}
}

// arFile writes an archive of one member with the given header name and data,
// with no symbol table.
func arFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	header := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "644", len(data))
	body := append([]byte("!<arch>\n"+header), data...)
	if len(data)%2 == 1 {
		body = append(body, '\n')
	}
	a := filepath.Join(t.TempDir(), "hand.a")
	if err := os.WriteFile(a, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return a
}

// Padding is accepted only in the form Apple's ar writes it: a BSD member, the
// object's bytes, then '\n' up to the next multiple of 8 and no further.
// Anything else in the tail, or the same tail on a GNU-format member, is a
// member that is not the verified object.
func TestArchiveRefusesWhatIsNotPadding(t *testing.T) {
	obj := bytes.Repeat([]byte{0xcf}, 529)
	o := filepath.Join(t.TempDir(), "odd.o")
	if err := os.WriteFile(o, obj, 0o644); err != nil {
		t.Fatal(err)
	}
	bsd := func(data []byte) []byte { return append([]byte("odd.o\x00\x00\x00"), data...) }
	pad := func(tail string) []byte { return append(append([]byte(nil), obj...), tail...) }
	if err := archiveHoldsExactly(arFile(t, "#1/8", bsd(pad("\n\n\n\n\n\n\n"))), o); err != nil {
		t.Fatalf("Apple's padding was refused: %v", err)
	}
	for what, a := range map[string]string{
		"a non-newline byte in the padding": arFile(t, "#1/8", bsd(pad("\n\n\n\n\n\nx"))),
		"NUL padding":                       arFile(t, "#1/8", bsd(pad("\x00\x00\x00\x00\x00\x00\x00"))),
		"padding short of the multiple":     arFile(t, "#1/8", bsd(pad("\n"))),
		"padding past the multiple":         arFile(t, "#1/8", bsd(pad("\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n"))),
		"padding inside a GNU member":       arFile(t, "odd.o/", pad("\n\n\n\n\n\n\n")),
	} {
		if err := archiveHoldsExactly(a, o); err == nil {
			t.Errorf("a member with %s was accepted", what)
		}
	}
}

// A BSD name length is read from the archive; a negative one sliced the member
// with a negative bound and panicked.
func TestNegativeBSDNameLengthIsRefused(t *testing.T) {
	a := arFile(t, "#1/-5", []byte("abcdefgh"))
	_, err := archiveMembers(a)
	if err == nil || !strings.Contains(err.Error(), "bad BSD name length") {
		t.Fatalf("err = %v, want a refusal of the BSD name length", err)
	}
}

// The padding rule at every length that matters: an aligned object takes no
// padding, and one at 1, 3 or 7 past a multiple of 8 takes exactly the '\n's
// that reach the next multiple. One byte more or less is a member that is not
// the verified object. Lengths straddle 512 because that is where a stray
// 8-byte round-up of an already aligned object would show.
func TestArchivePaddingAtEveryAlignment(t *testing.T) {
	bsd := func(data []byte) []byte { return append([]byte("odd.o\x00\x00\x00"), data...) }
	for n, want := range map[int]int{512: 0, 513: 7, 515: 5, 519: 1, 520: 0} { // want is typed, not computed by roundup8
		obj := bytes.Repeat([]byte{0xcf}, n)
		o := filepath.Join(t.TempDir(), "odd.o")
		if err := os.WriteFile(o, obj, 0o644); err != nil {
			t.Fatal(err)
		}
		withTail := func(k int) []byte { return append(append([]byte(nil), obj...), bytes.Repeat([]byte{'\n'}, k)...) }
		if err := archiveHoldsExactly(arFile(t, "#1/8", bsd(withTail(want))), o); err != nil {
			t.Errorf("%d bytes with the %d-byte padding Apple writes was refused: %v", n, want, err)
		}
		// No padding at all is exactly the object, and stays acceptable.
		if err := archiveHoldsExactly(arFile(t, "#1/8", bsd(withTail(0))), o); err != nil {
			t.Errorf("%d bytes with no padding was refused: %v", n, err)
		}
		for _, k := range []int{want + 1, want + 8} {
			if err := archiveHoldsExactly(arFile(t, "#1/8", bsd(withTail(k))), o); err == nil {
				t.Errorf("%d bytes with %d bytes of padding (longer than roundup8) was accepted", n, k)
			}
		}
		if want > 1 {
			if err := archiveHoldsExactly(arFile(t, "#1/8", bsd(withTail(want-1))), o); err == nil {
				t.Errorf("%d bytes with %d bytes of padding (short of roundup8) was accepted", n, want-1)
			}
		}
	}
}

// A member whose prefix is not the verified object is refused even at the exact
// padded length.
func TestArchiveRefusesAPaddedMemberWithADifferentPrefix(t *testing.T) {
	obj := bytes.Repeat([]byte{0xcf}, 515)
	o := filepath.Join(t.TempDir(), "odd.o")
	if err := os.WriteFile(o, obj, 0o644); err != nil {
		t.Fatal(err)
	}
	other := append([]byte(nil), obj...)
	other[100] ^= 0xff
	data := append(append([]byte("odd.o\x00\x00\x00"), other...), "\n\n\n\n\n"...)
	if err := archiveHoldsExactly(arFile(t, "#1/8", data), o); err == nil {
		t.Fatal("a padded member with one differing byte was accepted")
	}
}

// Archives cut short or lying about their sizes are errors, never a panic and
// never an accepted member.
func TestArchiveMembersRefusesTruncatedAndLyingArchives(t *testing.T) {
	obj := bytes.Repeat([]byte{1}, 64)
	o := filepath.Join(t.TempDir(), "odd.o")
	if err := os.WriteFile(o, obj, 0o644); err != nil {
		t.Fatal(err)
	}
	whole := arFile(t, "#1/8", append([]byte("odd.o\x00\x00\x00"), obj...))
	if err := archiveHoldsExactly(whole, o); err != nil {
		t.Fatalf("the intact archive was refused: %v", err)
	}
	raw, err := os.ReadFile(whole)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"cut inside the data":          raw[:len(raw)-10],
		"cut inside the header":        raw[:8+30],
		"a stray byte after the magic": append([]byte("!<arch>\n"), 'x'),
	}
	for what, b := range cases {
		a := filepath.Join(t.TempDir(), "cut.a")
		if err := os.WriteFile(a, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := archiveMembers(a); err == nil {
			t.Errorf("%s: archiveMembers accepted it", what)
		}
		if err := archiveHoldsExactly(a, o); err == nil {
			t.Errorf("%s: archiveHoldsExactly accepted it", what)
		}
	}
	if _, err := archiveMembers(arFile(t, "#1/4096", []byte("short"))); err == nil || !strings.Contains(err.Error(), "bad BSD name length") {
		t.Errorf("a BSD name longer than its member: err = %v", err)
	}
	if _, err := archiveMembers(arFile(t, "#1/zz", []byte("abcdefgh"))); err == nil {
		t.Error("a non-numeric BSD name length was accepted")
	}
}
