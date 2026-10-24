package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// R5 enforcer:
// Every #cgo import of a Rust export carries #cgo noescape and #cgo nocallback;
// no export calls back into Go.
func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `Usage: gussetvet [-exports file] [-tags list] [dir]

Checks the cgo shim in dir (default internal/ffi) against the Gusset rules:
R5 (every referenced export carries #cgo noescape and #cgo nocallback,
package-wide; no //export), R4 (no C.free on Rust memory) and R1 (every export
has a Go wrapper). R5's directive check and R1 read the files of dir that
go build compiles; R4 and the //export scan walk every file of the Go module
that contains dir, whatever its build constraints.

  -exports file   list of exported C symbols, one per line, '#' comments allowed
                  (default dir/exports.txt). An adopter names its own list here.
  -tags list      comma-separated build tags, as for go build -tags. Run once per
                  tag set you build with: -tags gusset_pkgconfig for Option A.

An empty or unreadable export list is an error: no export was checked.
`

// realMain parses args and returns the process exit status: 0 clean (or -h),
// 1 a violation or a check that could not run, 2 a usage error.
func realMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gussetvet", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	exports := fs.String("exports", "", "")
	tagList := fs.String("tags", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "gussetvet: %v\n\n%s", err, usage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(stderr, "gussetvet: want at most one directory, got %d\n\n%s", fs.NArg(), usage)
		return 2
	}
	exportsSet := false
	fs.Visit(func(f *flag.Flag) { exportsSet = exportsSet || f.Name == "exports" })
	if exportsSet && *exports == "" {
		fmt.Fprintf(stderr, "gussetvet: -exports needs a file\n\n%s", usage)
		return 2
	}
	tags, err := parseTags(*tagList)
	if err != nil {
		fmt.Fprintf(stderr, "gussetvet: -tags: %v\n\n%s", err, usage)
		return 2
	}
	targetDir := "internal/ffi"
	if fs.NArg() == 1 {
		targetDir = fs.Arg(0)
	}
	return runWith(targetDir, *exports, tags, stdout, stderr)
}

// buildTag is what a //go:build expression accepts as a tag.
var buildTag = regexp.MustCompile(`^[A-Za-z0-9_.]+$`)

// parseTags splits a -tags value on commas, as go build does, dropping empty
// entries. Anything that is not a tag is a usage error: go build's legacy
// space-separated form, or a stray quote, would otherwise become one tag no
// file names, and the run would check the default build while the caller
// believed it checked theirs.
func parseTags(s string) ([]string, error) {
	var tags []string
	for _, t := range strings.Split(s, ",") {
		if t == "" {
			continue
		}
		if !buildTag.MatchString(t) {
			return nil, fmt.Errorf("%q is not a build tag; separate tags with commas", t)
		}
		tags = append(tags, t)
	}
	return tags, nil
}

// buildContext is go/build's default context (GOOS, GOARCH, cgo, release
// tags) with tags added: the file set `go build -tags` would compile.
func buildContext(tags []string) *build.Context {
	ctx := build.Default
	ctx.BuildTags = append(append([]string(nil), build.Default.BuildTags...), tags...)
	return &ctx
}

// run checks targetDir against its own exports.txt with no build tags and
// returns the exit status.
func run(targetDir string, stdout, stderr io.Writer) int {
	return runWith(targetDir, "", nil, stdout, stderr)
}

// runWith checks targetDir against exportsFile (targetDir/exports.txt when
// empty), building with tags, and returns the process exit status.
func runWith(targetDir, exportsFile string, tags []string, stdout, stderr io.Writer) int {
	if exportsFile == "" {
		exportsFile = filepath.Join(targetDir, "exports.txt")
	}
	exports, err := readExports(exportsFile)
	if err != nil {
		fmt.Fprintf(stderr, "gussetvet: %v\n", err)
		return 1
	}
	ctx := buildContext(tags)

	violations, err := checkDirectives(ctx, targetDir, exports)
	if err != nil {
		fmt.Fprintf(stderr, "gussetvet: %v; R5 was not checked\n", err)
		return 1
	}

	root, err := moduleRoot(targetDir)
	if err != nil {
		fmt.Fprintf(stderr, "gussetvet: %v; R4 and the R5 callback scan were not run\n", err)
		return 1
	}

	crossFree, err := checkAllocatorSymmetry(root)
	if err != nil {
		fmt.Fprintf(stderr, "gussetvet: R4 scan failed: %v\n", err)
		return 1
	}
	violations = append(violations, crossFree...)

	callbacks, err := checkNoCallbacks(root)
	if err != nil {
		fmt.Fprintf(stderr, "gussetvet: R5 callback scan failed: %v\n", err)
		return 1
	}
	violations = append(violations, callbacks...)

	wrappers, err := checkWrappers(ctx, targetDir, exports)
	if err != nil {
		fmt.Fprintf(stderr, "gussetvet: R1 wrapper scan failed: %v\n", err)
		return 1
	}
	violations = append(violations, wrappers...)

	if len(violations) > 0 {
		fmt.Fprintln(stderr, "gussetvet: violations found:")
		for _, v := range violations {
			fmt.Fprintf(stderr, "  - %s\n", v)
		}
		return 1
	}

	fmt.Fprintln(stdout, "gussetvet: R5 checks passed (all exports carry noescape and nocallback)")
	fmt.Fprintf(stdout, "gussetvet: R4 checks passed (no C.free or preamble free() on Rust-owned memory under %s)\n", root)
	fmt.Fprintf(stdout, "gussetvet: R5 checks passed (no //export callbacks from C into Go under %s)\n", root)
	fmt.Fprintln(stdout, "gussetvet: R1 checks passed (every export has a Go wrapper)")
	return 0
}

// checkDirectives enforces R5 on the cgo preambles of the package in dir.
//
// cmd/cgo merges `#cgo` lines across every file of the package it is actually
// building, so noescape and nocallback are one set. A file must carry both
// only for exports it references as C.<name>. A second file whose only `#cgo`
// line is LDFLAGS is not a violation when the directives live on the file that
// references the export, or on any other file in that build.
//
// Which files those are is packageFiles: //go:build lines, GOOS/GOARCH
// filename suffixes and the -tags the caller builds with, not every non-test
// .go file in the directory. A //go:build ignore file or a windows file on
// darwin is not part of the build, and its directives must not satisfy a
// reference in a file that is. A //go:build gusset_pkgconfig shim is part of
// the build only under -tags gusset_pkgconfig, and is checked then.
//
// Only the comment attached to `import "C"` is a preamble. This used to treat
// every comment group containing "#cgo" as one, so a doc comment that merely
// named a directive failed the check for all 17 exports, and a preamble with no
// directives left passed as long as some other comment spelled them all out.
//
// Directives are compared as whole lines. strings.Contains over the preamble
// matched on prefixes: `#cgo noescape gusset_cancel_all` contains
// `#cgo noescape gusset_cancel`, so deleting *both* directives for
// `gusset_cancel` left R5 reporting "checks passed". That is a live pair in
// exports.txt, not a hypothetical — and R5 is the rule that keeps cgo from
// treating Go pointers as escaping into C, so losing it on one export is silent.
//
// Parsed file by file rather than with parser.ParseDir, which staticcheck
// rejects as deprecated (SA1019) — so `staticcheck ./...` in the lint job
// exited 1 on every commit, the same way the forbidden-pattern audit used to.
// The documented replacement is golang.org/x/tools/go/packages, a dependency
// this check does not need: it reads cgo preambles out of comments and never
// asks which package a file belongs to.
//
// A directory with no preamble carrying `#cgo` lines is an error, not a pass:
// "R5 checks passed" having inspected no preamble is the failure mode this
// audit exists to prevent.
func checkDirectives(ctx *build.Context, dir string, exports []string) ([]string, error) {
	filenames, err := packageFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	exportSet := make(map[string]bool)
	for _, fn := range exports {
		fn = strings.TrimSpace(fn)
		if fn == "" || strings.HasPrefix(fn, "#") {
			continue
		}
		exportSet[fn] = true
	}
	fset := token.NewFileSet()
	type cgoFile struct {
		filename string
		refs     []string
	}
	packageDirectives := make(map[string]bool)
	var files []cgoFile
	sawCgo := false
	for _, filename := range filenames {
		file, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse error in %s: %w", filename, err)
		}
		for _, line := range preambleLines(file) {
			text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line.text), "//"))
			if strings.HasPrefix(text, "#cgo ") {
				packageDirectives[text] = true
				sawCgo = true
			}
		}
		var refs []string
		seen := make(map[string]bool)
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "C" || !exportSet[sel.Sel.Name] || seen[sel.Sel.Name] {
				return true
			}
			seen[sel.Sel.Name] = true
			refs = append(refs, sel.Sel.Name)
			return true
		})
		files = append(files, cgoFile{filename: filename, refs: refs})
	}
	if !sawCgo {
		return nil, fmt.Errorf("no cgo preamble with #cgo lines in %s", dir)
	}
	var violations []string
	for _, f := range files {
		for _, fn := range f.refs {
			for _, kind := range []string{"noescape", "nocallback"} {
				want := fmt.Sprintf("#cgo %s %s", kind, fn)
				if !packageDirectives[want] {
					violations = append(violations, fmt.Sprintf("%s: missing '%s'", f.filename, want))
				}
			}
		}
	}
	return violations, nil
}

// packageFiles returns the files of dir that the build compiles into the
// package under ctx: non-test .go files whose //go:build line and GOOS/GOARCH
// filename suffix ctx.MatchFile accepts. R5's directive check and R1's wrapper
// scan both read exactly this list, so the two rules judge one file set. R1
// used to walk every .go file under dir, so a reference in a //go:build ignore
// file, another GOOS's file or a _test.go file (cgo refuses import "C" in
// tests) counted as a wrapper that no build compiles.
//
// _test.go stays out: the library build does not merge it. A MatchFile error is
// a failed check, not a file skipped: skipping it would hide its references.
func packageFiles(ctx *build.Context, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", dir, err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		match, err := ctx.MatchFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("build constraints for %s: %w", filepath.Join(dir, name), err)
		}
		if match {
			files = append(files, filepath.Join(dir, name))
		}
	}
	return files, nil
}

// moduleRoot returns the directory of the nearest go.mod at or above dir: the
// tree the R4 and R5-callback scans walk.
//
// Derived from targetDir rather than the working directory. Those scans used to
// walk ".", so `gussetvet /path/to/repo/internal/ffi` run from anywhere but the
// repository root — which is how install.sh runs it — checked an unrelated
// directory and reported R4 and R5 as passed.
func moduleRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", dir, err)
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("cannot stat %s: %w", filepath.Join(d, "go.mod"), err)
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no go.mod at or above %s, so there is no module to scan", abs)
		}
	}
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
// Only packageFiles count, the same set R5 checks.
func checkWrappers(ctx *build.Context, dir string, exports []string) ([]string, error) {
	filenames, err := packageFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	used := map[string]bool{}
	for _, filename := range filenames {
		file, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", filename, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "C" {
					used[sel.Sel.Name] = true
				}
			}
			return true
		})
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
			// c.Pos() is the first byte of the comment. A /* */ comment is one
			// ast.Comment spanning many physical lines; reporting every line at
			// that first position made an R4 free() on a later line look like
			// it was on the line of `/*`.
			pos := c.Pos()
			lines := strings.Split(c.Text, "\n")
			for i, l := range lines {
				out = append(out, preambleLine{text: l, pos: pos})
				if i+1 < len(lines) {
					pos += token.Pos(len(l) + 1)
				}
			}
		}
	}
	return out
}

// walkGo parses every .go file of the module at root, test files included. It
// visits the directories `go build ./...` would: Go's own rule skips names that
// start with "." or "_", `testdata`, and any subdirectory holding its own
// go.mod (another module, which is where "./..." stops). That last rule is what
// keeps a linked worktree or clone nested under the repository (for example
// .claude/worktrees/*, each with its own go.mod) out of the scan. A nested
// module that root's go.mod points at with `replace ... => ./dir` is built into
// the module's binaries, so it is scanned anyway.
//
// Two names are skipped only directly under root, where they are build output
// and not source: `target` (cargo's output directory) and `vendor` (a dependency
// tree). Below root they are ordinary directories that Go compiles, so they are
// scanned: a name-based skip at any depth let internal/target/x.go hide a
// violation. A .git entry means nothing here; go.mod alone decides whether a
// directory belongs to the module, so `touch hidden/.git` hides nothing.
//
// Test files count: a cross-free in a test corrupts the heap just as well.
// Build constraints and -tags are not applied, unlike packageFiles: R4 and the
// //export scan judge every file whatever it builds under, so a C.free in a
// linux-only file fails a run on darwin too.
func walkGo(root string, visit func(rel string, fset *token.FileSet, file *ast.File)) error {
	fset := token.NewFileSet()
	replaced, err := replacedDirs(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" {
				return filepath.SkipDir
			}
			if filepath.Dir(path) == root && (name == "target" || name == "vendor") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && !replaced[path] {
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

// replaceTarget matches the local-directory right-hand side of a go.mod replace
// directive, in the single-line and the block form.
var replaceTarget = regexp.MustCompile(`=>\s*"?(\.{1,2}/[^\s"]*|\.)"?\s*(//.*)?$`)

// replacedDirs returns the directories root's go.mod replaces a module with.
func replacedDirs(root string) (map[string]bool, error) {
	lines, err := readLines(filepath.Join(root, "go.mod"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil // a bare tree has no replacements
	}
	if err != nil {
		return nil, fmt.Errorf("read go.mod in %s: %w", root, err)
	}
	dirs := map[string]bool{}
	for _, line := range lines {
		if m := replaceTarget.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			dirs[filepath.Join(root, filepath.FromSlash(m[1]))] = true
		}
	}
	return dirs, nil
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

// readExports reads the export list at path: one symbol per line, blank lines
// and '#' comments dropped. A list with no symbol is an error, because every
// check below iterates it and would pass having inspected nothing.
func readExports(path string) ([]string, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no exports listed in %s; nothing was checked", path)
	}
	return out, nil
}
