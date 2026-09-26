.PHONY: all build test bench bench-crossover bench-scaling lint ci-local docs docs-check cross fuzz clean install uninstall

export PATH := $(PATH):$(shell go env GOPATH)/bin

PREFIX ?= $(shell if [ -w /usr/local ]; then echo /usr/local; else echo $(HOME)/.local; fi)
LIBDIR ?= $(PREFIX)/lib
INCLUDEDIR ?= $(PREFIX)/include
BINDIR ?= $(PREFIX)/bin
PKGCONFIGDIR ?= $(LIBDIR)/pkgconfig

all: build

build:
	cargo build --release
	go generate ./internal/ffi
	go build ./...

install: build
	@mkdir -p $(LIBDIR) $(INCLUDEDIR) $(BINDIR) $(PKGCONFIGDIR)
	@cp target/release/libgusset.a $(LIBDIR)/libgusset.a
	@chmod 644 $(LIBDIR)/libgusset.a
	@cp internal/ffi/gusset.h $(INCLUDEDIR)/gusset.h
	@chmod 644 $(INCLUDEDIR)/gusset.h
	@sed -e 's|@PREFIX@|$(PREFIX)|g' -e 's|@VERSION@|0.0.2|g' gusset.pc.in > $(PKGCONFIGDIR)/gusset.pc
	@chmod 644 $(PKGCONFIGDIR)/gusset.pc
	@go build -o $(BINDIR)/gussetvet ./tools/gussetvet
	@chmod 755 $(BINDIR)/gussetvet
	@echo "Gusset installed successfully to $(PREFIX)"

uninstall:
	@rm -f $(LIBDIR)/libgusset.a
	@rm -f $(INCLUDEDIR)/gusset.h
	@rm -f $(PKGCONFIGDIR)/gusset.pc
	@rm -f $(BINDIR)/gussetvet
	@echo "Gusset uninstalled from $(PREFIX)"

test:
	cargo test --workspace
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

bench:
	cargo build --release
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
bench-crossover:
	cargo build --release
	cargo build --release --manifest-path bench/seed/rs/Cargo.toml
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
bench-scaling:
	cargo build --release
	cargo build --release --manifest-path bench/seed/rs/Cargo.toml
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

# Supported platform set, compile-verified. Gusset is unix-only by construction
# (POSIX pipe, sigaltstack, pthread stack accounting); lib.rs fails the build on a
# non-unix target rather than letting it fail deep inside pool::sys.
CROSS_TARGETS ?= x86_64-unknown-linux-gnu aarch64-unknown-linux-gnu x86_64-unknown-linux-musl aarch64-apple-darwin
cross:
	@for t in $(CROSS_TARGETS); do \
		echo "== cargo check --target $$t"; \
		rustup target add $$t >/dev/null 2>&1 || true; \
		cargo check -p gusset --target $$t --all-targets || exit 1; \
	done
	@echo "cross: all supported targets compile"

# Native Go fuzzing over the FFI boundary. FUZZTIME is short by default so this is
# usable locally; CI runs it longer.
FUZZTIME ?= 30s
FUZZ_TARGETS ?= FuzzCallRefusesWithoutEngine FuzzDiagnosticEngineNeverAborts FuzzBufferLifecycle
fuzz:
	@for t in $(FUZZ_TARGETS); do \
		echo "== fuzzing $$t for $(FUZZTIME)"; \
		go test -run '^$$' -fuzz $$t -fuzztime $(FUZZTIME) ./tests/pitfalls/ || exit 1; \
	done

lint:
	# --all-targets: without it clippy skips test targets, and the R3
	# disallowed-method rules go unenforced in exactly the code most likely
	# to reach for unwrap.
	cargo clippy --workspace --all-targets -- -D warnings
	cargo fmt --all -- --check
	go run ./tools/gussetvet
	go vet ./...

ci-local: build test lint docs-check
	go test -bench . -benchtime=100ms ./bench/...

# R15: every performance number in the docs is generated from committed raw data.
# This target used to print "Documentation up to date" and regenerate nothing,
# while the README's figures disagreed with benchstat over the file they cited.
docs:
	@command -v benchstat >/dev/null 2>&1 || { \
		echo "benchstat not found; install with:"; \
		echo "  go install golang.org/x/perf/cmd/benchstat@latest"; exit 1; }
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
		echo "  go install golang.org/x/perf/cmd/benchstat@latest"; exit 1; }
	go run ./tools/benchdoc -check
	go run ./tools/benchplot -check

clean:
	cargo clean
	rm -f bench/results/tmp*

