# Tasks: 002 - Out-of-Process Crash Isolation

## Phase 1: Worker Process Contract (Implemented as an internal spike)
- [x] [T-001] [US1] Serve a `*gusset.Handle` from a worker process and announce its pool size in a versioned hello frame `internal/isolate/serve.go` `internal/isolate/wire.go`
- [x] [T-002] [P] [US1] Start a worker and poison the host-side `Proc` on worker death, failing in-flight tickets with `gusset.ErrPoisoned` `internal/isolate/proc.go` `internal/isolate/isolate_internal_test.go`
- [x] [T-003] [P] [US1] Keep a result already stored before the death retrievable by `Wait` `internal/isolate/proc.go` tests: go test ./internal/isolate/ -run TestProc_StoredResultSurvivesWorkerDeath
- [x] [T-004] [P] [US3] Bound frames and poison the stream on oversize, truncated or corrupt frames `internal/isolate/wire.go` `internal/isolate/isolate_protocol_internal_test.go`
- [x] [T-005] [P] [US4] Refuse a `*gusset.Buffer` input with `ErrBufferUnsupported` and pin the divergence with a parity test `internal/isolate/proc.go` `internal/isolate/isolate_internal_test.go`

## Phase 2: Supervisor and Restart Budget (Implemented as an internal spike)
- [x] [T-006] [US2] Restart a retired worker inside a `Policy` budget with `DefaultPolicy` of 3 restarts per minute and refuse invalid policies `internal/isolate/supervisor.go` `internal/isolate/supervisor_test.go`
- [x] [T-007] [P] [US2] Bound the handshake and a silent worker with `StartTimeout` and `StallTimeout` `internal/isolate/supervisor.go`
- [x] [T-008] [P] [US2] Kill the worker's whole process group on retirement and on `Close`, and return `ErrBudgetExhausted` distinctly from poison `internal/isolate/supervisor.go` `internal/isolate/proc.go`
- [x] [T-009] [P] [US1] Benchmark the isolate path and gate benchmark lists in CI `internal/isolate/bench_internal_test.go` `tests/ci/benchmark_lists_test.go`

## Phase 3: Specified but Not Built (`docs/ipc.md`, `docs/PLAN.md` Phase 4)
- [ ] [T-010] [US2] Implement worker heartbeats on a side channel that cannot be satisfied while an engine thread is stuck, without false-killing a worker still writing a large response `docs/ipc.md` `internal/isolate/proc.go`
- [ ] [T-011] [US4] Decide the Buffer carrier: a third buffer backing, an ownership rule for a dead worker's segment, and a host-side budget `docs/ipc.md` `DECISIONS.md` `buffer.go`
- [ ] [T-012] [P] [US3] Replace the JSON stand-in with the iceoryx2 shared-memory transport once its Go binding is released `docs/ipc.md` `internal/isolate/wire.go`
- [ ] [T-013] [US1] Demonstrate a GPU-driving engine (tessl) behind the daemon surviving an induced Metal fault with the Go service still serving (Phase 4 exit criterion) `docs/PLAN.md` `docs/ipc.md`
