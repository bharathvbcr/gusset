.PHONY: all build test bench lint ci-local docs docs-check cross fuzz clean install uninstall

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
	@sed -e 's|@PREFIX@|$(PREFIX)|g' -e 's|@VERSION@|0.0.1|g' gusset.pc.in > $(PKGCONFIGDIR)/gusset.pc
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
BENCH_RESULTS ?= bench/results/$(shell go env GOOS)-$(shell go env GOARCH)-go$(shell go env GOVERSION | sed 's/^go//')-rust$(shell rustc --version | cut -d' ' -f2).txt

bench:
	cargo build --release
	go test -bench . -benchtime=1s -count=$(BENCH_COUNT) ./bench/... | tee $(BENCH_RESULTS)

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

docs-check:
	@command -v benchstat >/dev/null 2>&1 || { \
		echo "benchstat not found; install with:"; \
		echo "  go install golang.org/x/perf/cmd/benchstat@latest"; exit 1; }
	go run ./tools/benchdoc -check

clean:
	cargo clean
	rm -f bench/results/tmp*

