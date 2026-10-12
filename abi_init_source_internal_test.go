package gusset

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// init() is the only caller of mustCheckABI, and nothing observable at run time
// tells a package whose init skipped the call from one that made it: the
// report init stores is gathered before the check, so TestInit_Ran... passes
// with the call deleted. The panic-on-drift behaviour is tested through
// mustCheckABI directly; this pins that init reaches it, before the runtime
// starts, by reading the source (I6/R12).
func TestInit_CallsMustCheckABIBeforeStartingTheRuntime(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "gusset.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var initFn *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
			initFn = fn
		}
	}
	if initFn == nil {
		t.Fatal("gusset.go has no init(): the ABI check at load time is gone")
	}

	var order []string
	ast.Inspect(initFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "mustCheckABI" {
				order = append(order, "mustCheckABI")
			}
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok && x.Name == "ffi" && fn.Sel.Name == "Init" {
				order = append(order, "ffi.Init")
			}
		}
		return true
	})
	if len(order) != 2 || order[0] != "mustCheckABI" || order[1] != "ffi.Init" {
		t.Fatalf("init must call mustCheckABI and then ffi.Init, calls seen in order: %v", order)
	}
}
