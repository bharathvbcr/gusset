package gusset_test

import (
	"bytes"
	"flag"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var updatePublicAPI = flag.Bool("update-public-api", false, "rewrite testdata/public_api.txt from the current exported surface")

const publicAPIFile = "testdata/public_api.txt"

// TestPublicSurfaceMatchesCommittedList is the Go half of what
// crates/gusset/tests/exports_match.rs does for the C ABI: the exported surface
// of package gusset is a committed file, and adding, removing or re-typing an
// export without updating it fails here. Several exports (Buffer.ID,
// TraceCarrier, OpcodeContextKey, MaxDrain, MaxBufferBytes, ErrGeneric,
// ExpectedAbiVersion) were in no document at all, so nothing noticed when the
// surface moved.
//
// Constant and variable values are not part of the list, only their names and
// declared types; functions, methods and the exported fields of structs and
// interfaces carry their full signatures.
//
// Regenerate with: go test -run TestPublicSurfaceMatchesCommittedList -update-public-api .
func TestPublicSurfaceMatchesCommittedList(t *testing.T) {
	got, err := publicSurface(".")
	if err != nil {
		t.Fatal(err)
	}
	if *updatePublicAPI {
		if err := os.MkdirAll(filepath.Dir(publicAPIFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(publicAPIFile, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(publicAPIFile)
	if err != nil {
		t.Fatalf("%v; create it with -update-public-api", err)
	}
	if got != string(want) {
		t.Errorf("the exported surface of package gusset differs from %s.\n%s\nIf the change is intended, regenerate with: go test -run TestPublicSurfaceMatchesCommittedList -update-public-api .", publicAPIFile, lineDiff(string(want), got))
	}
}

// publicSurface renders every exported declaration of the package in dir, one
// declaration per entry, sorted, using the build context's file selection so a
// platform-specific file is judged as the toolchain would judge it.
func publicSurface(dir string) (string, error) {
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	var entries []string
	for _, name := range pkg.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return "", err
		}
		// FileExports drops every unexported declaration, method and struct or
		// interface member in place; FilterDecl would keep the fields.
		ast.FileExports(file)
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || (d.Recv != nil && !exportedReceiver(d.Recv)) {
					continue
				}
				d.Doc, d.Body = nil, nil
				entries = append(entries, render(fset, d))
			case *ast.GenDecl:
				entries = append(entries, genDeclEntries(fset, d)...)
			}
		}
	}
	sort.Strings(entries)
	return "// Exported surface of package gusset. Generated: see public_api_test.go.\n" + strings.Join(entries, "\n") + "\n", nil
}

func exportedReceiver(recv *ast.FieldList) bool {
	if len(recv.List) != 1 {
		return false
	}
	typ := recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if idx, ok := typ.(*ast.IndexExpr); ok {
		typ = idx.X
	}
	id, ok := typ.(*ast.Ident)
	return ok && id.IsExported()
}

func genDeclEntries(fset *token.FileSet, d *ast.GenDecl) []string {
	var out []string
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if !s.Name.IsExported() {
				continue
			}
			s.Doc, s.Comment = nil, nil
			out = append(out, render(fset, &ast.GenDecl{Tok: token.TYPE, Specs: []ast.Spec{s}}))
		case *ast.ValueSpec:
			for _, n := range s.Names {
				if !n.IsExported() {
					continue
				}
				entry := d.Tok.String() + " " + n.Name
				if s.Type != nil {
					entry += " " + render(fset, s.Type)
				}
				out = append(out, entry)
			}
		}
	}
	return out
}

// render prints one entry. format.Node lays lines out from the original
// positions, so a comment that is dropped from the entry (a doc comment on an
// interface method or struct field) still leaves its blank line behind. Blank
// lines carry no part of a declaration, so they are removed: the surface is then
// the same whether or not, and however, a member is documented.
func render(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, node); err != nil {
		return "<unprintable: " + err.Error() + ">"
	}
	lines := strings.Split(buf.String(), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// lineDiff lists entries present in only one side. Entries are separated by
// newlines at the top level, and a multi-line entry is compared line by line,
// which is enough to name what moved.
func lineDiff(want, got string) string {
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			m[l]++
		}
		return m
	}
	w, g := count(want), count(got)
	var b strings.Builder
	for _, l := range strings.Split(want, "\n") {
		if w[l] > g[l] {
			b.WriteString("  removed: " + l + "\n")
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if g[l] > w[l] {
			b.WriteString("  added:   " + l + "\n")
		}
	}
	return b.String()
}

// The gate is only worth having if it fails. These feed publicSurface a package
// with and without a new export, so the comparison itself is exercised on every
// run instead of only when someone happens to add one.
func TestPublicSurfaceSeesAnAddedExport(t *testing.T) {
	dir := t.TempDir()
	write := func(src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package p\nfunc Open() error { return nil }\ntype T struct{ A int; b int }\nfunc (T) M() {}\nfunc (T) m() {}\nconst c = 1\n")
	before, err := publicSurface(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"func Open() error", "func (T) M()", "A int"} {
		if !strings.Contains(before, want) {
			t.Errorf("surface lacks %q:\n%s", want, before)
		}
	}
	for _, unwanted := range []string{"func (T) m()", " b int", "const c"} {
		if strings.Contains(before, unwanted) {
			t.Errorf("surface leaks unexported %q:\n%s", unwanted, before)
		}
	}
	for name, src := range map[string]string{
		"new func":       "package p\nfunc Open() error { return nil }\nfunc Extra() {}\ntype T struct{ A int; b int }\nfunc (T) M() {}\n",
		"new field":      "package p\nfunc Open() error { return nil }\ntype T struct{ A int; B int; b int }\nfunc (T) M() {}\n",
		"new method":     "package p\nfunc Open() error { return nil }\ntype T struct{ A int; b int }\nfunc (T) M() {}\nfunc (*T) N() {}\n",
		"changed sig":    "package p\nfunc Open(x int) error { return nil }\ntype T struct{ A int; b int }\nfunc (T) M() {}\n",
		"removed export": "package p\ntype T struct{ A int; b int }\nfunc (T) M() {}\n",
	} {
		write(src)
		after, err := publicSurface(dir)
		if err != nil {
			t.Fatal(err)
		}
		if after == before {
			t.Errorf("%s: the surface did not change, so the gate would not fire", name)
		}
		if lineDiff(before, after) == "" {
			t.Errorf("%s: lineDiff names nothing", name)
		}
	}
}

// A comment, or a blank line, inside an interface or struct is not part of the
// exported surface. format.Node prints positions from the original file, so a
// doc comment that is itself dropped still leaves its line gap behind, and the
// rendered entry gained blank lines whenever someone documented a member.
func TestPublicSurfaceIgnoresCommentsInsideAnEntry(t *testing.T) {
	const bare = "package p\n\ntype I interface {\n\tM() int\n\tN() int\n}\n\ntype S struct {\n\tA int\n\tB int\n}\n"
	const documented = "package p\n\n// I is an interface.\ntype I interface {\n\t// M is documented.\n\tM() int\n\n\t// N is documented.\n\tN() int // trailing\n}\n\n// S is a struct.\ntype S struct {\n\t// A is documented.\n\tA int\n\n\t// B is documented.\n\tB int\n}\n"
	surface := func(src string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := publicSurface(dir)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := surface(documented)
	if strings.Contains(strings.TrimSuffix(got, "\n"), "\n\n") {
		t.Errorf("a rendered entry contains a blank line:\n%q", got)
	}
	if want := surface(bare); got != want {
		t.Errorf("documenting members changed the surface.\n%s", lineDiff(want, got))
	}
}
