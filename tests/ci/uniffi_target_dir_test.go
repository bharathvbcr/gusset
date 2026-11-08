package ci_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	uniffiManifest  = "examples/uniffi_bindgen_go/rust/Cargo.toml"
	uniffiTargetDir = "--target-dir examples/uniffi_bindgen_go/rust/target"
)

// uniffiDir is the umbrella crate's directory: a `cd` into it, or a cargo that
// names its manifest, builds the umbrella.
const uniffiDir = "examples/uniffi_bindgen_go/rust"

// uniffiCd matches a `cd` into the crate directory (with or without a trailing
// slash), after which a bare `cargo build` builds the umbrella with no manifest
// path on the line at all.
var uniffiCd = regexp.MustCompile(`\bcd\s+(?:\./)?` + regexp.QuoteMeta(uniffiDir) + `/?(?:\s|;|&|$)`)

// unpinnedUniffiBuilds returns the commands in text that run cargo on the uniffi
// example crate without pinning its target directory to the crate's own.
//
// That crate is the docs/adoption.md umbrella: its staticlib is named
// libgusset.a. Without --target-dir it is written under CARGO_TARGET_DIR when
// that is set, and `make test` accepts CARGO_TARGET_DIR=<repo>/target (the
// linked-target-dir check). The umbrella then lands in target/debug, which
// internal/ffi/ffi.go names on the default link line and gen.go may hash, so
// the default build would link the example engine without asking.
//
// A command is judged whole, not by physical line: a backslash continuation
// puts the manifest path and --target-dir on different lines, and
// `cd examples/uniffi_bindgen_go/rust && cargo build` names no manifest at all.
// Each `;`/`&&`/`||`/pipe-separated cargo segment must carry the flag itself, so
// a pinned neighbour does not vouch for an unpinned one.
func unpinnedUniffiBuilds(text string) []string {
	var bad []string
	for _, ll := range logicalLines(strings.Split(text, "\n")) {
		inDir := uniffiCd.MatchString(ll.text)
		for _, seg := range commandSegments(ll.text) {
			seg = strings.TrimSpace(seg)
			if !strings.Contains(seg, "cargo ") {
				continue
			}
			if !strings.Contains(seg, uniffiManifest) && !inDir {
				continue
			}
			pinned := strings.Contains(seg, uniffiTargetDir) || (inDir && strings.Contains(seg, "--target-dir target"))
			if !pinned {
				bad = append(bad, seg)
			}
		}
	}
	return bad
}

func TestUniffiUmbrellaBuildsArePinnedToTheExampleTargetDir(t *testing.T) {
	files := []string{"../../Makefile"}
	workflows, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, workflows...)
	seen := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		seen += strings.Count(string(b), uniffiManifest)
		for _, line := range unpinnedUniffiBuilds(string(b)) {
			t.Errorf("%s: cargo on the uniffi umbrella without %q: %s", filepath.Base(f), uniffiTargetDir, line)
		}
	}
	if seen == 0 {
		t.Fatalf("no file names %s; this check examined nothing", uniffiManifest)
	}
}

func TestPlantedUnpinnedUniffiBuildIsCaught(t *testing.T) {
	planted := "test:\n\tcargo test --locked --manifest-path " + uniffiManifest + "\n" +
		"\tcargo build --locked --manifest-path " + uniffiManifest + " " + uniffiTargetDir + "\n" +
		"# cargo test --manifest-path " + uniffiManifest + " (a comment)\n"
	bad := unpinnedUniffiBuilds(planted)
	if len(bad) != 1 || !strings.HasPrefix(bad[0], "cargo test --locked") {
		t.Fatalf("want exactly the unpinned cargo test line, got %q", bad)
	}
}

// Each row is a command the single-line scanner got wrong: it read one physical
// line, so a continuation hid the missing flag, a `cd` hid the manifest, and a
// pinned flag on the next line was missed (a false alarm the other way).
func TestUniffiScannerHandlesContinuationsAndCd(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"manifest and flag on one line", "\tcargo build --manifest-path " + uniffiManifest + " " + uniffiTargetDir + "\n", 0},
		{"unpinned across a continuation", "\tcargo build --locked \\\n\t\t--manifest-path " + uniffiManifest + "\n", 1},
		{"pinned across a continuation", "\tcargo build --locked \\\n\t\t--manifest-path " + uniffiManifest + " \\\n\t\t" + uniffiTargetDir + "\n", 0},
		{"cargo on the line before the manifest", "\tcargo build \\\n\t\t--locked --manifest-path " + uniffiManifest + "\n", 1},
		{"cd then cargo, unpinned", "\tcd " + uniffiDir + " && cargo build --locked\n", 1},
		{"cd with a trailing slash", "\tcd " + uniffiDir + "/ && cargo test\n", 1},
		{"cd then cargo, pinned", "\tcd " + uniffiDir + " && cargo build --locked --target-dir target\n", 0},
		{"cd on a workflow run line", "        run: cd " + uniffiDir + " && cargo clippy --locked\n", 1},
		{"a pinned neighbour does not vouch", "\tcargo build --manifest-path " + uniffiManifest + " " + uniffiTargetDir + " && cargo test --manifest-path " + uniffiManifest + "\n", 1},
		{"cd into another directory", "\tcd crates/gusset && cargo build\n", 0},
		{"comment continuation is not a command", "# cargo build \\\n#   --manifest-path " + uniffiManifest + "\n", 0},
	}
	for _, c := range cases {
		if got := unpinnedUniffiBuilds(c.text); len(got) != c.want {
			t.Errorf("%s: want %d unpinned, got %d: %q", c.name, c.want, len(got), got)
		}
	}
}
