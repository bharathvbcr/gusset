# Changelog

## [0.0.1] - 2026-09-20

### Phase 0 · Seed Reproduction & Firewall Baseline
- Confirmed the audited document's firewall failure: `rs_guarded_doc` uses `CString::new(msg).unwrap()`, leading to double panic and `SIGABRT` under embedded NUL byte.
- Verified that Gusset's hardened firewall catches NUL-byte panics safely, returns `ErrPanic`, records file and line, and keeps the Go process alive.
- Toolchains used: Go 1.27.1 / Rust 1.98.0 (darwin/arm64, Apple M5 Pro).
- Measured numbers: parallel call 5.07 µs/op, no-op call 20.38 µs/op, submit/wait 23.68 µs/op, large buffer 32.19 µs/op, channel hop 18.32 ns/op (0 B/op, 0 allocs/op). Committed in `bench/results/darwin-arm64-go1.27.1-rust1.98.0.txt`.

### Phase 1 · Core Gusset Runtime (v0.1)
- Implemented Rust runtime crate `crates/gusset` with strict `#![deny(unsafe_code, unsafe_op_in_unsafe_fn, improper_ctypes_definitions, missing_docs)]` crate-wide (with `#[allow(unsafe_code)]` only on `ffi` and `pool::sys`).
- Three `#[repr(C)]` types with static size/alignment assertions: `CallHeader` (40 bytes), `FfiStatus` (48 bytes), `AbiLayout` (28 bytes).
- 14 exported C functions verified against `internal/ffi/exports.txt` and `nm` output in `tests/exports_match.rs` (Rule R1).
- Worker pool with 8 MiB explicit stack size and 64 KiB `sigaltstack` (Rule R8).
- Completion pipe over `os.Pipe` waking Go netpoller without pinning OS threads (Invariant I4).
- Custom `go vet` analyzer `tools/gussetvet` enforcing `#cgo noescape` and `#cgo nocallback` on all exports (Rule R5).
- 5-case panic zoo in `tests/panic_zoo/` passed (&str, String, non-string, NUL byte, Display panic).
- 8-case pitfall suite in `tests/pitfalls/` passed (ABI drift, poisoning, deadline, migration, large buffer, alloc stats, recursion, thread-cap soak).
- All 16 rules (R1–R16) enforced by tests or lints with zero unaddressed rules.

### Phase 2 · Second-App Validation
- Adopter validation in `crates/gusset-example` under CPU-bound batch arithmetic (vector sum-and-square) exercising cooperative cancellation checks.
- Public surface diff between v0.1 and v0.2: **0 new functions** (14 Rust exports and 10 Go entry points preserved strictly).

### Phase 3 · Public Release Preparation
- Complete `Makefile` with `all`, `build`, `test`, `bench`, `lint`, `ci-local`, and `clean` targets.
- Comprehensive `README.md` with measured numbers, architectural diagrams, quickstart, and invariants.
- CI workflows in `.github/workflows/`: `matrix.yml` (multi-OS, soak, race, lint), `nightly.yml` (Miri, ASan), `tip.yml` (Go tip weekly).

### Phase 4 · Out-of-Process IPC Specification
- Documented Phase 4 out-of-process Metal GPU crash isolation architecture in `docs/ipc.md` using `iceoryx2` upstream shared memory transport.
