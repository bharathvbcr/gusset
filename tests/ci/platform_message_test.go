package ci_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// platformRefusalPhrase is what the `cross` job greps for in the output of a
// build on an unsupported target. It has to be a substring of the
// compile_error! text in crates/gusset/src/lib.rs, or the job goes red on the
// next target it refuses (or, worse, is edited to match something else).
const platformRefusalPhrase = "gusset supports Linux and macOS only"

var compileErrorLiteral = regexp.MustCompile(`(?s)compile_error!\(\s*"((?:[^"\\]|\\.)*)"\s*\)`)

// platformGateProblems reads the compile_error! text out of a lib.rs source and
// reports what is wrong with it as a statement of the supported set. A Rust
// string literal's `\`-newline continuation drops the newline and the next
// line's leading whitespace, so it is applied before matching.
func platformGateProblems(src string) []string {
	m := compileErrorLiteral.FindStringSubmatch(src)
	if m == nil {
		return []string{"lib.rs has no compile_error!(\"...\") platform gate"}
	}
	msg := regexp.MustCompile(`\\\n\s*`).ReplaceAllString(m[1], "")
	var problems []string
	if !strings.Contains(msg, platformRefusalPhrase) {
		problems = append(problems, "the message does not contain "+`"`+platformRefusalPhrase+`"`+", the phrase the cross job greps for")
	}
	for _, name := range []string{"Linux", "macOS"} {
		if !strings.Contains(msg, name) {
			problems = append(problems, "the message does not name "+name+" as supported")
		}
	}
	if strings.Contains(strings.ToLower(msg), "unix targets") {
		problems = append(problems, `the message says "unix targets": FreeBSD, OpenBSD and illumos are unix and are refused`)
	}
	return problems
}

func TestPlatformGateMessageNamesTheSupportedSetNotAllOfUnix(t *testing.T) {
	b, err := os.ReadFile("../../crates/gusset/src/lib.rs")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range platformGateProblems(string(b)) {
		t.Error(p)
	}
}

func TestPlatformGateMessageRuleOnFixtures(t *testing.T) {
	const stale = "#[cfg(not(any(target_os = \"linux\", target_os = \"macos\")))]\n" +
		"compile_error!(\n" +
		"    \"gusset supports unix targets only (linux-gnu, linux-musl, macos). The completion \\\n" +
		"     pipe, sigaltstack and pthread stack accounting have no Windows implementation; \\\n" +
		"     see docs/platforms.md.\"\n" +
		");\n"
	got := strings.Join(platformGateProblems(stale), "\n")
	for _, want := range []string{"the phrase the cross job greps for", `says "unix targets"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("the stale unix-targets message was not reported for %q: %s", want, got)
		}
	}
	const good = "compile_error!(\n" +
		"    \"gusset supports Linux and macOS only (linux-gnu, linux-musl, macos); other \\\n" +
		"     targets are refused.\"\n" +
		");\n"
	if got := platformGateProblems(good); len(got) != 0 {
		t.Fatalf("a message naming Linux and macOS was refused: %v", got)
	}
	if got := platformGateProblems("pub mod pool;"); len(got) != 1 {
		t.Fatalf("a lib.rs with no gate must be reported, got %v", got)
	}
}
