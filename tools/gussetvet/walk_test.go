package main

import (
	"go/ast"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const crossFreeSrc = "package p\nimport \"C\"\nimport \"unsafe\"\nfunc g(p unsafe.Pointer) { C.free(p) }\n"

// tree builds a module under a temp dir: files maps a slash path to its content.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// reported returns the sorted files checkAllocatorSymmetry flags under root.
func reported(t *testing.T, root string) []string {
	t.Helper()
	v, err := checkAllocatorSymmetry(root)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, line := range v {
		files = append(files, filepath.ToSlash(line[:strings.Index(line, ":")]))
	}
	sort.Strings(files)
	return files
}

func eq(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

// The walk is Go's own: go.mod decides what belongs to the module. A linked
// worktree or clone nested under the repository (each has its own go.mod) is
// another module and stays out, so its copy of tests/cgoprobe no longer fails
// the root's lint. Everything `go build ./...` compiles is scanned, whatever
// the directory is called.
func TestWalkFollowsGoModBoundaries(t *testing.T) {
	root := tree(t, map[string]string{
		"go.mod":                           "module example.com/app\n\ngo 1.26\n",
		"plain/bad.go":                     crossFreeSrc,
		"internal/target/bad.go":           crossFreeSrc, // Go compiles a directory called target
		"internal/vendor/bad.go":           crossFreeSrc,
		"web/node_modules/bad.go":          crossFreeSrc,
		"nested/go.mod":                    "module example.com/nested\n\ngo 1.26\n",
		"nested/bad.go":                    crossFreeSrc, // another module
		".claude/worktrees/agent-x/go.mod": "module example.com/app\n\ngo 1.26\n",
		".claude/worktrees/agent-x/tests/cgoprobe/probe.go": crossFreeSrc,
		"target/bad.go":           crossFreeSrc, // cargo output at the root
		"vendor/bad.go":           crossFreeSrc, // dependency tree at the root
		"testdata/bad.go":         crossFreeSrc,
		"_old/bad.go":             crossFreeSrc,
		"tests/cgoprobe/probe.go": crossFreeSrc, // the root's own allowlisted probe
	})
	want := []string{"internal/target/bad.go", "internal/vendor/bad.go", "plain/bad.go", "web/node_modules/bad.go"}
	if got := reported(t, root); !eq(got, want) {
		t.Fatalf("reported %v, want %v", got, want)
	}
}

// A .git entry says nothing about whether Go compiles a directory. Treating it
// as a repository boundary let `touch hidden/.git` hide a violation from R4
// while `go build ./...` still compiled it.
func TestWalkIgnoresGitMarkers(t *testing.T) {
	root := tree(t, map[string]string{
		"go.mod":          "module example.com/app\n\ngo 1.26\n",
		"hidden/.git":     "",
		"hidden/bad.go":   crossFreeSrc,
		"clone/.git/HEAD": "ref: refs/heads/main\n",
		"clone/bad.go":    crossFreeSrc,
	})
	want := []string{"clone/bad.go", "hidden/bad.go"}
	if got := reported(t, root); !eq(got, want) {
		t.Fatalf("reported %v, want %v", got, want)
	}
}

// A nested module the root replaces with a local directory is compiled into the
// root module's binaries, so it is scanned even though it has its own go.mod.
func TestWalkScansLocallyReplacedModules(t *testing.T) {
	for name, gomod := range map[string]string{
		"single line": "module example.com/app\n\ngo 1.26\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ./dep\n",
		"block":       "module example.com/app\n\ngo 1.26\n\nrequire example.com/dep v0.0.0\n\nreplace (\n\texample.com/dep v0.0.0 => ./dep // local\n)\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := tree(t, map[string]string{
				"go.mod":       gomod,
				"dep/go.mod":   "module example.com/dep\n\ngo 1.26\n",
				"dep/bad.go":   crossFreeSrc,
				"other/go.mod": "module example.com/other\n\ngo 1.26\n",
				"other/bad.go": crossFreeSrc,
			})
			if got, want := reported(t, root), []string{"dep/bad.go"}; !eq(got, want) {
				t.Fatalf("reported %v, want %v", got, want)
			}
		})
	}
}

// The scanned set is the set `go list ./...` builds, on a tree that exercises
// every rule above. Directories Go ignores (. _ testdata, nested modules) and the
// two root-only build-output names are the declared differences.
func TestWalkMatchesGoList(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; this check did not run")
	}
	root := tree(t, map[string]string{
		"go.mod":                "module example.com/app\n\ngo 1.26\n",
		"a.go":                  "package app\n",
		"x/y/z.go":              "package z\n",
		"internal/target/t.go":  "package t\n",
		"web/node_modules/n.go": "package n\n",
		"hidden/.git":           "",
		"hidden/h.go":           "package h\n",
		"nested/go.mod":         "module example.com/nested\n\ngo 1.26\n",
		"nested/n.go":           "package nested\n",
		"_skip/s.go":            "package s\n",
		".dot/d.go":             "package d\n",
		"testdata/td.go":        "package td\n",
		"onlytests/o_test.go":   "package o\n",
	})
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	want := map[string]bool{}
	for _, d := range strings.Fields(string(out)) {
		rel, err := filepath.Rel(root, d)
		if err != nil {
			t.Fatal(err)
		}
		want[filepath.ToSlash(rel)] = true
	}
	got := map[string]bool{}
	err = walkGo(root, func(rel string, _ *token.FileSet, _ *ast.File) {
		d := filepath.ToSlash(filepath.Dir(rel))
		got[d] = true
	})
	if err != nil {
		t.Fatal(err)
	}
	for d := range want {
		if !got[d] {
			t.Errorf("go list builds %q, the scan skipped it", d)
		}
	}
	for d := range got {
		if !want[d] {
			t.Errorf("the scan visited %q, which go list does not build", d)
		}
	}
}
