# Feature: 002 - Out-of-Process Crash Isolation (Phase 4 Spike)

**Status**: Active Specification | **Canonical Owner**: Gusset (`github.com/bharathvbcr/gusset`) | **Implementation State**: Specified in `docs/ipc.md`; the Handle contract over a process boundary is prototyped in the internal package `internal/isolate` (not public). The shared-memory transport and the public `gusset-ipc` module are not built.

## 1. Problem Statement & Motivation

The in-process firewall of `specs/001-in-process-runtime-contract` survives panics, stack overflows and concurrency faults. It cannot survive what the operating system answers with `SIGKILL` or `SIGABRT`: an out-of-memory kill, a segfault in unsafe engine code, or a GPU driver reset under a Metal or CUDA engine (`docs/ipc.md`, Overview). For those engines the engine has to run in a worker process, and the Go service must keep serving when the worker dies.

`docs/ipc.md` specifies the target (`gusset-ipc`: an iceoryx2 shared-memory transport behind the identical `gusset.Handle` API, with a supervisor, heartbeats and a restart budget). `internal/isolate` is the spike that tests whether the Handle contract survives a process boundary before any transport work, using length-prefixed JSON frames as a stand-in. `DECISIONS.md` 2026-10-08 records the first spike and 2026-11-04 the supervisor; the public surface is unchanged by it.

## 2. User Stories & Acceptance Scenarios

### US1: A worker that dies does not take the host down (Priority: P1)

As a Go service author running a GPU-driving engine, I get errors for the calls that were in flight when the worker died, and my service keeps running (`docs/ipc.md`, "Fault containment").

**Acceptance Scenarios**:
- **Scenario 1: In-flight tickets fail with the poisoned error**
  - **Given** a `*isolate.Proc` whose worker is killed with `SIGKILL` or `SIGABRT` while tickets are in flight
  - **When** the caller waits on those tickets
  - **Then** each returns an error matching `gusset.ErrPoisoned`, and the host process is unaffected (`internal/isolate/isolate_internal_test.go`).
- **Scenario 2: A result already stored before the death is still delivered**
  - **Given** a ticket whose result the reader had stored before the worker exited
  - **When** the caller calls `Wait`
  - **Then** the stored result is returned, while later `Submit` calls are refused (`TestProc_StoredResultSurvivesWorkerDeath`, `DECISIONS.md` 2026-10-23).

### US2: A supervisor replaces a dead worker inside a budget (Priority: P1)

As an operator, I can bound how many times a worker is restarted, so a crash loop ends in an explicit refusal instead of an endless respawn.

**Acceptance Scenarios**:
- **Scenario 1: Restarts stop at the budget**
  - **Given** `isolate.Supervise(ctx, factory, policy)` with `MaxRestarts` and `Window` set
  - **When** workers keep dying within the window
  - **Then** the supervisor stops replacing them and returns `ErrBudgetExhausted`, distinct from a poison error (`TestSupervisor_BudgetStopsAtMaxRestarts` in `internal/isolate/supervisor_test.go`).
- **Scenario 2: Bad policies are refused, not defaulted**
  - **Given** a zero `Policy`, a negative `MaxRestarts`, a non-positive `Window` or a negative timeout
  - **When** `Supervise` is called
  - **Then** it refuses; `DefaultPolicy` (3 restarts in one minute) is opt-in (`internal/isolate/supervisor.go`).
- **Scenario 3: A silent worker or silent handshake is retired**
  - **Given** a worker that never sends its hello frame, or is alive but silent
  - **When** `StartTimeout` or `StallTimeout` (default 30 s each) elapses
  - **Then** the worker's process group is killed and reaped and one restart is spent (`DECISIONS.md` 2026-11-06 and 2026-11-07).

### US3: The process contract is versioned and bounded (Priority: P2)

As a maintainer, I need a host and a worker built from different revisions to refuse each other, and every frame to be size-bounded, so a corrupt or hostile stream cannot exhaust memory or be misread.

**Acceptance Scenarios**:
- **Scenario 1: Version mismatch is refused at the hello frame**
  - **Given** a worker whose hello frame carries a protocol version other than 1
  - **When** `isolate.Start` reads it
  - **Then** the start fails instead of proceeding (the check is in `internal/isolate/proc.go` at the hello read; no test named for it was found, see `research.md` Unverified).
- **Scenario 2: Oversized and truncated frames poison the stream**
  - **Given** a request frame above 64 KiB, a response frame above the result bound, or a frame cut mid-body
  - **When** the host or worker reads or writes it
  - **Then** it is refused and the stream is poisoned rather than resynchronised (`TestProc_OversizeFrameLatchesTheWorkerDead`, `TestProc_EOFMidFrameLatchesTheWorkerDead` in `internal/isolate/isolate_protocol_internal_test.go`; `TestReadFrame_RefusesOversizeBeforeAllocating`).

### US4: What the process boundary cannot carry is refused explicitly (Priority: P2)

As an adopter, I get a typed refusal, not silent divergence, when I pass a `*gusset.Buffer` to a worker-hosted handle.

**Acceptance Scenarios**:
- **Scenario 1: Buffer input is refused**
  - **Given** a `*isolate.Proc`
  - **When** `Submit` receives a `*gusset.Buffer`
  - **Then** it returns `isolate.ErrBufferUnsupported` (`internal/isolate/proc.go`, `TestParity_BufferInputDiverges`).

## 3. Success Metrics & Testability Criteria

- **SC-001**: `go test ./internal/isolate/` passes (the command `docs/ipc.md` gives). The directory holds 93 test functions by `rg` count; the suite was not run during authoring because it links the Rust archive.
- **SC-002**: The public surface does not grow: `testdata/public_api.txt` is unchanged by this feature and `internal/ffi/exports.txt` still lists 17 exports.
- **SC-003**: The parity tests run the same assertions against the in-process `*gusset.Handle` and `*isolate.Proc` (`forEachMode` in `internal/isolate/isolate_internal_test.go`).
- **SC-004**: The Phase 4 exit criterion in `docs/PLAN.md` is "tessl behind the daemon survives an induced Metal fault with the Go service still serving". It is not met and has no number to quote.

## 4. Edge Cases & Exclusions

- **Exclusion**: Heartbeats are specified, not implemented. A ping on the data pipe would share the lock held while writing a large response, and a side-channel ping stays green while an engine thread is stuck (`docs/ipc.md`, Prototype).
- **Exclusion**: `Pdeathsig` is not used (Go issue 27505; macOS has no such attribute), so a host `SIGKILL` can leave the worker running.
- **Exclusion**: `NewBuffer`, `CallBuffer` and `WaitBuffer` are not carried; a public carrier needs a third buffer backing, an ownership rule for a dead worker's segment, and a host-side budget.
- **Exclusion**: The in-process handle keeps `Close` then `Open` recovery with no supervisor (`DECISIONS.md` 2026-10-24); this supervisor lives in `internal/isolate` only.
- **Edge case**: The in-flight payload of a retired worker is not resubmitted; the specified retry belongs to the future daemon.
