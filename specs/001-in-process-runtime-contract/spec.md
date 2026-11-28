# Feature: 001 - In-Process Go-Rust Runtime Contract

**Status**: Active Specification | **Canonical Owner**: Gusset (`github.com/bharathvbcr/gusset`) | **Implementation State**: Shipped (Phases 0-2 per `AGENTS.md`), Phase 3 in progress, three exit criteria pending evidence.

## 1. Problem Statement & Motivation

Bindings between Go and Rust exist, but nothing sits between "a binding works" and "this runs in production without taking the Go process down". A Rust panic escaping `extern "C"` aborts the whole process, a goroutine blocked in cgo pins an OS thread until the 10,000-thread cap is fatal, Rust allocations are invisible to `GOMEMLIMIT`, and a handle whose engine panicked must not keep serving calls.

Gusset is that layer. It owns the panic firewall, bounded concurrency, deadlines and cancellation, poisoned handles, ABI verification at start-up, allocator accounting, and the CI matrix that proves them (`AGENTS.md`, "Mission and working rules"). It does not own type marshalling, cgo-free calling, or IPC transport. The design plan and the six invariants I1-I6 are in `docs/PLAN.md`; the hard rules R1-R16 and the guard that enforces each are in `AGENTS.md`.

This specification describes the in-process runtime as it ships: the Rust crate `crates/gusset`, the Go package `github.com/bharathvbcr/gusset`, the hand-maintained C header, and the enforcement tooling. The out-of-process variant is specified separately in `specs/002-out-of-process-isolation`.

## 2. User Stories & Acceptance Scenarios

### US1: A panicking engine never takes the Go process down (Priority: P1)

As a Go service author embedding a Rust engine, I get an error from the call that hit a panic, and the handle refuses further work, so a bug in the engine degrades one handle instead of aborting the service (I2, R2, R3, R10).

**Acceptance Scenarios**:
- **Scenario 1: Panic payloads of every shape are contained**
  - **Given** an engine that panics with a `&str`, a `String`, a payload containing a NUL byte, a non-string payload, a panicking `Display`, or a payload whose destructor panics
  - **When** a call reaches that panic through `Handle.Call` or `Handle.Submit` plus `Handle.Wait`
  - **Then** the caller receives a `*gusset.Error` carrying the panic code and the process keeps running, as exercised by the cases in `tests/panic_zoo/zoo_test.go` (including `TestPanicZoo_NulBytePayload`, the failure that aborted the seed prototype in `bench/seed`).
- **Scenario 2: A poisoned handle refuses new work until closed and reopened**
  - **Given** a handle whose ticket completed with the panic status
  - **When** the caller submits again
  - **Then** the call returns the poisoned error without entering Rust, and recovery is `Close` then `Open` (`TestPitfall_HandlePoisoning` in `tests/pitfalls/pitfalls_test.go`; `DECISIONS.md` 2026-10-24 records that there is no in-library supervisor).

### US2: The Go and Rust sides refuse to run on a mismatched ABI (Priority: P1)

As a maintainer, I get a refusal at start-up, not memory corruption at call time, when the compiled archive and the Go package disagree on version, struct size, alignment or named field offsets (I6, R1, R12).

**Acceptance Scenarios**:
- **Scenario 1: Export set is exactly the documented list**
  - **Given** the built static archive
  - **When** `tests/rust/exports_match.rs` diffs its symbol table against `internal/ffi/exports.txt`
  - **Then** exactly the 17 listed `gusset_*` names are exported and nothing else.
- **Scenario 2: Every drift is refused and named**
  - **Given** an ABI layout whose version, size, alignment or a field offset differs from the compiled-in constants
  - **When** the Go package checks it during initialisation
  - **Then** it refuses, naming the drifted field (`TestCheckABI_RefusesEveryDrift` in `abi_internal_test.go`; `tests/rust/header_match.rs` and `tests/rust/constants_match.rs` tie `internal/ffi/gusset.h` to the Rust and Go constants).

### US3: Concurrency, deadlines and cancellation are bounded (Priority: P1)

As an operator, I can bound how many OS threads and in-flight calls a handle uses, and a call's deadline is honoured to within one unit of engine work (I3, I4, I5, R7-R11).

**Acceptance Scenarios**:
- **Scenario 1: In-flight calls never exceed the pool size**
  - **Given** a handle opened with `WithPoolSize(n)`
  - **When** more than `n` goroutines call concurrently
  - **Then** excess callers wait on the Go-side semaphore rather than on OS threads, and the thread-count metric stays under pool size plus `GOMAXPROCS` plus a small margin (soak tests in `stress_internal_test.go` and `tests/pitfalls/`).
- **Scenario 2: Deadlines cross as a relative duration and cancel cooperatively**
  - **Given** a call whose `context.Context` has a deadline
  - **When** the engine runs past it
  - **Then** the header's relative `timeout_ns` and the per-ticket cancel flag stop the job at the next work-unit boundary, and the caller returns with the context error (`crates/gusset/src/pool/worker.rs`, `tests/rust/rust_deadline.rs`, `tests/pitfalls/deadline_detach_test.go`).

### US4: Memory ownership and accounting are explicit (Priority: P2)

As a service owner, I can see Rust-side memory and size `GOMEMLIMIT` against it, and memory is only freed by the side that allocated it (I1, R4, R16).

**Acceptance Scenarios**:
- **Scenario 1: Rust allocations are counted and bounded**
  - **Given** a handle with `WithBufferBudget`
  - **When** buffers are allocated through `Handle.NewBuffer` and the budget is exceeded
  - **Then** allocation is refused, `Stats()` reports live and peak bytes, and `AdviseMemoryLimit` derives a Go heap limit net of Rust usage (`buffer.go`, `stats.go`, `crates/gusset/src/alloc.rs`, `tests/pitfalls/buffer_budget_stress_test.go`).
- **Scenario 2: A cross-allocator free is detected, not tolerated**
  - **Given** a Go-side `C.free` of Rust memory
  - **When** it is made under AddressSanitizer
  - **Then** the mismatch is reported (`TestR4_CrossFreeIsDetectedUnderASan` in `tests/pitfalls/crossfree_asan_test.go`), and `tools/gussetvet` rejects the call statically outside `tests/cgoprobe`.

### US5: An adopter can wire an engine in without widening the public surface (Priority: P2)

As a second application (DevCouncil's `dc-glob` driver, the reference engine in `crates/gusset-example`), I follow `docs/adoption.md` to build one umbrella staticlib, register an engine, and call it through the unchanged Go API.

**Acceptance Scenarios**:
- **Scenario 1: The adoption recipe runs end to end**
  - **Given** a clean checkout
  - **When** `tests/adopter/walkthrough.sh` executes the steps of `docs/adoption.md`
  - **Then** the umbrella archive links, the engine answers, and no Rust export or Go entry point is added (`testdata/public_api.txt` is the pinned Go surface, checked by `public_api_test.go`).
- **Scenario 2: A second Rust staticlib is sealed or refused**
  - **Given** a second Rust staticlib in the same Go binary
  - **When** it is linked without `tools/gussetseal`
  - **Then** the documented R14 failure modes apply; with sealing, only the prefixed C ABI is left global.

## 3. Success Metrics & Testability Criteria

- **SC-001**: `internal/ffi/exports.txt` lists exactly 17 names (counted with `wc -l`) and `tests/rust/exports_match.rs` diffs the archive against it.
- **SC-002**: `go test ./tests/ci/ -count=1` passes; it was run during authoring and reported `ok` (5.8 s).
- **SC-003**: `testdata/public_api.txt` is the committed exported surface of package `gusset`, and `public_api_test.go` fails when the package drifts from it.
- **SC-004**: `tests/panic_zoo/zoo_test.go` holds seven tests (six panic shapes plus a success-path control), each of which must keep the process alive.
- **SC-005**: Every number in the README and `docs/` comes from committed `bench/results/` files via `make docs`; `make docs-check` fails on drift (R15).
- **SC-006**: `make ci-local` is the local gate the hand-off checklist requires before a commit; it was not run during authoring.

## 4. Edge Cases & Exclusions

- **Exclusion**: Process abort, `SIGKILL` and GPU driver resets are out of scope in-process; `specs/002-out-of-process-isolation` covers them.
- **Exclusion**: Type marshalling, cgo-free calling and IPC transport are not Gusset's job.
- **Exclusion**: Anything touching Go runtime internals (`//go:linkname`, `asmcgocall`, `purego`) is refused (R13, `tools/ci/lint.sh forbidden-patterns`).
- **Edge case**: Inputs over 4 KiB cannot be passed inline; they live in a Rust-owned `Buffer` (R16).
- **Edge case**: A zero or oversized pool size, unknown header flag bits and a zero-length buffer are refused with `FFI_BAD_ARG` (`internal/ffi/gusset.h`).
- **Edge case**: `Shutdown` is process-global and one-way, so its tests run in their own binaries (`tests/shutdown`, `tests/shutdown_incomplete`).
