#!/bin/sh
# lint.sh — the one lint pipeline. `make lint` and the CI "Lint & Boundary
# Audits" job both run exactly this, so passing it locally means passing there.
#
# usage: tools/ci/lint.sh [step ...]     run the named steps (default: all)
#        tools/ci/lint.sh install        install the pinned Go and Cargo tools
#        tools/ci/lint.sh list           print the step names
#
# Every step runs even when an earlier one failed, because the failure this
# replaces was a broken staticcheck silently skipping govulncheck, the
# forbidden-pattern audit and cargo-deny. The summary names each step as
# PASS, FAIL or SKIPPED, and the exit status is non-zero for anything but PASS.
# A tool that is missing, or not the pinned version, is a FAIL with the command
# that fixes it, never a quiet pass. The only way to skip a step is to say so:
#
#     LINT_SKIP="govulncheck cargo-deny" tools/ci/lint.sh
#
# which is reported as SKIPPED (explicit) and still exits non-zero, so an
# incomplete run cannot be mistaken for a clean one. CI never sets LINT_SKIP.

set -u

here=$(cd "$(dirname "$0")" && pwd)
cd "${LINT_ROOT:-$here/../..}" || exit 1
# shellcheck source=tools/ci/versions.env
. "$here/versions.env"

STEPS="gofmt go-vet gussetvet staticcheck govulncheck forbidden-patterns cargo-clippy cargo-fmt cargo-deny"

gobin=$(go env GOBIN 2>/dev/null)
[ -n "$gobin" ] || gobin="$(go env GOPATH 2>/dev/null)/bin"
PATH="$PATH:$gobin"

# need <tool> <wanted version substring> <install command>
need() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "$1 is not installed; run: $3 (or tools/ci/lint.sh install)" >&2
		return 1
	fi
	have=$("$1" "$4" 2>&1 | head -3 | tr '\n' ' ')
	case $have in
	*"$2"*) return 0 ;;
	esac
	echo "$1 is not the pinned $2 (found: $have); run: $3 (or tools/ci/lint.sh install)" >&2
	return 1
}

step_gofmt() {
	# The files git would commit (tracked plus untracked, minus ignored), not
	# whatever is on disk: `gofmt -l .` also walked target/, where local
	# scratch copies of the tree failed a check about the repository.
	#
	# One path per invocation, NUL from git ls-files through xargs -0. A pipe
	# into `grep -v` made grep's status the step's, so a parse error (stderr
	# only, empty stdout) exited 0. BSD xargs also returns 0 when the utility
	# exits 1, so the result is the log file, not xargs' status. bench/seed
	# stays excluded, matching the old `grep -v '^bench/seed/'`.
	list=$(mktemp) || {
		echo "gofmt: cannot create a temp file; the step did not run" >&2
		return 1
	}
	log=$(mktemp) || {
		rm -f "$list"
		echo "gofmt: cannot create a temp file; the step did not run" >&2
		return 1
	}
	if ! git ls-files -z --cached --others --exclude-standard -- '*.go' >"$list"; then
		rm -f "$list" "$log"
		echo "git ls-files failed; gofmt did not run" >&2
		return 1
	fi
	if [ ! -s "$list" ]; then
		rm -f "$list" "$log"
		return 0
	fi
	# xargs appends each path after the fixed arguments, so $1 is the log
	# and $2 is the file. (The other way round writes gofmt's status into
	# the source and leaves the log empty, which then looks like "did not run".)
	xargs -0 -n 1 sh -c '
		log=$1
		path=$2
		case "$path" in
		bench/seed/*) exit 0 ;;
		esac
		err=$(mktemp) || {
			echo "gofmt: cannot create a temp file; the step did not run" >>"$log"
			exit 1
		}
		# "--" so a tracked file named -ok.go is a path, not a flag.
		out=$(gofmt -l -- "$path" 2>"$err")
		status=$?
		if [ -s "$err" ] || [ "$status" -ne 0 ]; then
			echo "gofmt: $path" >>"$log"
			cat "$err" >>"$log"
			rm -f "$err"
			exit 1
		fi
		rm -f "$err"
		if [ -n "$out" ]; then
			printf "%s\n" "$path" >>"$log"
			exit 1
		fi
	' _ "$log" <"$list"
	xstatus=$?
	rm -f "$list"
	if [ -s "$log" ]; then
		echo "gofmt needed for:"
		cat "$log"
		rm -f "$log"
		return 1
	fi
	rm -f "$log"
	if [ "$xstatus" -ne 0 ]; then
		echo "gofmt did not run (xargs exit $xstatus)" >&2
		return 1
	fi
}

step_go_vet() { go vet ./...; }

# R5 enforcer.
step_gussetvet() { go run ./tools/gussetvet; }

step_staticcheck() {
	need staticcheck "(${STATICCHECK_VERSION#v})" "go install honnef.co/go/tools/cmd/staticcheck@$STATICCHECK_VERSION" -version || return 1
	# The version in the log is what a future "export data" failure is read against.
	staticcheck -version
	staticcheck ./...
}

step_govulncheck() {
	need govulncheck "govulncheck@$GOVULNCHECK_VERSION" "go install golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION" -version || return 1
	govulncheck ./...
}

# Forbidden-pattern audit (R13 and the GODEBUG ban).
#
# Scoped to tracked Go and Rust sources: grepping the whole tree matched the
# rules as written in AGENTS.md and docs/PLAN.md, and failed on every commit. A
# gate that can never pass trains people to ignore a red job.
#
# R4 is deliberately NOT checked here. A blanket `grep -F C.free` cannot tell a
# call from prose about the rule; gussetvet strips comments, carries an
# allowlist for the one package permitted the deliberate violation, and reports
# file:line. Two enforcers for one rule means the cruder one eventually fires on
# the stricter one's source.
step_forbidden_patterns() {
	sources=$(git ls-files '*.go' '*.rs') || {
		echo "git ls-files failed; the audit did not run" >&2
		return 1
	}
	fail=0
	# check <pattern> <message> [code-only]: with code-only, a hit on a line that
	# is a // comment is prose about the rule (gusset.go's package doc names the
	# thing it is not), not a use of it. Never for a pattern that is itself a
	# comment directive, such as //go:linkname.
	check() {
		hits=$(printf '%s\n' "$sources" | xargs grep -nF -- "$1" /dev/null)
		if [ -n "$hits" ] && [ "${3:-}" = code-only ]; then
			hits=$(printf '%s\n' "$hits" | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//')
		fi
		if [ -n "$hits" ]; then
			echo "::error::$2"
			echo "$hits"
			fail=1
		fi
	}
	check '//go:linkname' 'linkname forbidden (R13)'
	check 'asmcgocall' 'asmcgocall forbidden (R13)' code-only
	check 'cgocheck=0' 'GODEBUG cgocheck=0 forbidden'
	check 'invalidptr=0' 'GODEBUG invalidptr=0 forbidden'
	# //nolint is golangci-lint syntax; staticcheck ignores it, so the
	# suppression silently does nothing. Use `//lint:ignore <CHECK> <reason>`.
	check '//nolint' '//nolint is not honored by staticcheck; use //lint:ignore <CHECK> <reason>'
	return $fail
}

# --all-targets: without it clippy skips test targets, and the R3
# disallowed-method rules go unenforced in exactly the code most likely to
# reach for unwrap.
step_cargo_clippy() { cargo clippy --locked --workspace --all-targets -- -D warnings; }

step_cargo_fmt() { cargo fmt --all -- --check; }

# fuzz/ is its own workspace, so the root check never sees its dependencies.
step_cargo_deny() {
	need cargo-deny "cargo-deny $CARGO_DENY_VERSION" "cargo install cargo-deny --locked --version $CARGO_DENY_VERSION" --version || return 1
	cargo deny check || return 1
	cargo deny --manifest-path fuzz/Cargo.toml check
}

install_tools() {
	# Independent: a failed staticcheck install must not leave cargo-deny
	# uninstalled, or its step would fail for a reason that is not its own.
	failed=0
	go install "honnef.co/go/tools/cmd/staticcheck@$STATICCHECK_VERSION" || failed=1
	go install "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION" || failed=1
	cargo install cargo-deny --locked --version "$CARGO_DENY_VERSION" || failed=1
	return $failed
}

run_step() {
	fn="step_$(printf '%s' "$1" | tr '-' '_')"
	case " $STEPS " in
	*" $1 "*) ;;
	*)
		echo "lint.sh: unknown step '$1' (steps: $STEPS)" >&2
		return 2
		;;
	esac
	$fn
}

case ${1:-} in
install)
	install_tools
	exit $?
	;;
list)
	echo "$STEPS"
	exit 0
	;;
esac

[ $# -gt 0 ] || set -- $STEPS

# setup-go's '1.27.x' floats, so the compiler a run used is part of its result.
echo "toolchain: $(go version)"

summary=""
status=0
for s in "$@"; do
	case " ${LINT_SKIP:-} " in
	*" $s "*)
		echo "== $s: SKIPPED (explicit, LINT_SKIP)"
		summary="$summary$s SKIPPED (explicit)\n"
		status=1
		continue
		;;
	esac
	echo "== $s"
	if run_step "$s"; then
		summary="$summary$s PASS\n"
	else
		summary="$summary$s FAIL\n"
		status=1
	fi
done

printf '\nlint summary\n'
printf '%b' "$summary"
[ $status -eq 0 ] && echo "lint: every step passed" || echo "lint: NOT clean; at least one step failed or was skipped" >&2
exit $status
