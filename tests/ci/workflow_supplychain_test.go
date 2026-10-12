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

// buildFiles are the files whose cargo and tool invocations CI trusts.
var buildFiles = []string{"../../Makefile", "../../tools/ci/lint.sh"}

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
	cargoSubcmd = regexp.MustCompile(`\bcargo(?: \+\S+| \+\$\{\{[^}]*\}\})? (build|test|clippy|check)\b`)
	cargoRun    = regexp.MustCompile(`\bcargo(?: \+\S+| \+\$\{\{[^}]*\}\})? install\b`)
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
	for i, l := range lines {
		if isComment(l) || strings.Contains(l, "-Zbuild-std") || strings.Contains(l, "cargo miri") || strings.Contains(l, "echo ") {
			continue
		}
		if cargoSubcmd.MatchString(l) && !strings.Contains(l, "--locked") {
			out = append(out, name+":"+strconv.Itoa(i+1)+": cargo without --locked: "+strings.TrimSpace(l))
		}
	}
	return out
}

// toolInstallViolations reports an unpinned tool install: @latest, or a cargo
// install that is not both --locked and --version.
func toolInstallViolations(name string, lines []string) []string {
	var out []string
	for i, l := range lines {
		if isComment(l) || strings.Contains(l, "echo ") {
			continue
		}
		at := name + ":" + strconv.Itoa(i+1) + ": "
		if strings.Contains(l, "@latest") {
			out = append(out, at+"@latest: "+strings.TrimSpace(l))
		}
		if cargoRun.MatchString(l) && (!strings.Contains(l, "--locked") || !strings.Contains(l, "--version")) {
			out = append(out, at+"cargo install needs --locked and --version: "+strings.TrimSpace(l))
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
		if isComment(l) || !untrusted.MatchString(l) || envAssign.MatchString(l) {
			continue
		}
		// `if:` and `with:` expressions are evaluated by the runner, not a shell.
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "if:") || strings.HasPrefix(t, "description:") {
			continue
		}
		out = append(out, name+":"+strconv.Itoa(i+1)+": untrusted expression outside env: "+strings.TrimSpace(l))
	}
	return out
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
		{"injection through run", untrustedExpressionViolations("x", []string{"        run: echo ${{ inputs.seed }}"}), untrustedExpressionViolations("x", []string{"          SEED: ${{ inputs.seed }}"})},
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
