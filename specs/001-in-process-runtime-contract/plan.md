# Implementation Plan: 001 - In-Process Go-Rust Runtime Contract

## 1. Technical Context

- **Languages**: Rust (`crates/gusset`, `crates/gusset-example`, edition and `rust-version = "1.100"` in `Cargo.toml`) and Go (`go.mod` line `go 1.27`). The Go module is `github.com/bharathvbcr/gusset`.
- **Boundary**: cgo into a Rust `staticlib`. Exactly 17 `pub unsafe extern "C"` exports (`internal/ffi/exports.txt`); the hand-maintained header is `internal/ffi/gusset.h`, and the cgo bridge is `internal/ffi/ffi.go`.
- **Rust layout**: `crates/gusset/src/ffi/` (guard, status, field table, allocation exports), `crates/gusset/src/pool/` (worker pool, completion ring and pipe, queue, engine registry, lifecycle, diagnostic engine, buffers), `crates/gusset/src/alloc.rs` (counting allocator), `crates/gusset/src/header.rs` (`CallHeader`).
- **Go layout**: `gusset.go`, `handle.go`, `options.go`, `submit.go`, `wait.go`, `buffer.go`, `stats.go`, `drain.go`, `ticket_reader.go`, `tickets.go`.
- **Persistence**: none. Handles, tickets and buffers are process memory; durable evidence is committed benchmark output under `bench/results/`.
- **Enforcement tooling**: `tools/gussetvet` (go vet analyzer for R4/R5), `tools/gussetseal` (R14 sealing), `tools/benchdoc` and `tools/benchplot` (generated docs), `tools/ci/lint.sh` and `tools/ci/benchgate`.
- **Gates**: `.github/workflows/matrix.yml`, `nightly.yml`, `tip.yml`, `adopter.yml`; local entry point `make ci-local` in `Makefile`.

## 2. Constitution Check & Complexity Tracking

Judged against the nine articles of DevCouncil's `constitution.md`. That document is DevCouncil-specific; Gusset has no repo-local constitution and its equivalent rules are `AGENTS.md` (R1-R16) and `docs/PLAN.md` (I1-I6), so none was created.

| Article | Status | Compliance Details / Justification |
| :--- | :--- | :--- |
| **I: Library-First** | PASSED | Gusset is a library pair (Rust crate plus Go package); there is no daemon or CLI in the shipped surface. |
| **II: CLI as First-Class Consumer** | N/A | Gusset exposes a library API, not a capability CLI. Its scriptable surfaces are `make` targets and `tools/*`. |
| **III: Test-First** | PASSED | `AGENTS.md` rule 2 requires a new failure mode to get a test in the panic zoo or pitfall suite that fails on `main` first. Not re-verified here. |
| **IV: Fail-Closed Gates** | PASSED | Unknown header flag bits, bad pool sizes and ABI drift are refused (`internal/ffi/gusset.h`, `abi_internal_test.go`); `tests/ci` guards that CI checks fail when they did not run. |
| **V: Single Source of Truth** | PASSED | The export list lives once in `internal/ffi/exports.txt`; `tests/rust/{exports_match,constants_match,header_match}.rs` diff the other copies against it. |
| **VI: Memory & Allocator Symmetry** | PASSED | This article is the project's core: R4, R6 and R16, `Counting<A>` and the `*_free` exports. |
| **VII: Simplicity Limit (<= 3 modules)** | PASSED | The in-process surface adds no module; the public surface is pinned by `testdata/public_api.txt` and growing it needs a `DECISIONS.md` row. |
| **VIII: Anti-Abstraction** | PASSED | `AGENTS.md` rule 7 prefers deleting a feature over adding a knob; every option needs a test for both settings. |
| **IX: Observability & Auditability** | N/A | Gusset has no ledger. Its equivalent is `Stats()`, `Threads()` and `DrainLogs`, which carry Rust logs into `slog`. |

## 3. Complexity Tracking

*None for shipped work.* The open tasks in `tasks.md` add evidence and a publication step, not modules. The `internal/isolate` spike is tracked in `specs/002-out-of-process-isolation`.
