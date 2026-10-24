.PHONY: all linked-target-dir build test bench bench-crossover bench-scaling bench-dispatch lint alloc-gate ci-local docs docs-check cross fuzz clean install install-build uninstall

export PATH := $(PATH):$(shell go env GOPATH)/bin

PREFIX ?= $(shell if [ -w /usr/local ]; then echo /usr/local; else echo $(HOME)/.local; fi)
LIBDIR ?= $(PREFIX)/lib
INCLUDEDIR ?= $(PREFIX)/include
BINDIR ?= $(PREFIX)/bin
PKGCONFIGDIR ?= $(LIBDIR)/pkgconfig
# Staging root for packagers. Every file lands under $(DESTDIR)<dir>; the
# generated gusset.pc names the final <dir>, never DESTDIR.
DESTDIR ?=

# Cargo's output directory, as cargo itself resolves it from the environment.
TARGET_DIR ?= $(if $(CARGO_TARGET_DIR),$(CARGO_TARGET_DIR),target)

# The archive `install` puts at $(LIBDIR)/libgusset.a. By default Gusset's own,
# built here. Pass ARCHIVE=<umbrella>/target/release/libgusset.a to install the
# adopter's umbrella archive instead (docs/adoption.md, Option A): that is the
# only archive a Go binary can link, so it is the one gusset.pc must describe.
# An explicit ARCHIVE is not rebuilt.
INSTALL_BUILD := $(if $(filter undefined,$(origin ARCHIVE)),install-build)
ARCHIVE ?= $(TARGET_DIR)/release/libgusset.a

# The one place the version is typed is [workspace.package] in Cargo.toml.
# `cargo pkgid` prints path+file:///.../crates/gusset#0.0.2 (or #gusset@0.0.2).
# HASH, not a literal '#': GNU Make 3.81 (macOS's make) reads '#' inside
# $(shell …) as a comment and stops with "unterminated call to function".
HASH := \#
GUSSET_VERSION = $(shell cargo pkgid -p gusset 2>/dev/null | sed 's/.*[$(HASH)@]//')

all: build

# The directory cgo links from. internal/ffi/ffi.go names
# ${SRCDIR}/../../target/release and then target/debug in its default LDFLAGS,
# and internal/ffi/gen.go hashes the first libgusset.a it finds there, in the
# same order. A target that runs `cargo build` and then links Go against the
# result is only honest when cargo wrote into this directory: with
# CARGO_TARGET_DIR=/x, cargo builds /x/release/libgusset.a, cgo links whatever
# ./target still holds (an old archive, or nothing), and the hash describes
# that old archive. Those targets refuse instead, naming both archives.
# `install` honours CARGO_TARGET_DIR (ARCHIVE above) and does not link Go
# against the archive, so it does not take this check.
LINKED_TARGET_DIR := $(CURDIR)/target
TARGET_DIR_REFUSAL = cargo builds $(abspath $(TARGET_DIR))/release/libgusset.a \
	but internal/ffi links and gen.go hashes $(LINKED_TARGET_DIR)/release/libgusset.a \
	(CARGO_TARGET_DIR=$(CARGO_TARGET_DIR)). Unset CARGO_TARGET_DIR for this target \
	or set it to $(LINKED_TARGET_DIR)

# $(error) rather than a shell test, so `make -n build` refuses too: a dry run
# that prints a plan which would link the wrong archive is the same lie.
linked-target-dir:
	$(if $(filter-out $(LINKED_TARGET_DIR),$(abspath $(TARGET_DIR))),$(error $(TARGET_DIR_REFUSAL)))

build: linked-target-dir
	cargo build --locked --release
	go generate ./internal/ffi
	go build ./...

# Installing builds the archive and gussetvet only. `build` is not a
# dependency: an install needs the archive, not a Go tree linked against it,
# and it honours a CARGO_TARGET_DIR that `build` refuses. It also runs no
# `go generate`, so it writes nothing into the source tree.
install-build:
	cargo build --locked --release

# Shell-quote a make value: 'it'\''s'.
shq = '$(subst ','\'',$(1))'

# gusset.pc is filled by awk reading the values from the environment, so a path
# holding `|`, `&` or `\` is copied byte for byte. pkg-config splits Libs and
# Cflags on whitespace, so a directory with whitespace in it is refused rather
# than installed as a file that cannot be used.
install: $(INSTALL_BUILD)
	@version=$(call shq,$(GUSSET_VERSION)); \
	case "$$version" in [0-9]*.[0-9]*.[0-9]*) ;; \
		*) echo "install: cannot read the version from Cargo.toml (cargo pkgid gave '$$version')" >&2; exit 1;; esac; \
	for d in $(call shq,$(LIBDIR)) $(call shq,$(INCLUDEDIR)); do \
		case "$$d" in *[[:space:]]*) echo "install: '$$d' contains whitespace; pkg-config cannot use it" >&2; exit 1;; esac; \
	done; \
	[ -f $(call shq,$(ARCHIVE)) ] || { echo "install: archive $(call shq,$(ARCHIVE)) does not exist" >&2; exit 1; }; \
	go build -o $(call shq,$(TARGET_DIR))/gussetvet ./tools/gussetvet || exit 1; \
	install -d $(call shq,$(DESTDIR)$(LIBDIR)) $(call shq,$(DESTDIR)$(INCLUDEDIR)) \
		$(call shq,$(DESTDIR)$(BINDIR)) $(call shq,$(DESTDIR)$(PKGCONFIGDIR)) || exit 1; \
	install -m 644 $(call shq,$(ARCHIVE)) $(call shq,$(DESTDIR)$(LIBDIR))/libgusset.a || exit 1; \
	install -m 644 internal/ffi/gusset.h $(call shq,$(DESTDIR)$(INCLUDEDIR))/gusset.h || exit 1; \
	install -m 755 $(call shq,$(TARGET_DIR))/gussetvet $(call shq,$(DESTDIR)$(BINDIR))/gussetvet || exit 1; \
	GUSSET_PC_LIBDIR=$(call shq,$(LIBDIR)) GUSSET_PC_INCLUDEDIR=$(call shq,$(INCLUDEDIR)) GUSSET_PC_VERSION="$$version" \
	awk 'function repl(s, from, to,   out, i) { out = ""; while ((i = index(s, from)) > 0) { out = out substr(s, 1, i - 1) to; s = substr(s, i + length(from)) } return out s } \
		{ $$0 = repl($$0, "@LIBDIR@", ENVIRON["GUSSET_PC_LIBDIR"]); $$0 = repl($$0, "@INCLUDEDIR@", ENVIRON["GUSSET_PC_INCLUDEDIR"]); \
		  $$0 = repl($$0, "@VERSION@", ENVIRON["GUSSET_PC_VERSION"]); print }' gusset.pc.in \
		> $(call shq,$(DESTDIR)$(PKGCONFIGDIR))/gusset.pc || exit 1; \
	chmod 644 $(call shq,$(DESTDIR)$(PKGCONFIGDIR))/gusset.pc; \
	echo "Gusset $$version installed to" $(call shq,$(DESTDIR)$(PREFIX))

uninstall:
	@rm -f $(call shq,$(DESTDIR)$(LIBDIR))/libgusset.a
	@rm -f $(call shq,$(DESTDIR)$(INCLUDEDIR))/gusset.h
	@rm -f $(call shq,$(DESTDIR)$(PKGCONFIGDIR))/gusset.pc
	@rm -f $(call shq,$(DESTDIR)$(BINDIR))/gussetvet
	@echo Gusset uninstalled from $(call shq,$(DESTDIR)$(PREFIX))

test: linked-target-dir
	cargo test --locked --workspace
	go test -v ./...

# -count=10: benchstat refuses to quote a confidence interval below 6 samples, and
# the committed data used to have 5 — so every README figure was a point estimate
# with no dispersion behind it.
BENCH_COUNT ?= 10

# The sweep's own repetition count, lower than BENCH_COUNT because each of its
# repetitions is four concurrency levels up to 2048 callers rather than a 1s
# time budget — but never below 6, for the reason above.
SCALING_COUNT ?= 6

# Every recording goes through bench/record.sh. It holds a lock so a second run
# refuses instead of interleaving, checks that no other Go benchmark is running,
# measures CPU idle before and after, runs each arm in its own process into its
# own temporary file, and only then assembles the published file with a
# provenance header naming what it checked. A run that fails any of those writes
# nothing at all, leaving the previous honest measurement in place.
#
# A script rather than recipe lines because Make runs each line in its own
# shell, so a lock held across several commands and a trap that survives an
# interrupt cannot be written here. The lock is the part that mattered: two
# working sessions on this checkout each ran a recording target, both appended
# to one results file, and a third benchmark process competed for the same
# cores. Nothing about that requires a mistake — it is what a shared checkout
# does — which is why the fix has to be mechanical rather than a rule about
# being careful.
BENCH_RESULTS ?= bench/results/$(shell go env GOOS)-$(shell go env GOARCH)-go$(shell go env GOVERSION | sed 's/^go//')-rust$(shell rustc --version | cut -d' ' -f2).txt

bench: linked-target-dir
	cargo build --locked --release
	RECORD_DIR=. RECORD_PKG=./bench/... bench/record.sh $(BENCH_RESULTS) 1s $(BENCH_COUNT) .

# Raw blocking cgo versus Gusset on byte-identical work.
#
# A separate target, a separate module and a separate results directory.
# Separate module because Go forbids cgo in _test.go files and neither
# libffibench nor a benchmark-only Gusset dependency belongs in the main
# module's build graph. Separate directory because tools/benchdoc refuses to run
# when bench/results holds more than one .txt — it will not guess which platform
# the README is describing — and these are a different measurement, not another
# platform's copy of the same one.
#
# The thread-pressure half runs a fixed iteration count rather than a time
# budget: each iteration dispatches 512 concurrent calls, so a 1s budget would
# take minutes to no extra effect.
CROSS_SUFFIX ?= $(shell go env GOOS)-$(shell go env GOARCH)-go$(shell go env GOVERSION | sed 's/^go//')-rust$(shell rustc --version | cut -d' ' -f2).txt
bench-crossover: linked-target-dir
	cargo build --locked --release
	cargo build --locked --release --manifest-path bench/seed/rs/Cargo.toml
	@# Each (shape, transport) arm in its own process.
	@#
	@# Not tidiness: a parallel blocking-cgo arm leaves a few hundred Ms behind,
	@# Go never destroys an M, and every arm that runs afterwards in that process
	@# pays for them. Whether that effect is large enough to matter here has not
	@# been measured — the file that was cited as proof of it turned out to have
	@# been written by two concurrent recordings, so its "degradation" was
	@# unattributable. The isolation stays because the cost is one process per
	@# arm and the alternative is unfalsifiable.
	@#
	@# One output file rather than four because benchplot and benchstat both take
	@# a set of benchmarks; record.sh assembles the arms after they all pass.
	bench/record.sh bench/results/crossover/transport-$(CROSS_SUFFIX) 1s $(BENCH_COUNT) \
		CrossoverSerial/RawCgo CrossoverSerial/Gusset \
		CrossoverParallel/RawCgo CrossoverParallel/Gusset
	@# One transport per file, and count=1.
	@#
	@# This measures threads, not time, and Go never destroys an M — so every
	@# thread a run creates is still there for the next one. Both transports in
	@# one file would make the Gusset figure inherit the ~200 threads blocking
	@# cgo just created, and count>1 would make every repetition after the first
	@# read a delta of zero against threads its own predecessor made. Either way
	@# the measurement reports that nothing happened. Isolation is the
	@# measurement.
	bench/record.sh bench/results/crossover/threads-rawcgo-$(CROSS_SUFFIX) 5x 1 \
		'ThreadPressure/10000000it/RawCgo'
	bench/record.sh bench/results/crossover/threads-gusset-$(CROSS_SUFFIX) 5x 1 \
		'ThreadPressure/10000000it/Gusset'

# The concurrency sweep the thread-cost chart is drawn from.
#
# bench-crossover answers "does blocking cgo grow the thread pool" at one
# concurrency level. The curve is what tells the two shapes apart: blocking cgo
# tracking concurrency against Gusset tracking the pool. `tools/benchplot` reads
# exactly these two files, and named this target in three of its error messages
# while the target did not exist — so the committed chart data had no documented
# way to be regenerated.
#
# Isolation matters more here than anywhere else. Both transports in one process
# makes every row report the same inherited high-water mark, and the chart then
# draws two identical flat lines, which reads as "cgo is fine". The benchmark
# withholds its absolute peak once it sees a second transport in the process, so
# a contaminated run now fails benchplot rather than charting a false result.
bench-scaling: linked-target-dir
	cargo build --locked --release
	cargo build --locked --release --manifest-path bench/seed/rs/Cargo.toml
	@# Repeated, unlike the thread-pressure target's -count=1.
	@#
	@# Repetitions of the *same* transport in one process are safe here in a way
	@# a second transport is not: the peak they inherit is one this transport
	@# caused, and benchplot takes a median. Without repetitions there is nothing
	@# to take a median of, and a single unlucky run goes straight into a chart —
	@# the first recorded sweep had a 2048-concurrency outlier at 4.5s against a
	@# true value near 0.97s, which would have drawn Gusset as 4.5x slower than
	@# blocking cgo at high concurrency. It is not.
	@#
	@# SCALING_COUNT, not 5. This target was written with -count=5, which is one
	@# below the threshold named forty lines up: benchstat will not quote a
	@# confidence interval under 6 samples, and it duly answered
	@# "need >= 6 samples" for every row of this sweep. A median with no
	@# dispersion behind it is the defect BENCH_COUNT already exists to prevent,
	@# reintroduced in the target that was added later.
	bench/record.sh bench/results/crossover/scaling-rawcgo-$(CROSS_SUFFIX) 3x $(SCALING_COUNT) \
		'ThreadScaling/RawCgo'
	bench/record.sh bench/results/crossover/scaling-gusset-$(CROSS_SUFFIX) 3x $(SCALING_COUNT) \
		'ThreadScaling/Gusset'

# The in-process dispatch comparison, crates/gusset/benches/dispatch.rs: the
# diagnostic engine against registered engines, opcode 0 and nonzero. A cargo
# arm of record.sh, so it takes the same lock, idle checks and provenance header
# as the Go arms. COUNT is the bench's --rounds (its floor is 10); BENCHTIME does
# not apply. cargo bench builds its own binary and links no Go, so this target
# needs neither the linked archive nor `cargo build --release`. A subdirectory
# for the reason bench-crossover has one: benchdoc reads bench/results/*.txt as
# one platform's copy of one measurement.
bench-dispatch:
	RECORD_DIR=. bench/record.sh bench/results/dispatch/$(CROSS_SUFFIX) 1s $(BENCH_COUNT) \
		cargo:gusset/dispatch

# Supported platform set, compile-verified. Gusset is unix-only by construction
# (POSIX pipe, sigaltstack, pthread stack accounting); lib.rs fails the build on a
# non-unix target rather than letting it fail deep inside pool::sys.
#
# x86_64-apple-darwin is in the set because docs/platforms.md and CONTRIBUTING.md
# call it supported; it was the one claim `make cross` never compiled. Adding a
# target that cannot be installed is an error, not a skip: a platform that was
# not compiled must not read as one that was.
CROSS_TARGETS ?= x86_64-unknown-linux-gnu aarch64-unknown-linux-gnu x86_64-unknown-linux-musl x86_64-apple-darwin aarch64-apple-darwin
cross:
	@for t in $(CROSS_TARGETS); do \
		echo "== cargo check --target $$t"; \
		rustup target add $$t || { echo "cross: could not install target $$t"; exit 1; }; \
		cargo check --locked -p gusset --target $$t --all-targets || exit 1; \
	done
	@echo "cross: all supported targets compile"

# Native Go fuzzing over the FFI boundary. FUZZTIME is short by default so this is
# usable locally; CI runs it longer.
FUZZTIME ?= 30s
FUZZ_TARGETS ?= FuzzCallRefusesWithoutEngine FuzzDiagnosticEngineNeverAborts FuzzBufferLifecycle
# The completion-record parsers (pipe bytes and ring slots) are internal to the
# root package, so they fuzz there.
FUZZ_TARGETS_ROOT ?= FuzzTicketReaderPipeBytes FuzzRingSlotDecode \
	FuzzTicketReaderPipeSplitReads FuzzTicketReaderPipeRecords FuzzTicketReaderFakeRing \
	FuzzTicketReaderRustRing FuzzOpcodeFromContext FuzzReadTraceCarrier \
	FuzzExtractCallHeaderDeadline FuzzHandleOpSequence
fuzz:
	@for t in $(FUZZ_TARGETS); do \
		echo "== fuzzing $$t for $(FUZZTIME)"; \
		go test -run '^$$' -fuzz "^$$t\$$" -fuzztime $(FUZZTIME) ./tests/pitfalls/ || exit 1; \
	done
	@for t in $(FUZZ_TARGETS_ROOT); do \
		echo "== fuzzing $$t for $(FUZZTIME)"; \
		go test -run '^$$' -fuzz "^$$t\$$" -fuzztime $(FUZZTIME) -fuzzminimizetime 10x . || exit 1; \
	done

# One pipeline, shared with the CI "Lint & Boundary Audits" job: gofmt, go vet,
# gussetvet, staticcheck, govulncheck, the forbidden-pattern audit, version parity, clippy
# (--all-targets, so the R3 disallowed-method rules reach test code), cargo fmt
# and cargo-deny over both the root and fuzz/ workspaces. A missing or wrongly
# versioned tool fails the step with the command that installs it; see
# tools/ci/lint.sh and tools/ci/versions.env.
lint:
	tools/ci/lint.sh

# The zero-allocation gate (R5), shared with the CI bench job. Reads raw
# `go test -bench -benchmem` output. ChannelHop is the pure-Go control and must
# stay at 0; the cgo call path measures 1 alloc/op (1 B/op) today, so
# CallNoop and SubmitWait are bounded at 1. A benchmark missing from the file
# fails the gate rather than passing it.
ALLOC_FILE ?= bench/results/allocs.tmp
ALLOC_BOUNDS ?= -max ChannelHop=0 -max GussetCallNoop=1 -max GussetSubmitWait=1
alloc-gate:
	go run ./tools/ci/benchgate allocs $(ALLOC_BOUNDS) $(ALLOC_FILE)

ci-local: build test lint docs-check
	go test -run '^$$' -bench . -benchtime=100ms -benchmem ./bench/... > $(ALLOC_FILE) || { cat $(ALLOC_FILE); exit 1; }
	@cat $(ALLOC_FILE)
	$(MAKE) alloc-gate

# R15: every performance number in the docs is generated from committed raw data.
# This target used to print "Documentation up to date" and regenerate nothing,
# while the README's figures disagreed with benchstat over the file they cited.
docs:
	@command -v benchstat >/dev/null 2>&1 || { \
		echo "benchstat not found; install with:"; \
		echo "  . tools/ci/versions.env && go install golang.org/x/perf/cmd/benchstat@\$$BENCHSTAT_VERSION"; exit 1; }
	go run ./tools/benchdoc
	@# benchplot redraws docs/img/*.svg from the same committed raw data.
	@# It shipped with a -check flag that nothing ran and no target invoked, so
	@# the charts could disagree with the data they claim to plot — and did:
	@# crossover.svg was drawn from transport samples taken before the
	@# per-arm process isolation landed.
	go run ./tools/benchplot

docs-check:
	@command -v benchstat >/dev/null 2>&1 || { \
		echo "benchstat not found; install with:"; \
		echo "  . tools/ci/versions.env && go install golang.org/x/perf/cmd/benchstat@\$$BENCHSTAT_VERSION"; exit 1; }
	go run ./tools/benchdoc -check
	go run ./tools/benchplot -check

clean:
	cargo clean
	rm -f bench/results/tmp*

