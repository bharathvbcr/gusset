package ci_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every exported identifier of the root package carries its own doc comment.
// A comment above a parenthesised `var (` or `const (` group documents the
// group; godoc shows the names with no text of their own, which is how
// ErrGeneric, ErrPanic, ErrPoisoned, ErrBadArg and the Expected* constants went
// undocumented. A trailing `// ...` on the same line counts, as it does in
// godoc.

// undocumentedExports parses one package's sources and returns the exported
// functions, methods on exported types, types and group members with no doc
// comment of their own.
func undocumentedExports(t *testing.T, fset *token.FileSet, files map[string]*ast.File) []string {
	t.Helper()
	var out []string
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || d.Doc != nil {
					continue
				}
				if d.Recv != nil && len(d.Recv.List) > 0 && !exportedReceiver(d.Recv.List[0].Type) {
					continue
				}
				out = append(out, fset.Position(d.Pos()).String()+": func "+d.Name.Name)
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() && s.Doc == nil && (d.Lparen.IsValid() || d.Doc == nil) {
							out = append(out, fset.Position(s.Pos()).String()+": type "+s.Name.Name)
						}
					case *ast.ValueSpec:
						if s.Doc != nil || s.Comment != nil || (!d.Lparen.IsValid() && d.Doc != nil) {
							continue
						}
						for _, n := range s.Names {
							if n.IsExported() {
								out = append(out, fset.Position(n.Pos()).String()+": "+d.Tok.String()+" "+n.Name)
							}
						}
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func exportedReceiver(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.Ident:
			return x.IsExported()
		default:
			return false
		}
	}
}

func parseNonTest(t *testing.T, dir string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = f
	}
	return fset, files
}

func TestEveryExportedIdentifierOfTheRootPackageIsDocumented(t *testing.T) {
	fset, files := parseNonTest(t, "../..")
	if len(files) < 5 {
		t.Fatalf("parsed %d root-package files; the directory layout changed", len(files))
	}
	for _, u := range undocumentedExports(t, fset, files) {
		t.Errorf("%s has no doc comment of its own", u)
	}
}

func TestUndocumentedExportsRuleOnFixtures(t *testing.T) {
	const src = `package p

// Documented has a comment.
func Documented() {}

func Bare() {}

type hidden struct{}

func (hidden) Exported() {}

type Shown struct{}

func (Shown) Method() {}

var (
	// A is documented.
	A = 1
	B = 2
	C = 3 // C is documented by a trailing comment.
)

// One comment over a group documents the group, not its members.
const (
	D = 1
	E = 2
)

// F is a single documented value.
const F = 1
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	got := undocumentedExports(t, fset, map[string]*ast.File{"p.go": f})
	var names []string
	for _, g := range got {
		names = append(names, g[strings.LastIndex(g, " ")+1:])
	}
	if want := "Bare,Method,Shown,B,D,E"; !sameSet(names, strings.Split(want, ",")) {
		t.Fatalf("flagged %v; want exactly %s", names, want)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
