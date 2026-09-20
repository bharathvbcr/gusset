.PHONY: all build test bench lint ci-local docs clean install uninstall

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
	@sed -e 's|@PREFIX@|$(PREFIX)|g' -e 's|@VERSION@|0.1.0|g' gusset.pc.in > $(PKGCONFIGDIR)/gusset.pc
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

bench:
	cargo build --release
	go test -bench . -benchtime=1s ./bench/...

lint:
	cargo clippy --workspace -- -D warnings
	go run ./tools/gussetvet
	go vet ./...

ci-local: build test lint
	go test -bench . -benchtime=100ms ./bench/...

docs:
	@echo "Documentation up to date"

clean:
	cargo clean
	rm -f bench/results/tmp*

