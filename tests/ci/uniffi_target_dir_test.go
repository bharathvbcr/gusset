package ci_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	uniffiManifest  = "examples/uniffi_bindgen_go/rust/Cargo.toml"
	uniffiTargetDir = "--target-dir examples/uniffi_bindgen_go/rust/target"
)

// unpinnedUniffiBuilds returns the lines in text that run cargo on the uniffi
// example crate without pinning its target directory to the crate's own.
//
// That crate is the docs/adoption.md umbrella: its staticlib is named
// libgusset.a. Without --target-dir it is written under CARGO_TARGET_DIR when
// that is set, and `make test` accepts CARGO_TARGET_DIR=<repo>/target (the
// linked-target-dir check). The umbrella then lands in target/debug, which
// internal/ffi/ffi.go names on the default link line and gen.go may hash, so
// the default build would link the example engine without asking.
func unpinnedUniffiBuilds(text string) []string {
	var bad []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, uniffiManifest) || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.Contains(trimmed, "cargo ") {
			continue
		}
		if !strings.Contains(trimmed, uniffiTargetDir) {
			bad = append(bad, trimmed)
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
