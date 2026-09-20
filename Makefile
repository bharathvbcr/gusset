.PHONY: all build test bench lint ci-local docs clean

all: build

build:
	cargo build --release
	go generate ./internal/ffi
	go build ./...

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
