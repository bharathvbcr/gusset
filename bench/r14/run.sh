#!/bin/sh
# Every `(export VAR=… && cmd)` here is a subshell on purpose: it scopes VAR to
# one call, which is what SC2030/SC2031 warn about.
# shellcheck disable=SC2030,SC2031
# run.sh [build|check] — the R14 harness (docs/rfc-r14.md). Run from anywhere;
# exits non-zero when any scenario does not do what the RFC says it does.
#
#   naive     engine A + engine B linked as built          -> must FAIL to link
#   nocomdat  symbols localized, COMDAT left shared (ELF)  -> must CRASH at runtime
#   sealed    both engines sealed by tools/gussetseal      -> must PASS (3 runs)
#   race      sealed, built with -race                     -> must PASS
#   cgocheck2 sealed, GOEXPERIMENT=cgocheck2               -> must PASS
#   asan      sealed, go build -asan (linux-gnu only)      -> must PASS
#   asan-rust as asan, with libgusset.a and both engines   -> must PASS
#             built with -Zsanitizer=address (linux-gnu only)
#   nolto     engines built without LTO: naive must FAIL to link or FAIL the
#             isolation checks (it can link and silently share one std), and
#             sealed must PASS
#   skew      sealed, engine B built by R14_SKEW_TOOLCHAIN -> must PASS
#   internal  -linkmode=internal                           -> must FAIL, on Gusset's
#             own symbols too: Gusset already requires external linking
#
# Every scenario also links Gusset's own libgusset.a, unsealed, and drives its
# panic firewall: R14 keeps exactly one unsealed Rust archive per binary.
#
# A scenario that cannot run prints a line starting SKIPPED and says why; the
# r14 CI job fails on any. Go's -asan does not exist on darwin or musl, which
# is printed as n/a instead: no configuration of this machine could run it.
#
# Phases. With no argument both run. `build` runs cargo only; `check` runs the
# rest against archives a `build` left in place, and needs no Rust toolchain.
# Splitting them is how linux/x86-64 is checked from an arm64 host: rustc
# crashes under qemu-user, but the Go toolchain, the linker and the binaries
# do not, so cargo cross-builds natively and `check` runs emulated.
#
# Environment:
#   R14_TARGET            Rust target triple (default: the host's). Set GOARCH
#                         to match when it is not the host's architecture.
#   R14_SKEW_TOOLCHAIN    toolchain for engine B in `skew` (default 1.97)
#   R14_ASAN_TOOLCHAIN    nightly for `asan-rust` (default: RUST_NIGHTLY from
#                         .github/workflows/matrix.yml, so the pin has one owner)
#   R14_NO_ASAN           a reason; reports both ASan scenarios SKIPPED with it.
#                         For emulated checks: ASan's allocator aborts under
#                         qemu-user on x86-64 even for a plain C malloc/free.
set -eu

phase=${1:-all}
case "$phase" in build | check | all) ;; *)
	echo "usage: $0 [build|check]" >&2
	exit 2
	;;
esac

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
out="$here/target"
target=${R14_TARGET:-}
rel=${target:+$target/}release
skew=${R14_SKEW_TOOLCHAIN:-1.97}
nightly=${R14_ASAN_TOOLCHAIN:-$(sed -n 's/^  RUST_NIGHTLY: *//p' "$root/.github/workflows/matrix.yml")}
mkdir -p "$out"

say() { printf '\n== %s ==\n' "$*"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# The target triple's OS, or the host's when no target is set. Decides which
# scenarios apply; the binaries always run on the machine running `check`.
os=$(uname -s)
libc=gnu
case "$target" in
*-apple-darwin) os=Darwin ;;
*-linux-musl) os=Linux libc=musl ;;
*-linux-gnu) os=Linux ;;
"") if [ "$os" = Linux ] && (ldd --version 2>&1 || true) | grep -qi musl; then libc=musl; fi ;;
*) fail "R14_TARGET=$target is not in docs/platforms.md's supported set" ;;
esac

gusset_lib="$root/target/$rel"
archive() { echo "$out/cargo_$1/$rel/libengine_${1%%_*}.a"; }

# ---------------------------------------------------------------- build phase

cargo_build() { # dir target-dir toolchain-or-empty [cargo args...]
	dir=$1 tdir=$2 tc=$3
	shift 3
	(cd "$dir" && CARGO_TARGET_DIR="$tdir" cargo ${tc:+"+$tc"} build --release -q \
		${target:+--target "$target"} "$@")
}

# -Zsanitizer needs an explicit --target, or the build scripts are
# instrumented too (nightly.yml's asan job passes one for the same reason).
asan_build() { # dir target-dir
	# cargo_build already passes R14_TARGET when it is set.
	if [ -n "$target" ]; then
		(export RUSTFLAGS=-Zsanitizer=address && cargo_build "$1" "$2" "$nightly")
	else
		(export RUSTFLAGS=-Zsanitizer=address && cargo_build "$1" "$2" "$nightly" \
			--target "$(rustc -vV | sed -n 's/^host: //p')")
	fi
}

if [ "$phase" != check ]; then
	say "build${target:+ for $target}"
	cargo_build "$root" "$root/target" "" -p gusset
	for e in a b; do
		cargo_build "$here/engine_$e" "$out/cargo_$e" ""
		(export CARGO_PROFILE_RELEASE_LTO=false CARGO_PROFILE_RELEASE_CODEGEN_UNITS=16 &&
			cargo_build "$here/engine_$e" "$out/cargo_${e}_nolto" "")
	done
	rm -f "$out/cargo_b_skew/toolchain"
	if rustup run "$skew" rustc -V >/dev/null 2>&1; then
		cargo_build "$here/engine_b" "$out/cargo_b_skew" "$skew"
		echo "A: $(rustc -V) / B: $(rustup run "$skew" rustc -V)" >"$out/cargo_b_skew/toolchain"
	fi
	rm -f "$out/cargo_gusset_asan/toolchain"
	if [ "$os" = Linux ] && [ "$libc" = gnu ] && rustup run "$nightly" rustc -V >/dev/null 2>&1; then
		# The asan archives land under the triple even without R14_TARGET.
		asan_target=${target:-$(rustc -vV | sed -n 's/^host: //p')}
		asan_build "$root" "$out/cargo_gusset_asan"
		for e in a b; do asan_build "$here/engine_$e" "$out/cargo_${e}_asan"; done
		echo "$asan_target" >"$out/cargo_gusset_asan/toolchain"
	fi
fi
[ "$phase" = build ] && exit 0

# ---------------------------------------------------------------- check phase

# The library directories go into CGO_LDFLAGS, scenario first, then Gusset's:
# CGO_LDFLAGS precedes internal/ffi's own -L, so a cross or instrumented
# libgusset.a wins over the host one in target/release.
#
# The Go build cache does not hash external archives (AGENTS.md, "Stale .a not
# rebuilt"), so each link first stamps their hash into a generated source file;
# a changed archive then always changes the build, as archive_hash.go does for
# internal/ffi.
hash_of() {
	if command -v sha256sum >/dev/null 2>&1; then cat "$@" | sha256sum; else cat "$@" | shasum -a 256; fi | cut -c1-64
}
link_host() { # libdir gussetdir binary [go build flags...] -> build output, status kept
	dir=$1 gdir=$2 bin=$3
	shift 3
	printf '// Code generated by bench/r14/run.sh; DO NOT EDIT.\n\npackage main\n\nconst linkedArchives = "%s"\n' \
		"$(hash_of "$dir"/*.a "$gdir/libgusset.a")" >"$here/host/zz_archives.go"
	(cd "$here/host" && CGO_LDFLAGS="-L$dir -L$gdir" go build "$@" -o "$bin" . 2>&1)
}

seal_pair() { # dir archive-a archive-b
	mkdir -p "$1"
	"$out/gussetseal" -prefix ea_ -o "$1/libengine_a.a" "$2"
	"$out/gussetseal" -prefix eb_ -o "$1/libengine_b.a" "$3"
}

expect_naive_failure() { # dir archive-a archive-b
	mkdir -p "$1"
	cp "$2" "$3" "$1/"
	if log=$(link_host "$1" "$gusset_lib" "$1/host"); then
		fail "naive link in $1 succeeded; R14's premise no longer holds on this toolchain"
	fi
	echo "$log" | grep -q 'rust_eh_personality' ||
		fail "naive link in $1 failed, but not on rust_eh_personality: $log"
	echo "link failed as R14 predicts. Duplicate symbols:"
	echo "$log" | grep -oE "(duplicate symbol '[^']+'|multiple definition of \`[^']+')" | sort -u
}

expect_pass() { # binary label
	"$1" 2>"$1.stderr" || fail "$2 (stderr in $1.stderr)"
}

need() { [ -f "$1" ] || fail "$1 is missing; run '$0 build' first"; }

# go turns cgo off by default whenever GOARCH is not the host's, and the host is
# nothing but cgo.
export CGO_ENABLED=1

a=$(archive a)
b=$(archive b)
for f in "$gusset_lib/libgusset.a" "$a" "$b" "$(archive a_nolto)" "$(archive b_nolto)"; do need "$f"; done

say "check${target:+ for $target} on $(uname -sm)"
(cd "$root" && go build -o "$out/gussetseal" ./tools/gussetseal)
for f in "$gusset_lib/libgusset.a" "$a" "$b"; do echo "$(wc -c <"$f") $f"; done

say "naive: two archives as built"
expect_naive_failure "$out/naive" "$a" "$b"

if [ "$os" = Linux ]; then
	say "nocomdat: symbols localized, personality COMDAT group left shared"
	# What a localize-only recipe produces. gussetseal refuses to write this, so
	# it is built by hand here to keep the failure it prevents on record.
	mkdir -p "$out/nocomdat"
	localize_only() { # engine-letter archive
		ld -r --whole-archive "$2" -o "$out/nocomdat/e$1.o"
		objcopy --wildcard --keep-global-symbol="e${1}_*" \
			--remove-section=.llvmbc --remove-section=.llvmcmd "$out/nocomdat/e$1.o"
		rm -f "$out/nocomdat/libengine_$1.a"
		ar rcs "$out/nocomdat/libengine_$1.a" "$out/nocomdat/e$1.o"
	}
	localize_only a "$a"
	localize_only b "$b"
	if "$out/gussetseal" -prefix ea_ -verify "$out/nocomdat/ea.o" 2>/dev/null; then
		fail "gussetseal -verify accepted a shared COMDAT group"
	fi
	link_host "$out/nocomdat" "$gusset_lib" "$out/nocomdat/host" || fail "nocomdat did not link"
	if "$out/nocomdat/host" >"$out/nocomdat/log" 2>&1; then
		fail "nocomdat passed; renaming COMDAT groups is no longer needed"
	fi
	echo "gussetseal -verify refused it, and it crashed as expected:"
	grep -m2 -E '^(SIGSEGV|signal arrived)' "$out/nocomdat/log" || echo "(no Go traceback: the process died outside Go's signal handler)"
fi

say "sealed: both engines sealed"
seal_pair "$out/sealed" "$a" "$b"
link_host "$out/sealed" "$gusset_lib" "$out/sealed/host" || fail "sealed did not link"
for i in 1 2 3; do expect_pass "$out/sealed/host" "sealed run $i"; done
echo "binary bytes: $(wc -c <"$out/sealed/host")"

say "race: sealed, -race"
link_host "$out/sealed" "$gusset_lib" "$out/sealed/host-race" -race || fail "race did not link"
expect_pass "$out/sealed/host-race" "race run"

say "cgocheck2: sealed, GOEXPERIMENT=cgocheck2"
# A subshell export, not VAR=x before a function call: POSIX leaves whether
# that assignment reaches the function's commands unspecified.
(export GOEXPERIMENT=cgocheck2 && link_host "$out/sealed" "$gusset_lib" "$out/sealed/host-cgocheck2") ||
	fail "cgocheck2 did not link"
expect_pass "$out/sealed/host-cgocheck2" "cgocheck2 run"

say "asan: sealed, go build -asan (Rust not instrumented)"
if [ "$os" != Linux ] || [ "$libc" = musl ]; then
	echo "n/a: Go's -asan supports linux-gnu only"
elif [ -n "${R14_NO_ASAN:-}" ]; then
	echo "SKIPPED: $R14_NO_ASAN"
else
	link_host "$out/sealed" "$gusset_lib" "$out/sealed/host-asan" -asan || fail "asan did not link"
	(export ASAN_OPTIONS=detect_leaks=0 && expect_pass "$out/sealed/host-asan" "asan run")
fi

say "asan-rust: libgusset.a and both engines built with -Zsanitizer=address"
if [ "$os" != Linux ] || [ "$libc" = musl ]; then
	echo "n/a: Go's -asan supports linux-gnu only"
elif [ -n "${R14_NO_ASAN:-}" ]; then
	echo "SKIPPED: $R14_NO_ASAN"
elif [ ! -f "$out/cargo_gusset_asan/toolchain" ]; then
	echo "SKIPPED: toolchain $nightly was not installed at build time (set R14_ASAN_TOOLCHAIN)"
else
	at=$(cat "$out/cargo_gusset_asan/toolchain")
	ga="$out/cargo_gusset_asan/$at/release"
	seal_pair "$out/asan-rust" "$out/cargo_a_asan/$at/release/libengine_a.a" "$out/cargo_b_asan/$at/release/libengine_b.a"
	# Instrumented code carries asan.module_ctor COMDAT groups as well as the
	# personality's; the seal must have renamed them all to pass -verify.
	link_host "$out/asan-rust" "$ga" "$out/asan-rust/host" -asan || fail "asan-rust did not link"
	nm "$out/asan-rust/host" | grep -q 'asan.module_ctor' ||
		fail "asan-rust linked, but no instrumented Rust module constructor is in the binary"
	# ASan's per-image registration flag must be one, or every module registers
	# all instrumented globals again (gussetseal's imageShared).
	flags=$(nm "$out/asan-rust/host" | grep -c ' ___asan_globals_registered$' || true)
	[ "$flags" = 1 ] || fail "asan-rust binary has $flags ___asan_globals_registered flags, want 1"
	(export ASAN_OPTIONS=detect_leaks=0 && expect_pass "$out/asan-rust/host" "asan-rust run")
	echo "instrumented with $nightly ($at)"
fi

say "nolto: engines built with lto = false and 16 codegen units"
an=$(archive a_nolto)
bn=$(archive b_nolto)
# Without LTO the naive link can SUCCEED: when both archives come from one
# toolchain, their std members have identical mangled names, the linker pulls
# one copy, and both engines silently share it — panic hook, allocator shim and
# all. The host's isolation checks must catch that; a link failure is the other
# acceptable outcome (toolchain skew, a platform's linker resolving differently).
mkdir -p "$out/nolto-naive"
cp "$an" "$bn" "$out/nolto-naive/"
if link_host "$out/nolto-naive" "$gusset_lib" "$out/nolto-naive/host" >"$out/nolto-naive/link.log"; then
	if "$out/nolto-naive/host" >"$out/nolto-naive/log" 2>&1; then
		fail "nolto naive linked and passed the isolation checks; the engines did not share std after all"
	fi
	echo "linked WITHOUT error, then failed the isolation checks — one std, silently shared:"
	grep -E '^FAIL' "$out/nolto-naive/log"
else
	grep -q 'rust_eh_personality' "$out/nolto-naive/link.log" ||
		fail "nolto naive link failed, but not on rust_eh_personality: $(cat "$out/nolto-naive/link.log")"
	echo "link failed on rust_eh_personality"
fi
seal_pair "$out/nolto" "$an" "$bn"
link_host "$out/nolto" "$gusset_lib" "$out/nolto/host" || fail "nolto sealed did not link"
expect_pass "$out/nolto/host" "nolto sealed run"

say "skew: engine B built with Rust $skew"
if [ -f "$out/cargo_b_skew/toolchain" ]; then
	seal_pair "$out/skew" "$a" "$(archive b_skew)"
	link_host "$out/skew" "$gusset_lib" "$out/skew/host" || fail "skew did not link"
	expect_pass "$out/skew/host" "skew run"
	cat "$out/cargo_b_skew/toolchain"
else
	echo "SKIPPED: toolchain $skew was not installed at build time (set R14_SKEW_TOOLCHAIN)"
fi

say "internal: -linkmode=internal"
if log=$(link_host "$out/sealed" "$gusset_lib" "$out/sealed/host-internal" -ldflags=-linkmode=internal); then
	fail "internal linking succeeded; the RFC says Gusset requires external linking"
fi
echo "$log" | grep -q 'gusset_' ||
	fail "internal link failed, but not on Gusset's own symbols, so sealing may be the cause: $log"
echo "failed on Gusset's own symbols as well; external linking is already required"

say "all scenarios behaved as docs/rfc-r14.md states"
