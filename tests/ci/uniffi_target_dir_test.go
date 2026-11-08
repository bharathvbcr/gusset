package ci_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	uniffiManifest  = "examples/uniffi_bindgen_go/rust/Cargo.toml"
	uniffiTargetDir = "--target-dir examples/uniffi_bindgen_go/rust/target"
)

// uniffiDir is the umbrella crate's directory: a cargo that runs in it, or names
// its manifest, builds the umbrella.
const uniffiDir = "examples/uniffi_bindgen_go/rust"

var (
	uniffiPinned    = regexp.MustCompile(`--target-dir[= ]\s*(?:\./)?` + regexp.QuoteMeta(uniffiDir) + `/target\b`)
	uniffiPinnedRel = regexp.MustCompile(`--target-dir[= ]\s*(?:\./)?target\b`)
	// cargo as a command word, followed by any whitespace (a tab too).
	cargoWord = regexp.MustCompile(`(?:^|\s)cargo\s`)
)

// cdTo is the directory after `cd arg` (or `pushd arg`) from cur, relative to
// the repository root; "" is the root. A target it cannot resolve (absolute,
// `~`, a variable, `cd -`, no argument) is "?", which is never the crate.
func cdTo(cur, arg string) string {
	arg = strings.Trim(arg, `"'`)
	if arg == "" || cur == "?" || strings.ContainsAny(arg[:1], "/~$-") {
		return "?"
	}
	d := path.Clean(path.Join(cur, arg))
	if d == "." {
		return ""
	}
	return d
}

// uniffiUnits splits a file into the units one shell runs: each Makefile
// recipe line is its own shell, so a `cd` on one line does not reach the next;
// a workflow `run:` body is one shell for all of its lines.
func uniffiUnits(name, text string) [][]logicalLine {
	lines := strings.Split(text, "\n")
	var units [][]logicalLine
	if strings.HasSuffix(name, "Makefile") {
		for _, ll := range logicalLines(lines) {
			units = append(units, []logicalLine{ll})
		}
		return units
	}
	for _, b := range runBlocks(lines) {
		units = append(units, logicalLines(b.body))
	}
	return units
}

// unpinnedUniffiBuilds returns the commands in text (a Makefile when name ends
// in Makefile, else a workflow) that run cargo on the uniffi example crate
// without pinning its target directory to the crate's own.
//
// That crate is the docs/adoption.md umbrella: its staticlib is named
// libgusset.a. Without --target-dir it is written under CARGO_TARGET_DIR when
// that is set, and `make test` accepts CARGO_TARGET_DIR=<repo>/target (the
// linked-target-dir check). The umbrella then lands in target/debug, which
// internal/ffi/ffi.go names on the default link line and gen.go may hash, so
// the default build would link the example engine without asking.
//
// A command is judged whole, not by physical line: a backslash continuation
// puts the manifest path and --target-dir on different lines, and a cargo with
// no manifest builds the umbrella when the shell is in its directory. The
// working directory is tracked through `cd` and `pushd`/`popd` within one
// shell (quoted or not, `cd a && cd b`, `cd d; cd ..`). Each `;`/`&&`/`||`/
// pipe-separated cargo segment must carry the flag itself, `--target-dir x` or
// `--target-dir=x`, so a pinned neighbour does not vouch for an unpinned one.
//
// Limits of a line scanner, accepted: a `working-directory:` key on a step is
// not read; `( cd d && cargo build ); cargo test` is read as if the second
// cargo ran in d; a directory reached through a variable or `cd -` is unknown.
func unpinnedUniffiBuilds(name, text string) []string {
	var bad []string
	for _, unit := range uniffiUnits(name, text) {
		cur := ""
		var stack []string
		for _, ll := range unit {
			for _, seg := range commandSegments(ll.text) {
				seg = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(seg), "( "))
				f := strings.Fields(seg)
				arg := ""
				if len(f) > 1 {
					arg = f[1]
				}
				switch {
				case len(f) > 0 && f[0] == "cd":
					cur = cdTo(cur, arg)
				case len(f) > 0 && f[0] == "pushd":
					stack = append(stack, cur)
					cur = cdTo(cur, arg)
				case len(f) > 0 && f[0] == "popd":
					if n := len(stack); n > 0 {
						cur, stack = stack[n-1], stack[:n-1]
					} else {
						cur = "?"
					}
				}
				if !cargoWord.MatchString(seg) {
					continue
				}
				inDir := cur == uniffiDir
				if !strings.Contains(seg, uniffiManifest) && !inDir {
					continue
				}
				if !uniffiPinned.MatchString(seg) && !(inDir && uniffiPinnedRel.MatchString(seg)) {
					bad = append(bad, seg)
				}
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
		for _, line := range unpinnedUniffiBuilds(f, string(b)) {
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
	bad := unpinnedUniffiBuilds("Makefile", planted)
	if len(bad) != 1 || !strings.HasPrefix(bad[0], "cargo test --locked") {
		t.Fatalf("want exactly the unpinned cargo test line, got %q", bad)
	}
}

// Each row is a command the single-line scanner got wrong: it read one physical
// line, so a continuation hid the missing flag, a `cd` hid the manifest, and a
// pinned flag on the next line was missed (a false alarm the other way).
func TestUniffiScannerHandlesContinuationsAndCd(t *testing.T) {
	// A workflow step whose run: block holds the given lines.
	yml := func(lines ...string) string {
		return "      - name: s\n        run: |\n          " + strings.Join(lines, "\n          ") + "\n"
	}
	d := uniffiDir
	pin := "--target-dir " + d + "/target"
	cases := []struct {
		name string
		file string // "" is a Makefile
		text string
		want int
	}{
		{"equals-form --target-dir is pinned", "", "\tcargo build --manifest-path " + uniffiManifest + " --target-dir=" + d + "/target\n", 0},
		{"equals-form with the wrong dir is not", "", "\tcargo build --manifest-path " + uniffiManifest + " --target-dir=target\n", 1},
		{"cd d; cd ..; cargo is no longer in d", "x.yml", yml("cd " + d + "; cd ..; cargo build"), 0},
		{"cd d && cd .. && cargo is no longer in d", "x.yml", yml("cd " + d + " && cd .. && cargo build"), 0},
		{"cd into the parent only", "x.yml", yml("cd examples/uniffi_bindgen_go && cargo build"), 0},
		{"pushd d && cargo", "x.yml", yml("pushd " + d + " && cargo build"), 1},
		{"pushd, popd, then cargo is out of d", "x.yml", yml("pushd " + d + " && popd && cargo build"), 0},
		{"quoted cd", "x.yml", yml(`cd "` + d + `" && cargo build`), 1},
		{"single-quoted cd", "x.yml", yml(`cd '` + d + `' && cargo build`), 1},
		{"cd a && cd b && cargo", "x.yml", yml("cd examples/uniffi_bindgen_go && cd rust && cargo build"), 1},
		{"cd on its own line, cargo on the next line of one run block", "x.yml", yml("cd "+d, "cargo build --locked"), 1},
		{"the same two lines in a Makefile are two shells", "", "\tcd " + d + "\n\tcargo build --locked\n", 0},
		{"cd on its own line, pinned cargo on the next", "x.yml", yml("cd "+d, "cargo build --locked --target-dir target"), 0},
		{"cargo then a tab then build", "", "\tcargo\tbuild --manifest-path " + uniffiManifest + "\n", 1},
		{"tab after cargo in a cd'd run", "x.yml", yml("cd " + d + " && cargo\ttest"), 1},
		{"a run block's cd does not reach the next step", "x.yml", yml("cd "+d) + yml("cargo build"), 0},
		{"pinned in a run block", "x.yml", yml("cargo build --manifest-path " + uniffiManifest + " " + pin), 0},
		{"manifest and flag on one line", "", "\tcargo build --manifest-path " + uniffiManifest + " " + uniffiTargetDir + "\n", 0},
		{"unpinned across a continuation", "", "\tcargo build --locked \\\n\t\t--manifest-path " + uniffiManifest + "\n", 1},
		{"pinned across a continuation", "", "\tcargo build --locked \\\n\t\t--manifest-path " + uniffiManifest + " \\\n\t\t" + uniffiTargetDir + "\n", 0},
		{"cargo on the line before the manifest", "", "\tcargo build \\\n\t\t--locked --manifest-path " + uniffiManifest + "\n", 1},
		{"cd then cargo, unpinned", "", "\tcd " + uniffiDir + " && cargo build --locked\n", 1},
		{"cd with a trailing slash", "", "\tcd " + uniffiDir + "/ && cargo test\n", 1},
		{"cd then cargo, pinned", "", "\tcd " + uniffiDir + " && cargo build --locked --target-dir target\n", 0},
		{"cd on a workflow run line", "x.yml", "        run: cd " + uniffiDir + " && cargo clippy --locked\n", 1},
		{"a pinned neighbour does not vouch", "", "\tcargo build --manifest-path " + uniffiManifest + " " + uniffiTargetDir + " && cargo test --manifest-path " + uniffiManifest + "\n", 1},
		{"cd into another directory", "", "\tcd crates/gusset && cargo build\n", 0},
		{"comment continuation is not a command", "", "# cargo build \\\n#   --manifest-path " + uniffiManifest + "\n", 0},
	}
	for _, c := range cases {
		file := c.file
		if file == "" {
			file = "Makefile"
		}
		if got := unpinnedUniffiBuilds(file, c.text); len(got) != c.want {
			t.Errorf("%s: want %d unpinned, got %d: %q", c.name, c.want, len(got), got)
		}
	}
}
