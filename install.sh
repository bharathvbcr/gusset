#!/usr/bin/env bash
# Gusset: One-command installer
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/bharathvbcr/gusset/main/install.sh | bash
# Or locally:
#   ./install.sh

set -euo pipefail

# ANSI colors
BOLD="\033[1m"
GREEN="\033[0;32m"
YELLOW="\033[0;33m"
RED="\033[0;31m"
BLUE="\033[0;34m"
RESET="\033[0m"

info() {
    printf "${BLUE}==>${RESET} ${BOLD}%s${RESET}\n" "$1"
}

success() {
    printf "${GREEN}==>${RESET} ${BOLD}%s${RESET}\n" "$1"
}

warn() {
    printf "${YELLOW}warning:${RESET} %s\n" "$1"
}

error() {
    printf "${RED}error:${RESET} %s\n" "$1" >&2
    exit 1
}

# 1. Dependency checks
info "Checking toolchain prerequisites..."

if ! command -v cargo >/dev/null 2>&1; then
    error "Rust / Cargo is required to build Gusset. Install via https://rustup.rs"
fi

if ! command -v go >/dev/null 2>&1; then
    error "Go (>= 1.26) is required to build Gusset. Install via https://golang.org"
fi

# 2. Workspace acquisition
SCRIPT_DIR=""
TEMP_DIR=""
cleanup() {
    if [ -n "$TEMP_DIR" ] && [ -d "$TEMP_DIR" ]; then
        rm -rf "$TEMP_DIR"
    fi
}
trap cleanup EXIT

if [ -f "Cargo.toml" ] && [ -d "crates/gusset" ] && [ -d "internal/ffi" ]; then
    SCRIPT_DIR="$(pwd)"
    info "Installing from local source repository at $SCRIPT_DIR"
else
    info "Cloning Gusset from repository..."
    TEMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'gusset-install')"
    git clone --depth 1 https://github.com/bharathvbcr/gusset.git "$TEMP_DIR"
    SCRIPT_DIR="$TEMP_DIR"
fi

cd "$SCRIPT_DIR"

# 3. Determine install prefix
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

info "Target prefix: $PREFIX"

# 4. Build release artifacts
info "Compiling Rust static library (release)..."
cargo build --release --manifest-path "$SCRIPT_DIR/Cargo.toml"

STATICLIB="$SCRIPT_DIR/target/release/libgusset.a"
if [ ! -f "$STATICLIB" ]; then
    error "Static library not found at $STATICLIB after cargo build"
fi

info "Updating Go FFI metadata..."
go generate ./internal/ffi

info "Building gussetvet linter..."
go build -o "$SCRIPT_DIR/target/gussetvet" ./tools/gussetvet

# 5. Install files
info "Installing files to $PREFIX..."
mkdir -p "$LIBDIR" "$INCLUDEDIR" "$BINDIR" "$PKGCONFIGDIR"

cp "$STATICLIB" "$LIBDIR/libgusset.a"
chmod 644 "$LIBDIR/libgusset.a"

cp "$SCRIPT_DIR/internal/ffi/gusset.h" "$INCLUDEDIR/gusset.h"
chmod 644 "$INCLUDEDIR/gusset.h"

# In-tree copy for immediate go build / test convenience if in local clone
if [ -d "$SCRIPT_DIR/internal/ffi" ]; then
    cp "$STATICLIB" "$SCRIPT_DIR/internal/ffi/libgusset.a" 2>/dev/null || true
fi

if [ -f "$SCRIPT_DIR/gusset.pc.in" ]; then
    sed -e "s|@PREFIX@|$PREFIX|g" -e "s|@VERSION@|0.1.0|g" "$SCRIPT_DIR/gusset.pc.in" > "$PKGCONFIGDIR/gusset.pc"
    chmod 644 "$PKGCONFIGDIR/gusset.pc"
fi

cp "$SCRIPT_DIR/target/gussetvet" "$BINDIR/gussetvet"
chmod 755 "$BINDIR/gussetvet"

# Also go install if Go environment is configured
if command -v go >/dev/null 2>&1; then
    (cd "$SCRIPT_DIR" && go install ./tools/gussetvet >/dev/null 2>&1 || true)
fi

# 6. Verification smoke test
info "Running installation verification..."
"$BINDIR/gussetvet" "$SCRIPT_DIR/internal/ffi" >/dev/null

success "Gusset installed successfully!"

printf "\n"
printf "${BOLD}================================================================${RESET}\n"
printf "  ${GREEN}Gusset Installation Complete${RESET}\n"
printf "${BOLD}================================================================${RESET}\n"
printf "  Static Library : %s\n" "$LIBDIR/libgusset.a"
printf "  C Header       : %s\n" "$INCLUDEDIR/gusset.h"
printf "  Pkg-config     : %s\n" "$PKGCONFIGDIR/gusset.pc"
printf "  Linter Tool    : %s\n" "$BINDIR/gussetvet"
printf "${BOLD}================================================================${RESET}\n\n"

# Environment hints
ENV_HINTS=0
if [[ ":$PATH:" != *":$BINDIR:"* ]]; then
    warn "$BINDIR is not in your PATH. Add it with:"
    printf "    export PATH=\"%s:\$PATH\"\n\n" "$BINDIR"
    ENV_HINTS=1
fi

if [[ ":${PKG_CONFIG_PATH:-}:" != *":$PKGCONFIGDIR:"* ]]; then
    info "To use gusset with pkg-config, configure:"
    printf "    export PKG_CONFIG_PATH=\"%s:\${PKG_CONFIG_PATH:-}\"\n\n" "$PKGCONFIGDIR"
    ENV_HINTS=1
fi

info "To start using Gusset in your Go code:"
printf "    go get github.com/bharathvbcr/gusset\n\n"
