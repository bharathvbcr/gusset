# Technical Research & Alternatives

## 1. Options Evaluated

- **Separate process versus in-process firewall.** A GPU driver reset or OOM kill ends the process; only a process boundary contains it (`docs/ipc.md`).
- **iceoryx2 versus a custom shared-memory transport.** Gusset keeps a thin adapter over iceoryx2 and avoids maintaining its own (`docs/ipc.md` Decision Record). The upstream Go binding is the gating dependency.
- **Responses on stdout versus an inherited descriptor.** An engine's `println!` would corrupt framing on stdout, so responses use descriptor 3 (`DECISIONS.md` 2026-10-24).
- **`Pdeathsig`.** Rejected: fires on thread death (Go issue 27505) and does not exist on macOS (`docs/ipc.md`).
- **Heartbeats on the data pipe.** Rejected for now: the ping would contend with a large response write and can false-kill; a side-channel ping would stay green while an engine thread is stuck (`docs/ipc.md`).
- **In-library supervisor for the public handle.** Declined; recovery is `Close` then `Open` (`DECISIONS.md` 2026-10-24). The isolate supervisor does not reopen that decision (2026-11-04).

## 2. Benchmarks & Memory Impact

- `internal/isolate/bench_internal_test.go` holds isolate benchmarks, and `tests/ci/benchmark_lists_test.go` requires each benchmark to be gated or manual-only. No isolate figure is quoted here.
- The JSON transport copies and base64-encodes every payload, so it says nothing about iceoryx2's cost (`docs/ipc.md`).

## 3. Navigation Evidence

- Waylode Insights and `waylode_collision_risk` returned `ok: true` with no collisions (one worktree).
- DevMap's index is built but stale against the source tree (`source_freshness: false`, generation 1056), so no DevMap query was used as evidence. Symbols and tests here were located with `rg` and direct reads, not graph-confirmed. That gap stands.

## 4. Unverified

- `go test ./internal/isolate/` was not run (needs the built Rust archive; this checkout has no `target/`). The 93 is a function count.
- No test named for the hello-frame version refusal was found; the check exists at `internal/isolate/proc.go` line 240 only.
- That `ErrBudgetExhausted` is what the supervisor returns once restarts are spent is taken from `internal/isolate/supervisor.go` comments and `TestSupervisor_BudgetStopsAtMaxRestarts`'s name, not from a run.
- The Phase 4 exit criterion (a GPU engine surviving an induced Metal fault behind a daemon) has no record in this repository.
- The upstream iceoryx2 Go binding's current state was not re-checked; `docs/ipc.md` dates its claims 2026-09-18 and 2026-10-24.
