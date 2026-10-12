package ci_test

import (
	"os"
	"regexp"
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
