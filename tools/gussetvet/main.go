package main

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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

	if len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "gussetvet: violations found:")
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  - %s\n", v)
		}
		os.Exit(1)
	}

	fmt.Println("gussetvet: R5 checks passed (all exports carry noescape and nocallback)")
	fmt.Println("gussetvet: R4 checks passed (no C.free on Rust-owned memory)")
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
func checkAllocatorSymmetry(root string) ([]string, error) {
	var violations []string

	// Assembled from fragments so the literal never appears in this file. The
	// alternative — exempting the checker from its own rule — is how the CI lint
	// job came to fail on every commit by matching the rules as written in
	// AGENTS.md, and an exemption here would also hide a genuine violation if this
	// tool ever grew a cgo call.
	needle := "C." + "free("

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "target", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel := strings.TrimPrefix(path, "./")
		if crossFreeAllowed[rel] {
			return nil
		}

		lines, err := readLines(path)
		if err != nil {
			return err
		}
		for i, line := range lines {
			// Ignore the rule as written in a comment, so documenting R4 does not
			// violate it — the same mistake that made the CI lint job fail on
			// every commit by matching its own rule text.
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			if strings.Contains(code, needle) {
				violations = append(violations, fmt.Sprintf(
					"%s:%d: R4: C.free on Rust-owned memory; use gusset_buf_free / Buffer.Free",
					rel, i+1))
			}
		}
		return nil
	})

	return violations, err
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
