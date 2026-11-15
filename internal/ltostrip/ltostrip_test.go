package ltostrip

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// ltoSection is the section a Rust staticlib's std members carry, as this
// host's objcopy names it, and the asm that makes one.
func ltoSection() (name, asm string) {
	if runtime.GOOS == "darwin" {
		return "__LLVM,__bitcode", ".section __LLVM,__bitcode\n.byte 1,2,3,4\n.text\n"
	}
	return ".llvmbc", ".section .llvmbc,\"\",%progbits\n.byte 1,2,3,4\n.text\n"
}

// object compiles one C object defining symbol, with the LTO section when lto.
func object(t *testing.T, dir, file, symbol string, lto bool) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Fatalf("no cc on PATH: cgo cannot work here either, so this test cannot run: %v", err)
	}
	src := "int " + symbol + " = 1;\n"
	if lto {
		_, asm := ltoSection()
		src += "__asm__(\"" + strings.NewReplacer("\n", "\\n", "\"", "\\\"").Replace(asm) + "\");\n"
	}
	c := filepath.Join(dir, file+".c")
	if err := os.WriteFile(c, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	o := filepath.Join(dir, file+".o")
	if out, err := exec.Command(cc, "-c", "-o", o, c).CombinedOutput(); err != nil {
		t.Fatalf("cc -c %s: %v\n%s", c, err, out)
	}
	return o
}

// archive builds an ar archive of the objects, with a symbol table.
func archive(t *testing.T, dir, name string, objs ...string) string {
	t.Helper()
	ar, err := exec.LookPath("ar")
	if err != nil {
		t.Fatalf("no ar on PATH: %v", err)
	}
	a := filepath.Join(dir, name)
	args := append([]string{"rcs", a}, objs...)
	if out, err := exec.Command(ar, args...).CombinedOutput(); err != nil {
		t.Fatalf("ar %v: %v\n%s", args, err, out)
	}
	return a
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestObjectSections(t *testing.T) {
	dir := t.TempDir()
	want, _ := ltoSection()

	got, err := ObjectSections(read(t, object(t, dir, "lto", "with_lto", true)))
	if err != nil || !slices.Equal(got, []string{want}) {
		t.Errorf("object with LTO IR: sections = %v, err = %v; want [%s]", got, err, want)
	}
	got, err = ObjectSections(read(t, object(t, dir, "clean", "without_lto", false)))
	if err != nil || len(got) != 0 {
		t.Errorf("clean object: sections = %v, err = %v; want none", got, err)
	}

	// A member the check cannot read is an error, never a clean result.
	for name, data := range map[string][]byte{
		"text":          []byte("not an object file at all"),
		"empty":         nil,
		"raw bitcode":   []byte("BC\xc0\xde and then some bitcode"),
		"truncated":     []byte("\x7fELF\x02\x01"),
		"fat mach-o":    {0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 0},
		"gnu ld script": []byte("INPUT(-lfoo)\n"),
	} {
		if _, err := ObjectSections(data); err == nil {
			t.Errorf("%s: got no error; an unreadable member must not look clean", name)
		}
	}
}

func TestReadArchive_RejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.a")
	for name, data := range map[string]string{
		"no magic":       "hello",
		"short header":   "!<arch>\nfoo",
		"bad size field": "!<arch>\n" + strings.Repeat("x", 48) + "zzzzzzzzzz" + "`\n",
	} {
		if err := os.WriteFile(bad, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadArchive(bad); err == nil {
			t.Errorf("%s: ReadArchive accepted it", name)
		}
	}
}

func TestStrip_RemovesTheSectionAndKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	in := archive(t, dir, "in.a",
		object(t, dir, "a", "a_sym", true),
		object(t, dir, "b", "b_sym", false),
		object(t, dir, "c", "c_sym", true))
	if f, err := Scan(in); err != nil || len(f) != 2 {
		t.Fatalf("setup: Scan(in) = %v, %v; want two findings", f, err)
	}
	before := read(t, in)

	out := filepath.Join(dir, "out.a")
	res, err := Strip(context.Background(), in, out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if res.Members != 3 || res.Stripped != 2 || res.Tool == "" {
		t.Errorf("Result = %+v; want 3 members, 2 stripped, a tool named", res)
	}
	if f, err := Scan(out); err != nil || len(f) != 0 {
		t.Errorf("Scan(out) = %v, %v; want clean", f, err)
	}
	if !bytes.Equal(read(t, in), before) {
		t.Error("Strip changed its input when writing a separate output")
	}
	// Same members, same globals.
	was, _ := ReadArchive(in)
	now, err := ReadArchive(out)
	if err != nil || len(now) != len(was) {
		t.Fatalf("members after = %d (%v), before = %d", len(now), err, len(was))
	}
	for i := range now {
		a, _ := definedGlobals(was[i].Data)
		b, _ := definedGlobals(now[i].Data)
		if now[i].Name != was[i].Name || !slices.Equal(a, b) || len(b) == 0 {
			t.Errorf("member %d: %s %v became %s %v", i, was[i].Name, a, now[i].Name, b)
		}
	}
}

func TestStrip_InPlaceAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	a := archive(t, dir, "x.a", object(t, dir, "a", "a_sym", true))
	if _, err := Strip(context.Background(), a, a); err != nil {
		t.Fatalf("in place: %v", err)
	}
	if f, err := Scan(a); err != nil || len(f) != 0 {
		t.Fatalf("after in-place strip: %v, %v", f, err)
	}
	// A second run finds nothing to remove, so it needs no tool at all.
	t.Setenv("OBJCOPY", filepath.Join(dir, "no-such-objcopy"))
	first := read(t, a)
	res, err := Strip(context.Background(), a, a)
	if err != nil || res.Stripped != 0 || res.Tool != "" {
		t.Fatalf("second strip = %+v, %v; want a no-op with no tool", res, err)
	}
	if !bytes.Equal(read(t, a), first) {
		t.Error("a no-op strip changed the archive")
	}
}

// A tool that exits 0 and removes nothing must not read as success.
func TestStrip_VerifiesWhatTheToolDid(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no true(1): %v", err)
	}
	dir := t.TempDir()
	in := archive(t, dir, "in.a", object(t, dir, "a", "a_sym", true))
	out := filepath.Join(dir, "out.a")
	if err := os.WriteFile(out, []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OBJCOPY", truePath)
	if _, err := Strip(context.Background(), in, out); err == nil || !strings.Contains(err.Error(), "still has") {
		t.Fatalf("Strip with a tool that removes nothing: err = %v; want a 'still has' refusal", err)
	}
	if got := read(t, out); string(got) != "previous" {
		t.Errorf("a refused strip replaced the output: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

// rawArchive writes an ar archive of the given members byte by byte, so a
// member the host's ar would refuse or drop (Apple's ranlib silently omits a
// non-object) is still in it.
func rawArchive(t *testing.T, path string, members ...Member) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.Name+"/", 0, 0, 0, "644", len(m.Data))
		b.Write(m.Data)
		if len(m.Data)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStrip_RefusesWhatItCannotInspect(t *testing.T) {
	dir := t.TempDir()
	good := read(t, object(t, dir, "a", "a_sym", true))
	in := filepath.Join(dir, "in.a")
	rawArchive(t, in,
		Member{Name: "a.o", Data: good},
		Member{Name: "notes.o", Data: []byte("plain text, not an object")})
	if _, err := ReadArchive(in); err != nil {
		t.Fatalf("setup: the hand-built archive does not parse: %v", err)
	}
	out := filepath.Join(dir, "out.a")
	if _, err := Strip(context.Background(), in, out); err == nil {
		t.Fatal("Strip accepted an archive with a member it cannot read")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a refused strip wrote its output")
	}
}

func TestFindTool_ProbesBeforeTrusting(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "objcopy")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\necho 'dyld: Library not loaded: libLLVM.dylib' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OBJCOPY", broken)
	_, err := FindTool(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not run") || !strings.Contains(err.Error(), "libLLVM") {
		t.Fatalf("FindTool with a tool that cannot load: err = %v; want it named with the loader's own message", err)
	}
}

func TestStripTree(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "build", "pkg", "out")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	dirty := archive(t, nested, "libx.a", object(t, nested, "a", "a_sym", true))
	clean := archive(t, root, "liby.a", object(t, root, "b", "b_sym", false))
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("not a library"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := StripTree(context.Background(), root)
	if err != nil || n != 1 {
		t.Fatalf("StripTree = %d, %v; want one archive rewritten", n, err)
	}
	for _, p := range []string{dirty, clean} {
		if f, err := Scan(p); err != nil || len(f) != 0 {
			t.Errorf("Scan(%s) = %v, %v", p, f, err)
		}
	}
}

// GNU ar stores a name over 15 bytes as "/offset" into the "//" table and ends a
// short one with "/". Objcopy rebuilds that table, so only the resolved name can
// be compared between an archive and its stripped copy; comparing the raw field
// made every Linux strip look like a reordering.
func TestReadArchive_ResolvesGNULongNames(t *testing.T) {
	long1, long2 := "a_very_long_member_name.o", "another_quite_long_name.o"
	build := func(refs ...string) []byte {
		var b bytes.Buffer
		b.WriteString("!<arch>\n")
		member := func(field string, body string) {
			fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", field, 0, 0, 0, "644", len(body))
			b.WriteString(body)
			if len(body)%2 == 1 {
				b.WriteByte('\n')
			}
		}
		table := long1 + "/\n" + long2 + "/\n"
		member("//", table)
		for i, r := range refs {
			member(r, fmt.Sprintf("body%d", i))
		}
		return b.Bytes()
	}
	path := filepath.Join(t.TempDir(), "gnu.a")
	write := func(data []byte) {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(build("/0", fmt.Sprintf("/%d", len(long1)+2), "short.o/"))
	members, err := ReadArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range members {
		got = append(got, m.Name)
	}
	if want := []string{long1, long2, "short.o"}; !slices.Equal(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}

	// A reference outside the table, or into a table that is not there, is an
	// error: a name it cannot resolve is a name it cannot compare.
	write(build("/9999"))
	if _, err := ReadArchive(path); err == nil {
		t.Error("a long-name reference past the end of the table was accepted")
	}
	write(build("/abc"))
	if _, err := ReadArchive(path); err == nil {
		t.Error("a non-numeric long-name reference was accepted")
	}
}
