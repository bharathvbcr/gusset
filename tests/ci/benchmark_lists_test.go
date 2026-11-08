package ci_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every benchmark in the repository is either compared against its base by the
// Matrix bench job (gatedBenchmarks) or run by hand on purpose
// (manualOnlyBenchmarks, each with its reason). One on neither list fails
// here. Before this rule the job compared only package ./bench, so a 30%
// slowdown of the completion transport left CI green and nobody had decided
// that it should.
//
// The lists are the canonical record; this file is where to add a benchmark.
// A name is "<package dir>:<func name>", the directory relative to the repo
// root ("." is the root package). A Rust bench is "crates/gusset/benches/<file>".
// Entries that no longer match a benchmark fail too, so a rename cannot leave a
// stale gate behind.

// gatedBenchmarks are compared by the bench job in .github/workflows/matrix.yml.
// Package bench runs whole (`-test.bench .`); the root and isolate packages run
// the names in the job's ROOT_BENCH and ISOLATE_BENCH patterns, which
// TestGatedBenchmarksAreWhatTheJobRuns checks against this list.
var gatedBenchmarks = []string{
	"bench:BenchmarkBufferAllocParallel",
	"bench:BenchmarkChannelHop",
	"bench:BenchmarkDeadlineExpiry",
	"bench:BenchmarkDeadlineInFlight",
	"bench:BenchmarkDiscard",
	"bench:BenchmarkGussetBufferLarge",
	"bench:BenchmarkGussetBufferLargeZeroCopy",
	"bench:BenchmarkGussetCallNoop",
	"bench:BenchmarkGussetCallParallel",
	"bench:BenchmarkGussetCallResult",
	"bench:BenchmarkGussetSubmitWait",
	"bench:BenchmarkGussetSubmitWaitResult",
	"bench:BenchmarkGussetWaitBuffer",
	"bench:BenchmarkGussetWaitBufferResultSize",
	"bench:BenchmarkHandleChurn",
	"bench:BenchmarkStats",
	"bench:BenchmarkSumSquares",
	".:BenchmarkCallNoopBare",
	".:BenchmarkCallNoopTraced",
	".:BenchmarkCompletionTransport",
	"internal/isolate:BenchmarkProcCallEcho",
	"internal/isolate:BenchmarkSupervisorCallEcho",
}

// manualOnlyBenchmarks are not in the bench job. The value is why, and it must
// not be empty.
var manualOnlyBenchmarks = map[string]string{
	"internal/isolate:BenchmarkSupervisorRestart": "one restart is a kill plus a process start and handshake, dominated by the kernel and the runner's load; it swings well past the gate's 10% between identical runs, so gating it would fail on noise. Record it with RECORD_DIR=. RECORD_PKG=./internal/isolate bench/record.sh OUT 1s COUNT BenchmarkSupervisorRestart.",

	"crates/gusset/benches/dispatch.rs":       "cargo bench compares its own arms (diagnostic vs registered engine, opcode 0 vs nonzero) interleaved inside one process and has no base-versus-head mode. `make bench-dispatch` records it.",
	"crates/gusset/benches/alloc_counters.rs": "cargo bench of the allocator counters under 1 to 8 threads, with no base-versus-head mode. `make bench-alloc-counters` records it; its Go side, BenchmarkBufferAllocParallel, is gated.",

	"bench/seed/go:BenchmarkBatch16":             "seed study of the cgo boundary in its own module (bench/seed/go/go.mod); recorded by `make bench-crossover`, not part of the product's regression set.",
	"bench/seed/go:BenchmarkBatch256":            "seed study; see BenchmarkBatch16.",
	"bench/seed/go:BenchmarkBatch4096":           "seed study; see BenchmarkBatch16.",
	"bench/seed/go:BenchmarkCgoCNoop":            "seed study; see BenchmarkBatch16.",
	"bench/seed/go:BenchmarkCgoNoopParallel":     "seed study; see BenchmarkBatch16.",
	"bench/seed/go:BenchmarkCgoRustNoop":         "seed study; see BenchmarkBatch16.",
	"bench/seed/go:BenchmarkChannelHop":          "seed study; see BenchmarkBatch16. The gated control of the same name is bench:BenchmarkChannelHop.",
	"bench/seed/go:BenchmarkCrossoverParallel":   "seed study, minutes per iteration (512 concurrent calls each); recorded by `make bench-crossover`.",
	"bench/seed/go:BenchmarkCrossoverSerial":     "seed study; recorded by `make bench-crossover`.",
	"bench/seed/go:BenchmarkPureGoLoopItem":      "seed study; see BenchmarkBatch16.",
	"bench/seed/go:BenchmarkThreadPressure":      "measures threads, not time, one transport per process; recorded by `make bench-crossover`.",
	"bench/seed/go:BenchmarkThreadScaling":       "measures threads, not time; recorded by `make bench-scaling`.",
	"bench/seed/go:BenchmarkUnbufferedRoundTrip": "seed study; see BenchmarkBatch16.",
	"bench/seed/esc:BenchmarkNoEscape":           "escape-analysis demonstration in its own module (bench/seed/esc/go.mod); it backs the allocation claim, not a timing.",
	"bench/seed/esc:BenchmarkPlain":              "escape-analysis demonstration; see BenchmarkNoEscape.",
}

// benchmarkFuncRe is Go's own rule: Benchmark followed by anything but a
// lower-case letter (go help testfunc). The AST check below adds the
// *testing.B parameter.
var benchmarkFuncRe = regexp.MustCompile(`^Benchmark([^a-z]|$)`)

// discoverBenchmarks returns every benchmark under root as "<dir>:<func>" for
// Go and "crates/gusset/benches/<file>" for Rust, with the path it was found
// in. It parses *_test.go files, so a benchmark behind a build tag is still
// seen, and skips directories no source lives in.
func discoverBenchmarks(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			switch name := d.Name(); {
			case rel == ".":
			case strings.HasPrefix(name, "."), name == "target", name == "node_modules", name == "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		slash := filepath.ToSlash(rel)
		if m, _ := filepath.Match("crates/gusset/benches/*.rs", slash); m {
			found[slash] = slash
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("%s: %v", slash, perr)
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !benchmarkFuncRe.MatchString(fn.Name.Name) || !takesTestingB(fn) {
				continue
			}
			found[dir+":"+fn.Name.Name] = slash
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// takesTestingB reports whether fn has the one parameter *testing.B.
func takesTestingB(fn *ast.FuncDecl) bool {
	params := fn.Type.Params
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) > 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && sel.Sel.Name == "B"
}

// benchmarkListViolations is the rule: every found benchmark is on exactly one
// list, every listed name was found, and every manual-only entry says why.
func benchmarkListViolations(found map[string]string, gated []string, manual map[string]string) []string {
	var out []string
	onGated := map[string]bool{}
	for _, n := range gated {
		if onGated[n] {
			out = append(out, n+": listed twice in gatedBenchmarks")
		}
		onGated[n] = true
	}
	for name, where := range found {
		_, isManual := manual[name]
		switch {
		case onGated[name] && isManual:
			out = append(out, name+": on both gatedBenchmarks and manualOnlyBenchmarks; pick one")
		case !onGated[name] && !isManual:
			out = append(out, name+" ("+where+"): on neither list. Add it to gatedBenchmarks in tests/ci/benchmark_lists_test.go and to the bench job, or to manualOnlyBenchmarks with the reason it is not compared")
		}
	}
	for n := range onGated {
		if _, ok := found[n]; !ok {
			out = append(out, n+": in gatedBenchmarks but no such benchmark exists; remove or rename it")
		}
	}
	for n, why := range manual {
		if _, ok := found[n]; !ok {
			out = append(out, n+": in manualOnlyBenchmarks but no such benchmark exists; remove or rename it")
		}
		if strings.TrimSpace(why) == "" {
			out = append(out, n+": manual-only with no reason")
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryBenchmarkIsGatedOrManualOnly(t *testing.T) {
	found := discoverBenchmarks(t, "../..")
	if len(found) < 30 {
		t.Fatalf("discovered only %d benchmarks; the walk is broken, not the repository", len(found))
	}
	for _, v := range benchmarkListViolations(found, gatedBenchmarks, manualOnlyBenchmarks) {
		t.Error(v)
	}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A benchmark nobody listed must fail, and listing it must make it pass. The
// fixture also holds what must not count: a helper with the wrong signature, a
// lower-case benchmark-ish name, and a method.
func TestPlantedUnlistedBenchmarkIsCaught(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "pkg", "planted_test.go"), `package pkg

import "testing"

func BenchmarkPlanted(b *testing.B)       {}
func BenchmarkListed(b *testing.B)        {}
func Benchmarkhelper(b *testing.B)        {}
func BenchmarkNotOne(b *testing.B, n int) {}
func benchmarkInner(b *testing.B)         {}

type T struct{}

func (T) BenchmarkMethod(b *testing.B) {}
`)
	writeFixture(t, filepath.Join(root, "crates", "gusset", "benches", "newbench.rs"), "fn main() {}\n")
	writeFixture(t, filepath.Join(root, "crates", "gusset", "benches", "common", "mod.rs"), "pub fn x() {}\n")

	found := discoverBenchmarks(t, root)
	want := []string{"crates/gusset/benches/newbench.rs", "pkg:BenchmarkListed", "pkg:BenchmarkPlanted"}
	var got []string
	for n := range found {
		got = append(got, n)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("discovered %v, want %v", got, want)
	}

	gated := []string{"pkg:BenchmarkListed"}
	manual := map[string]string{}
	v := benchmarkListViolations(found, gated, manual)
	if len(v) != 2 || !strings.Contains(strings.Join(v, "\n"), "pkg:BenchmarkPlanted") ||
		!strings.Contains(strings.Join(v, "\n"), "crates/gusset/benches/newbench.rs") {
		t.Fatalf("unlisted benchmarks not both reported: %q", v)
	}

	manual["pkg:BenchmarkPlanted"] = "not worth gating"
	manual["crates/gusset/benches/newbench.rs"] = "cargo bench"
	if v := benchmarkListViolations(found, gated, manual); len(v) != 0 {
		t.Fatalf("listed benchmarks still fail: %q", v)
	}

	// The other failure shapes: a stale entry, a reasonless manual entry, a
	// name on both lists.
	manual["pkg:BenchmarkGone"] = "was renamed"
	manual["pkg:BenchmarkPlanted"] = " "
	v = benchmarkListViolations(found, append(gated, "pkg:BenchmarkPlanted", "pkg:BenchmarkGone2"), manual)
	text := strings.Join(v, "\n")
	for _, want := range []string{
		"pkg:BenchmarkGone: in manualOnlyBenchmarks but no such benchmark exists",
		"pkg:BenchmarkGone2: in gatedBenchmarks but no such benchmark exists",
		"pkg:BenchmarkPlanted: manual-only with no reason",
		"pkg:BenchmarkPlanted: on both",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing violation %q in:\n%s", want, text)
		}
	}
}

// jobEnvPattern returns the names inside the bench job's NAME: '^(A|B)$' env
// value.
func jobEnvPattern(t *testing.T, matrix, name string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s+` + name + `: '\^\(([A-Za-z|]+)\)\$'\s*$`).FindStringSubmatch(matrix)
	if m == nil {
		t.Fatalf("matrix.yml has no %s: '^(A|B)$' in the bench job", name)
	}
	return strings.Split(m[1], "|")
}

// Listing a benchmark as gated is only true if the job runs it, in both the
// base and the head binary, from the package the list names.
func TestGatedBenchmarksAreWhatTheJobRuns(t *testing.T) {
	var bench string
	for _, j := range splitJobs(workflows(t)["matrix.yml"]) {
		if strings.HasPrefix(strings.TrimSpace(j), "bench:") {
			bench = j
		}
	}
	if bench == "" {
		t.Fatal("matrix.yml has no bench job")
	}

	run := map[string][]string{
		".":                {},
		"internal/isolate": {},
	}
	run["."] = append(run["."], jobEnvPattern(t, bench, "ROOT_BENCH")...)
	run["internal/isolate"] = append(run["internal/isolate"], jobEnvPattern(t, bench, "ISOLATE_BENCH")...)

	gated := map[string]bool{}
	for _, g := range gatedBenchmarks {
		gated[g] = true
		dir, fn, _ := strings.Cut(g, ":")
		if dir == "bench" {
			continue
		}
		names, ok := run[dir]
		if !ok {
			t.Errorf("%s is gated but the bench job runs nothing from %s", g, dir)
			continue
		}
		if !contains(names, fn) {
			t.Errorf("%s is on gatedBenchmarks but the job's pattern for %s runs only %v", g, dir, names)
		}
	}
	for dir, names := range run {
		for _, fn := range names {
			if !gated[dir+":"+fn] {
				t.Errorf("the bench job runs %s from %s but gatedBenchmarks does not list it", fn, dir)
			}
		}
	}

	// Both sides of the comparison must build and run all three packages.
	for _, want := range []string{
		`go test -c -o "$RUNNER_TEMP/new.test" ./bench`,
		`go test -c -o "$RUNNER_TEMP/new-root.test" .`,
		`go test -c -o "$RUNNER_TEMP/new-isolate.test" ./internal/isolate`,
		`go test -c -o "$RUNNER_TEMP/old.test" ./bench`,
		`go test -c -o "$RUNNER_TEMP/old-root.test" .`,
		`go test -c -o "$RUNNER_TEMP/old-isolate.test" ./internal/isolate`,
		`bench_side old "$RUNNER_TEMP/base" old.txt`,
		`bench_side new "$PWD" new.txt`,
		`-test.bench .)`,
		`-test.bench "$ROOT_BENCH"`,
		`-test.bench "$ISOLATE_BENCH"`,
		`benchgate regress -threshold 10 -min-samples 10 delta.csv`,
	} {
		if !strings.Contains(bench, want) {
			t.Errorf("the bench job lost %q: a package would be compared on one side only or not at all", want)
		}
	}
	if m := regexp.MustCompile(`for _ in \$\(seq (\d+)\)`).FindStringSubmatch(bench); m == nil || m[1] != "10" {
		t.Errorf("the bench job's sample loop is not seq 10, which -min-samples 10 assumes: %v", m)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
