#!/usr/bin/env bash
# Walks docs/adoption.md as an adopter would, on this machine, in a temporary
# directory, and fails on the first step that does not do what the document says.
#
#   tests/adopter/walkthrough.sh            # from a Gusset checkout
#   WORKDIR=/some/dir tests/adopter/walkthrough.sh
#
# The code blocks are extracted from the document, not retyped here, so editing
# the document in a way that stops it compiling fails this script. What it
# covers:
#
#   steps 1-2   the umbrella crate (Cargo.toml block, build.rs block, engine block)
#   step 3      Option B (CGO_LDFLAGS) and Option A (make install ARCHIVE=... then
#               -tags gusset_pkgconfig), both against a copy of the module with no
#               target/ and read-only, the way the module cache is
#   step 5      the Allocator snippet, compiled when the umbrella's own build.rs
#               (the document's) sets the cfg; gusset's probe printing the same
#               line does not count. A skip is a GitHub warning, not a silent echo. A copy of the
#               snippet with a type that does not exist must fail to compile.
#               GUSSET_REQUIRE_STEP5=1 fails the run when the snippet was not
#               compiled (the nightly job).
#   installers  make install (PREFIX, DESTDIR, odd characters, tracked files) and
#               install.sh (from a file, piped, bad ref, pinned commit) installing
#               the same set of files
#   gussetvet   run from the install against the adopter's shim, default build
#               and -tags gusset_pkgconfig
#
# Nothing is installed outside WORKDIR. Needs cargo, go, make, pkg-config, git.

set -euo pipefail

CHECKOUT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOC="$CHECKOUT/docs/adoption.md"
W="${WORKDIR:-$(mktemp -d 2>/dev/null || mktemp -d -t gusset-adopter)}"
W="$(cd "$W" && pwd)"
mkdir -p "$W"
OWN_WORKDIR=0
[ -z "${WORKDIR:-}" ] && OWN_WORKDIR=1

step() { printf '\n== %s\n' "$*"; }
fail() { printf 'walkthrough: FAILED: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required and was not found"; }
expect_fail() { # description command...
	local what="$1"
	shift
	if "$@" >"$W/expect_fail.out" 2>&1; then
		cat "$W/expect_fail.out" >&2
		fail "$what: succeeded, but must fail"
	fi
}

# umbrella_sets_step5_cfg BUILD_DIR PACKAGE: whether PACKAGE's own build script
# printed cargo::rustc-cfg=gusset_allocator_api. gusset's allocator probe prints
# that identical line into gusset's own output under BUILD_DIR, so a grep over
# all of BUILD_DIR reported the snippet compiled when the document's build.rs set
# nothing. Only PACKAGE's output is read, in either cargo layout:
#   BUILD_DIR/PACKAGE-<hash>/output       (stable 1.99)
#   BUILD_DIR/PACKAGE/<hash>/run/stdout   (nightly-2026-10-05, the step5-snippet job)
# Exactly one must exist: none means the check could not run, which is a
# failure, not a skip.
umbrella_sets_step5_cfg() {
	local f hash found=""
	for f in "$1/$2"-*/output "$1/$2"/*/run/stdout; do
		[ -f "$f" ] || continue
		case "$f" in
		"$1/$2"-*/output)
			hash="${f#"$1/$2"-}"
			hash="${hash%/output}"
			;;
		*)
			hash="${f#"$1/$2"/}"
			hash="${hash%/run/stdout}"
			;;
		esac
		# A bare hash only: PACKAGE-other-<hash> is another package.
		case "$hash" in "" | *[!0-9a-f]*) continue ;; esac
		[ -z "$found" ] || fail "two build-script outputs for $2 under $1: $found and $f"
		found="$f"
	done
	[ -n "$found" ] || fail "no build-script output for the umbrella package $2 under $1; the step 5 check could not run"
	grep -x -F -q 'cargo::rustc-cfg=gusset_allocator_api' "$found"
}

cleanup() {
	if [ -d "$W/module" ]; then chmod -R u+w "$W/module" 2>/dev/null || true; fi
	if [ "$OWN_WORKDIR" = 1 ] && [ -z "${KEEP:-}" ]; then rm -rf "$W"; else printf 'workdir kept: %s\n' "$W"; fi
}
trap cleanup EXIT

for t in cargo go make pkg-config git awk tar install; do need "$t"; done
export CARGO_TARGET_DIR="${CARGO_TARGET_DIR:-$W/cargo-target}"
# Nothing here may reach for the real /usr/local.
unset GUSSET_PREFIX GUSSET_LIBDIR GUSSET_INCLUDEDIR GUSSET_BINDIR GUSSET_REF GUSSET_REPO_URL GUSSET_COMMIT PREFIX
unset CGO_LDFLAGS CGO_CFLAGS PKG_CONFIG_PATH

# --- the document is the source of the code --------------------------------

# extract LANG MARKER: the body of the one fenced block of LANG containing MARKER,
# dedented by its fence's indentation. Zero or several matches is an error: the
# walkthrough would otherwise silently test a different block than the reader sees.
extract() {
	awk -v lang="$1" -v marker="$2" '
		function indent(s) { match(s, /^ */); return RLENGTH }
		inb && $0 ~ /^ *```[ ]*$/ { if (index(buf, marker) > 0) { n++; out = buf } inb = 0; next }
		!inb && $0 ~ ("^ *```" lang "[ ]*$") { inb = 1; ind = indent($0); buf = ""; next }
		inb { line = $0; if (indent(line) >= ind) line = substr(line, ind + 1); buf = buf line "\n" }
		END { if (n != 1) { printf "found %d %s blocks containing \"%s\"\n", n, lang, marker > "/dev/stderr"; exit 3 } printf "%s", out }
	' "$DOC" || fail "docs/adoption.md: cannot extract the $1 block containing \"$2\""
}

CARGO_TOML="$(extract toml '[lib]')"
ENGINE_RS="$(extract rust 'myservice_engine_init')"
SHIM_GO="$(extract go 'registerEngine')"
BUILD_RS="$(extract rust 'DEP_GUSSET_ALLOCATOR_API')"
STEP5_RS="$(extract rust 'BufferAlloc, JobOutput')"
# cargo names a build script's output directory after the [package], not the [lib].
UMB_PACKAGE="$(printf '%s\n' "$CARGO_TOML" | sed -n '/^\[package\]/,/^\[/{s/^name = "\([^"]*\)".*/\1/p;}')"
[ -n "$UMB_PACKAGE" ] || fail "docs/adoption.md step 1 has no [package] name for the step 5 check to read"

step "no Rust block in the document names the crate 'gusset' after step 1 renamed it"
for block in "$ENGINE_RS" "$STEP5_RS"; do
	if printf '%s\n' "$block" | grep -E '(^|[^_A-Za-z])gusset::' >/dev/null; then
		printf '%s\n' "$block" | grep -nE '(^|[^_A-Za-z])gusset::' >&2
		fail "a Rust block uses gusset:: but step 1 names the dependency gusset_core"
	fi
done

# --- a copy of the module as `go get` would deliver it ---------------------

step "module copy without target/, read-only"
mkdir -p "$W/module"
tar -C "$CHECKOUT" --exclude=./target --exclude=./.git --exclude=./.devcouncil --exclude=./.claude -cf - . | tar -C "$W/module" -xf -
[ ! -e "$W/module/target" ] || fail "module copy contains target/"
chmod -R a-w "$W/module"

# --- steps 1-2: the umbrella crate ------------------------------------------

step "steps 1-2: build the umbrella crate"
UMB="$W/adopter/umbrella"
mkdir -p "$UMB/src" "$W/adopter/my-engine/src"
cat >"$W/adopter/my-engine/Cargo.toml" <<'EOF'
[package]
name = "my-engine"
version = "0.1.0"
edition = "2021"

# Standalone: a WORKDIR inside another Cargo workspace must not adopt this crate.
[workspace]
EOF
cat >"$W/adopter/my-engine/src/lib.rs" <<'EOF'
pub fn run(text: &str) -> String {
    text.to_uppercase()
}
EOF
# The dependency line names a release tag; point it at the module copy so the
# walkthrough tests this commit. The line must exist: that is what we replace.
printf '%s\n' "$CARGO_TOML" | grep -E '^gusset_core = \{ package = "gusset", git = ' >/dev/null ||
	fail "docs/adoption.md step 1 no longer has the gusset_core git dependency line this script substitutes"
printf '%s\n' "$CARGO_TOML" |
	sed "s|^gusset_core = .*|gusset_core = { package = \"gusset\", path = \"$W/module/crates/gusset\" }|" >"$UMB/Cargo.toml"
# Standalone: cargo refuses a package under another workspace's root that the
# workspace does not list. The document's block has no [workspace] table of its own.
if printf '%s\n' "$CARGO_TOML" | grep -E '^\[workspace' >/dev/null; then
	fail "docs/adoption.md step 1 now has a [workspace] table; drop the one this script appends"
fi
printf '\n[workspace]\n' >>"$UMB/Cargo.toml"
printf '%s\n' "$BUILD_RS" >"$UMB/build.rs"
{
	printf '%s\n' "$ENGINE_RS"
	cat <<'EOF'

#[cfg(gusset_allocator_api)]
mod step5;
EOF
} >"$UMB/src/lib.rs"
# Step 5 is a fragment: give it the two helpers it calls and a function to live in.
{
	cat <<'EOF'
fn estimate(input: &[u8]) -> usize {
    input.len()
}

fn render_into(
    out: &mut Vec<u8, gusset_core::BufferAlloc>,
    input: &[u8],
    _ctx: &gusset_core::JobContext,
) -> Result<(), String> {
    out.extend_from_slice(input);
    Ok(())
}

pub fn register() {
EOF
	printf '%s\n' "$STEP5_RS" | sed 's/^/    /'
	printf '}\n'
} >"$UMB/src/step5.rs"
cp "$UMB/Cargo.toml" "$UMB/Cargo.toml.orig"
(cd "$UMB" && CARGO_TARGET_DIR="$UMB/target" cargo build --release 2>&1 | tail -5)
[ -f "$UMB/target/release/libgusset.a" ] || fail "umbrella did not produce libgusset.a ([lib] name = \"gusset\")"
# The build script always prints cargo::rustc-check-cfg=cfg(gusset_allocator_api).
# Only cargo::rustc-cfg=gusset_allocator_api, from the umbrella's own build
# script, means step5.rs was compiled.
if umbrella_sets_step5_cfg "$UMB/target/release/build" "$UMB_PACKAGE"; then
	echo "step 5 snippet compiled (compiler reports the Allocator API)"
else
	echo "::warning::step 5 snippet not compiled here: this compiler's allocator probe is off (gusset_core::ALLOCATOR_API)"
	if [ "${GUSSET_REQUIRE_STEP5:-}" = 1 ]; then
		fail "step 5 snippet was not compiled and GUSSET_REQUIRE_STEP5=1"
	fi
fi

step "step 5: a snippet with a bad type fails to compile"
BAD="$W/adopter/umbrella-bad"
mkdir -p "$BAD/src"
cp "$UMB/Cargo.toml" "$UMB/build.rs" "$BAD/"
grep -v '^#\[cfg(gusset_allocator_api)]$' "$UMB/src/lib.rs" >"$BAD/src/lib.rs"
grep -q '^mod step5;$' "$BAD/src/lib.rs" || fail "could not force the step 5 module into the negative compile"
{
	cat <<'EOF'
fn estimate(input: &[u8]) -> usize {
    input.len()
}

fn render_into(
    out: &mut Vec<u8, gusset_core::BufferAlloc>,
    input: &[u8],
    _ctx: &gusset_core::JobContext,
) -> Result<(), String> {
    out.extend_from_slice(input);
    Ok(())
}

pub fn register() {
EOF
	printf '%s\n' "$STEP5_RS" | sed 's/Vec<u8, BufferAlloc>/Vec<u8, Step5NotAType>/' | sed 's/^/    /'
	printf '}\n'
} >"$BAD/src/step5.rs"
grep -q 'Step5NotAType' "$BAD/src/step5.rs" || fail "the broken snippet lost its bad type"
expect_fail "step 5 snippet with a type that does not exist" \
	bash -c "cd '$BAD' && CARGO_TARGET_DIR='$BAD/target' cargo check --release"
grep -F 'Step5NotAType' "$W/expect_fail.out" >/dev/null || {
	cat "$W/expect_fail.out" >&2
	fail "the compiler did not report Step5NotAType; the negative compile did not type-check the snippet"
}

# --- the Go application -----------------------------------------------------

step "step 2: the cgo shim and a caller"
APP="$W/adopter/app"
mkdir -p "$APP"
printf '%s\n' "$SHIM_GO" >"$APP/shim.go"
cat >"$APP/go.mod" <<EOF
module example.com/toy

go 1.26

require github.com/bharathvbcr/gusset v0.0.0

replace github.com/bharathvbcr/gusset => $W/module
EOF
cat >"$APP/main.go" <<'EOF'
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bharathvbcr/gusset"
)

func main() {
	registerEngine()
	h, err := gusset.Open(gusset.WithPoolSize(2))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := h.Call(ctx, []byte("hello adopter"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		os.Exit(1)
	}
	fmt.Printf("%s\n", out)
}
EOF
# The list lives beside the app, not in it: gussetvet defaults to <dir>/exports.txt,
# so a copy inside the app would give the "no list" check below a list to find.
cat >"$W/adopter/engine-exports.txt" <<'EOF'
# the C symbols this shim imports
myservice_engine_init
EOF
check_toy() { # binary
	local got
	got="$("$1")" || fail "$1 exited non-zero"
	[ "$got" = "HELLO ADOPTER" ] || fail "$1 printed '$got', want 'HELLO ADOPTER'"
	echo "  ran: $got"
}

step "step 3, Option B: CGO_LDFLAGS at the umbrella's output"
(cd "$APP" && CGO_LDFLAGS="-L$UMB/target/release" go build -o "$W/toy-b" .)
check_toy "$W/toy-b"

step "step 3, Option B without the flag fails the way the document says"
expect_fail "a build with no archive on the link path" bash -c "cd '$APP' && go build -o '$W/toy-none' ."
grep -E "library 'gusset' not found|cannot find -lgusset|library not found for -lgusset" "$W/expect_fail.out" >/dev/null ||
	{ cat "$W/expect_fail.out" >&2; fail "the failure is not the documented 'library gusset not found'"; }

# --- make install -----------------------------------------------------------

EXPECT_VERSION="$(sed -n '/^\[workspace.package\]/,/^\[/{s/^version = "\(.*\)"/\1/p;}' "$CHECKOUT/Cargo.toml")"
[ -n "$EXPECT_VERSION" ] || fail "could not read the version from Cargo.toml"
HASH_BEFORE="$(cksum <"$CHECKOUT/internal/ffi/archive_hash.go")"
# internal/ffi/libgusset.a is ignored, so a dev checkout may already hold a
# leftover one. The installs must leave it as they found it: absent, or the
# same bytes. Asserting it is absent failed a local run nothing had written to.
LIBGUSSET="$CHECKOUT/internal/ffi/libgusset.a"
file_state() { if [ -e "$1" ]; then cksum <"$1"; else echo absent; fi; }
LIBGUSSET_BEFORE="$(file_state "$LIBGUSSET")"
check_libgusset() { # who
	local now
	now="$(file_state "$LIBGUSSET")"
	[ "$now" = "$LIBGUSSET_BEFORE" ] && return 0
	case "$LIBGUSSET_BEFORE:$now" in
	absent:*) fail "$1 wrote libgusset.a into internal/ffi" ;;
	*:absent) fail "$1 removed the libgusset.a internal/ffi held before the installs" ;;
	*) fail "$1 rewrote the libgusset.a internal/ffi held before the installs" ;;
	esac
}

fileset() { (cd "$1" && find . -type f | LC_ALL=C sort); }
want_files="./bin/gussetvet
./include/gusset.h
./lib/libgusset.a
./lib/pkgconfig/gusset.pc"

step "make install into a temp prefix: artifacts, version, pkg-config paths"
PM="$W/prefix-make"
make -C "$CHECKOUT" install PREFIX="$PM" >/dev/null
[ "$(fileset "$PM")" = "$want_files" ] || { fileset "$PM" >&2; fail "make install installed the wrong set of files"; }
[ "$(PKG_CONFIG_PATH="$PM/lib/pkgconfig" pkg-config --modversion gusset)" = "$EXPECT_VERSION" ] ||
	fail "gusset.pc version is not Cargo.toml's $EXPECT_VERSION"
[ "$(PKG_CONFIG_PATH="$PM/lib/pkgconfig" pkg-config --variable=libdir gusset)" = "$PM/lib" ] || fail "gusset.pc libdir is not $PM/lib"
PKG_CONFIG_PATH="$PM/lib/pkgconfig" pkg-config --cflags gusset | grep -F -- "-I$PM/include" >/dev/null || fail "gusset.pc cflags do not name $PM/include"
[ "$(cksum <"$CHECKOUT/internal/ffi/archive_hash.go")" = "$HASH_BEFORE" ] || fail "make install rewrote the tracked internal/ffi/archive_hash.go"
check_libgusset "an install"

step "make install honours LIBDIR/INCLUDEDIR overrides in the .pc"
PL="$W/prefix-libdir"
make -C "$CHECKOUT" install PREFIX="$PL" LIBDIR="$PL/lib64" INCLUDEDIR="$PL/hdr" >/dev/null
[ "$(PKG_CONFIG_PATH="$PL/lib64/pkgconfig" pkg-config --variable=libdir gusset)" = "$PL/lib64" ] || fail "the .pc ignores the LIBDIR override"
[ "$(PKG_CONFIG_PATH="$PL/lib64/pkgconfig" pkg-config --variable=includedir gusset)" = "$PL/hdr" ] || fail "the .pc ignores the INCLUDEDIR override"
[ -f "$PL/lib64/libgusset.a" ] && [ -f "$PL/hdr/gusset.h" ] || fail "LIBDIR/INCLUDEDIR overrides were not used for the files"

step "make install with | & and a space in the prefix or DESTDIR"
PW="$W/we|ird&dir"
make -C "$CHECKOUT" install PREFIX="$PW" DESTDIR="$W/stage dir" >/dev/null
[ -f "$W/stage dir$PW/lib/libgusset.a" ] || fail "DESTDIR was not honoured"
[ ! -e "$PW" ] || fail "DESTDIR install wrote outside the staging root"
grep -F "libdir=$PW/lib" "$W/stage dir$PW/lib/pkgconfig/gusset.pc" >/dev/null || { cat "$W/stage dir$PW/lib/pkgconfig/gusset.pc" >&2; fail "the staged .pc does not name the final libdir"; }
make -C "$CHECKOUT" uninstall PREFIX="$PW" DESTDIR="$W/stage dir" >/dev/null
[ -z "$(fileset "$W/stage dir")" ] || fail "uninstall left files behind"
expect_fail "a prefix with a space (pkg-config cannot use it)" make -C "$CHECKOUT" install PREFIX="$W/with space"
expect_fail "an ARCHIVE that does not exist" make -C "$CHECKOUT" install PREFIX="$W/x" ARCHIVE="$W/nope.a"

step "the plain install's archive is not the adopter's: a consumer cannot link it"
expect_fail "linking the toy against Gusset's own installed archive" \
	bash -c "cd '$APP' && GOCACHE='$W/gocache-plain' PKG_CONFIG_PATH='$PM/lib/pkgconfig' go build -tags gusset_pkgconfig -o '$W/toy-plain' ."
grep -E "myservice_engine_init" "$W/expect_fail.out" >/dev/null || { cat "$W/expect_fail.out" >&2; fail "the plain-archive link failed, but not on the adopter's symbol"; }

# --- step 3, Option A --------------------------------------------------------

step "step 3, Option A: install the umbrella archive, build with -tags gusset_pkgconfig"
PA="$W/prefix-umbrella"
make -C "$CHECKOUT" install ARCHIVE="$UMB/target/release/libgusset.a" PREFIX="$PA" >/dev/null
cmp "$UMB/target/release/libgusset.a" "$PA/lib/libgusset.a" || fail "the installed archive is not the umbrella's"
# Go caches the cgo flags pkg-config printed, keyed without PKG_CONFIG_PATH, so an
# earlier pkg-config build against another prefix would link that prefix's -L. Each
# pkg-config build therefore gets its own GOCACHE.
(cd "$APP" && GOCACHE="$W/gocache-a" PKG_CONFIG_PATH="$PA/lib/pkgconfig" go build -tags gusset_pkgconfig -o "$W/toy-a" .)
check_toy "$W/toy-a"

# --- gussetvet from the install ---------------------------------------------

step "gussetvet from the install, on the adopter's shim"
"$PA/bin/gussetvet" -h >/dev/null || fail "gussetvet -h did not exit 0"
EXPORTS="$W/adopter/engine-exports.txt"
[ ! -e "$APP/exports.txt" ] || fail "the toy app holds an exports.txt, so gussetvet has a default list to find"
"$PA/bin/gussetvet" -exports "$EXPORTS" "$APP"
# Option A builds with -tags gusset_pkgconfig; vet the files that build compiles.
"$PA/bin/gussetvet" -tags gusset_pkgconfig -exports "$EXPORTS" "$APP"
: >"$W/empty-exports.txt"
expect_fail "an empty export list" "$PA/bin/gussetvet" -exports "$W/empty-exports.txt" "$APP"
expect_fail "gussetvet with no export list in an adopter repo" "$PA/bin/gussetvet" "$APP"
grep -F "exports.txt" "$W/expect_fail.out" >/dev/null || { cat "$W/expect_fail.out" >&2; fail "gussetvet refused, but not over the missing exports.txt"; }

# --- install.sh ---------------------------------------------------------------

step "install.sh run from a file installs the same set as make install"
PS="$W/prefix-script"
(cd "$W" && GUSSET_PREFIX="$PS" bash "$CHECKOUT/install.sh" >/dev/null)
[ "$(fileset "$PS")" = "$want_files" ] || { fileset "$PS" >&2; fail "install.sh installed a different set of files than make install"; }
[ "$(PKG_CONFIG_PATH="$PS/lib/pkgconfig" pkg-config --variable=libdir gusset)" = "$PS/lib" ] || fail "install.sh's .pc names the wrong libdir"
[ "$(PKG_CONFIG_PATH="$PS/lib/pkgconfig" pkg-config --modversion gusset)" = "$EXPECT_VERSION" ] || fail "install.sh's .pc version is not Cargo.toml's"
cmp "$PM/include/gusset.h" "$PS/include/gusset.h" || fail "headers differ"
[ "$(cksum <"$CHECKOUT/internal/ffi/archive_hash.go")" = "$HASH_BEFORE" ] || fail "install.sh rewrote the tracked internal/ffi/archive_hash.go"
check_libgusset "install.sh"

step "install.sh with a |, & and \\ in the prefix"
PO="$W/p|q&r"
(cd "$W" && GUSSET_PREFIX="$PO" bash "$CHECKOUT/install.sh" >/dev/null)
[ "$(PKG_CONFIG_PATH="$PO/lib/pkgconfig" pkg-config --variable=libdir gusset)" = "$PO/lib" ] || fail "install.sh mangled a prefix holding | and &"

step "install.sh piped from inside a checkout must not build that checkout"
expect_fail "a piped install with an unreachable repository, run from a checkout" \
	bash -c "cd '$CHECKOUT' && GUSSET_PREFIX='$W/prefix-piped-bad' GUSSET_REPO_URL='file://$W/does-not-exist' bash < '$CHECKOUT/install.sh'"
[ ! -e "$W/prefix-piped-bad/lib/libgusset.a" ] || fail "piped install built the working directory instead of cloning"

step "install.sh piped: clones GUSSET_REF from GUSSET_REPO_URL, verifies GUSSET_COMMIT"
git clone -q --no-local "$CHECKOUT" "$W/mirror"
git -C "$W/mirror" -c user.name=walkthrough -c user.email=walkthrough@example.invalid tag walkthrough-tag
TAG_COMMIT="$(git -C "$W/mirror" rev-parse HEAD)"
mkdir -p "$W/empty-cwd"
PP="$W/prefix-piped"
(cd "$W/empty-cwd" && GUSSET_PREFIX="$PP" GUSSET_REPO_URL="file://$W/mirror" GUSSET_REF=walkthrough-tag GUSSET_COMMIT="$TAG_COMMIT" bash <"$CHECKOUT/install.sh" >/dev/null)
[ "$(fileset "$PP")" = "$want_files" ] || fail "the piped install installed the wrong set of files"
expect_fail "a GUSSET_COMMIT that is not the tag's commit" \
	bash -c "cd '$W/empty-cwd' && GUSSET_PREFIX='$W/prefix-pinned-bad' GUSSET_REPO_URL='file://$W/mirror' GUSSET_REF=walkthrough-tag GUSSET_COMMIT=0000000000000000000000000000000000000000 bash < '$CHECKOUT/install.sh'"
expect_fail "a ref that does not exist" \
	bash -c "cd '$W/empty-cwd' && GUSSET_PREFIX='$W/prefix-noref' GUSSET_REPO_URL='file://$W/mirror' GUSSET_REF=no-such-ref bash < '$CHECKOUT/install.sh'"

printf '\nwalkthrough: OK (version %s, %s)\n' "$EXPECT_VERSION" "$(uname -sm)"
