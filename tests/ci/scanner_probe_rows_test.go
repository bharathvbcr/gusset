package ci_test

import (
	"strings"
	"testing"
)

// Permanent planted rows for every detection the reviewers' probes found a
// scanner could lose when the shared readers (logicalLines, commandSegments,
// blankQuotes, stripComment) were changed. Each row is a case that an earlier
// reader flagged and that must stay flagged. They sit in one table, grouped by
// the rule they exercise, so a change to a shared reader fails here by name
// instead of silently dropping a detection. A row's want is the exact count for
// the audit and ASan rules (which count things) and "at least one violation"
// for the rest.
//
// Not rows, by decision: a single `&` between two bench runs, `bash -c "...
// unskipped && ...skipped"` for the bench rule, and a bench line after an
// unbalanced `don't` (gaps that were already there), and `cargo test --test a
// # --test c` for the ASan rule (the comment no longer counts, on purpose).

type probeRow struct {
	rule, name string
	want       int // exact count when exact is set, else a floor of 1
	exact      bool
	got        func() int
}

func probeRows() []probeRow {
	var rows []probeRow
	step := func(body ...string) []string { return runStep("", body...) }
	tee := func(name string, body ...string) {
		rows = append(rows, probeRow{"tee", name, 1, false, func() int { return len(teeWithoutPipefailViolations("x", step(body...))) }})
	}
	lk := func(name string, lines []string) {
		rows = append(rows, probeRow{"locked", name, 1, false, func() int { return len(lockedViolations("x.yml", lines)) }})
	}
	ti := func(name string, lines []string) {
		rows = append(rows, probeRow{"toolinstall", name, 1, false, func() int { return len(toolInstallViolations("x.yml", lines)) }})
	}
	au := func(name string, want int, lines []string) {
		rows = append(rows, probeRow{"audit-invocations", name, want, true, func() int {
			n := 0
			for _, ll := range logicalLines(lines) {
				n += len(scriptInvocations(ll.text))
			}
			return n
		}})
	}
	wb := func(name string, lines []string) {
		rows = append(rows, probeRow{"wholebench", name, 1, false, func() int {
			return len(wholePackageBenchViolations("x", lines, []string{"BenchmarkHandleChurn"}))
		}})
	}
	rf := func(name string, lines []string) {
		rows = append(rows, probeRow{"rustupfetch", name, 1, false, func() int { return len(rustupFetchViolations("x", lines)) }})
	}
	uf := func(name, file, text string) {
		rows = append(rows, probeRow{"uniffi", name, 1, false, func() int { return len(unpinnedUniffiBuilds(file, text)) }})
	}
	im := func(name string, lines []string) {
		rows = append(rows, probeRow{"images", name, 1, false, func() int {
			n := 0
			for _, u := range commandImages(lines) {
				if !strings.Contains(u.ref, "@sha256:") {
					n++
				}
			}
			return n
		}})
	}
	as := func(name string, want int, body ...string) {
		rows = append(rows, probeRow{"asan", name, want, true, func() int {
			return len(asanRunBinaries(append([]string{"jobs:", "  sanitizer:", "    steps:"}, step(body...)...)))
		}})
	}

	// tee
	tee("uncalled one-line function sets pipefail", "f() { set -o pipefail; }", "go test | tee out")
	tee("escaped apostrophe outside quotes", `echo it\'s; go test | tee out`)
	tee("escaped double quotes outside quotes", `echo \"x\"; go test | tee out`)
	tee("echo with escaped double quotes", `echo "say \"hi\" now" | tee out`)
	tee("hash in quotes", `echo "a # b"; go test | tee out`)
	tee("url fragment", `curl https://x#y | tee out`)
	tee("$# before the pipe", `echo $#; go test | tee out`)
	tee("${#a} before the pipe", `n=${#a}; go test | tee out`)
	tee("tee inside $( )", `x=$(go test | tee out)`)
	tee("pipefail set in a subshell, tee outside", "( set -o pipefail ); go test | tee out")
	tee("backslash in single quotes", `echo 'a\'; go test | tee out`)
	tee("escaped backslash in double quotes", `echo "a\\"; go test | tee out`)
	tee("the '\\'' idiom", `echo 'it'\''s'; go test | tee out`)
	tee("printf with a single-quoted backslash", `printf 'a\' ; go test | tee out`)
	tee("trailing backslash at the end of the body", `go test | tee out \`)
	tee("echo with an escaped quote before the pipe", `echo "x\"" | tee out`)
	tee("quoted << is not a heredoc", `echo "use cat <<EOF"`, "go test | tee out")
	tee("arithmetic << is not a heredoc", "x=$((x<<y))", "go test | tee out")
	tee("set -- -o pipefail sets positional parameters", "set -- -o pipefail", "go test | tee out")

	// locked
	lk("run | then cargo build", step("cargo build"))
	lk("url fragment before cargo", step("curl https://x#y && cargo build"))
	lk("$# before cargo", step("echo $# ; cargo build"))
	lk("${#a} before cargo", step("n=${#a}; cargo build"))
	lk("quoted hash before cargo", step(`echo "a # b"; cargo build`))
	lk("escaped quote in echo before cargo", step(`echo "a\" # b"; cargo build`))
	lk("&& continuation", step("cargo build &&", "cargo test --locked"))
	lk("comment line first in the body", step("# note", "cargo build"))
	lk("a && continued if: then run", []string{"      - name: s", "        if: a &&", "          b", "        run: cargo build"})
	lk("a description | block then run", []string{"    inputs:", "      x:", "        description: |", "          words", "      - run: cargo build"})
	lk("unbalanced apostrophe then hash", step(`echo don't # x; cargo build`))
	lk("Makefile $$# then cargo", []string{"\t@n=$$#; cargo build"})
	lk("pipe continuation into cargo", step("echo x |", "cargo build"))
	lk("trailing comment hides --locked", step("cargo build # --locked"))
	lk("bash -c with cargo && cargo", step(`bash -c "cargo build --locked && cargo test"`))
	lk("sh -c with cargo ; cargo", step(`sh -c 'cargo test --locked; cargo build'`))
	lk("docker run sh -c with cargo && cargo", step(`docker run --rm img sh -c "cargo build --locked && cargo clippy"`))

	// tool install
	ti("@latest", step("go install x@latest"))
	ti("a chained cargo install", step("cargo install a --locked --version 1 &&", "cargo install b"))
	ti("@latest after $# &&", step("echo $# && go install x@latest"))
	ti("sh -c with a chained cargo install", step(`sh -c 'cargo install a --locked --version 1 && cargo install b'`))

	// script audit: every script on a line, counted
	au("two scripts joined by &&", 2, step("./tools/ci/lint.sh &&", "./tests/other.sh"))
	au("two scripts on separate lines", 2, step("./tools/ci/lint.sh", "./tests/other.sh"))
	au("two scripts joined by |", 2, step("./tools/ci/lint.sh |", "./tests/other.sh"))
	au("the same script twice on one line", 2, step("./tools/ci/lint.sh && ./tools/ci/lint.sh"))
	au("three scripts joined by && and |", 3, step("./tools/ci/lint.sh && ./tests/other.sh | bench/x.sh"))

	// whole-package bench
	wb("a second run without the skip, joined by &&", step(
		`go test -run '^$' -bench . -skip '^(BenchmarkHandleChurn)$' ./bench/... &&`,
		`go test -run '^$' -bench . ./bench/...`))
	wb("a second run without the skip, on its own line", step(
		`go test -run '^$' -bench . -skip '^(BenchmarkHandleChurn)$' ./bench/...`,
		`go test -run '^$' -bench . ./bench/...`))
	wb("a plain run", step(`go test -run '^$' -bench . ./bench/...`))
	wb("-bench=. with no skip", step(`go test -bench=. ./bench/...`))
	wb("skip only in a trailing comment", step(`go test -run '^$' -bench . ./bench/... # -skip BenchmarkHandleChurn`))
	wb("an unskipped run in $( ) before a skipped one", step(`x=$(go test -bench . ./bench/...) && go test -bench . -skip '^(BenchmarkHandleChurn)$' ./bench/...`))
	wb("a skipped run in $( ) before an unskipped one", step(`x=$(go test -bench . -skip '^(BenchmarkHandleChurn)$' ./bench/...) && go test -bench . ./bench/...`))
	wb("|| join", step(`go test -bench . ./bench/... || go test -bench . -skip '^(BenchmarkHandleChurn)$' ./bench/...`))
	wb("; join", step(`go test -bench . ./bench/... ; go test -bench . -skip '^(BenchmarkHandleChurn)$' ./bench/...`))
	wb("pipe join", step(`go test -bench . ./bench/... | tee a ; go test -bench . -skip '^(BenchmarkHandleChurn)$' ./bench/...`))
	wb("subshell shape, second side unskipped", step(`(cd "$dir/bench" && "$RUNNER_TEMP/$tag.test" -test.bench . -test.skip '^(BenchmarkHandleChurn)$') && (cd "$dir/x" && "$RUNNER_TEMP/y.test" -test.bench .)`))

	// rustup fetch
	rf("curl rustup.rs", step("curl https://sh.rustup.rs | sh"))
	rf("a chained apk add rustup", step("apk add git &&", "apk add rustup"))
	rf("apk add rustup inside sh -c", step(`docker run alpine sh -c 'apk add git && apk add rustup'`))

	// uniffi
	uf("cd && newline cargo", "x.yml", strings.Join(step("cd examples/uniffi_bindgen_go/rust &&", "cargo build"), "\n"))
	uf("manifest with the flag only in a comment", "x.yml", strings.Join(step("cargo build --manifest-path examples/uniffi_bindgen_go/rust/Cargo.toml # --target-dir t"), "\n"))
	uf("Makefile manifest", "Makefile", "x:\n\tcargo build --manifest-path examples/uniffi_bindgen_go/rust/Cargo.toml\n")
	uf("Makefile manifest after $$#", "Makefile", "x:\n\techo $$#; cargo build --manifest-path examples/uniffi_bindgen_go/rust/Cargo.toml\n")

	// images
	im("docker run unpinned", step("docker run alpine:3"))
	im("a pinned pull, then an unpinned run", step("docker pull a@sha256:0000000000000000000000000000000000000000000000000000000000000000 &&", "docker run alpine:3"))
	im("an unpinned run after a url fragment", step("curl https://x#y; docker run alpine:3"))

	// asan binaries
	as("two tests joined by &&", 2, "cargo test --test a &&", "cargo test --test b")
	return rows
}

func TestEveryProbedDetectionStaysDetected(t *testing.T) {
	rows := probeRows()
	if len(rows) < 65 {
		t.Fatalf("only %d probe rows; the table was truncated", len(rows))
	}
	for _, r := range rows {
		got := r.got()
		switch {
		case r.exact && got != r.want:
			t.Errorf("%s: %s: counted %d, want exactly %d", r.rule, r.name, got, r.want)
		case !r.exact && got < 1:
			t.Errorf("%s: %s: no longer flagged", r.rule, r.name)
		}
	}
}

// Compliant steps a scanner flags anyway. They pin the CURRENT over-flagging,
// like TestTeeRuleKnownOverFlagging: a better scanner may stop flagging them
// and should then change this table; nothing here is a requirement.
func TestKnownOverFlaggingOfOtherRules(t *testing.T) {
	// A `;` inside a quoted string splits the command for the cargo rules, which
	// are deliberately not quote-aware (see commandSegments): the cargo in the
	// string is read as a command without --locked.
	if v := lockedViolations("x.yml", runStep("", `printf 'cargo build; ' ; cargo build --locked`)); len(v) == 0 {
		t.Log("a quoted `cargo build;` is no longer read as a command; drop this row")
	}
}
