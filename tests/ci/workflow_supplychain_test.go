package ci_test

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Supply-chain and gate-shape rules beyond workflow_hardening_test.go. Each
// rule is a pure function over lines, so the same code runs over the real
// files and over a planted violation: a rule that cannot fail is not a rule,
// and TestPlanted* proves each one can.

// buildFiles are the files whose cargo and tool invocations CI trusts: the
// Makefile and every script a workflow runs (TestWorkflowScriptsAreAudited
// fails when a workflow runs one that is not listed here or exempted below).
//
// tests/adopter/walkthrough.sh is exempt: it generates a throwaway adopter
// crate that has no Cargo.lock by design, so --locked would always fail there.
var scriptExempt = map[string]string{
	"tests/adopter/walkthrough.sh": "builds a generated adopter crate with no lockfile",
}
var buildFiles = []string{"../../Makefile", "../../tools/ci/lint.sh", "../../bench/r14/run.sh"}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")
}

func isComment(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }

var (
	cargoSubcmd = regexp.MustCompile(`\bcargo(?: \+\S+| \+\$\{\{[^}]*\}\}| \$\{[^}]*\})? (build|test|clippy|check|run|bench|doc|metadata)\b`)
	cargoRun    = regexp.MustCompile(`\bcargo(?: \+\S+| \+\$\{\{[^}]*\}\}| \$\{[^}]*\})? install\b`)
	envAssign   = regexp.MustCompile(`^\s+[A-Za-z_][A-Za-z0-9_]*:\s*\$\{\{`)
	untrusted   = regexp.MustCompile(`\$\{\{\s*(inputs\.|github\.event\.|github\.head_ref)`)
)

// lockedViolations reports cargo build/test/clippy/check invocations that do
// not pass --locked, so a stale or missing Cargo.lock fails the job instead of
// resolving newer dependencies than the ones that were reviewed.
//
// Two exemptions, both because the flag is not verified to compose: -Zbuild-std
// (the std workspace carries its own lockfile) and `cargo miri` (it drives its
// own sysroot build). Echo lines print a command and do not run it.
func lockedViolations(name string, lines []string) []string {
	var out []string
	for _, ll := range logicalLines(lines) {
		for _, seg := range commandSegments(ll.text) {
			if strings.Contains(seg, "-Zbuild-std") || strings.Contains(seg, "cargo miri") {
				continue
			}
			if cargoSubcmd.MatchString(seg) && !strings.Contains(seg, "--locked") {
				out = append(out, name+":"+strconv.Itoa(ll.n)+": cargo without --locked: "+strings.TrimSpace(seg))
			}
		}
	}
	return out
}

type logicalLine struct {
	n    int // 1-based number of its first physical line
	text string
}

var echoString = regexp.MustCompile(`\becho\s+("[^"]*"|'[^']*')`)

// logicalLines joins backslash continuations, drops comments, and blanks the
// string an echo prints: that text is not a command, but a command after it on
// the same line (`echo "cargo check"; cargo check`) still is, and `&& echo ok`
// no longer hides the command before it.
func logicalLines(lines []string) []logicalLine {
	var out []logicalLine
	for i := 0; i < len(lines); i++ {
		if isComment(lines[i]) {
			continue
		}
		start, text := i+1, lines[i]
		for strings.HasSuffix(strings.TrimRight(text, " \t"), `\`) && i+1 < len(lines) {
			text = strings.TrimSuffix(strings.TrimRight(text, " \t"), `\`) + " " + strings.TrimSpace(lines[i+1])
			i++
		}
		out = append(out, logicalLine{start, echoString.ReplaceAllString(text, "echo")})
	}
	return out
}

var commandSplit = regexp.MustCompile(`;|&&|\|\||\||\)`)

// commandSegments splits a logical line at shell command separators, so
// --locked has to appear in the same command as the cargo it governs.
func commandSegments(s string) []string { return commandSplit.Split(s, -1) }

// toolInstallViolations reports an unpinned tool install: @latest, or a cargo
// install that is not both --locked and --version.
func toolInstallViolations(name string, lines []string) []string {
	var out []string
	for _, ll := range logicalLines(lines) {
		at := name + ":" + strconv.Itoa(ll.n) + ": "
		if strings.Contains(ll.text, "@latest") {
			out = append(out, at+"@latest: "+strings.TrimSpace(ll.text))
		}
		for _, seg := range commandSegments(ll.text) {
			if cargoRun.MatchString(seg) && (!strings.Contains(seg, "--locked") || !strings.Contains(seg, "--version")) {
				out = append(out, at+"cargo install needs --locked and --version: "+strings.TrimSpace(seg))
			}
		}
	}
	return out
}

// topLevelPermissionViolations requires a workflow-level `permissions:` block
// that grants contents: read and nothing else. Without it a job inherits the
// repository default, which can be write.
func topLevelPermissionViolations(name string, lines []string) []string {
	for i, l := range lines {
		if strings.TrimRight(l, " ") == "jobs:" {
			break
		}
		if strings.TrimRight(l, " ") != "permissions:" {
			continue
		}
		var grants []string
		for _, g := range lines[i+1:] {
			if strings.TrimSpace(g) == "" || isComment(g) {
				continue
			}
			if !strings.HasPrefix(g, "  ") {
				break
			}
			grants = append(grants, strings.TrimSpace(g))
		}
		if len(grants) == 1 && grants[0] == "contents: read" {
			return nil
		}
		return []string{name + ": top-level permissions must be exactly `contents: read`, got " + strings.Join(grants, ", ")}
	}
	return []string{name + ": no top-level `permissions:` block; jobs inherit the repository default token"}
}

// untrustedExpressionViolations reports ${{ inputs.* / github.event.* /
// github.head_ref }} outside an env assignment. Interpolated into a `run:`
// script it is shell injection; passed through env it is data.
func untrustedExpressionViolations(name string, lines []string) []string {
	var out []string
	for i, l := range lines {
		if isComment(l) || !untrusted.MatchString(l) {
			continue
		}
		t := strings.TrimSpace(l)
		// `if:` is evaluated by the runner, not a shell.
		if strings.HasPrefix(t, "if:") {
			continue
		}
		// Exempt only a `KEY: ${{ ... }}` mapping entry whose enclosing block is
		// `env:`. `run:`, `script:` and `with:` inputs are script-like or reach
		// a script, so they are never exempt even though they have the same
		// `key: ${{` shape.
		if envAssign.MatchString(l) && enclosingKey(lines, i) == "env:" {
			continue
		}
		out = append(out, name+":"+strconv.Itoa(i+1)+": untrusted expression outside an env: block: "+t)
	}
	return out
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// enclosingKey is the nearest earlier line indented less than line i, trimmed.
func enclosingKey(lines []string, i int) string {
	n := indentOf(lines[i])
	for j := i - 1; j >= 0; j-- {
		if strings.TrimSpace(lines[j]) == "" || isComment(lines[j]) {
			continue
		}
		if indentOf(lines[j]) < n {
			return strings.TrimSpace(lines[j])
		}
	}
	return ""
}

func TestEveryCargoInvocationIsLocked(t *testing.T) {
	for name, lines := range workflows(t) {
		for _, v := range lockedViolations(name, lines) {
			t.Error(v)
		}
	}
	for _, p := range buildFiles {
		for _, v := range lockedViolations(p, readLines(t, p)) {
			t.Error(v)
		}
	}
}

var scriptInvocation = regexp.MustCompile(`(?:^|[\s;&(|])((?:\./)?(?:bench|tests|tools)/[\w./-]+\.sh)\b`)

// Every shell script a workflow runs is scanned for unlocked cargo, or exempted
// above with a reason; a new script cannot slip past the audit.
func TestWorkflowScriptsAreAudited(t *testing.T) {
	audited := map[string]bool{}
	for _, p := range buildFiles {
		audited[strings.TrimPrefix(p, "../../")] = true
	}
	for name, lines := range workflows(t) {
		for _, ll := range logicalLines(lines) {
			if m := scriptInvocation.FindStringSubmatch(ll.text); m != nil {
				s := strings.TrimPrefix(m[1], "./")
				if _, err := os.Stat("../../" + s); err != nil {
					t.Errorf("%s:%d runs %s, which does not exist", name, ll.n, s)
				} else if !audited[s] && scriptExempt[s] == "" {
					t.Errorf("%s:%d runs %s, which is neither in buildFiles nor exempted", name, ll.n, s)
				}
			}
		}
	}
}

func TestToolInstallsArePinnedEverywhere(t *testing.T) {
	for name, lines := range workflows(t) {
		for _, v := range toolInstallViolations(name, lines) {
			t.Error(v)
		}
	}
	for _, p := range buildFiles {
		for _, v := range toolInstallViolations(p, readLines(t, p)) {
			t.Error(v)
		}
	}
}

func TestEveryWorkflowDeclaresReadOnlyTopLevelPermissions(t *testing.T) {
	for name, lines := range workflows(t) {
		for _, v := range topLevelPermissionViolations(name, lines) {
			t.Error(v)
		}
	}
}

func TestUntrustedExpressionsReachShellOnlyThroughEnv(t *testing.T) {
	for name, lines := range workflows(t) {
		for _, v := range untrustedExpressionViolations(name, lines) {
			t.Error(v)
		}
	}
}

// The issue-writing job holds issues: write, so it must run nothing but the
// pinned github-script action: no checkout (covered elsewhere) and no shell
// that a build job's output could steer.
func TestWriteTokenJobsRunNoShell(t *testing.T) {
	for name, lines := range workflows(t) {
		for _, job := range splitJobs(lines) {
			if strings.Contains(job, ": write") && regexp.MustCompile(`(?m)^\s+(- )?run:`).MatchString(job) {
				t.Errorf("%s %s: a job with a write permission has a run: step", name, strings.TrimSpace(strings.SplitN(job, "\n", 2)[0]))
			}
		}
	}
}

func splitJobs(lines []string) []string {
	var jobs []string
	inJobs := false
	for _, l := range lines {
		if strings.TrimRight(l, " ") == "jobs:" {
			inJobs = true
			continue
		}
		if !inJobs {
			continue
		}
		if jobHeader.MatchString(l) {
			jobs = append(jobs, "")
		}
		if len(jobs) > 0 {
			jobs[len(jobs)-1] += l + "\n"
		}
	}
	return jobs
}

// cargo-fuzz has no --locked flag, so the nightly job proves the committed
// fuzz/Cargo.lock is current before it builds.
func TestFuzzWorkspaceIsLockedAndChecked(t *testing.T) {
	if _, err := os.Stat("../../fuzz/Cargo.lock"); err != nil {
		t.Fatalf("fuzz/Cargo.lock is not committed: %v", err)
	}
	for _, l := range readLines(t, "../../fuzz/.gitignore") {
		if strings.TrimSpace(l) == "Cargo.lock" {
			t.Error("fuzz/.gitignore ignores Cargo.lock")
		}
	}
	nightly := strings.Join(workflows(t)["nightly.yml"], "\n")
	if !strings.Contains(nightly, "cargo metadata --locked --format-version 1 --manifest-path fuzz/Cargo.toml") {
		t.Error("nightly.yml does not check fuzz/Cargo.lock with cargo metadata --locked")
	}
}

// A replay seed a person types into workflow_dispatch must reach the hammer.
func TestNightlyHammerJobsTakeTheReplaySeed(t *testing.T) {
	lines := workflows(t)["nightly.yml"]
	for _, job := range splitJobs(lines) {
		title := strings.TrimSpace(strings.SplitN(job, "\n", 2)[0])
		if title != "hammer:" && title != "hammer-heap:" {
			continue
		}
		if !strings.Contains(job, "GUSSET_HAMMER_SEED: ${{ inputs.hammer_seed }}") {
			t.Errorf("nightly.yml %s does not set GUSSET_HAMMER_SEED from the hammer_seed input", title)
		}
	}
	if !strings.Contains(strings.Join(lines, "\n"), "hammer_seed:") {
		t.Error("nightly.yml declares no hammer_seed dispatch input")
	}
}

// The gates that used to pass without looking. These assert the wiring; the
// parsing itself is tested in tools/ci/benchgate.
func TestGateWiring(t *testing.T) {
	mk := strings.Join(readLines(t, "../../Makefile"), "\n")
	matrix := strings.Join(workflows(t)["matrix.yml"], "\n")

	bounds := regexp.MustCompile(`(?m)^ALLOC_BOUNDS \?=(.*)$`).FindStringSubmatch(mk)
	if bounds == nil {
		t.Fatal("Makefile has no ALLOC_BOUNDS")
	}
	benchSrc, err := os.ReadFile("../../bench/bench_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`-max (\w+)=`).FindAllStringSubmatch(bounds[1], -1) {
		if !strings.Contains(string(benchSrc), "func Benchmark"+m[1]+"(") {
			t.Errorf("ALLOC_BOUNDS names %s, which bench/bench_test.go does not define: the gate would fail forever or, worse, be edited away", m[1])
		}
	}
	if !strings.Contains(matrix, "make alloc-gate ALLOC_FILE=new.txt") {
		t.Error("the bench job does not run the allocation gate")
	}
	if !strings.Contains(matrix, "::error title=Benchmark regression gate did not run") {
		t.Error("the bench job's no-base path no longer raises an error annotation")
	}
	// A comparison that could not run must fail the job unless a person opted
	// out with the dispatch input; the opt-out arrives through env, not inline.
	for _, want := range []string{
		"allow_no_base:",
		"ALLOW_NO_BASE: ${{ inputs.allow_no_base }}",
		`"${ALLOW_NO_BASE:-}" != "true"`,
		`echo "BENCH_NOT_COMPARED=1" >> "$GITHUB_ENV"`,
		"if: (success() || failure()) && env.BENCH_NOT_COMPARED == '1'",
	} {
		if !strings.Contains(matrix, want) {
			t.Errorf("the bench no-base path lost %q: a skipped comparison would read as a pass", want)
		}
	}
	if m := regexp.MustCompile(`(?s)name: Fail when nothing was compared.*?exit 1`).FindString(matrix); m == "" {
		t.Error("the bench job has no step that fails when nothing was compared")
	}
	if !strings.Contains(matrix, "benchgate regress") {
		t.Error("the bench job does not run the regression gate")
	}
	if !regexp.MustCompile(`(?m)^CROSS_TARGETS \?=.*\bx86_64-apple-darwin\b`).MatchString(mk) {
		t.Error("CROSS_TARGETS does not include x86_64-apple-darwin")
	}
	if !strings.Contains(matrix, "tools/ci/lint.sh "+"staticcheck") || !strings.Contains(mk, "tools/ci/lint.sh\n") {
		t.Error("make lint and the CI lint job do not both run tools/ci/lint.sh")
	}
	// Set equality with tools/ci/lint.sh STEPS, not the one substring above.
	// The table cases prove a planted extra step and a dropped if: can fail.
	if probs := lintStepMismatches(lintSteps(t), workflows(t)["matrix.yml"]); len(probs) != 0 {
		for _, p := range probs {
			t.Error(p)
		}
	}
}

// lintSteps is what `lint.sh list` prints, which is STEPS. Parsing the script
// text would miss a list target that drifted from the variable.
func lintSteps(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("sh", "../../tools/ci/lint.sh", "list")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("lint.sh list: %v\n%s", err, out)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		t.Fatal("lint.sh list printed no steps")
	}
	return fields
}

func TestLintStepSetEqualityOnFixtures(t *testing.T) {
	steps := []string{"gofmt", "go-vet"}
	const good = `
jobs:
  lint:
    steps:
      - name: Install the pinned lint tools
        run: tools/ci/lint.sh install
      - name: Gofmt
        if: success() || failure()
        run: tools/ci/lint.sh gofmt
      - name: Go Vet
        if: success() || failure()
        run: tools/ci/lint.sh go-vet
`
	if probs := lintStepMismatches(steps, strings.Split(good, "\n")); len(probs) != 0 {
		t.Fatalf("compliant fixture flagged: %v", probs)
	}
	cases := []struct {
		name  string
		steps []string
		yaml  string
		want  string
	}{
		{
			name:  "extra STEPS token",
			steps: []string{"gofmt", "go-vet", "planted-extra"},
			yaml:  good,
			want:  "planted-extra",
		},
		{
			name:  "dropped if",
			steps: steps,
			yaml: strings.Replace(good,
				"      - name: Gofmt\n        if: success() || failure()\n        run: tools/ci/lint.sh gofmt\n",
				"      - name: Gofmt\n        run: tools/ci/lint.sh gofmt\n", 1),
			want: "gofmt",
		},
		{
			name:  "matrix step not in STEPS",
			steps: steps,
			yaml: good + `
      - name: Planted
        if: success() || failure()
        run: tools/ci/lint.sh planted-step
`,
			want: "planted-step",
		},
		{
			name:  "if is not exactly success() || failure()",
			steps: steps,
			yaml:  strings.Replace(good, "if: success() || failure()", "if: always()", 1),
			want:  "gofmt",
		},
	}
	for _, c := range cases {
		probs := lintStepMismatches(c.steps, strings.Split(c.yaml, "\n"))
		if len(probs) == 0 {
			t.Errorf("%s: the planted mismatch was not caught", c.name)
			continue
		}
		joined := strings.Join(probs, "\n")
		if !strings.Contains(joined, c.want) {
			t.Errorf("%s: problems %q do not mention %q", c.name, joined, c.want)
		}
	}
}

// lintStepMismatches reports set differences between STEPS and the matrix lint
// job, and any lint step whose if: is not exactly success() || failure().
// install and list are not steps; they are the script's other entry points.
func lintStepMismatches(steps, lines []string) []string {
	var job string
	for _, j := range splitJobs(lines) {
		if strings.HasPrefix(strings.TrimSpace(j), "lint:") {
			job = j
			break
		}
	}
	if job == "" {
		return []string{"no lint job"}
	}
	want := map[string]bool{}
	for _, s := range steps {
		want[s] = true
	}
	got := map[string]bool{}
	var problems []string
	for _, st := range workflowSteps(strings.Split(job, "\n")) {
		m := lintRun.FindStringSubmatch(strings.TrimSpace(stepRunBody(st)))
		if m == nil {
			continue
		}
		name := m[1]
		if name == "install" || name == "list" {
			continue
		}
		got[name] = true
		if !want[name] {
			problems = append(problems, "matrix lint step "+name+" is not in STEPS")
		}
		guard, ok := stepIfGuard(st)
		if !ok || guard != "success() || failure()" {
			shown := guard
			if !ok {
				shown = "<missing>"
			}
			problems = append(problems, "lint step "+name+" lacks exactly the success() || failure() guard (got "+shown+")")
		}
	}
	for _, s := range steps {
		if !got[s] {
			problems = append(problems, "STEPS entry "+s+" has no matching matrix step")
		}
	}
	return problems
}

var lintRun = regexp.MustCompile(`^tools/ci/lint\.sh ([A-Za-z0-9-]+)$`)

func stepIfGuard(step []string) (string, bool) {
	for _, l := range step {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "if:") {
			return strings.TrimSpace(strings.TrimPrefix(trim, "if:")), true
		}
	}
	return "", false
}

// independentSteps names, per matrix job, the steps that check something the
// step before them does not, so a red earlier step must not skip them. On
// 320c56c (Matrix 39270656716) all five were skipped after an unrelated test
// failed, and the job read as one failure instead of six unknowns.
var independentSteps = map[string][]string{
	"unit":          {"Go suite under GOEXPERIMENT=simd"},
	"allocator-api": {"Go interop stress under the race detector", "Fallback path on the same compiler (GUSSET_ALLOCATOR_API=0)"},
	"race":          {"Aggressive GC Test"},
	"bench":         {"Allocation gate (ChannelHop 0, cgo path bounded)"},
}

func TestIndependentStepsRunAfterAFailure(t *testing.T) {
	for _, p := range independentStepMismatches(independentSteps, workflows(t)["matrix.yml"]) {
		t.Error(p)
	}
}

func TestIndependentStepsOnFixtures(t *testing.T) {
	want := map[string][]string{"unit": {"Second suite"}}
	const good = `
jobs:
  unit:
    steps:
      - name: First suite
        run: go test ./...
      - name: Second suite
        if: success() || failure()
        run: go test -tags x ./...
`
	if probs := independentStepMismatches(want, strings.Split(good, "\n")); len(probs) != 0 {
		t.Fatalf("compliant fixture flagged: %v", probs)
	}
	for name, yaml := range map[string]string{
		"dropped if":   strings.Replace(good, "        if: success() || failure()\n", "", 1),
		"other guard":  strings.Replace(good, "if: success() || failure()", "if: always()", 1),
		"step renamed": strings.Replace(good, "Second suite", "Renamed suite", 1),
		"job missing":  strings.Replace(good, "  unit:", "  other:", 1),
	} {
		if probs := independentStepMismatches(want, strings.Split(yaml, "\n")); len(probs) == 0 {
			t.Errorf("%s: the planted violation was not caught", name)
		}
	}
}

// independentStepMismatches reports each named step that is missing from its
// job or whose if: is not exactly success() || failure(), the lint steps' guard.
func independentStepMismatches(want map[string][]string, lines []string) []string {
	jobs := map[string]string{}
	for _, j := range splitJobs(lines) {
		key, _, _ := strings.Cut(strings.TrimSpace(j), ":")
		jobs[key] = j
	}
	var problems []string
	for job, names := range want {
		body, ok := jobs[job]
		if !ok {
			problems = append(problems, "matrix has no job "+job)
			continue
		}
		steps := map[string][]string{}
		for _, st := range workflowSteps(strings.Split(body, "\n")) {
			name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(st[0]), "- name:"))
			steps[name] = st
		}
		for _, name := range names {
			st, ok := steps[name]
			if !ok {
				problems = append(problems, job+": no step named "+name)
				continue
			}
			if guard, ok := stepIfGuard(st); !ok || guard != "success() || failure()" {
				problems = append(problems, job+": step "+name+" lacks exactly the success() || failure() guard")
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// The race detector and the GOGC=1 pass both have to see internal/, including
// internal/isolate. The -v log is what lists it; this pins the command lines.
func TestRaceAndGOGCCoverInternal(t *testing.T) {
	matrix := strings.Join(workflows(t)["matrix.yml"], "\n")
	for _, want := range []string{
		"go test -race -v -timeout 20m . ./tests/pitfalls/... ./tests/panic_zoo/... ./internal/...",
		"GOGC=1 go test -v -count=5 -timeout 20m . ./tests/panic_zoo/... ./tests/pitfalls/... ./internal/...",
	} {
		if !strings.Contains(matrix, want) {
			t.Errorf("race job lost %q", want)
		}
	}
}

// Every workflow step whose run body invokes `go test` sets
// GUSSET_REQUIRE_PROBES=1. Cargo-only steps do not. A planted step with neither
// the env key nor the shell assignment must fail this check.
func TestGoTestStepsRequireProbes(t *testing.T) {
	for name, lines := range workflows(t) {
		for _, miss := range goTestStepsMissingProbe(lines) {
			t.Errorf("%s: %s", name, miss)
		}
	}
	const planted = `
jobs:
  sample:
    steps:
      - name: Cargo only
        run: cargo test --locked --workspace
      - name: Go tests
        env:
          GUSSET_REQUIRE_PROBES: '1'
        run: go test ./...
      - name: Missing
        run: go test -count=1 .
`
	miss := goTestStepsMissingProbe(strings.Split(planted, "\n"))
	if len(miss) != 1 || !strings.Contains(miss[0], "Missing") {
		t.Fatalf("planted go test step without the env: got %v", miss)
	}
	ok := strings.Replace(planted, "      - name: Missing\n        run: go test -count=1 .\n",
		"      - name: Missing\n        run: GUSSET_REQUIRE_PROBES=1 go test -count=1 .\n", 1)
	if miss := goTestStepsMissingProbe(strings.Split(ok, "\n")); len(miss) != 0 {
		t.Fatalf("shell assignment was not accepted: %v", miss)
	}
}

// A comment that names the env is not the env. Either form used to satisfy
// stepSetsRequireProbes, so a go test step with no real assignment looked armed.
func TestHostileCommentDoesNotSetProbe(t *testing.T) {
	const planted = `
jobs:
  sample:
    steps:
      - name: Commented env
        run: |
          # GUSSET_REQUIRE_PROBES=1
          go test ./...
      - name: YAML comment env
        # GUSSET_REQUIRE_PROBES: '1'
        run: go test -count=1 .
      - name: Real env
        env:
          GUSSET_REQUIRE_PROBES: '1'
        run: go test ./...
      - name: Real shell
        run: GUSSET_REQUIRE_PROBES=1 go test ./...
      - name: Cargo only
        run: |
          # GUSSET_REQUIRE_PROBES=1
          cargo test --locked --workspace
`
	miss := goTestStepsMissingProbe(strings.Split(planted, "\n"))
	joined := strings.Join(miss, "\n")
	for _, name := range []string{"Commented env", "YAML comment env"} {
		if !strings.Contains(joined, name) {
			t.Errorf("comment form %q was accepted as setting the probe env: %v", name, miss)
		}
	}
	for _, name := range []string{"Real env", "Real shell", "Cargo only"} {
		if strings.Contains(joined, name) {
			t.Errorf("%s was reported missing the probe env: %v", name, miss)
		}
	}
	if len(miss) != 2 {
		t.Fatalf("want exactly the two comment steps, got %v", miss)
	}
}

// The gate models the go test process. YAML env is the step's initial
// exported value. export sets and exports (the last assignment on that
// command wins); a bare assignment changes the shell value and does not
// export; unset clears both. A prefix on go test applies only to that
// process. Every go test in the step has to see exactly 1.
//
// Residuals, not modeled: Export -n, Plus assign, Or skip export, And keeps
// one, YAML comment value, YAML quote space, Unset function, Command unset,
// Builtin unset, Absolute env, Env dash i.
func TestHostileLaunderDoesNotSetProbe(t *testing.T) {
	planted := `
jobs:
  sample:
    steps:
      - name: Echo launder
        run: |
          echo GUSSET_REQUIRE_PROBES=1
          go test ./...
      - name: Other command
        run: |
          GUSSET_REQUIRE_PROBES=1 echo hi
          go test ./...
      - name: Printf launder
        run: |
          printf GUSSET_REQUIRE_PROBES=1
          go test ./...
      - name: YAML single
        env:
          GUSSET_REQUIRE_PROBES: '1'
        run: go test ./...
      - name: YAML double
        env:
          GUSSET_REQUIRE_PROBES: "1"
        run: go test ./...
      - name: YAML bare
        env:
          GUSSET_REQUIRE_PROBES: 1
        run: go test ./...
      - name: Shell on go test
        run: GUSSET_REQUIRE_PROBES=1 go test ./... # trailing
      - name: Export then go
        run: |
          export GUSSET_REQUIRE_PROBES=1
          go test ./...
      - name: Bare assignment
        run: |
          GUSSET_REQUIRE_PROBES=1
          go test ./...
      - name: Unset
        run: |
          export GUSSET_REQUIRE_PROBES=1
          unset GUSSET_REQUIRE_PROBES
          go test ./...
      - name: Bare reassignment
        run: |
          export GUSSET_REQUIRE_PROBES=1
          GUSSET_REQUIRE_PROBES=0
          go test ./...
      - name: Env command
        run: env GUSSET_REQUIRE_PROBES=1 go test ./...
      - name: Value twelve
        env:
          GUSSET_REQUIRE_PROBES: 12
        run: go test ./...
      - name: Value true
        env:
          GUSSET_REQUIRE_PROBES: true
        run: go test ./...
      - name: Shell twelve
        run: GUSSET_REQUIRE_PROBES=12 go test ./...
      - name: Quoted space
        run: GUSSET_REQUIRE_PROBES='1 ' go test ./...
      - name: Other var is one
        run: GOGC=1 go test ./...
      - name: Cargo only
        run: |
          echo GUSSET_REQUIRE_PROBES=1
          cargo test --locked
      - name: Unset dash dash
        run: |
          export GUSSET_REQUIRE_PROBES=1
          unset -- GUSSET_REQUIRE_PROBES
          go test ./...
      - name: Unset quoted
        run: |
          export GUSSET_REQUIRE_PROBES=1
          unset "GUSSET_REQUIRE_PROBES"
          go test ./...
      - name: One line unset
        run: export GUSSET_REQUIRE_PROBES=1; unset GUSSET_REQUIRE_PROBES; go test ./...
      - name: One line and zero
        run: export GUSSET_REQUIRE_PROBES=1 && GUSSET_REQUIRE_PROBES=0 && go test ./...
      - name: And go test
        run: export GUSSET_REQUIRE_PROBES=1 && go test ./...
      - name: Bare spaces
        run: |
            GUSSET_REQUIRE_PROBES=1   
          go test ./...
      - name: Env zero after export
        run: |
          export GUSSET_REQUIRE_PROBES=1
          env GUSSET_REQUIRE_PROBES=0 go test ./...
      - name: YAML then unset
        env:
          GUSSET_REQUIRE_PROBES: '1'
        run: |
          unset GUSSET_REQUIRE_PROBES
          go test ./...
      - name: YAML then prefix zero
        env:
          GUSSET_REQUIRE_PROBES: '1'
        run: GUSSET_REQUIRE_PROBES=0 go test ./...
      - name: YAML then bare zero
        env:
          GUSSET_REQUIRE_PROBES: "1"
        run: |
          GUSSET_REQUIRE_PROBES=0
          go test ./...
      - name: Export last zero
        run: |
          export GUSSET_REQUIRE_PROBES=1 GUSSET_REQUIRE_PROBES=0
          go test ./...
      - name: Export last one
        run: |
          export GUSSET_REQUIRE_PROBES=0 GUSSET_REQUIRE_PROBES=1
          go test ./...
      - name: Second go test
        run: |
          export GUSSET_REQUIRE_PROBES=1
          go test ./a
          unset GUSSET_REQUIRE_PROBES
          go test ./b
      - name: Prefix only first
        run: |
          GUSSET_REQUIRE_PROBES=1 go test ./a
          go test ./b
      - name: Export current
        run: |
          GUSSET_REQUIRE_PROBES=1
          export GUSSET_REQUIRE_PROBES
          go test ./...
`
	// A trailing space is part of the value. TrimSpace would launder it into 1.
	planted += "      - name: Trailing space\n        env:\n          GUSSET_REQUIRE_PROBES: 1 \n        run: go test ./...\n"
	miss := goTestStepsMissingProbe(strings.Split(planted, "\n"))
	joined := strings.Join(miss, "\n")
	for _, name := range []string{
		"Echo launder", "Other command", "Printf launder",
		"Bare assignment", "Bare spaces",
		"Unset", "Bare reassignment",
		"Value twelve", "Value true", "Shell twelve", "Quoted space",
		"Other var is one", "Trailing space",
		"Unset dash dash", "Unset quoted", "One line unset", "One line and zero",
		"Env zero after export",
		"YAML then unset", "YAML then prefix zero", "YAML then bare zero",
		"Export last zero", "Second go test", "Prefix only first",
	} {
		if !strings.Contains(joined, name) {
			t.Errorf("%q was accepted as setting the probe env:\n%s", name, joined)
		}
	}
	for _, name := range []string{
		"YAML single", "YAML double", "YAML bare", "Shell on go test",
		"Export then go", "Env command", "Cargo only",
		"And go test", "Export last one", "Export current",
	} {
		if strings.Contains(joined, name) {
			t.Errorf("%s was reported missing the probe env:\n%s", name, joined)
		}
	}
}

func goTestStepsMissingProbe(lines []string) []string {
	var out []string
	for _, st := range workflowSteps(lines) {
		if !stepInvokesGoTest(st) || stepSetsRequireProbes(st) {
			continue
		}
		out = append(out, "go test step does not set GUSSET_REQUIRE_PROBES=1: "+stepLabel(st))
	}
	return out
}

func stepLabel(step []string) string {
	for _, l := range step {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "name:") || strings.HasPrefix(trim, "- name:") {
			return strings.TrimSpace(trim)
		}
		if strings.HasPrefix(trim, "run:") {
			return trim
		}
	}
	if len(step) == 0 {
		return "<empty>"
	}
	return strings.TrimSpace(step[0])
}

// workflowSteps splits a workflow (or one job) into steps. A `      #` comment
// between steps is not part of either step, so a comment that mentions
// `go test` cannot force a cargo step to carry the probe env.
func workflowSteps(lines []string) [][]string {
	var steps [][]string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			steps = append(steps, cur)
			cur = nil
		}
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "      - "):
			flush()
			cur = []string{l}
		case strings.HasPrefix(l, "      #"):
			flush()
		case cur != nil && (strings.HasPrefix(l, "        ") || strings.TrimSpace(l) == ""):
			cur = append(cur, l)
		default:
			if strings.TrimSpace(l) != "" {
				flush()
			}
		}
	}
	flush()
	return steps
}

func stepRunBody(step []string) string {
	var b strings.Builder
	inRun := false
	for _, l := range step {
		trim := strings.TrimSpace(l)
		if !inRun {
			if !strings.HasPrefix(trim, "run:") {
				continue
			}
			rest := strings.TrimSpace(strings.TrimPrefix(trim, "run:"))
			if rest == "|" || rest == ">" || rest == "|-" || rest == ">-" {
				inRun = true
				continue
			}
			return rest
		}
		if strings.TrimSpace(l) == "" {
			b.WriteByte('\n')
			continue
		}
		if !strings.HasPrefix(l, "          ") {
			break
		}
		b.WriteString(strings.TrimRight(l, " \t"))
		b.WriteByte('\n')
	}
	return b.String()
}

var goTestCmd = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])go test(?:\s|$)`)

func stepInvokesGoTest(step []string) bool {
	for _, line := range strings.Split(stepRunBody(step), "\n") {
		if goTestCmd.MatchString(stripShellComment(line)) {
			return true
		}
	}
	return false
}

func stripShellComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
}

// probeVarState is the shell variable, separate from whether it is exported.
// A child sees it only when it is exported and the value is exactly 1, unless
// that child's own prefix assignment says otherwise.
type probeVarState struct {
	value    string
	present  bool
	exported bool
}

func (s probeVarState) childSeesOne() bool {
	return s.exported && s.present && s.value == "1"
}

// stepSetsRequireProbes is true only when every go test in the step would see
// GUSSET_REQUIRE_PROBES=1. YAML env is the initial exported value, including
// when that value is not 1. The script can unset it or replace it. export
// sets and exports; a bare assignment changes the value only; unset clears
// both. A prefix on the go test command (or env(1)) applies to that process
// and does not change the shell. A comment, an echo, or a prefix on another
// command does not.
func stepSetsRequireProbes(step []string) bool {
	var sh probeVarState
	if val, ok := yamlInitialProbe(step); ok {
		sh.value = val
		sh.present = true
		sh.exported = true
	}
	return everyGoTestSeesProbe(stepRunBody(step), sh)
}

// yamlInitialProbe returns the step env value. The key being present is
// enough; the value does not have to be 1. The last entry wins.
func yamlInitialProbe(step []string) (string, bool) {
	inRun := false
	inEnv := false
	found := false
	val := ""
	for _, l := range step {
		if inRun {
			if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "          ") {
				continue
			}
			inRun = false
		}
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "run:") {
			rest := strings.TrimSpace(strings.TrimPrefix(trim, "run:"))
			inEnv = false
			if rest == "|" || rest == ">" || rest == "|-" || rest == ">-" {
				inRun = true
			}
			continue
		}
		if trim == "env:" {
			inEnv = true
			continue
		}
		if !inEnv || trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		// A key at the same indent as `env:` is the next step field.
		if strings.HasPrefix(l, "        ") && !strings.HasPrefix(l, "          ") {
			inEnv = false
			continue
		}
		if v, ok := yamlProbeValue(l); ok {
			val = v
			found = true
		}
	}
	return val, found
}

// yamlProbeValue returns the env value. One leading separator after the colon
// is syntax; a trailing space is part of the value, so `1 ` is not `1`.
func yamlProbeValue(line string) (string, bool) {
	s := strings.TrimLeft(line, " \t")
	if s == "" || s[0] == '#' {
		return "", false
	}
	const key = "GUSSET_REQUIRE_PROBES:"
	if !strings.HasPrefix(s, key) {
		return "", false
	}
	val := strings.TrimLeft(s[len(key):], " \t")
	if val == "" {
		return "", true
	}
	if val[0] == '\'' || val[0] == '"' {
		q := val[0]
		rest := val[1:]
		end := strings.IndexByte(rest, q)
		if end < 0 {
			return val, true
		}
		if after := rest[end+1:]; after != "" {
			return val, true
		}
		return rest[:end], true
	}
	return val, true
}

func everyGoTestSeesProbe(body string, sh probeVarState) bool {
	saw := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(stripShellComment(line))
		for _, seg := range shellSegments(line) {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			if applyShellProbe(seg, &sh) {
				continue
			}
			runs, explicit, val := probeOnGoTest(seg)
			if !runs {
				continue
			}
			saw = true
			sees := sh.childSeesOne()
			if explicit {
				sees = val == "1"
			}
			if !sees {
				return false
			}
		}
	}
	return saw
}

// applyShellProbe updates the shell variable for export, unset, or a bare
// assignment. A prefix on a command is not a shell assignment.
func applyShellProbe(seg string, sh *probeVarState) bool {
	assigns, argv := parseSimpleCommand(seg)
	if len(argv) == 0 {
		val, ok := assigns["GUSSET_REQUIRE_PROBES"]
		if !ok {
			return false
		}
		sh.value = val
		sh.present = true
		return true
	}
	switch argv[0] {
	case "export":
		return applyExportProbe(argv, sh)
	case "unset":
		return applyUnsetProbe(argv, sh)
	default:
		return false
	}
}

func applyExportProbe(argv []string, sh *probeVarState) bool {
	applied := false
	for _, a := range argv[1:] {
		name, val, ok := splitAssign(a)
		if ok {
			if name != "GUSSET_REQUIRE_PROBES" {
				continue
			}
			sh.value = val
			sh.present = true
			sh.exported = true
			applied = true
			continue
		}
		if a == "GUSSET_REQUIRE_PROBES" {
			sh.exported = true
			applied = true
		}
	}
	return applied
}

func applyUnsetProbe(argv []string, sh *probeVarState) bool {
	applied := false
	for _, a := range argv[1:] {
		if a == "--" {
			continue
		}
		if a == "GUSSET_REQUIRE_PROBES" {
			*sh = probeVarState{}
			applied = true
		}
	}
	return applied
}

// probeOnGoTest reports whether seg runs go test, and the value when this
// command sets GUSSET_REQUIRE_PROBES itself (a prefix, or an env(1) argument).
func probeOnGoTest(seg string) (runs, explicit bool, val string) {
	assigns, argv := parseSimpleCommand(seg)
	if len(argv) > 0 && argv[0] == "env" {
		i := 1
		for i < len(argv) {
			name, v, ok := splitAssign(argv[i])
			if !ok {
				break
			}
			assigns[name] = v
			i++
		}
		argv = argv[i:]
	}
	if len(argv) < 2 || argv[0] != "go" || argv[1] != "test" {
		return false, false, ""
	}
	v, ok := assigns["GUSSET_REQUIRE_PROBES"]
	return true, ok, v
}

func parseSimpleCommand(seg string) (map[string]string, []string) {
	assigns := map[string]string{}
	fields := shellFields(seg)
	i := 0
	for i < len(fields) {
		name, val, ok := splitAssign(fields[i])
		if !ok {
			break
		}
		assigns[name] = val
		i++
	}
	return assigns, fields[i:]
}

func splitAssign(f string) (string, string, bool) {
	i := strings.IndexByte(f, '=')
	if i <= 0 || !isShellIdent(f[:i]) {
		return "", "", false
	}
	return f[:i], f[i+1:], true
}

func isShellIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func shellFields(s string) []string {
	var fields []string
	var b strings.Builder
	var quote byte
	flush := func() {
		if b.Len() == 0 {
			return
		}
		fields = append(fields, b.String())
		b.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			b.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return fields
}

func shellSegments(line string) []string {
	var segs []string
	var b strings.Builder
	var quote byte
	flush := func() {
		segs = append(segs, b.String())
		b.Reset()
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			b.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			b.WriteByte(c)
		case c == ';':
			flush()
		case c == '&' && i+1 < len(line) && line[i+1] == '&':
			flush()
			i++
		case c == '|' && i+1 < len(line) && line[i+1] == '|':
			flush()
			i++
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return segs
}

func TestPlantedViolationsAreCaught(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		good []string
	}{
		{"unlocked cargo", lockedViolations("x", []string{"        run: cargo build --release"}), lockedViolations("x", []string{"        run: cargo build --locked --release"})},
		{"unlocked cargo with toolchain expression", lockedViolations("x", []string{"  cargo +${{ env.RUST_NIGHTLY }} build --release"}), lockedViolations("x", []string{"  cargo +${{ env.RUST_NIGHTLY }} build --locked --release"})},
		{"@latest", toolInstallViolations("x", []string{"go install example.com/t@latest"}), toolInstallViolations("x", []string{"go install example.com/t@v1.2.3"})},
		{"cargo install without version", toolInstallViolations("x", []string{"cargo install cargo-fuzz --locked"}), toolInstallViolations("x", []string{"cargo install cargo-fuzz --locked --version 0.13.2"})},
		{"cargo install without --locked", toolInstallViolations("x", []string{"cargo install cargo-fuzz --version 0.13.2"}), nil},
		{"no top-level permissions", topLevelPermissionViolations("x", []string{"name: x", "on: push", "jobs:", "  a:"}), topLevelPermissionViolations("x", []string{"permissions:", "  contents: read", "", "jobs:"})},
		{"top-level write", topLevelPermissionViolations("x", []string{"permissions:", "  contents: read", "  issues: write", "jobs:"}), nil},
		{"injection through run body", untrustedExpressionViolations("x", []string{"      - name: s", "        run: echo ${{ inputs.seed }}"}), untrustedExpressionViolations("x", []string{"      - name: s", "        env:", "          SEED: ${{ inputs.seed }}", "        run: echo \"$SEED\""})},
		{"run: is the expression", untrustedExpressionViolations("x", []string{"      - name: s", "        run: ${{ inputs.hammer_seed }}"}), nil},
		{"github-script script: is the expression", untrustedExpressionViolations("x", []string{"        with:", "          script: ${{ github.event.issue.title }}"}), nil},
		{"expression in a block-scalar body", untrustedExpressionViolations("x", []string{"        run: |", "          echo ${{ github.head_ref }}"}), nil},
		{"with: input carrying head_ref", untrustedExpressionViolations("x", []string{"        with:", "          ref: ${{ github.head_ref }}"}), nil},
		{"echo hides an unlocked cargo", lockedViolations("x", []string{"        run: cargo test -p gusset && echo ok"}), lockedViolations("x", []string{"        run: cargo test --locked -p gusset && echo ok"})},
		{"unlocked cargo run", lockedViolations("x", []string{"  cargo run --release"}), nil},
		{"unlocked cargo bench", lockedViolations("x", []string{"  cargo bench"}), nil},
		{"unlocked cargo doc", lockedViolations("x", []string{"  cargo doc --no-deps"}), nil},
		{"unlocked in a shell function", lockedViolations("x", []string{`	(cd "$dir" && cargo ${tc:+"+$tc"} build --release -q \`, `		${target:+--target "$target"} "$@")`}), lockedViolations("x", []string{`	(cd "$dir" && cargo ${tc:+"+$tc"} build --release -q \`, `		--locked "$@")`})},
		{"--locked on the continuation line", lockedViolations("x", []string{"        run: cargo build --release \\", "          --target x"}), lockedViolations("x", []string{"        run: cargo build --release \\", "          --locked"})},
		{"locked echo string does not launder a bare cargo", lockedViolations("x", []string{`echo "cargo check --locked"; cargo check -p gusset`}), nil},
	}
	for _, c := range cases {
		if len(c.got) == 0 {
			t.Errorf("%s: the planted violation was not caught", c.name)
		}
		if len(c.good) != 0 {
			t.Errorf("%s: the compliant form was flagged: %v", c.name, c.good)
		}
	}
	if len(lockedViolations("x", []string{"  cargo test -p gusset -Zbuild-std"})) != 0 {
		t.Error("the documented -Zbuild-std exemption stopped working")
	}
}
