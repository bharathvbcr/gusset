# Verification Plan: 002 - Out-of-Process Crash Isolation

## Deterministic Verification Matrix

| Check | Command | Expected Outcome | Run during authoring |
| :--- | :--- | :--- | :--- |
| Spec schema and strictness | `devcouncil spec validate 002-out-of-process-isolation --strict --json --project-root /Users/bharath/Code/devtools/gusset` | `valid: true`, no blocking findings | yes |
| Isolate suite | `go -C /Users/bharath/Code/devtools/gusset test ./internal/isolate/` | passes | no (needs the Rust archive) |
| Budget behaviour | `go -C /Users/bharath/Code/devtools/gusset test ./internal/isolate/ -run TestSupervisor_BudgetStopsAtMaxRestarts` | passes | no |
| CI wiring guards | `go -C /Users/bharath/Code/devtools/gusset test ./tests/ci/ -count=1` | `ok` | yes, passed |

## Not Verified Here

The Phase 4 exit criterion needs a GPU engine and a daemon; neither exists in this repository.
