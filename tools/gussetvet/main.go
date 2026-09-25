package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// R5 enforcer:
// Every #cgo import of a Rust export carries #cgo noescape and #cgo nocallback;
// no export calls back into Go.
func main() {
	targetDir := "internal/ffi"
	if len(os.Args) > 1 {
		targetDir = os.Args[1]
	}

	exportsFile := filepath.Join(targetDir, "exports.txt")
	exports, err := readLines(exportsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gussetvet: failed to read %s: %v\n", exportsFile, err)
		os.Exit(1)
	}

	// Parsed file by file rather than with parser.ParseDir, which staticcheck
	// rejects as deprecated (SA1019) — so `staticcheck ./...` in the lint job
	// exited 1 on every commit, the same way the forbidden-pattern audit used to.
	// The documented replacement is golang.org/x/tools/go/packages, a dependency
	// this check does not need: it reads cgo preambles out of comments and never
	// asks which package a file belongs to.
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gussetvet: cannot read %s: %v\n", targetDir, err)
		os.Exit(1)
	}

	fset := token.NewFileSet()
	var violations []string
	parsed := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		filename := filepath.Join(targetDir, name)

		file, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gussetvet: parse error in %s: %v\n", filename, err)
			os.Exit(1)
		}
		parsed++

		// Look for cgo preamble in comments
		for _, commentGroup := range file.Comments {
			text := commentGroup.Text()
			if !strings.Contains(text, "#cgo") {
				continue
			}

			// Collect the directives as whole lines.
			//
			// This used to be strings.Contains over the entire preamble, which
			// matched on prefixes: `#cgo noescape gusset_cancel_all` contains
			// `#cgo noescape gusset_cancel`, so deleting *both* directives for
			// `gusset_cancel` left R5 reporting "checks passed". That is a live
			// pair in exports.txt, not a hypothetical — and R5 is the rule that
			// keeps cgo from treating Go pointers as escaping into C, so losing it
			// on one export is silent.
			directives := make(map[string]bool)
			for _, line := range strings.Split(text, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "#cgo ") {
					directives[line] = true
				}
			}

			for _, fn := range exports {
				fn = strings.TrimSpace(fn)
				if fn == "" || strings.HasPrefix(fn, "#") {
					continue
				}

				noescape := fmt.Sprintf("#cgo noescape %s", fn)
				nocallback := fmt.Sprintf("#cgo nocallback %s", fn)

				if !directives[noescape] {
					violations = append(violations, fmt.Sprintf("%s: missing '%s'", filename, noescape))
				}
				if !directives[nocallback] {
					violations = append(violations, fmt.Sprintf("%s: missing '%s'", filename, nocallback))
				}
			}
		}
	}

	// A directory that parsed nothing would report "R5 checks passed" having
	// inspected no cgo preamble at all — the failure mode this whole audit exists
	// to prevent, one level up.
	if parsed == 0 {
		fmt.Fprintf(os.Stderr, "gussetvet: no non-test .go files in %s; R5 was not checked\n", targetDir)
		os.Exit(1)
	}

	crossFree, err := checkAllocatorSymmetry(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gussetvet: R4 scan failed: %v\n", err)
		os.Exit(1)
	}
	violations = append(violations, crossFree...)

	callbacks, err := checkNoCallbacks(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gussetvet: R5 callback scan failed: %v\n", err)
		os.Exit(1)
	}
	violations = append(violations, callbacks...)

	wrappers, err := checkWrappers(targetDir, exports)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gussetvet: R1 wrapper scan failed: %v\n", err)
		os.Exit(1)
	}
	violations = append(violations, wrappers...)

	if len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "gussetvet: violations found:")
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  - %s\n", v)
		}
		os.Exit(1)
	}

	fmt.Println("gussetvet: R5 checks passed (all exports carry noescape and nocallback)")
	fmt.Println("gussetvet: R4 checks passed (no C.free or preamble free() on Rust-owned memory)")
	fmt.Println("gussetvet: R5 checks passed (no //export callbacks from C into Go)")
	fmt.Println("gussetvet: R1 checks passed (every export has a Go wrapper)")
}

// crossFreeAllowed lists the paths permitted to call C.free.
//
// `tests/cgoprobe` frees memory it malloc'd itself, and holds the deliberate
// cross-free that the ASan job runs as a negative test. Both are the point of that
// package, so it is named here rather than being excluded by a pattern that would
// also silently exempt future files.
var crossFreeAllowed = map[string]bool{
	filepath.Join("tests", "cgoprobe", "probe.go"):     true,
	filepath.Join("tests", "cgoprobe", "crossfree.go"): true,
}

// checkAllocatorSymmetry enforces R4 statically: Go must never call C.free on
// memory Rust allocated.
//
// R4 previously had only a runtime enforcer — a deliberate cross-free under ASan —
// which did not exist, and which would in any case only run nightly on Linux. A
// `C.free` on a Rust pointer is visible in the source, so it can be caught on every
// commit on every platform instead of waiting for a sanitizer to notice the heap
// is corrupt.
//
// Checked on the syntax tree, not the text. A line scan missed a reference that
// is not a call (`f := C.free` and a later `f(p)`), and needed ad-hoc comment
// stripping that a string literal containing "//" defeated. Any reference to the
// C pseudo-package's free — call or value — is a violation, as is a cgo preamble
// that calls free() itself, which is the same cross-free one level down.
func checkAllocatorSymmetry(root string) ([]string, error) {
	var violations []string
	err := walkGo(root, func(rel string, fset *token.FileSet, file *ast.File) {
		if crossFreeAllowed[rel] {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "C" && sel.Sel.Name == "free" {
				violations = append(violations, fmt.Sprintf(
					"%s:%d: R4: C.free on Rust-owned memory; use gusset_buf_free / Buffer.Free",
					rel, fset.Position(sel.Pos()).Line))
			}
			return true
		})
		for _, line := range preambleLines(file) {
			if preambleFree.MatchString(line.text) {
				violations = append(violations, fmt.Sprintf(
					"%s:%d: R4: free() in a cgo preamble; Rust memory is released only by its own *_free export",
					rel, fset.Position(line.pos).Line))
			}
		}
	})
	return violations, err
}

// preambleFree matches a call to free( in C code, not gusset_buf_free( or
// gusset_status_free(.
var preambleFree = regexp.MustCompile(`(^|[^A-Za-z0-9_])free\s*\(`)

// checkNoCallbacks enforces the other half of R5: no Go function is exported
// to C. `//export` makes a Go function callable from C, and a Rust engine that
// reached it would be a Rust thread calling into Go — the callback every
// `#cgo nocallback` directive promises cgo will never happen.
func checkNoCallbacks(root string) ([]string, error) {
	var violations []string
	err := walkGo(root, func(rel string, fset *token.FileSet, file *ast.File) {
		for _, cg := range file.Comments {
			for _, c := range cg.List {
				if strings.HasPrefix(c.Text, "//export ") {
					violations = append(violations, fmt.Sprintf(
						"%s:%d: R5: //export makes Go callable from C; Gusset exports must never call back into Go",
						rel, fset.Position(c.Pos()).Line))
				}
			}
		}
	})
	return violations, err
}

// checkWrappers requires a Go reference to every export in exports.txt from the
// ffi package. An export with no wrapper is ABI surface nothing exercises or
// verifies from Go; exports_match proves the symbol exists, not that it is used.
func checkWrappers(dir string, exports []string) ([]string, error) {
	used := map[string]bool{}
	err := walkGo(dir, func(rel string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "C" {
					used[sel.Sel.Name] = true
				}
			}
			return true
		})
	})
	if err != nil {
		return nil, err
	}
	var violations []string
	for _, fn := range exports {
		fn = strings.TrimSpace(fn)
		if fn == "" || strings.HasPrefix(fn, "#") {
			continue
		}
		if !used[fn] {
			violations = append(violations, fmt.Sprintf(
				"%s: R1: export %s has no Go wrapper (no C.%s reference)", dir, fn, fn))
		}
	}
	return violations, nil
}

type preambleLine struct {
	text string
	pos  token.Pos
}

// preambleLines returns the lines of the comment attached to `import "C"`.
func preambleLines(file *ast.File) []preambleLine {
	var out []preambleLine
	for _, imp := range file.Imports {
		if imp.Path.Value != `"C"` {
			continue
		}
		doc := imp.Doc
		if doc == nil {
			for _, d := range file.Decls {
				if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
					for _, sp := range g.Specs {
						if sp == imp {
							doc = g.Doc
						}
					}
				}
			}
		}
		if doc == nil {
			continue
		}
		for _, c := range doc.List {
			for _, l := range strings.Split(c.Text, "\n") {
				out = append(out, preambleLine{text: l, pos: c.Pos()})
			}
		}
	}
	return out
}

// walkGo parses every non-test .go file under root, skipping build output and
// dependency trees. Test files are included: a cross-free in a test corrupts
// the heap just as well.
func walkGo(root string, visit func(rel string, fset *token.FileSet, file *ast.File)) error {
	fset := token.NewFileSet()
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "target", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		visit(rel, fset, file)
		return nil
	})
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}
