# Tasks: 001 - In-Process Go-Rust Runtime Contract

## Phase 1: Panic Firewall and Poisoning (Shipped)
- [x] [T-001] [US1] Contain panics at the FFI boundary with `ffi_guard_code`, nested `catch_unwind` around message formatting, and `ptr::write` outputs `crates/gusset/src/ffi/guard.rs` `crates/gusset/src/ffi/status.rs` tests: go test ./tests/panic_zoo/
- [x] [T-002] [P] [US1] Fail the build when a profile sets `panic = "abort"` (R2) `crates/gusset/build.rs`
- [x] [T-003] [US1] Poison a handle on `FFI_PANIC` and recover by `Close` then `Open` `handle.go` `tests/pitfalls/pitfalls_test.go`
- [x] [T-004] [P] [US1] Prove a worker survives a panicking job and counted buffer bytes are reclaimed `crates/gusset/src/pool/worker.rs` `tests/rust/rust_worker_containment.rs` `tests/rust/rust_unwind_reclaim.rs`

## Phase 2: ABI Contract (Shipped)
- [x] [T-005] [US2] Keep the 17 exports in one list and diff the archive symbol table against it `internal/ffi/exports.txt` `tests/rust/exports_match.rs`
- [x] [T-006] [P] [US2] Tie the C header to the Rust and Go constants `internal/ffi/gusset.h` `tests/rust/header_match.rs` `tests/rust/constants_match.rs`
- [x] [T-007] [US2] Refuse ABI version, size, alignment and field-offset drift at Go initialisation `abi_internal_test.go` `internal/ffi/ffi.go` `crates/gusset/src/ffi/fields.rs`
- [x] [T-008] [P] [US2] Pin the exported Go surface `testdata/public_api.txt` `public_api_test.go`
- [x] [T-009] [P] [US2] Enforce R4 and R5 statically with a custom vet analyzer `tools/gussetvet` `tools/ci/lint.sh`

## Phase 3: Bounded Concurrency, Deadlines, Cancellation (Shipped)
- [x] [T-010] [US3] Bound in-flight calls by pool size with a Go semaphore and a Rust-owned completion ring `handle.go` `submit.go` `crates/gusset/src/pool/ring.rs` `crates/gusset/src/pool/completion.rs`
- [x] [T-011] [P] [US3] Carry deadlines as relative `timeout_ns` and cancel per ticket through an atomic flag `crates/gusset/src/pool/worker.rs` `tests/rust/rust_deadline.rs` `tests/pitfalls/deadline_detach_test.go`
- [x] [T-012] [P] [US3] Soak the thread count and the goroutine-leak profile `stress_internal_test.go` `tests/pitfalls/gates_test.go`
- [x] [T-013] [US3] Drain and stop process-wide with `Shutdown`, tested in isolated binaries `gusset.go` `tests/shutdown` `tests/shutdown_incomplete`

## Phase 4: Memory Ownership and Accounting (Shipped)
- [x] [T-014] [US4] Count Rust allocations and expose `Stats` and `AdviseMemoryLimit` `crates/gusset/src/alloc.rs` `stats.go`
- [x] [T-015] [P] [US4] Allocate large inputs and results as Rust-owned buffers under a budget `buffer.go` `crates/gusset/src/pool/buffers.rs` `tests/pitfalls/buffer_budget_stress_test.go`
- [x] [T-016] [P] [US4] Detect a cross-allocator free under AddressSanitizer `tests/pitfalls/crossfree_asan_test.go` `tests/cgoprobe`

## Phase 5: Adoption and Enforcement Tooling (Shipped)
- [x] [T-017] [US5] Provide a reference engine and the umbrella recipe `crates/gusset-example/src/lib.rs` `docs/adoption.md`
- [x] [T-018] [P] [US5] Execute the adoption recipe end to end `tests/adopter/walkthrough.sh` `.github/workflows/adopter.yml`
- [x] [T-019] [P] [US5] Seal a second Rust staticlib (R14) `tools/gussetseal`
- [x] [T-020] [P] [US5] Generate benchmark docs from committed results and check drift (R15) `tools/benchdoc` `bench/results` `docs/benchmarks.md`
- [x] [T-021] [P] [US5] Guard CI workflows and Makefile wiring with Go tests `tests/ci/ci_local_test.go` `tests/ci/workflow_hardening_test.go` tests: go test ./tests/ci/ -count=1
- [x] [T-022] [P] [US5] Ship the typed Go client over a Rust reference engine `examples/uniffi_bindgen_go/README.md` `examples/uniffi_bindgen_go/testdata/golden.txt`

## Phase 6: Open Work Recorded in AGENTS.md and docs/PLAN.md
- [ ] [T-023] [US3] Record evidence that the Go service in front of tessl/sparsl runs on Gusset (Phase 1 exit criterion, no record in this repository) `AGENTS.md` `docs/PLAN.md`
- [ ] [T-024] [P] [US5] Record both adopter apps green on Linux and macOS, and tessl/sparsl showing no thread growth under the soak test (Phase 2 exit criteria) `AGENTS.md` `docs/PLAN.md` `docs/integrations.md`
- [ ] [T-025] [P] [US5] Publish the crate and Go module (Phase 3; tags `v0.0.1` to `v0.0.3` exist and `AGENTS.md` states it is not published to crates.io) `Cargo.toml` `install.sh` `gusset.pc.in`
