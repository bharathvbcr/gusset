package ci_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Rules for CI checks that can report success without having run. Each is a
// pure function over lines or names, so the same code judges the real tree and
// a planted violation; TestPlantedCheckWiringViolationsAreCaught proves each
// one can fail.

// --- a pipe into tee must run under pipefail ---------------------------------

type runBlock struct {
	n     int      // 1-based line of the `run:` key
	body  []string // the script's lines
	shell string   // the step's `shell:` value, if any
}

var (
	runKey   = regexp.MustCompile(`^(\s*)(- )?run:\s*(.*)$`)
	shellKey = regexp.MustCompile(`^\s*shell:\s*(.*)$`)
	// `cmd | tee` and `cmd |& tee` (stderr into the pipe); `||` is not a pipe.
	teePipe     = regexp.MustCompile(`(?:^|[^|])\|&?\s*(?:sudo\s+)?tee\b`)
	setPipefail = regexp.MustCompile(`\bset\s+(?:-[A-Za-z]+\s+)*-[A-Za-z]*o\s+pipefail\b`)
	// `set +o pipefail` (or `set +eo pipefail`) turns it off again.
	unsetPipefail = regexp.MustCompile(`\bset\s+(?:[-+][A-Za-z]+\s+)*\+[A-Za-z]*o\s+pipefail\b`)
	shellOpt      = regexp.MustCompile(`-[A-Za-z]*o\s+pipefail\b`)
)

// runBlocks extracts every `run:` step body together with its step's `shell:`.
func runBlocks(lines []string) []runBlock {
	var out []runBlock
	for i, l := range lines {
		m := runKey.FindStringSubmatch(l)
		if m == nil || isComment(l) {
			continue
		}
		keyCol := len(m[1])
		if m[2] != "" {
			keyCol += 2
		}
		b := runBlock{n: i + 1}
		if rest := strings.TrimSpace(m[3]); strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) != "" && indentOf(lines[j]) <= keyCol {
					break
				}
				b.body = append(b.body, lines[j])
			}
		} else {
			b.body = []string{rest}
		}
		b.shell = stepShell(lines, i, keyCol)
		out = append(out, b)
	}
	return out
}

// stepShell finds the `shell:` sibling of the run key at line i: a line at the
// same indentation, inside the same step (the step ends at the next `- ` at
// the step's own indentation, or at a shallower line).
func stepShell(lines []string, i, keyCol int) string {
	dash := keyCol - 2
	lo := i
	if !strings.HasPrefix(strings.TrimSpace(lines[i]), "- ") {
		for lo = i - 1; lo >= 0; lo-- {
			if strings.TrimSpace(lines[lo]) != "" && indentOf(lines[lo]) <= dash {
				break
			}
		}
	}
	for j := lo; j < len(lines); j++ {
		if j > lo && j != i && strings.TrimSpace(lines[j]) != "" && indentOf(lines[j]) <= dash {
			break
		}
		if j == i {
			continue
		}
		if indentOf(lines[j]) == keyCol || (j == lo && strings.HasPrefix(strings.TrimSpace(lines[j]), "- shell:")) {
			if m := shellKey.FindStringSubmatch(strings.Replace(lines[j], "- shell:", "  shell:", 1)); m != nil {
				return strings.TrimSpace(m[1])
			}
		}
	}
	return ""
}

// teeWithoutPipefailViolations reports a `cmd | tee f` whose step neither sets
// pipefail before it nor runs under `shell: bash -o pipefail {0}`. Without it
// the pipeline's status is tee's, so a failing command is green; the unset
// `shell:` default is `bash -e {0}`, which has no pipefail.
func teeWithoutPipefailViolations(name string, lines []string) []string {
	var out []string
	for _, b := range runBlocks(lines) {
		// A `shell: bash -o pipefail {0}` step starts armed, but a later
		// `set +o pipefail` in its script disarms it as much as in any other.
		armed := shellOpt.MatchString(b.shell)
		for _, ll := range logicalLines(b.body) {
			if unsetPipefail.MatchString(ll.text) {
				armed = false
			} else if setPipefail.MatchString(ll.text) {
				armed = true
			}
			if teePipe.MatchString(ll.text) && !armed {
				out = append(out, name+":"+strconv.Itoa(b.n)+": a pipe into tee with no `set -o pipefail` before it and no `shell: bash -o pipefail {0}`: "+strings.TrimSpace(ll.text))
			}
		}
	}
	return out
}

func TestEveryTeePipeRunsUnderPipefail(t *testing.T) {
	teeLines := 0
	for name, lines := range workflows(t) {
		for _, b := range runBlocks(lines) {
			for _, ll := range logicalLines(b.body) {
				if teePipe.MatchString(ll.text) {
					teeLines++
				}
			}
		}
		for _, v := range teeWithoutPipefailViolations(name, lines) {
			t.Error(v)
		}
	}
	if teeLines == 0 {
		t.Error("no `| tee` in any workflow: the rule matched nothing, so it proves nothing; the extractor is broken or the rule is obsolete")
	}
}

// --- every Go fuzz target is in a Makefile list ------------------------------

// fuzzLists maps each Makefile variable to the package directory its recipe
// fuzzes. The recipe names the package; this table is the other half of that
// pairing and a mismatch fails below.
var fuzzLists = map[string]string{
	"FUZZ_TARGETS":      "tests/pitfalls",
	"FUZZ_TARGETS_ROOT": ".",
}

var fuzzFunc = regexp.MustCompile(`(?m)^func (Fuzz\w*)\(\w+ \*testing\.F\)`)

// makefileVars reads `NAME ?= a b c` assignments, joining backslash
// continuations.
func makefileVars(lines []string) map[string][]string {
	vars := map[string][]string{}
	assign := regexp.MustCompile(`^([A-Z_]+)\s*[?:]?=\s*(.*)$`)
	for i := 0; i < len(lines); i++ {
		m := assign.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		text := m[2]
		for strings.HasSuffix(text, `\`) && i+1 < len(lines) {
			i++
			text = strings.TrimSuffix(text, `\`) + " " + strings.TrimSpace(lines[i])
		}
		vars[m[1]] = strings.Fields(text)
	}
	return vars
}

// fuzzSetViolations compares the Fuzz functions found per package directory
// with the Makefile lists. `go test -fuzz '^Name$'` exits 0 when nothing
// matches, so a renamed or unlisted target would go unfuzzed and stay green;
// set equality is the only thing that notices.
func fuzzSetViolations(found map[string][]string, vars map[string][]string) []string {
	var out []string
	listed := map[string][]string{}
	for v, dir := range fuzzLists {
		names, ok := vars[v]
		if !ok {
			out = append(out, "Makefile defines no "+v+" (the fuzz list for "+dir+")")
		}
		listed[dir] = names
	}
	dirs := map[string]bool{}
	for d := range found {
		dirs[d] = true
	}
	for d := range listed {
		dirs[d] = true
	}
	var sorted []string
	for d := range dirs {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)
	for _, d := range sorted {
		if _, ok := listed[d]; !ok {
			out = append(out, d+" has Fuzz targets "+strings.Join(found[d], ", ")+" and no Makefile list fuzzes that directory")
			continue
		}
		have, want := map[string]int{}, map[string]int{}
		for _, n := range found[d] {
			have[n]++
		}
		for _, n := range listed[d] {
			want[n]++
			if want[n] == 2 {
				out = append(out, d+": "+n+" is listed twice in the Makefile")
			}
		}
		for n := range have {
			if want[n] == 0 {
				out = append(out, d+": func "+n+" is in no Makefile fuzz list, so `make fuzz` never runs it")
			}
		}
		for n := range want {
			if have[n] == 0 {
				out = append(out, d+": the Makefile lists "+n+", which no *_test.go in that directory defines; its -fuzz pattern matches nothing and exits 0")
			}
		}
	}
	sort.Strings(out)
	return out
}

func goFuzzTargets(t *testing.T) map[string][]string {
	t.Helper()
	root := "../.."
	found := map[string][]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "target", "node_modules", "fuzz":
				if p != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dir, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		for _, m := range fuzzFunc.FindAllStringSubmatch(string(b), -1) {
			found[filepath.ToSlash(dir)] = append(found[filepath.ToSlash(dir)], m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestMakefileFuzzListsEqualTheGoFuzzTargets(t *testing.T) {
	found := goFuzzTargets(t)
	total := 0
	for _, names := range found {
		total += len(names)
	}
	if total == 0 {
		t.Fatal("found no `func Fuzz*` in any *_test.go: the scan matched nothing, so set equality proves nothing")
	}
	mk := readLines(t, "../../Makefile")
	for _, v := range fuzzSetViolations(found, makefileVars(mk)) {
		t.Error(v)
	}
	// The pairing in fuzzLists must be the one the recipe runs.
	recipe := strings.Join(mk, "\n")
	for _, want := range []string{
		`$(FUZZ_TARGETS); do`,
		`-fuzztime $(FUZZTIME) ./tests/pitfalls/ || exit 1`,
		`$(FUZZ_TARGETS_ROOT); do`,
		`-fuzzminimizetime 10x . || exit 1`,
	} {
		if !strings.Contains(recipe, want) {
			t.Errorf("Makefile fuzz recipe no longer contains %q; fuzzLists pairs each list with a directory by that recipe", want)
		}
	}
}

// --- every Rust test binary is in the ASan job or excluded with a reason -----

// asanExcluded lists the `-p gusset` test binaries the nightly AddressSanitizer
// job does not run, each with the concrete reason. An entry here that the job
// runs, or that names no binary, fails below, so the list cannot rot.
var asanExcluded = map[string]string{
	"exports_match":      "reads exports.txt and the archive's symbol table; executes no gusset code under test",
	"header_match":       "parses gusset.h and the Rust sources as text; executes no gusset code under test",
	"constants_match":    "compares #define and const lines as text; executes no gusset code under test",
	"header_field_types": "compares field types in gusset.h and the Rust sources as text; executes no gusset code under test",
	"resource_hygiene":   "it counts glibc malloc arenas and /proc/self/maps lines, and pins one arena with mallopt(M_ARENA_MAX, 1); under ASan the allocator is ASan's, so mallopt returns 0 and the maps count would measure ASan's own mappings and quarantine; it runs un-sanitized on the Linux matrix rows (Rust 1.97 floor, Allocator API on ubuntu-latest, Unit & Pitfalls on ubuntu-24.04-arm), and hygiene_baseline and handle_fd_reuse are the procfs checks the ASan job keeps",
	"ffi_exports_bound":  "its own header says it commits up to 1 GiB per gusset_buf_alloc call; under ASan's shadow and quarantine that exceeds the runner, and the nightly cargo-fuzz job (ffi_exports, -rss_limit_mb=2048) is its sanitizer coverage",
}

// rustTestBinaries lists the integration-test binaries of crates/gusset: every
// [[test]] in its Cargo.toml plus each tests/*.rs and tests/*/main.rs cargo
// discovers on its own.
func rustTestBinaries(t *testing.T) []string {
	t.Helper()
	set := map[string]bool{}
	name := regexp.MustCompile(`^name\s*=\s*"([^"]+)"`)
	inTest := false
	for _, l := range readLines(t, "../../crates/gusset/Cargo.toml") {
		s := strings.TrimSpace(l)
		if strings.HasPrefix(s, "[") {
			inTest = s == "[[test]]"
			continue
		}
		if m := name.FindStringSubmatch(s); m != nil && inTest {
			set[m[1]] = true
		}
	}
	entries, err := os.ReadDir("../../crates/gusset/tests")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch {
		case !e.IsDir() && strings.HasSuffix(e.Name(), ".rs"):
			set[strings.TrimSuffix(e.Name(), ".rs")] = true
		case e.IsDir():
			if _, err := os.Stat(filepath.Join("../../crates/gusset/tests", e.Name(), "main.rs")); err == nil {
				set[e.Name()] = true
			}
		}
	}
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var testFlag = regexp.MustCompile(`--test\s+([A-Za-z0-9_]+)`)

// asanRunBinaries is every `--test X` the sanitizer job passes to cargo test.
func asanRunBinaries(lines []string) map[string]bool {
	run := map[string]bool{}
	for _, job := range splitJobs(lines) {
		if strings.TrimSpace(strings.SplitN(job, "\n", 2)[0]) != "sanitizer:" {
			continue
		}
		for _, b := range runBlocks(strings.Split(job, "\n")) {
			for _, ll := range logicalLines(b.body) {
				if !strings.Contains(ll.text, "cargo test") {
					continue
				}
				for _, m := range testFlag.FindAllStringSubmatch(ll.text, -1) {
					run[m[1]] = true
				}
			}
		}
	}
	return run
}

func asanCoverageViolations(binaries []string, run map[string]bool, excluded map[string]string) []string {
	var out []string
	known := map[string]bool{}
	for _, b := range binaries {
		known[b] = true
		reason, isExcluded := excluded[b]
		switch {
		case run[b] && isExcluded:
			out = append(out, b+" is run by the ASan job and also excluded; drop the exclusion")
		case !run[b] && !isExcluded:
			out = append(out, b+" is a test binary the nightly ASan job neither runs nor excludes with a reason")
		case isExcluded && strings.TrimSpace(reason) == "":
			out = append(out, b+" is excluded with an empty reason")
		}
	}
	for b := range run {
		if !known[b] {
			out = append(out, "the ASan job runs --test "+b+", which is no test binary of crates/gusset")
		}
	}
	for b := range excluded {
		if !known[b] {
			out = append(out, "asanExcluded names "+b+", which is no test binary of crates/gusset")
		}
	}
	sort.Strings(out)
	return out
}

func TestNightlyASanRunsEveryRustTestBinaryOrNamesWhyNot(t *testing.T) {
	binaries := rustTestBinaries(t)
	run := asanRunBinaries(workflows(t)["nightly.yml"])
	if len(binaries) == 0 || len(run) == 0 {
		t.Fatalf("found %d test binaries and %d in the ASan job: the scan matched nothing, so coverage is unproven", len(binaries), len(run))
	}
	for _, v := range asanCoverageViolations(binaries, run, asanExcluded) {
		t.Error(v)
	}
}

// --- planted violations ------------------------------------------------------

func TestPlantedCheckWiringViolationsAreCaught(t *testing.T) {
	step := func(shell string, body ...string) []string {
		l := []string{"      - name: s"}
		if shell != "" {
			l = append(l, "        shell: "+shell)
		}
		l = append(l, "        run: |")
		for _, b := range body {
			l = append(l, "          "+b)
		}
		return append(l, "      - name: next", "        run: true")
	}
	tee := "go test ./... | tee out.txt"
	cases := []struct {
		name string
		got  []string
		good []string
	}{
		{"tee with no pipefail", teeWithoutPipefailViolations("x", step("", tee)), teeWithoutPipefailViolations("x", step("", "set -o pipefail", tee))},
		{"pipefail set after the tee", teeWithoutPipefailViolations("x", step("", tee, "set -o pipefail")), nil},
		{"set -e alone is not pipefail", teeWithoutPipefailViolations("x", step("", "set -e", tee)), teeWithoutPipefailViolations("x", step("", "set -euo pipefail", tee))},
		{"tee on a continuation line", teeWithoutPipefailViolations("x", step("", "go test \\", "  ./... | tee out.txt")), teeWithoutPipefailViolations("x", step("", "set -eo pipefail", "go test \\", "  ./... | tee out.txt"))},
		{"single-line run", teeWithoutPipefailViolations("x", []string{"      - run: go test ./... | tee out.txt"}), teeWithoutPipefailViolations("x", []string{"      - shell: bash -o pipefail {0}", "        run: go test ./... | tee out.txt"})},
		{"shell without pipefail", teeWithoutPipefailViolations("x", step("bash -e {0}", tee)), teeWithoutPipefailViolations("x", step("bash -o pipefail {0}", tee))},
		{"stderr-merging pipe into tee", teeWithoutPipefailViolations("x", step("", "go test ./... |& tee out.txt")), teeWithoutPipefailViolations("x", step("", "set -o pipefail", "go test ./... |& tee out.txt"))},
		{"|& tee on a continuation line", teeWithoutPipefailViolations("x", step("", "go test \\", "  ./... |& tee out.txt")), nil},
		{"set +o pipefail before the tee", teeWithoutPipefailViolations("x", step("", "set -o pipefail", "set +o pipefail", tee)), teeWithoutPipefailViolations("x", step("", "set +o pipefail", "set -o pipefail", tee))},
		{"set +eo pipefail before the tee", teeWithoutPipefailViolations("x", step("", "set -eo pipefail", "set +eo pipefail", tee)), nil},
		{"set +o pipefail disarms a pipefail shell", teeWithoutPipefailViolations("x", step("bash -o pipefail {0}", "set +o pipefail", tee)), teeWithoutPipefailViolations("x", step("bash -o pipefail {0}", tee))},
		{"an earlier step's pipefail does not carry over", teeWithoutPipefailViolations("x", append(step("", "set -o pipefail", "true"), step("", tee)...)), nil},
		{"unlisted Fuzz target", fuzzSetViolations(map[string][]string{".": {"FuzzA", "FuzzB"}, "tests/pitfalls": {"FuzzP"}}, map[string][]string{"FUZZ_TARGETS": {"FuzzP"}, "FUZZ_TARGETS_ROOT": {"FuzzA"}}), fuzzSetViolations(map[string][]string{".": {"FuzzA"}, "tests/pitfalls": {"FuzzP"}}, map[string][]string{"FUZZ_TARGETS": {"FuzzP"}, "FUZZ_TARGETS_ROOT": {"FuzzA"}})},
		{"renamed Fuzz target", fuzzSetViolations(map[string][]string{".": {"FuzzARenamed"}, "tests/pitfalls": {"FuzzP"}}, map[string][]string{"FUZZ_TARGETS": {"FuzzP"}, "FUZZ_TARGETS_ROOT": {"FuzzA"}}), nil},
		{"duplicate list entry", fuzzSetViolations(map[string][]string{".": {"FuzzA"}, "tests/pitfalls": {"FuzzP"}}, map[string][]string{"FUZZ_TARGETS": {"FuzzP"}, "FUZZ_TARGETS_ROOT": {"FuzzA", "FuzzA"}}), nil},
		{"Fuzz target in an unmapped directory", fuzzSetViolations(map[string][]string{".": {"FuzzA"}, "tests/pitfalls": {"FuzzP"}, "internal/x": {"FuzzX"}}, map[string][]string{"FUZZ_TARGETS": {"FuzzP"}, "FUZZ_TARGETS_ROOT": {"FuzzA"}}), nil},
		{"missing list variable", fuzzSetViolations(map[string][]string{".": {"FuzzA"}, "tests/pitfalls": {"FuzzP"}}, map[string][]string{"FUZZ_TARGETS": {"FuzzP"}}), nil},
		{"unrun, unexcluded binary", asanCoverageViolations([]string{"a", "b"}, map[string]bool{"a": true}, nil), asanCoverageViolations([]string{"a", "b"}, map[string]bool{"a": true}, map[string]string{"b": "text only"})},
		{"exclusion with no reason", asanCoverageViolations([]string{"a"}, nil, map[string]string{"a": " "}), nil},
		{"stale exclusion of a run binary", asanCoverageViolations([]string{"a"}, map[string]bool{"a": true}, map[string]string{"a": "why"}), nil},
		{"exclusion naming no binary", asanCoverageViolations([]string{"a"}, map[string]bool{"a": true}, map[string]string{"gone": "why"}), nil},
		{"--test naming no binary", asanCoverageViolations([]string{"a"}, map[string]bool{"a": true, "typo": true}, nil), nil},
	}
	for _, c := range cases {
		if len(c.got) == 0 {
			t.Errorf("%s: the planted violation was not caught", c.name)
		}
		if len(c.good) != 0 {
			t.Errorf("%s: the compliant form was flagged: %v", c.name, c.good)
		}
	}
}

// `a || tee f` runs tee only when a fails; nothing is piped, so pipefail is moot.
func TestOrTeeIsNotAPipe(t *testing.T) {
	if teePipe.MatchString("false || tee out.txt") {
		t.Error("`||` matched as a pipe into tee")
	}
	if !teePipe.MatchString("go test |& tee out.txt") || !teePipe.MatchString("go test | sudo tee out.txt") {
		t.Error("`|&` or `| sudo tee` was not matched")
	}
}

func TestMakefileVarsJoinContinuations(t *testing.T) {
	got := makefileVars([]string{"FUZZ_TARGETS_ROOT ?= FuzzA FuzzB \\", "\tFuzzC", "other:"})
	if strings.Join(got["FUZZ_TARGETS_ROOT"], ",") != "FuzzA,FuzzB,FuzzC" {
		t.Errorf("continuation not joined: %v", got)
	}
}
