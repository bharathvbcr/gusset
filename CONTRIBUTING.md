# Contributing to Gusset

Thank you for your interest in contributing to Gusset! Gusset is the runtime contract for running a Rust engine inside a Go service in production.

Because Gusset is a safety bulkhead protecting Go processes from memory corruption, uncatchable panics, thread exhaustion, and descriptor leaks, all contributions must uphold strict invariants.

---

## The Core Invariants (I1–I6)

Every contribution must preserve the six core runtime invariants:

1. **(I1) Memory Ownership:** Memory is freed by the allocator that created it via exported `*_free` functions. Go never calls `C.free` on Rust memory (`gussetvet` enforced).
2. **(I2) Panic Firewall:** No Rust panic crosses the FFI boundary. A caught panic poisons the handle; subsequent calls fail fast with `ErrPoisoned`.
3. **(I3) Deadline & Cancellation:** Deadlines and cancellations are enforced inside Rust between work units using relative `timeout_ns` and per-job `AtomicBool` flags.
4. **(I4) Bounded Concurrency:** In-flight calls per handle never exceed the configured pool size. Callers park on the Go semaphore, never on an OS thread in cgo.
5. **(I5) Rust-Owned Stacks:** Heavy Rust work runs on Rust-spawned threads with an explicit 8 MiB stack, never on the caller's g0 stack (musl's default is 128 KiB). Each worker installs a guard-paged `sigaltstack` of at least 64 KiB. A stack overflow is still fatal.
6. **(I6) ABI Verification:** Go `init()` verifies ABI version, struct sizes, and alignments against Rust before the process starts serving.

---

## Toolchain & Prerequisites

Gusset supports the latest and latest-minus-one releases of both Go and Rust:
- **Go:** `1.26+` (developed on `1.27.x`)
- **Rust:** `1.97+` (developed on `1.98.x`)
- **Supported Platforms:** Unix only (`linux-gnu`, `linux-musl`, `darwin/arm64`, `darwin/amd64`). Windows is unsupported by construction.

---

## Development Workflow

### 1. Build from Source
```bash
make build
```
This compiles `crates/gusset` in release mode, generates the cgo archive hash, and builds all Go packages.

### 2. Run Test Suites
```bash
make test
```
This runs:
- `cargo test --workspace`: 20 Rust unit and integration tests (including symbol export verification and header type diffs).
- `go test -v ./...`: Go unit tests, 5-case panic zoo (`tests/panic_zoo`), and 13 pitfall tests (`tests/pitfalls`).

### 3. Run Linters and Custom Analyzers
```bash
make lint
```
This runs:
- `cargo clippy --workspace --all-targets -- -D warnings`: Clippy with strict lints (including disallowed methods like `CString::new`, `Option::unwrap`, and `expect` in FFI code).
- `cargo fmt --all -- --check`: Rust formatting.
- `go run ./tools/gussetvet`: Custom `go vet` analyzer verifying that all cgo exports carry `#cgo noescape` and `#cgo nocallback` (Rule R5), and that Go never calls `C.free` on Rust memory (Rule R4).
- `go vet ./...`: Standard Go vet.

### 4. Benchmark Verification (Rule R15)
Performance figures in documentation are **never typed by hand**. They are generated directly from committed `benchstat` runs:
```bash
make docs-check   # Verifies README.md matches bench/results/
make docs         # Regenerates the README benchmark table using tools/benchdoc
```

---

## Hard Rules for PRs

1. **No Breaking Public Surface:** The C ABI boundary is strictly 17 exported functions (verified by `tests/exports_match.rs`), and the Go API is capped at 12 public entry points. Adding a function requires an entry in `DECISIONS.md`.
2. **Break It Before You Fix It:** Every bug fix must include a test in `tests/panic_zoo/` or `tests/pitfalls/` that reproduces the failure on unmodified code before the fix lands.
3. **No Go Runtime Internals:** `//go:linkname`, `asmcgocall`, `purego`, and private runtime symbol tampering are prohibited.
4. **No New Dependencies Without Discussion:** External dependencies add maintenance burden and potential security attack surface. Propose additions in an issue first.
5. **Panic Profile is Strictly Unwind:** `panic = "abort"` is forbidden in all profiles, because `catch_unwind` cannot catch an abort (Rule R2, enforced by `build.rs`).

---

## Submitting Pull Requests

1. Fork the repository and create a feature branch (`git checkout -b feature/my-feature`).
2. Implement your changes following the rules above.
3. Ensure local verification passes: `make ci-local`.
4. Write clear commit messages referencing which invariant (I1–I6) your change protects.
5. Open a Pull Request on GitHub describing your changes, motivation, and verification steps.
