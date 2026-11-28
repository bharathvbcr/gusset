# Gusset Specifications

7-layer specifications in the DevCouncil format (`devcouncil spec`), describing Gusset as it is plus the open work that `AGENTS.md`, `docs/PLAN.md` and `docs/ipc.md` record. Gusset has no repo-local constitution; each `plan.md` judges the spec against the applicable articles of DevCouncil's `constitution.md` and marks the rest N/A. The repository's own rules are `AGENTS.md` (R1-R16) and `docs/PLAN.md` (I1-I6).

## Active Specifications

| ID | Title | Scope | State | Entry |
|---|---|---|---|---|
| `001` | In-Process Go-Rust Runtime Contract | Panic firewall, poisoned handles, 17-export C ABI and its drift checks, bounded pool, deadlines and cancellation, allocator accounting, adoption and CI enforcement tooling | Shipped; three exit-criteria or publication tasks open | [`001-in-process-runtime-contract/spec.md`](001-in-process-runtime-contract/spec.md) |
| `002` | Out-of-Process Crash Isolation | `internal/isolate` worker protocol and supervisor (spike), plus the specified `gusset-ipc` daemon | Internal spike implemented; heartbeats, Buffer carrier, shared-memory transport and the Phase 4 exit demonstration open | [`002-out-of-process-isolation/spec.md`](002-out-of-process-isolation/spec.md) |

## Validation

The gate is `make check-specs`, which runs `devcouncil spec validate --all --strict` from the repository root and is part of `make ci-local`. It needs the `devcouncil` binary on `PATH` and fails loudly without it; it is not run in GitHub Actions. To validate one spec:

```bash
devcouncil spec validate 001-in-process-runtime-contract --strict --json --project-root "$PWD"
devcouncil spec validate 002-out-of-process-isolation --strict --json --project-root "$PWD"
```
