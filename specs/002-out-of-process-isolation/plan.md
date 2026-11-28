# Implementation Plan: 002 - Out-of-Process Crash Isolation

## 1. Technical Context

- **Language**: Go only for the spike (`internal/isolate/{proc,serve,supervisor,wire}.go`). It wraps the public `*gusset.Handle`; no Rust export and no Go entry point is added.
- **Transport (spike)**: length-prefixed JSON frames, a 4-byte big-endian length then a JSON body (`writeFrame` in `internal/isolate/wire.go`). Requests go to the worker's stdin; responses go on a protocol pipe appended to `cmd.ExtraFiles` and named in `GUSSET_ISOLATE_PROTOCOL_FD`, so the engine keeps its own stdout.
- **Transport (specified, not built)**: iceoryx2 shared memory behind the identical `Handle` API (`docs/ipc.md`). The Go binding for iceoryx2 is being contributed upstream (`AGENTS.md`; `docs/ipc.md` Decision Record).
- **Persistence**: none. Restart budgets are in-memory per supervisor.
- **Hosts the spike was designed for**: GPU-driving engines (tessl on Metal). No GPU run is recorded in this repository.

## 2. Constitution Check & Complexity Tracking

Judged against DevCouncil's `constitution.md` as in `specs/001-in-process-runtime-contract/plan.md`; no repo-local constitution exists or was created.

| Article | Status | Compliance Details / Justification |
| :--- | :--- | :--- |
| **I: Library-First** | PASSED | `internal/isolate` is a library package with no CLI or daemon yet. |
| **II: CLI as First-Class Consumer** | N/A | No capability is exposed through a CLI; the specified daemon (`tessld`) is future work. |
| **III: Test-First** | PASSED | The package carries 93 test functions beside 4 source files (`rg` count); order of writing not re-verified. |
| **IV: Fail-Closed Gates** | PASSED | Zero or invalid policies are refused, a frame over its bound is refused, a worker that misses the handshake is killed and charged a restart (`internal/isolate/supervisor.go`). |
| **V: Single Source of Truth** | PASSED | The `Caller` interface is shared by `*gusset.Handle`-backed and `*Proc` modes, and the parity tests run one assertion set against both. One documented duplication: the typed-nil trace-carrier recover (`DECISIONS.md` 2026-10-23). |
| **VI: Memory & Allocator Symmetry** | PASSED | Rust memory never crosses the process boundary; buffers are refused (`ErrBufferUnsupported`) until a shared-segment ownership rule exists. |
| **VII: Simplicity Limit (<= 3 modules)** | PASSED | One internal package. The future `gusset-ipc` module plus an iceoryx2 binding would approach the limit and must be justified here before it is built. |
| **VIII: Anti-Abstraction** | PASSED | The supervisor stays internal; `DECISIONS.md` 2026-10-24 declined an in-process supervisor. |
| **IX: Observability & Auditability** | N/A | No ledger; restarts surface as errors to callers. |

## 3. Complexity Tracking

| Item | Why it is here |
| :--- | :--- |
| Process-group kill (`Setpgid`, `killTree`) | A killed worker's children would otherwise survive; recorded in `DECISIONS.md` 2026-11-07 and 2026-11-08. |
| JSON stand-in transport | Chosen only so the contract can be tested before the shared-memory binding exists; it makes no performance claim. |
