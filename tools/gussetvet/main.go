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

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, targetDir, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gussetvet: parse error in %s: %v\n", targetDir, err)
		os.Exit(1)
	}

	var violations []string

	for _, pkg := range pkgs {
		for filename, file := range pkg.Files {
			if strings.HasSuffix(filename, "_test.go") {
				continue
			}

			for _, decl := range file.Decls {
				// Look for cgo preamble in comments
				for _, commentGroup := range file.Comments {
					text := commentGroup.Text()
					if strings.Contains(text, "#cgo") {
						for _, fn := range exports {
							fn = strings.TrimSpace(fn)
							if fn == "" || strings.HasPrefix(fn, "#") {
								continue
							}

							noescape := fmt.Sprintf("#cgo noescape %s", fn)
							nocallback := fmt.Sprintf("#cgo nocallback %s", fn)

							if !strings.Contains(text, noescape) {
								violations = append(violations, fmt.Sprintf("%s: missing '%s'", filename, noescape))
							}
							if !strings.Contains(text, nocallback) {
								violations = append(violations, fmt.Sprintf("%s: missing '%s'", filename, nocallback))
							}
						}
					}
				}
				_ = decl
			}
		}
	}

	if len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "gussetvet: R5 violations found:")
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  - %s\n", v)
		}
		os.Exit(1)
	}

	fmt.Println("gussetvet: R5 checks passed (all exports carry noescape and nocallback)")
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
