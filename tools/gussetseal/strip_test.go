package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bharathvbcr/gusset/internal/ltostrip"
)

// ltoSource is C that defines sym and carries the LTO section a Rust staticlib's
// std members carry, as this host's objcopy names it.
func ltoSource(sym string) string {
	asm := `.section .llvmbc,\"\",%progbits\n.byte 1,2,3,4\n.text\n`
	if runtime.GOOS == "darwin" {
		asm = `.section __LLVM,__bitcode\n.byte 1,2,3,4\n.text\n`
	}
	return "int " + sym + " = 1;\n__asm__(\"" + asm + "\");\n"
}

func TestStripLTOMain_FileMode(t *testing.T) {
	need(t, "cc", "ar")
	dir := t.TempDir()
	in := archive(t, dir, ltoSource("sym_a"))
	if found, err := ltostrip.Scan(in); err != nil || len(found) == 0 {
		t.Fatalf("setup: the input should carry an LTO section: %v, %v", found, err)
	}
	out := filepath.Join(dir, "out.a")
	if code := stripLTOMain(out, "", []string{in}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if found, err := ltostrip.Scan(out); err != nil || len(found) != 0 {
		t.Errorf("the output still carries LTO sections: %v, %v", found, err)
	}
	if found, _ := ltostrip.Scan(in); len(found) == 0 {
		t.Error("the input was modified; -o must leave it alone")
	}
}

func TestStripLTOMain_TreeModeRewritesInPlace(t *testing.T) {
	need(t, "cc", "ar")
	root := t.TempDir()
	nested := filepath.Join(root, "build", "pkg", "out")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	a := archive(t, nested, ltoSource("sym_a"))
	if code := stripLTOMain("", root, nil); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if found, err := ltostrip.Scan(a); err != nil || len(found) != 0 {
		t.Errorf("the archive under the tree still carries LTO sections: %v, %v", found, err)
	}
}

// Anything that is not exactly one of the two modes is a usage error, never a
// silent no-op.
func TestStripLTOMain_UsageErrors(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		out, tree string
		args      []string
	}{
		"nothing":             {},
		"output but no input": {out: filepath.Join(dir, "o.a")},
		"tree and output":     {out: filepath.Join(dir, "o.a"), tree: dir},
		"tree and an input":   {tree: dir, args: []string{"in.a"}},
		"two inputs":          {out: filepath.Join(dir, "o.a"), args: []string{"a.a", "b.a"}},
	} {
		if code := stripLTOMain(c.out, c.tree, c.args); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}
