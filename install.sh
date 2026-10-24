#!/usr/bin/env bash
# Gusset: installer. Builds the release archive and gussetvet and installs them
# with `make install`, the one owner of what an install consists of.
#
# From a release tag (the default):
#   curl -fsSL https://raw.githubusercontent.com/bharathvbcr/gusset/<tag>/install.sh | bash
# From a checkout, building that checkout:
#   ./install.sh
#
# Environment:
#   GUSSET_REF        tag or branch to clone when not run from a checkout
#                     (default: GUSSET_DEFAULT_REF below)
#   GUSSET_COMMIT     if set, the commit the cloned ref must resolve to; the
#                     install stops before building anything otherwise
#   GUSSET_REPO_URL   repository to clone (default: the upstream GitHub repository)
#   GUSSET_PREFIX     install prefix (also PREFIX); GUSSET_LIBDIR, GUSSET_INCLUDEDIR,
#                     GUSSET_BINDIR override the sub-directories
#   DESTDIR           staging root, as for `make install`. A tree whose make install
#                     ignores DESTDIR (v0.0.2 and earlier) is refused before building
#   CARGO_TARGET_DIR  where cargo builds; honoured
#
# The script builds the directory it lives in only when it was run from a file
# inside a Gusset checkout. Piped into bash it never builds the working
# directory, whatever that directory looks like.

set -euo pipefail

GUSSET_DEFAULT_REF="v0.0.3"
GUSSET_UPSTREAM_URL="https://github.com/bharathvbcr/gusset.git"

BOLD=$'\033[1m'
GREEN=$'\033[0;32m'
YELLOW=$'\033[0;33m'
RED=$'\033[0;31m'
BLUE=$'\033[0;34m'
RESET=$'\033[0m'

info() { printf '%s==>%s %s%s%s\n' "$BLUE" "$RESET" "$BOLD" "$1" "$RESET"; }
success() { printf '%s==>%s %s%s%s\n' "$GREEN" "$RESET" "$BOLD" "$1" "$RESET"; }
warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$1"; }
error() {
	printf '%serror:%s %s\n' "$RED" "$RESET" "$1" >&2
	exit 1
}

# 1. Toolchain
info "Checking toolchain prerequisites..."
for tool in cargo go make; do
	command -v "$tool" >/dev/null 2>&1 || error "$tool is required to build Gusset (Rust: https://rustup.rs, Go >= 1.26: https://go.dev)"
done

# 2. Source
TEMP_DIR=""
cleanup() {
	if [ -n "$TEMP_DIR" ] && [ -d "$TEMP_DIR" ]; then
		rm -rf "$TEMP_DIR"
	fi
}
trap cleanup EXIT

# BASH_SOURCE[0] is empty when the script arrives on stdin. Only a script that is
# a file inside a checkout builds that checkout.
SRC=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
	here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
	if [ -f "$here/Cargo.toml" ] && [ -d "$here/crates/gusset" ] && [ -d "$here/internal/ffi" ]; then
		SRC="$here"
		info "Installing from the checkout at $SRC"
	fi
fi

if [ -z "$SRC" ]; then
	command -v git >/dev/null 2>&1 || error "git is required to fetch Gusset"
	REF="${GUSSET_REF:-$GUSSET_DEFAULT_REF}"
	case "$REF" in
	-* | "") error "GUSSET_REF '$REF' is not a tag or branch name" ;;
	esac
	URL="${GUSSET_REPO_URL:-$GUSSET_UPSTREAM_URL}"
	info "Cloning $URL at $REF..."
	TEMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'gusset-install')"
	git clone --quiet --depth 1 --branch "$REF" -- "$URL" "$TEMP_DIR/gusset"
	SRC="$TEMP_DIR/gusset"
	head="$(git -C "$SRC" rev-parse HEAD)"
	info "Cloned commit $head"
	if [ -n "${GUSSET_COMMIT:-}" ] && [ "$head" != "$GUSSET_COMMIT" ]; then
		error "$REF resolves to $head, not the GUSSET_COMMIT $GUSSET_COMMIT; nothing was built"
	fi
fi

# The tree's `make install` must honour DESTDIR. Step 4 hands it DESTDIR, LIBDIR
# and PKGCONFIGDIR, and step 5 runs gussetvet from $DESTDIR$BINDIR. The install
# recipe of v0.0.2 and earlier never reads DESTDIR: it copies into the prefix
# itself, so a staged install writes into the real prefix and then fails to find
# gussetvet under DESTDIR. This reads that tree's Makefile, before anything is
# built: the recipe lines (tab-indented) of the `install:` rule must name
# $(DESTDIR) or ${DESTDIR}. A missing Makefile or rule fails the same way.
install_recipe_uses_destdir() { # makefile
	awk '
		/^install:/ { inr = 1; next }
		inr && /^\t/ { if (index($0, "$(DESTDIR)") || index($0, "${DESTDIR}")) found = 1; next }
		inr && (/^[ \t]*$/ || /^#/) { next }
		inr { inr = 0 }
		END { exit !found }
	' "$1"
}
if [ ! -f "$SRC/Makefile" ] || ! install_recipe_uses_destdir "$SRC/Makefile"; then
	error "the Makefile at ${REF:-$SRC} does not honour DESTDIR: its install recipe never uses \$(DESTDIR), so \`make install\` would write into the prefix itself (Gusset v0.0.2 and earlier). Nothing was built. Install $GUSSET_DEFAULT_REF or a later release tag."
fi

# 3. Prefix
if [ -n "${GUSSET_PREFIX:-}" ]; then
	PREFIX="$GUSSET_PREFIX"
elif [ -n "${PREFIX:-}" ]; then
	PREFIX="$PREFIX"
elif [ -w "/usr/local" ] || [ "${EUID:-$(id -u)}" -eq 0 ]; then
	PREFIX="/usr/local"
else
	PREFIX="$HOME/.local"
fi
LIBDIR="${GUSSET_LIBDIR:-$PREFIX/lib}"
INCLUDEDIR="${GUSSET_INCLUDEDIR:-$PREFIX/include}"
BINDIR="${GUSSET_BINDIR:-$PREFIX/bin}"
PKGCONFIGDIR="$LIBDIR/pkgconfig"
DESTDIR="${DESTDIR:-}"
info "Target prefix: $PREFIX"

# 4. Build and install: the Makefile owns both, so this installs exactly what
# `make install` does. It writes no tracked file.
info "Building and installing..."
make -C "$SRC" install \
	PREFIX="$PREFIX" LIBDIR="$LIBDIR" INCLUDEDIR="$INCLUDEDIR" BINDIR="$BINDIR" \
	PKGCONFIGDIR="$PKGCONFIGDIR" DESTDIR="$DESTDIR"

# 5. Verification: the installed linter checks the source it was built from.
info "Running installation verification..."
"$DESTDIR$BINDIR/gussetvet" "$SRC/internal/ffi" >/dev/null

success "Gusset installed successfully!"

printf '\n'
printf '%s================================================================%s\n' "$BOLD" "$RESET"
printf '  %sGusset Installation Complete%s\n' "$GREEN" "$RESET"
printf '%s================================================================%s\n' "$BOLD" "$RESET"
printf '  Static Library : %s\n' "$LIBDIR/libgusset.a"
printf '  C Header       : %s\n' "$INCLUDEDIR/gusset.h"
printf '  Pkg-config     : %s\n' "$PKGCONFIGDIR/gusset.pc"
printf '  Linter Tool    : %s\n' "$BINDIR/gussetvet"
printf '%s================================================================%s\n\n' "$BOLD" "$RESET"

if [[ ":$PATH:" != *":$BINDIR:"* ]]; then
	warn "$BINDIR is not in your PATH. Add it with:"
	printf '    export PATH="%s:$PATH"\n\n' "$BINDIR"
fi

info "This is Gusset's own archive. A Go binary links one archive built from your"
info "umbrella crate; to install that one for pkg-config, see docs/adoption.md (Option A)."
