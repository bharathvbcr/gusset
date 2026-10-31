# Changelog

## [Unreleased]

Changes since v0.0.3. The C ABI layout is unchanged (17 exports, ABI layout version 2): the status-code change below is a change of value, not of layout, so an installed v0.0.3 library and a rebuilt Go package still agree on every struct.

### Changed

- **`WaitBuffer` (and `CallBuffer`) carry a result of 4 KiB or less that the engine returned as bytes in 64-byte aligned Go memory instead of allocating a Rust buffer for it (`ft-8a8279f5`, DECISIONS.md 2026-10-26).** The result was already in Go memory when `WaitBuffer` received it; the old path then called `BufAlloc` through cgo, copied it into Rust, and registered an `AddCleanup`. Now `Buffer.ID()` is 0 for these results (as it already was for an empty result and for a result collected from a poisoned handle), `Stats().LiveBytes` no longer counts them, and `WaitBuffer` takes no `cgoMu` for them. A result whose bytes `drainPipe` left 64-byte aligned is adopted as the `Buffer`'s own backing, with no second copy; any other is copied into aligned memory. The alignment is tested on every result at run time, and the size-class placement that makes a 4096-byte result aligned is only what decides whether the fast path is taken. Adoption is safe because nothing else holds that slice once `waitBuffer` has it (the argument is in `waitBuffer`'s comment), and the view is cut to `cap == len` so an `append` to `Bytes()` reallocates. It exists because with the copy alone `GussetWaitBufferResultSize` at 4096 B was slower than `main` beyond the PR benchgate's 10% threshold: two Go allocations and two copies of the result against one. A result over 4 KiB, and an engine's own buffer (`JobOutput::Buffer` or `JobOutput::Allocated`) of any size, is unchanged: a take buffer wrapped in place under `cgoMu`, Rust memory with a non-zero `ID()`. `Bytes`, `Free`, `Submit`/`CallBuffer` input and the `ErrClosed` answer after `Close` has begun behave as before. No performance figure is claimed here: the committed A/B is recorded separately. I1: the `WaitBuffer` write-after-free of v0.0.3 cannot recur on this path because nothing of the handle's is written. `TestWaitBuffer_CloseCannotFreeTheResultBeingFilled` was reworked to assert that, rather than the lock ordering that protected the Rust fill; the old assertion has no code left to apply to.
- **C hosts: argument refusals now return `FFI_BAD_ARG`, not `FFI_ERR` (`ft-3d60ee16`, DECISIONS.md 2026-10-24).** `gusset_handle_open` returns `FFI_BAD_ARG` for a `pool_size` over `GUSSET_MAX_POOL_SIZE`, a negative `pipe_write_fd`, and a descriptor that is not the write end of an open pipe. `gusset_submit` returns it for unknown `CallHeader` flag bits (after the poisoned, closed and shutting-down checks, so those still report themselves). `gusset_buf_alloc` returns it for a `len` of 0 or over `GUSSET_MAX_BUFFER_BYTES`. A host that tested for `FFI_ERR` on any of these must test for `FFI_BAD_ARG`; `gusset.h` now lists every refusal that returns it. Every other refusal keeps its code. Go callers are unaffected: `errors.Is(err, ErrBadArg)` is the documented check and Go validates the pool size and buffer length before it calls in. Rust callers of `Handle::open`, `submit` and `buf_alloc_published` see no change; their `Err(String)` signatures are as before.
- **`Open` returns an error for a nil `Option` instead of panicking.** `Open(nil)` and `Open(WithPoolSize(2), nil)` return an error matching `ErrBadArg` before anything is allocated. `Option` is a func type, so nil compiles; `Open` called it unchecked and panicked with a nil dereference. No new exported identifier: the error is a `*Error` with code `FFI_BAD_ARG`. The `Option` godoc says so.
- **Go's own refusals of a pool size, a buffer budget and a buffer length match `ErrBadArg` too (DECISIONS.md 2026-10-24).** `WithPoolSize` below 1 or over `MaxPoolSize` and a negative `WithBufferBudget` (all three reported by `Open`), and `NewBuffer` with a length of zero or less or over `MaxBufferBytes`, returned a plain error that matched no `gusset` sentinel, while the same mistakes through the C ABI were already `FFI_BAD_ARG`. They are now `*Error` values with code `FFI_BAD_ARG`, so `errors.Is(err, gusset.ErrBadArg)` holds, and `ErrGeneric` does not. **The error string changes** for these five refusals: the prefix `gusset: ` becomes `gusset error [4]: ` (`Error.Error()` renders `gusset error [<code>]: <msg>`), and the text after it is unchanged, so `strings.Contains` on the old wording still matches but a comparison of the whole string, or `strings.HasPrefix(err.Error(), "gusset: ")`, does not. No new exported identifier and no change to `testdata/public_api.txt`; the `ErrBadArg`, `Open`, `WithPoolSize`, `WithBufferBudget` and `NewBuffer` godoc say so.
- **DevMap MCP configuration unpinned from main checkout root; agent guides use `devmap gap-record` (`ft-197f6d41`, PR #40, DECISIONS.md 2026-10-27).** Removed `--root` pin in `.mcp.json`, `.claude/mcp.json`, and `.cursor/mcp.json` so the server serves any worktree by `repo_path`. Regenerated `CLAUDE.md` and `.cursor/rules/devmap.mdc` via `devmap build --manifest --guides`; `CLAUDE.md` now names `devmap gap-record` instead of the write-restricted `.devcouncil/codeintel/sessions/gaps.jsonl`.

### Tests

- **`TestRead_SteadyFramesAfterTheProbeAreAllDelivered` judges an undelivered frame from the clock, so a host stall no longer fails a correct reader (card ft-735a4757; tests only, no I1-I6 invariant touched).** Matrix run 40648589935, attempt 1, `Unit & Pitfalls (macos-latest)`: ticket 1 delivered, tickets 2 to 20 got `worker process exited (read |0: i/o timeout)`, in 0.07 s. The same test takes 0.12 to 0.14 s when it passes (20 frames 5 ms apart), so the reader ended before the writer could have finished, with only frame 1 in: the probe armed after frame 1 expired because frame 2 did not arrive for 50 ms. That is `queuedFrameProbe` doing what `proc.go` says, against a writer the host held for that long, not a probe that was not re-armed. A frozen process reproduces it: SIGSTOP for 70 ms about every 1.3 s over 400 runs of the old test failed 28 of them (36 freezes), all with the `i/o timeout` shape; 500 runs at an ambient load of 6 to 8 and 400 runs of an instrumented copy under 24 `yes` burners (load up to 30) did not fail once, so what fails it is a stall of the whole process, not CPU contention. Deleting the re-arm (`proc.go`, the `if probed` block) fails the old and the new test alike at about 8 frames in, 0.3 to 1.5 ms after the last frame it delivered. The writer now returns when each write began, and `stalledNotLost` reads the attempt: the reader dies at the earliest one probe after the last frame it delivered, so a death sooner than that, or with the writer's own writes less than half a probe apart, is the one-shot probe and fails at once, with the distance, the frame count and the widest write gap in the message. A writer gap of half a probe or more is a stall, and so is a whole probe of silence after the last or second to last frame with the writer on schedule; a whole probe of silence in mid-stream with the writer on schedule is not (a freeze shows as a writer gap first, and what it matches is a reader that sat on frames already in the pipe), so it fails. A stall: the attempt runs again, up to 10 (the `Shutdown` tests' bound, DECISIONS.md 2026-10-27), then `hostprobe.Skip`, which fails under `GUSSET_REQUIRE_PROBES=1`. Same freeze harness on the new test: 400 of 400 pass, 36 of them after a second attempt, every one reported as a writer stall (an earlier version without the mid-stream rule passed 60 of 60 against a reader that slept 60 ms after a re-arm in about a third of its attempts, by retrying until an attempt was clean; with it that reader fails 25 of 60 and 20 of 60 in the two `GUSSET_REQUIRE_PROBES` modes); the re-arm deleted: 100 of 100 fail as "the probe was not re-armed", none as a stall. Known limit, not fixed here: a re-arm window cut to between about 12 and 50 ms is invisible to this test (the frames are 5 ms apart over 100 ms), as it was to the one-shot test on a quiet host; catching it needs a planned gap of about 0.8 of a probe in the stream. Not done: `TestDrain_SteadyFramesAfterTheWorkerExitsAreAllDelivered` has the same shape with a window of `protocolDrain` plus the probe (250 ms), so it needs a freeze about five times longer; it is unchanged and no stall has been seen there.
- **`bench/record_test.sh` reaps the processes it signals before it removes its scratch tree, and signals only the ones it started (card ft-b58e1c66; tests only, no I1-I6 invariant touched).** On PR #44's `ubuntu-24.04-arm` leg every case printed `ok` and the step then failed with `rm: cannot remove '.../sigTERM.wGnP6x': Directory not empty`. `mutant_case` runs `signal_case` in a subshell, so the stat feed that case started was recorded in a variable the subshell took with it: `cleanup` never signalled it, and it went on writing `stat.new` into its case directory while `rm -rf` ran. Every process the run starts is now registered in a file under the scratch tree (`note_pid`, with `forget_pid` for one a case has reaped, in place of the `PIDS=` resets that made `zombie_case` strand two `python3 ... sleep(300)` helpers each run), and `cleanup` signals them, waits (bounded, `wait_gone`) for each to be gone, escalates to KILL and waits again. Each entry carries the process's start time (`ps -o lstart=`; the stub `go` writes its own and its grandchild's), and a pid whose start time no longer matches, because the kernel reused the number, is neither signalled nor waited on. `cleanup` ignores INT, TERM and HUP while it runs, skips an entry that is not a decimal pid above 1 (a `0` made `kill -TERM 0` signal its own group), reports a survivor or an `rm` failure without replacing a failure status already set, and still exits with the run's own status. Four cases drive a second run of the script and judge its cleanup: `straggler_case` (three writers, one from a subshell, one ignoring TERM), `reused_pid_case`, `malformed_entry_case` and `signal_during_cleanup_case`.
- **The `Shutdown` incomplete-drain tests prove their job was still running, or say the host stalled (card ft-a0485e94, item 4; no I1-I6 invariant touched).** Diagnostic mode 17 holds 250 ms, its u8 ceiling, and both tests read it as still running at every `Shutdown`. A planted stall of 300 ms before the first call failed a correct `Shutdown`, and one of 170 ms before the clamp check let an unclamped `Shutdown` pass, because the wrapped 99 ms budget outlasted the job's remainder. Both now take the instant before `Submit` as the floor of the job's life (the engine sleeps at least 250 ms from a later instant). A drain that returns before `submit + 250 ms` certainly met a running job, so an unexpected answer there fails. After it the answer is the host's: the Rust test discards the attempt and runs again (up to 20), the Go test runs again before the first `Shutdown` (up to 10) and then uses `hostprobe.Skip`, which fails under `GUSSET_REQUIRE_PROBES=1`. The Go clamp check is `MaxDrain+1ms`, which wraps to 0 ms, so an unclamped call fails for any job still running at its start; it is judged from the instant the call itself began, which `shutdownWithin` now returns, because a clamped call outlasts the job and cannot be checked afterwards. A stall inside the call, between that instant and Shutdown's read of the in-flight count, can still let an unclamped call pass; `boundary_inputs_internal_test.go` checks the clamp deterministically. A `Shutdown` that returns 0 with work in flight fails both tests and one without the `MaxDrain` clamp fails the Go test; the clamp mutant with a 170 ms stall passes the old Go test and fails the new one. No diagnostic mode was added (`DECISIONS.md` 2026-10-27).
- **The Rust deadline test judges what the engine ran, not a wall-clock margin (card ft-a0485e94, item 2; I3).** `deadline_alone_stops_a_multi_unit_job_within_one_unit` (now `deadline_alone_cancels_a_multi_unit_job_with_deadline_exceeded`, because it no longer asserts "within one unit") asserted `elapsed < 60 ms + 400 ms`, a bound a stalled host exceeds with a correct deadline. `tests/rust_deadline.rs` (its own binary, because it registers engines in the process-global opcode registry) runs an engine that counts the units it ran, each a sleep of at least 10 ms behind `ctx.check()`. At most `deadline / unit` = 6 units can start before a 60 ms deadline whatever the load, since load only delays a check, so `a_deadline_stops_a_multi_unit_job_between_units` asserts that count. A deadline noticed 80 ms late ran 13 units and failed it, and the old test passed it. `an_expired_deadline_never_enters_the_engine` replaces the `< 400 ms` bound of `deadline_edges_expire_at_once_or_never_early` the same way, and fails with the pre-start check removed. Both old tests keep their result and lower-bound assertions, which still fail with the deadline disabled. Noise floor of the bound it replaces: 60 runs at load about 20 measured 60.1 to 71.7 ms.
- **`waitbuffer_adopt_internal_test.go` covers adopting a `WaitBuffer` result.** `TestAdoptHeapBuffer_AdoptsAlignedAndCopiesUnaligned` places its slices itself. `TestWaitBuffer_EverySizeIsAlignedAndCorrectWhetherAdoptedOrCopied` runs every size from 1 to 4096 B and asserts from the slice the waiter was handed that it is adopted exactly when aligned. `TestCallBuffer_EverySizeIsAlignedAndCorrect` does the same through `CallBuffer`, with sizes over 4 KiB. `TestWaitBuffer_ResultsDoNotAliasEachOther` runs 320 concurrent jobs, then scribbles and appends through each result while checking every other. `TestWaitBuffer_AlignedResultCostsOnlyTheBufferHeader` asserts a 4096 B `WaitBuffer` allocates at most one more object than `Wait`. With adoption switched off the first, second and fifth fail (the allocation test reports 3 allocs/op against `Wait`'s 1); the aliasing test is a guard and passes either way.
- **`TestTakeResult_CloseCannotFreeTheViewBeingUsed` holds the Rust-owned half of I1 deterministically.** With `WaitBuffer`'s inline result in Go memory, `TestWaitBuffer_CloseCannotFreeTheResultBeingFilled` no longer asserts that `Close` waits on `cgoMu`, and no other test did for the results that are still Rust memory: a result over 4 KiB, promoted by the worker onto a take buffer, which `Wait` copies out and `WaitBuffer` wraps, both under `cgoMu`. `TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose` and the stress hammers cover it only when the scheduler lands a `Close` in the gap. The new test starts a `Close` through `testHookBeforeTakeView` (in `wait.go`, set only by tests) while the take buffer is about to be used, for 4097 and 16384 bytes through `Wait` and `WaitBuffer`, and fails if `Close` finishes before the copy or the wrap. Releasing `cgoMu` before the copy or the wrap, the shape of the v0.0.3 write-after-free, fails all four subtests. The copy of a 49-byte to 4 KiB take result in `drainPipe` holds `cgoMu` from `ffi.Take` to the free in one critical section and is not hooked.
- `ErrShutdownIncomplete` is exercised end to end. `tests/rust_shutdown.rs` submits a non-cooperative diagnostic job (mode 17), waits for the engine's own gauge to show it running, calls `shutdown(10ms)`, asserts work remains and `gusset_shutdown` returns `FFI_ERR`, then re-arms. `tests/shutdown_incomplete` asserts `Shutdown` returns `ErrShutdownIncomplete` for the same work, and that a negative budget is zero and a budget past `MaxDrain` is capped rather than wrapped, each with work in flight. A `Shutdown` that returned nil with undrained work passed every earlier test.
- The two cancel messages Go maps to `ErrShutdown` and `context.Canceled`, `cancelled: Shutdown` and `cancelled: Explicit`, are pinned in `tests/rust_shutdown.rs` beside the pinned `cancelled: DeadlineExceeded`.
- A second test catches a handle that reads its completion descriptor instead of taking it (`ft-3d60ee16`, criterion 11). Replacing the `swap(-1)` in `release_pipe_with` with a `load` was caught by one test, the close-count test. `a_second_release_cannot_close_the_descriptor_it_gave_up` finds it through the effect: the first release hands back the number without closing it, the second runs the real `close(2)`, and the descriptor must still carry a write to the read end. It never frees and re-acquires a number, so it cannot race the module's parallel tests. A second `libc::close` inside `close_fd` stays uncaught, by design (`sys.rs`, `close_fd`). Test-only; no production change.
- `TestPitfall_OpenNilOptionIsRefusedNotPanicked`, `TestFFI_ArgumentRefusalsAreBadArg` and the Rust `argument_refusal_tests` pin the two changes above, and that the refusals outside the list stay `FFI_ERR`.

### Dependencies

- **libc 0.2.191 in both lockfiles, and `pool::sys` uses its Apple `F_SETNOSIGPIPE` and `F_GETNOSIGPIPE` (`ft-d3d3888d`).**
  - `Cargo.lock` moves from 0.2.189 and `fuzz/Cargo.lock` from 0.2.190. Both `Cargo.toml` files still say `"0.2"`.
  - rust-lang/libc#5609 added the two constants in 0.2.191, with the values gusset had hard-coded (73 and 74). The local copies are removed.
  - The `fcntl` calls are unchanged, as are the export count and the ABI.

### 2026-10-27 · The allocator counters add with `fetch_add` and sit on their own cache lines (card ft-8a8279f5, R10)

Rust allocator accounting and its benchmarks; no export, ABI or public-surface change (`AllocStats` keeps its layout, 17 exports, 13 Go entry points). Protects I1: `AdviseMemoryLimit` reads the live total these counters keep, and none of the three may wrap or read small after reaching the top. **Landed in PR #38 (`8ef9659`, protects I1) after an interleaved A/B (`RECORD_VARIANTS`, `BenchmarkBufferAllocParallel`) against the instrument base showed it significantly faster.**

#### Changed
- **Adds no longer run a compare-and-swap loop.** `LIVE_BYTES`, `PEAK_BYTES` and `ALLOC_COUNT` were three adjacent statics, each updated by a `compare_exchange_weak` loop that retried under parallel allocation and shared a cache line. They are now one `Counters` value: the live total and the count (both written by every allocation) share one 128-byte-aligned line, the peak has another, and `COUNTING_ACTIVE` a third. An add is one load plus one `fetch_add`; the peak is raised with `fetch_max`, still only when the new total is higher. The decrement stays a saturating compare-and-swap loop, because no headroom argument covers a subtraction and a `fetch_sub` past zero would wrap.
- **Saturation is kept, and its bound moves.** A `fetch_add` that checks first cannot wrap only if there is room for the adds already in flight, so each counter now saturates at `usize::MAX / 2` (`i64::MAX`) and a reader sees `usize::MAX` from there on. The argument is on `add_saturating`: a `fetch_add` happens only after a check that saw the value below the bound and adds at most 256 GiB, so the stored value stays below `usize::MAX` for up to 2^25 threads adding at once. Nothing observable changes: no heap or count approaches `i64::MAX`, and `AdviseMemoryLimit` already clamped `LiveBytes` to `math.MaxInt64`.
- **Reading is unchanged.** `get_alloc_stats` and `gusset_alloc_stats` are still three independent relaxed loads (live, peak, count), not a snapshot; `peak` can still trail `live` for the moment between an add and its peak update. Both are now written down on `Counters` and `get_alloc_stats`.

#### Benchmarks
- **The interleaved A/B against the instrument base is in `bench/results/gp-feat-001/r10-*`.** `BufferAllocParallel` is significantly faster and `GussetCallParallel` is `~`; summarise each file with `benchstat -ignore ld -col 'variant@(base cand)' -filter '.unit:sec/op'`.
- **`BenchmarkBufferAllocParallel` and `crates/gusset/benches/alloc_counters.rs` put the counters under parallel load**, which nothing did before (`make bench-alloc-counters` records the second). The Go arm is the one to A/B: `bench/record.sh` refuses `RECORD_VARIANTS` on cargo arms.

#### Tests
- The saturation, floor, peak and count properties run on private `Counters` values from several threads with a reader sampling between operations, so they are deterministic, Miri-able and cannot disturb the lib tests that share the process-wide counters. They fail against a plain `fetch_add` (wraps from `usize::MAX - 10000`), an uncapped compare-and-swap (stores above the bound), a `fetch_sub` decrement (wraps below zero), a peak that is never raised, and counters that share a line.

### 2026-10-27 · A/B records of the rejected perf candidates R6, R11, G8 and R8 (card ft-8a8279f5)

Recorded results only; no code, export, ABI or public-surface change.

#### Benchmarks
- **`bench/results/gp-feat-001/` now holds the interleaved A/B records behind four rejected candidates.** R6, R11 and G8 show no significant difference on `GussetCallNoop` or `GussetCallParallel`. R8 makes `WaitBuffer` faster from 49 B to 4 KiB but plain `Wait` slower over the same sizes, past the 5% bar. The owner rejected all four on 2026-10-27. Wave 5 tagged those tips `archive/perf/*` and deleted the branches; the README still names each tip and the command that reproduces it.

### 2026-10-26 · Quiet-host baseline for the dispatch, deadline, Discard and Stats benchmarks (card ft-568a30e2, criterion 4)

Recorded results only; no code, export, ABI or public-surface change, and the README's benchmark table is unchanged.

#### Benchmarks
- **`bench/results/dispatch/` holds the first recording of the benchmarks PR #17 added.** `make bench-dispatch` recorded the Rust dispatch pair (diagnostic vs registered engine on opcode 0, and a nonzero registered opcode) in interleaved rounds, and `make bench` recorded the Go suite including `BenchmarkDeadlineExpiry`, `BenchmarkDeadlineInFlight`, `BenchmarkDiscard` and `BenchmarkStats`. Both ran through `bench/record.sh` with its idle floor met before and after. The Go file sits in the subdirectory rather than replacing the file the README is generated from, because two of the README's rows came out noisier in this recording; the directory's README says which and why.

### 2026-10-26 · A result-size sweep for the egress paths (card ft-8a8279f5, R8/G7 instrument)

Benchmarks only; no runtime, export, ABI or public-surface change. No results are recorded here.

#### Benchmarks
- **`bench/result_size_test.go` measures the result egress paths across the size thresholds.** `BenchmarkGussetCallResult`, `BenchmarkGussetSubmitWaitResult` and `BenchmarkGussetWaitBufferResultSize` run the diagnostic echo at 0, 48, 49, 256, 1024 and 4096 bytes (and 4097 for the two that can submit it), straddling `INLINE_RESULT_MAX` (48) and the 4 KiB promotion threshold. Until now no benchmark returned a result between a no-op and 64 KiB, so the R8 (worker-side promotion) and G7 (heap-backed `WaitBuffer`) candidates had nothing to be A/B'd against.

### 2026-10-26 · `docs/integrations.md` records the Manvi and GitPulse survey (card ft-83151a08)

Documentation and changelog only; no runtime, export, ABI or public-surface change (17 exports, 13 Go entry points). No I1 to I6 invariant is touched. The survey was read-only: nothing was built or run in Manvi or GitPulse, and `git` against those checkouts was refused to the session, so the files read are their working trees.

#### Documentation
- **Manvi's `gusset-check` is read and recorded as inferred, not run.** It wraps DevCouncil's `gussetfn.SelfTest` behind a `gusset.MaxPoolSize` guard (`manvi/gussetcheck/check_cgo.go`), so it is the same check the page already describes for DevCouncil. It is a gate in Manvi's `verify.sh` and release build. Every `gusset.` identifier `gussetfn` uses exists in this tree and `internal/ffi/exports.txt` is unchanged since Manvi's pin (`320c56c`, 142 commits behind), so it should still print `ok`; running it needs a cgo build and was not done.
- **GitPulse's Go-to-Rust surface is one check program.** `go/` holds the same check behind a `gusset` build tag; the product (`src-tauri`) is Rust and reaches Go and Rust through a `manvi serve` stdio child, the `devmap` binary and vendored `devmap-*` crates. Neither repository has `import "C"` or `#cgo` in its Go. GitPulse's CI pin (`92c2056`) is 309 commits behind.
- **Manvi does run `devmap` from long-lived Go processes, and the page's earlier conclusion changes in one place.** The agent harness and `manvi serve` start one `devmap` process per query, up to 26 for one `graph_context` call, through DevCouncil's `dc/devmap`. A warm pool would therefore have a repeated-query caller, and would belong in `dc/devmap`. The page does not recommend building it: no measurement of real sessions exists, and Manvi deliberately reads `status` once per call, which a warm process must keep. DevCouncil's own paths and the verdict on running `devmap` through Gusset are unchanged.
- **The pending list is rewritten item by item**: two items done, one surveyed and not run, one still pending (is the pool worth building), and the pins in both apps reported as behind and left alone. Section headers keep their 2026-09-27 timings; the new sections say they were surveyed on 2026-10-26.

### 2026-10-26 · The parallel crossover rows are labelled with the recorded GOMAXPROCS, not a typed "18 goroutines"

Docs tooling and tests; no runtime, export, ABI or public-surface change. No I1 to I6 invariant is touched: this protects R15 (every number in docs is generated). `make docs-check` is clean and `docs/choosing.md` is byte-identical, because the committed recording was made at GOMAXPROCS 18.

#### Changed
- **`tools/benchplot` reads the parallel arm's goroutine count off the recording.** The table header and the crossover table's row were the literal `18 goroutines`. `BenchmarkCrossoverParallel` is `b.RunParallel` with the default parallelism (`bench/seed/go/crossover_test.go` never calls `SetParallelism`), so it runs GOMAXPROCS goroutines, and `go test` writes that GOMAXPROCS on every benchmark name as the `-N` suffix. A recording from a host with another core count would have been labelled 18 and passed `make docs-check`, which compares the generated block only with itself. `benchfile.Row` now keeps the suffix it used to discard (`Procs`), `File.ProcsOf` returns the one value a prefix's rows share, and `benchplot` builds both labels from it. It refuses, naming the lines, a recording whose parallel rows disagree, have no suffix (`go test` omits it at GOMAXPROCS 1, and a hand-edited file looks the same), or are absent.
- The cost-per-call table moved from `renderDocTables` into `renderCostTable`, so the label has one owner and the table can be tested without the scaling recordings.

#### Tests
- `TestParallelLabelFollowsTheRecordedGOMAXPROCS` renders both generated tables from a recording with `-8` suffixes and asserts `8 goroutines` appears and `18 goroutines` does not. `TestParallelLabelRefusesMixedGOMAXPROCS`, `TestParallelLabelRefusesRowsWithoutASuffix` and `TestParallelLabelRefusesARecordingWithoutTheParallelArm` pin the refusals. `TestParallelLabelOfTheCommittedRecordingIs18` pins the committed data. Against a `parallelGoroutines` that returns the typed `18 goroutines`, the first four fail and the fifth passes.

### 2026-10-26 · A failed R14 harness run leaves its stderr in the job log and as an artifact

Harness script, workflow and tests; no runtime, export, ABI or public-surface change. No I1 to I6 invariant is touched: this is R14 evidence, so that the next red R14 job says why.

On PR #24 (a docs-only change) the `R14 sealed staticlibs (ubuntu-latest)` job failed once in `asan-rust run` (Matrix run 40383974940) with only a path to `host.stderr`. The runner was gone, nothing was uploaded, and the same job passed on the rerun, so the cause cannot be recovered. The flake itself is not diagnosed here.

#### Changed
- **`expect_pass` in `bench/r14/run.sh` prints what the binary wrote to stderr when it fails.** It is the only helper in the script that sent output to a file and failed with the path alone; every scenario with a runtime check (sealed, race, cgocheck2, asan, asan-rust, nolto, skew) goes through it. A failure now prints the exit status (above 128 is a signal), then the last 200 lines of the file, each cut to 500 bytes and prefixed `R14-STDERR:`, before the unchanged `FAIL:` line and exit status 1. A passing run stays quiet. POSIX `sh`, `tail -n`, `cut -c` and `sed`, so macOS and Linux behave the same.
- **The `r14` and `r14-musl` jobs in `matrix.yml` upload `bench/r14/target/**/*.stderr` on failure** (`if: failure()`, the same pinned `actions/upload-artifact` as the fuzz jobs; `r14-stderr-<os>` per matrix leg, `r14-musl-stderr`). The Rosetta leg writes under the same directory, so its files replace the native leg's of the same name.

#### Tests
- `TestEveryR14JobUploadsItsStderrOnFailure` holds every job that runs `bench/r14/run.sh` to that upload, after the harness step, with an artifact name that carries the matrix leg; `TestPlantedR14JobWithoutItsStderrIsCaught` plants the violations (no upload, no `if`, success only, wrong files, one name for every leg, upload before the harness).
- `TestR14ExpectPass*` extract run.sh's own `fail` and `expect_pass` and run them under `sh -eu` against stub binaries: a failing run's stderr and exit status reach the output and the exit status is still 1; a passing run prints nothing; 500 lines of stderr print only the last 200, cut to 500 bytes; a silent SIGSEGV says its stderr was empty and reports 139.

### 2026-10-26 · Three tests and gates that could pass without examining what their names claim (card N2, items 1, 3, 5)

Tests only; no runtime, export, ABI or public-surface change (17 exports, 13 Go entry points). Each change below was shown to fail on a planted mutation or bypass of what it claims to check, and to pass on the real tree.

#### Tests
- **`TestBug_SemaphoreInFlightBreachOnTimeout` now abandons through the path its name claims, and watches the job's whole time in the engine (I4).** It abandoned the first job with `Discard`, so it never reached the ctx-deadline abandon branch of `waitInternal`; planting `s.releaseSemLocked(ticket)` in that branch passed it (seven other tests, among them `TestStress_MassiveConcurrentChaos` and `FuzzHandleOpSequence`, failed on it). The first caller now leaves through `Wait` with a deadline, once a second handle's read of the process-wide engine gauge shows the job is running (a Wait deadline reaches Rust as a cancel, and a job cancelled before it starts rightly returns its permit). The second caller then waits for a permit under a 30 s hang guard, not a 50 ms window, and must be handed it no earlier than the job's 250 ms hold after the submit; the engine's peak must stay 1. A release delayed by 60, 120 or 200 ms used to pass because the 50 ms window saw only the start of the job: each now fails 10 runs of 10 (`a second job was admitted 131ms after a 250 ms job was submitted, while it was still running in the engine (I4 breach)`), as does an immediate release (12.9 ms). No upper bound on when the permit arrives is asserted. If a stall of this process lets the job finish before the Wait, the scenario is retried up to three times before the test skips (fails under `GUSSET_REQUIRE_PROBES=1`). `Discard`'s own permit retention stays pinned by `discard_internal_test.go`.
- **The Miri-ignore guard reads every source file of the `--lib` target, and follows helpers however they are called (I1: Miri must run the lib tests it can).** `TestLibTestsThatNeedTheOSAreIgnoredUnderMiri` scanned `pool/` only, so `ffi/mod.rs` was never looked at, and it matched only `make_pipe(`, `Handle::open(` and `libc::pipe(` in a test's own body, so a test calling the module's `pipe()` helper (as `ffi/mod.rs`'s do) was invisible even in a scanned file. It now walks `crates/gusset/src` and treats any function that reaches the OS, directly or through another function, as an OS function, to a fixed point; it sees `name(`, `Type::name(`, `module::path::name(`, `name::<T>(`, `.name(` and a function passed as a value, with a qualified call counted when its qualifier is a type or module the crate defines (std's `File::open(` is not a crate function called `open`). Comments and string and char literals are blanked first, so a `"}"` no longer ends a function body early; multi-line attributes, including `#[cfg_attr(\n miri,\n ignore\n)]`, are read whole, and `ignore` is matched as a word (`ignored` is not it). `libc::socketpair` and `UnixStream::pair` count as the OS (every test using socketpair is already ignored and reaches fcntl or setsockopt; not checked against a Miri run). The guard fails if the parsed tests are not exactly the `#[test]` attribute lines in the tree. Planted on the real tree: an unignored test calling `Rig::start()`, which calls `pipe()`, passed the old guard and fails the new; a multi-line ignore failed the old guard and passes the new.
- **The rustup-init rule recognises every run, every step layout and every wrapper, and a check that cannot stop the step, or that is undone, no longer counts.** The rule matched only `rustup-init -y` and `rustup-init --` and read only steps whose first line was `run:`. A run is now any command whose command word is `rustup-init`, behind `sudo`, `env`, `timeout N`, `nice`, `sh`, `. path` and the like or inside `$(...)`; an unnamed step (`- run: |`), a one-line run, any block scalar header (`|+`, `>-`, `|2`, `|-  # why`) and `.yaml` workflow files are read, and `TestEveryRunKeyIsInAParsedStep` fails on a run key the step parser did not reach (a step at another indentation). The `buildFiles` scripts are scanned too (`rustupScriptViolations`). A `sha256sum -c` counts only when its failure stops the step: no `||`, or exactly `|| exit N` (N nonzero) or `|| false` ending the list or followed by `&&`; `|| true`, `|| echo "would exit 1"`, `|| exit 1 || true`, a handler group, a backgrounded check, `true || check` and a body that runs `set +e` are refused. A downloader, copier or redirect that names the checked path, or the file's name, between the check and the run is refused. Known limits are written in the rule's doc comment (a check inside an `if` branch, a path through a shell variable). The test also fails if the workflows hold fewer than the two runs the musl jobs make. Planting an unnamed `- run: |` with an unchecked run in `matrix.yml` failed the new test and passed the old.
- **The image-pinning rule covers the build scripts and names the image operand.** `TestEveryContainerImageIsPinnedByDigest` read workflows only; a `docker run alpine:3.24` in the Makefile or a script in `buildFiles` passed it. It now applies the docker rule to each `buildFiles` entry and includes those files in the one-tag-one-digest drift check. The image is the first argument that is not an option or an option's value, so `docker run -e BASE=golang:1@sha256:<hex> alpine:3.24` is reported; `docker container run`, `docker image pull`, `nerdctl`, `$(DOCKER)`, `${DOCKER}` and `docker --context x run` are read; `$(shell pwd)`, `$(CURDIR)` and nested make functions no longer cut a command before its image. Compose files and `buildx` are not read: the repository uses neither.

### 2026-10-26 · Hand-typed doc numbers corrected or removed, and the v0.0.3 CI sentences annotated (card N3)

Documentation and changelog only; no runtime, export, ABI or public-surface change (17 exports, 13 Go entry points). It protects R15 (every number in docs is generated); no I1 to I6 runtime invariant is touched.

#### Documentation
- **`docs/choosing.md` no longer types the thread counts under the concurrency table.** The sentence said 2048 callers cost Gusset "~21 threads rather than ~370". The blocking-cgo figure matched no row of the committed sweep (`bench/results/crossover/scaling-rawcgo-darwin-arm64-go1.27.1-rust1.98.0.txt`). The sentence now points at the 2048 row of the generated table, so no count is typed in prose.
- **The crossover prose in `docs/choosing.md` no longer types "2x", "1.1x" or "18 goroutines".** It refers to the limits and the goroutine count the generated crossover table states, so a re-recording on a machine with a different `GOMAXPROCS` cannot leave the prose wrong. (The generator itself still spells "18 goroutines" in `tools/benchplot/main.go` and `crossover.go`; that is not changed here.)
- **`docs/adoption.md` no longer claims the chunked check made the example engine "about 4× faster".** Only the chunked kernel is committed (`bench/results/linux-amd64-vm/simd.txt`); no run with `ctx.check()` inside the loop is, so the size of the speedup has no committed benchmark behind it. The sentence says so.

#### Changelog
- **A dated note under `[0.0.3]` records that "GitHub Actions has not run this tree" is stale.** The per-section sentences are left as written; the note cites the passing Matrix runs on the release (run 40080429800 on the `v0.0.3` tag `ffae8aa`) and on the merges before it.

### 2026-10-26 · The crossover sweep gains its 1 µs and 10 µs arms; the table's ratios stop contradicting its bars (card ft-8a8279f5)

Benchmark source, tooling and tests; no runtime, export, ABI or public-surface change. No baseline is recorded here: the new arms have no data until a quiet-host re-record, and the committed `bench/results/` files and `docs/choosing.md` are untouched. This protects I6 (the docs describe what the code and the recorded data do).

#### Benchmarks
- **`workSizes` in `bench/seed/go/crossover_test.go` gains 1_450 and 14_500 iterations.** At 0.70–0.72 ns an iteration on the recording host (the committed `1000it-` calibrations) these are about 1.01–1.04 µs and 10.1–10.4 µs, so their calibrated labels read 1 µs and 10 µs rather than falling just under the truncation boundary (1_400 would label as about 976–1005 ns); they are the arms the crossover table is asked to answer for; the existing 1_000 and 10_000 arms (about 0.7 µs and 7 µs) stay. The labels are still calibrated on the host that runs the arm. `tools/benchplot` reads the arms back out of the benchmark names, so a recording made before this change renders as it did (`make docs-check` is clean on the committed data) and a recording that has the new arms charts and tabulates all seven. A smoke run (`-benchtime=1x`) confirms the arms execute; it is not a recording.

#### Tooling
- **The crossover table and chart no longer print a ratio that contradicts the bar it is read against.** `tableRatio` and `ratioLabel` printed 0.998 as "1.00×" (a win shown as a tie; `aheadCell` listed it as ahead at "1.00×"), 1.002 as "1.00×", 1.104 as "1.10×" (over the 1.1× bar shown as on it) and 1.099 as "1.10×"; the chart printed 1.002 as "+0%" and 0.998 as "−0%". Both now go through `clearRound`, which adds decimals until the printed number is on the same side of 1, 1.10 and 2 as the value and equals a bar only when the value does: "0.998×", "1.002×", "1.099×", "1.104×". Ratios away from the bars print as before, so no committed figure changes.
- **`mustOne`'s refusal of a second matching results file names a remedy that exists.** It told the reader to "pass one explicitly", and `benchplot` takes no file argument (its only flag is `-check`). It now lists the matching files and says to move the one not to be charted out of the directory. The decision is `resolveOne`, so a test can read it.

#### Tests
- `tools/benchplot/ratio_test.go`: table-driven boundary tests for `tableRatio` and `ratioLabel` (0.998, 1.0, 1.002, 1.099, 1.1, 1.104, and the 2× bar) that also check the printed value against every bar; `aheadCell` and `withinCell` cells at the same boundaries; the seven-arm sweep (work discovery, lookup of 1_000 against 10_000 and 1_400 against 14_000, chart ticks, the block's arm list); a recording without the new arms; and the refusal's message. The ratio and message tests fail against the previous code.

### 2026-10-24 · Poisoned-handle recovery and the engine registry's semantics (card ft-8a827cf5)

Documentation, godoc, rustdoc and tests; no runtime, export, ABI or public-surface change (17 exports, 13 Go entry points).

#### Documentation
- **Recovery from a poisoned handle is `Close` plus `Open`, with no in-library supervisor** (`DECISIONS.md`, 2026-10-24). The entry-point budget is untouched.
- **`docs/adoption.md` step 2 gains the recovery recipe and the routing table.** A panic leaves the process-global registry as it found it, so a handle opened after `Close` runs the same engine closure with its torn state (a `Mutex` the panic poisoned poisons the new handle on its first job). When state may be torn the adopter calls `clear_engine_handlers` from an export of its own and installs every engine again. The section also says what the docs left out: `set_engine_handler` replaces the global slot (the last init wins), `register_engine` refuses a duplicate opcode, and the global engine serves opcode 0 only.
- **The `default_dispatch` rustdoc no longer contradicts the code.** It listed "opcode-specific wins, then the global engine" for every opcode. A nonzero opcode with no registry entry is refused with `no engine handler registered for opcode N`; only opcode 0 reaches the global handler or the diagnostic engine. `set_engine_handler`, `register_engine` and `clear_engine_handlers` document what they replace, refuse and clear.
- **`WithOpcode` and `ContextWithOpcode` state the routing rule** in their godoc.

#### Tests
- `crates/gusset/tests/engine_registry_recovery.rs`, its own binary because it installs a global handler. `a_recovered_handle_runs_the_torn_engine_until_the_adopter_clears_and_reinstalls` shows what a recovered handle runs: the torn closure, on the global slot and on an opcode entry, until the registry is cleared and installed again. A reinstall without the clear is refused for an opcode entry.
- `set_engine_handler_replaces_the_global_slot_and_the_last_install_wins` and `a_nonzero_opcode_with_only_a_global_handler_is_refused` (also with `GUSSET_FLAG_DIAGNOSTIC_ENGINE` set) pin the install and routing rules above.

### 2026-10-24 · Documentation drift: the docs say what the code does (card ft-86b4ae63)

Documentation, one comment-only Go change and two `compile_error!` text changes; no runtime, export, ABI or public-surface change. Where a document and the code disagreed, the document changed.

#### Documentation
- **`DECISIONS.md` renders as one table.** A blank line at row 43, there since 2026-09-20, ended the table, so rows 44 onward showed as running text. Removed; no row text changed. `TestDecisionsLogRendersAsOneTable` fails on a blank or prose line between the first and last row.
- **Three superseding rows, appended.** `cargo-fuzz` now runs nightly (the 2026-09-20 row rejected it); the supported set is Linux and macOS, and the Windows blocking-call clause of the Completion wakeup row is void; the isolate protocol answers on descriptor 3, not stdout.
- **The testing table names every CI job and every libFuzzer target.** New rows for `go-floor`, `rust-floor`, `allocator-api`, `walkthrough`, `hammer`, `step5-snippet`, `go-fuzz` and `cargo-fuzz`; the five `fuzz/fuzz_targets/` targets are named. The `race-gc` row is `race`, the job's real id. `TestTestingPlanNamesEveryCIJobAndFuzzTarget` fails when a job id or a fuzz target has no mention.
- **AGENTS.md enforcers that did not exist are corrected.** The Windows row no longer names a CI leg (the `cross` job asserts the build fails); the uniffi row says no example exists; the allocation row names `make alloc-gate` and its `ALLOC_BOUNDS`; the shutdown row no longer cites a `TestMain` that does not exist. R16 no longer names a zero-allocation large-input test that does not exist, and `docs/why.md` describes the thread-cap soak as it is (peak sampling, bound of +8). The `go build -a` and `assign_through_raw` rows were already fixed.
- **Phase status carries its pending criteria.** Phase 1 cites the green `main` Matrix run 40080429800 on `ffae8aa` and lists the tessl/sparsl exit criterion as pending; Phase 2 lists the criteria that have no evidence here; the uniffi-bindgen-go example is pending, not shipped. `docs/PLAN.md` repeats this and drops the "blocking fallback stays supported" claim (no export is synchronous).
- **Type count, example engine and the seed README.** `#[repr(C)]`: four types in the ABI manifest, six in the crate (`RingShared` and `Slot` are read through `GUSSET_RING_*` offsets). `docs/PLAN.md` describes `gusset-example` as the Rust-only engine it is (opcodes 10 to 13). `bench/seed/README.md` cites R5, not R13, for `#cgo noescape`.
- **Every benchmark figure outside a generated block is labelled or gone.** `bench/seed/README.md` is marked historical, hand-recorded, with no raw file. The SIMD tables in `docs/choosing.md` name `simd.txt` and `simd-ring-multiversion.txt` (medians re-derived from them). The devmap timings in `docs/integrations.md`, the `dc-glob` run in `docs/adoption.md` and the README, and the seed figures in `docs/why.md` are marked historical one-offs. Third-party Wasm figures with no recorded source, `< 5 µs` submit, and the hand-typed thread ratios in `docs/why.md` are removed.
- **iceoryx2 facts.** `eclipse-iceoryx/iceoryx2-go` exists (created 2026-10-23, otterdog `.eclipsefdn#28`) and holds only its initial commit; its pull request #1 is open. `docs/ipc.md`, `docs/PLAN.md`, `docs/choosing.md` and `docs/why.md` say so as of 2026-10-24.
- **The isolate protocol is described as it runs.** `docs/ipc.md`: requests on stdin, responses on the descriptor `isolate.Start` appends to `cmd.ExtraFiles` (3 when none were passed), named by `GUSSET_ISOLATE_PROTOCOL_FD`.
- **Toolchain prose no longer types a patch version.** `AGENTS.md`, `CONTRIBUTING.md` and `docs/why.md` say Go `1.27.x` and Rust `stable`, as the workflows do; the recorded pair is in the generated README block.
- **Platform prose says Linux and macOS.** `AGENTS.md`, `CONTRIBUTING.md`, `README.md`, `docs/README.md`, `docs/choosing.md`, `docs/rfc-r14.md` and `docs/platforms.md` no longer call the supported set "unix".

#### Code
- **The unsupported-target error no longer claims unix is supported (I6).** `crates/gusset/src/lib.rs` said `gusset supports unix targets only` on FreeBSD, OpenBSD and illumos, which it refuses. It now says `gusset supports Linux and macOS only`; the `cross` job's three greps and `platform_gate_test.go` use the new phrase, and `TestPlatformGateMessageNamesTheSupportedSetNotAllOfUnix` ties the phrase to the `compile_error!` text.
- **Every exported identifier of the root package has its own doc comment.** `ErrGeneric`, `ErrPanic`, `ErrPoisoned`, `ErrBadArg` and the nine `Expected*` constants sat in groups with no per-name comment (`gusset.go`, comments only). `TestEveryExportedIdentifierOfTheRootPackageIsDocumented` fails on any exported function, method, type or group member without one.

### 2026-10-24 · The bench recorder's guards are tested, wired into CI, and tighter

From the `ft-568a30e2` bench evidence pipeline card. Tooling and tests only: no runtime, export, ABI or public Go API change. No baseline is recorded here: the host is loaded, and a shared CI runner is not a quiet one.

#### Benchmarks
- **`bench/record.sh` refuses what it used to publish.** `COUNT` must be a positive integer in both modes (a `0` or non-number skipped the variant loop and moved a header-only file over the last good one), a repeated variant name is refused (it listed one arm twice and doubled its rows), and an arm that exits 0 with no `Benchmark` row stops the recording with the previous file untouched.
- **Lock liveness keys on the owner pid, not the process group.** A recorder SIGKILLed inside a long-lived parent's group left a lock that named a live group, so every later run refused until the parent died, and the refusal advised `kill -TERM -<pgid>` on that parent's group. The pid (and, against reuse, that it is still in the recorded group) decides, the refusal names the pid, and takeover of a dead lock is serialized by a `mkdir` claim: an 8-way race against a dead lock showed that renaming the stale lock away can move a live recorder's lock that replaced it.
- **A zombie lock or claim holder counts as gone.** `ps -p` lists a SIGKILLed recorder whose parent has not reaped it, so `lock_holder_alive` read it as running: its lock was refused against, and its takeover claim was retried, indefinitely. It now also reads `ps -o stat=` and treats a state containing `Z` as dead (procps and BSD `ps` alike), for the lock's owner and the claim's owner; the pid-reuse group check is unchanged. `zombie_case` in `bench/record_test.sh` makes a real zombie (a parent that forks and never waits) and fails against the previous `record.sh` for both the lock and the claim.
- **HUP and QUIT are trapped like INT and TERM**, and the lock staging directory and takeover claim are removed on every exit the script can handle. A signal that landed between starting a child and recording its group killed the child's pid only; the group is now looked up when the signal arrives.
- **`record.sh` can record a cargo bench.** An arm written `cargo:PACKAGE/BENCH` runs `cargo bench` (built first, `COUNT` as `--rounds`), and `foreign_benchmarks` now sees a cargo bench binary and `cargo bench` itself, so a concurrent one refuses the recording as a concurrent `go test -bench` does. `make bench-dispatch` records `crates/gusset/benches/dispatch.rs` into `bench/results/dispatch/`.
- **A failing arm now says why.** A cargo arm whose build failed never opened its output file, and the failure report read that file, so under `set -e` the script exited before printing the compiler's diagnostic. Both files exist before the arm runs, and the report cannot itself fail.
- **The provenance header carries the commit that was benchmarked.** `gusset-bench-commit` (full hash, `unknown` when git cannot say) and `gusset-bench-commit-modified` (tracked paths that differed), so a figure can be told apart from code that has since changed.
- **`dispatch.rs` interleaves.** It runs at least 10 rounds, the order alternating by round (ABBA) after a discarded round 0, and prints benchstat-compatible lines grouped by arm with the execution order in comment lines. A fourth arm, opcode 0 on the diagnostic engine with a nonzero opcode registered, separates the registry's presence from its lookup. No dependency was added.
- **`BenchmarkDeadlineInFlight` measures a deadline that expires inside the engine.** The Submit context carries the deadline, the Wait context has none, so the only way Wait returns `DeadlineExceeded` is the engine's own `Cancelled` completion; it reports `engine-cancel/op`, 1.00 whenever the figure is meaningful. `BenchmarkDeadlineExpiry` is documented as the Go early return and keeps its name, because the benchmark gate would read a rename as a dropped benchmark plus a new one.

#### Tests and CI
- **`bench/record_test.sh` runs in CI**, as one step of the Matrix `unit` job (ubuntu, ubuntu-arm, macOS; no new job), and `tests/ci` pins that step. Its signal cases now use a stub `go` that starts a grandchild, signal only once it exists, check the grandchild died and a canary in the recorder's own group survived (INT, TERM, HUP and QUIT, in a session of its own and in a shared group), and the suite shows that case failing against a recorder that kills only the child's pid. New cases: the three refusals above, a dead lock and a no-pgid lock past its grace period, an 8-way race for each, SIGKILL in a shared group, the cargo arm, foreign cargo benchmarks, the header commit, and a signal inside the lock-staging window.
- **The Linux branch of `cpu_idle` runs in CI.** The old `uname` stub forced Darwin on every host, so the `/proc/stat` branch never ran; the stub is now used only to steer a case onto the other platform's branch, and an unstubbed case checks that the host's own measurement produces a reading. `RECORD_PROC_STAT` moves where that branch reads, for the test's controlled counters; the idle threshold stays fixed.
- **`bench/.probe/` and the lock's stage and claim directories are gitignored**, and `record_test.sh` removes its scratch tree on every exit path, which it checks about itself.

### 2026-10-24 · GP-FEAT-001 instrument items: the crossover is generated, the ring header is derived (card ft-8a8279f5)

No benchmark was recorded for this entry and none of it makes a performance claim; it reads only data already committed under `bench/results/`. Invariants protected: I6 (Go and Rust agree on the completion record layout the reader parses by offset, as `constants_match` checks for the other layout constants) and R15 (every figure in the docs is generated).

- **`docs/choosing.md` states the measured crossover.** `tools/benchplot` adds a generated table to the existing `BENCHPLOT` block: from which work size Gusset is within 2x and within 1.1x of raw cgo, serial and with 18 goroutines, and in which arms it is ahead (none, in the committed recording). It is built from `bench/results/crossover/transport-*.txt` by the same medians as the table above it, so `make docs-check` fails when it drifts. A new paragraph says where raw cgo wins, and that plain Go is an inference, not a measurement, below a bare cgo call. The recording's arms are noop, ~0.7, ~7, ~72 and ~720 µs: there is no ~1 µs or ~10 µs arm yet, and the table says so rather than interpolating.
- **`InlineRecordMax` is used.** `ticket_reader.go` typed the completion record's 16-byte header as a literal at every parse offset; it is now `inlineHeaderBytes = ffi.InlineRecordMax - ffi.InlineResultMax`. `inline_record_header_is_the_two_words_the_go_reader_derives` in `tests/constants_match.rs` pins that difference to the ticket and length words in Rust and in `gusset.h`, requires the Go declaration to be that expression, and fails if the reader types the header size as a literal again. The existing per-constant check cannot see a header that grows in Rust and `gusset.h` together.
- **No code change for the `CallNoop` allocation count.** `bench/results/gp-feat-001/profiles/callnoop-23e925b.mem.pb.gz` attributes the one allocation per call to `drainPipe`'s copy of the result bytes (12,509 of 12,838 sampled objects for 200,000 calls; the tiny allocator profiles 1-byte allocations once per 16-byte block). The README already reads 1 alloc/op, and `make alloc-gate` bounds it at 1.

### 2026-10-24 · The musl jobs' container image and rustup-init are pinned (card ft-415ddf1f, criterion 7)

Workflow, `tools/ci/versions.env` and test changes only: no runtime, export, ABI or public Go API change, no job added, renamed or removed (the required check names are unchanged). GitHub Actions has not run this tree; the new install step could not be run locally either (the podman machine does not start here), so its first real run is the Matrix `r14-musl` and `musl` jobs.
- **`container: golang:1.27-alpine` is `golang:1.27-alpine@sha256:b8c9b82a...`** in both musl jobs: the digest of the multi-arch index the tag resolved to on 2026-10-24 (Go 1.27.3 on Alpine 3.24.3, built 2026-10-21), read from the registry's `docker-content-digest` and re-hashed from the manifest bytes. The musl jobs therefore test Go 1.27.3 until the digest is bumped, instead of floating with the patch release as the setup-go jobs do (DECISIONS.md 2026-10-24).
- **rustup-init is `RUSTUP_VERSION=1.29.2`, downloaded from `static.rust-lang.org` and checked with `sha256sum -c` against `RUSTUP_INIT_SHA256_X86_64_MUSL`**, both in `tools/ci/versions.env`. The step no longer installs Alpine's `rustup` package, whose rustup-init was a build nobody here checked. The published checksum and the one computed from the downloaded file agree.
- **apk packages stay unpinned, on purpose.** Alpine's index keeps only the newest build of each package, so an exact `build-base=0.5-r4` stops resolving when Alpine rebuilds it and fails a required check with no change of ours; `build-base` is also a meta-package whose gcc and make would still float. The repositories and signing keys come from the pinned image, and each musl job now prints `apk info -v` so the toolchain it ran with is on record.
- **`tests/ci/workflow_images_test.go`** fails on a `container:` (scalar or `image:` mapping), `services:` image, `docker://` action or `docker`/`podman run|pull|create` that is not `repo:tag@sha256:<64 hex>` with a trailing comment, on one `repo:tag` pinned to two digests, and on a step that runs rustup-init without the pinned version, the pinned checksum and a `sha256sum -c` before the run. `apk add rustup` and `sh.rustup.rs` are refused. `TestPlantedImageViolationsAreCaught` plants each violation, including a 63-hex digest, an uppercase digest and a `sha512` digest. On origin/main's workflows the new tests fail on matrix.yml:547, :579, :555 and :594.

## [0.0.3] - 2026-10-24

Release since v0.0.2 (`b6f5200`). It fixes a write-after-free that v0.0.2 shipped (Security, below), and only this release is supported (`SECURITY.md`). The C ABI is unchanged: 17 exports, ABI layout version 2, and `gusset.h` differs from v0.0.2 only in comments. The Go API gains `Handle.Discard`, the thirteenth entry point, and the exported sentinels `ErrClosed`, `ErrNilHandle`, `ErrNilContext`, `ErrNilBuffer` and `ErrInputTooLarge`. One Go behaviour change: a `WaitBuffer` that loses a race with `Close` returns a buffer whose `Bytes` is nil instead of `ErrClosed`. `install.sh` defaults to this tag and refuses, before building, a tree whose `make install` ignores `DESTDIR`; `tools/ci/lint.sh version-parity` holds CHANGELOG, `SECURITY.md`, `install.sh` and `docs/adoption.md` to the version in `Cargo.toml`. The `FFI_ERR` to `FFI_BAD_ARG` status change (`ft-3d60ee16`) is not in this release; it ships in the next one with its own note. The dated sections after Security are the entries as they landed on `main`, newest first.

Note (2026-10-26, added after release; the per-section text below is unchanged): the sentences "GitHub Actions has not run this tree" in this section were true when each section was written, and are no longer true of the release. The v0.0.3 tag is `ffae8aa`, and the Matrix run on that commit, run 40080429800, passed all 21 of its jobs. The release PR's final head, `0860a01`, passed the Matrix on its pull request (run 40078391383), and every merge to `main` from PR #5 (`2ed9a29`, run 40060453498) through the release has a passing push run of its own (for example run 40077242984 on `910961a`). The one failing pull-request run on the release branch (run 40077294133, `2492753`) was followed by the passing run on `0860a01` before the merge.

### Security

- **v0.0.2 and earlier: `WaitBuffer` could write a result into freed memory (I1).** Affected: 0.0.2 and earlier. Patched: 0.0.3, by `38f0296`. For an inline result, `WaitBuffer` allocated a Rust buffer, took its view with `Bytes` (which releases `cgoMu` on return) and copied the result into it with no lock held. `Close` frees every buffer of the handle under that lock, so a `Close` in the gap freed the buffer first and up to 4 KiB of result landed in a freed C-heap chunk. The buffer is now filled before `cgoMu` is released. It needs a `Close` racing a `WaitBuffer` on the same handle. `TestWaitBuffer_CloseCannotFreeTheResultBeingFilled` holds the window open and fails on the old code; the 2026-10-12 section below has the details. Upgrade to v0.0.3: there is no backport. A GitHub security advisory will follow.

### 2026-10-24 · One probe switch per workflow, and the Skip contract written down

From the 2026-10-23 audit of `ft-1f4a1228`, the hostprobe contract criterion and A-9 (approved as a fenced follow-up once main was green). Workflow and test changes only: no runtime, export, ABI or public Go API change, and no hot path, so no benchmark delta is claimed. GitHub Actions has not run this tree; the step and job names are unchanged, so the required checks are too.

#### CI
- **`GUSSET_REQUIRE_PROBES: '1'` is set once, in the top-level `env:` of `matrix.yml`, `nightly.yml` and `tip.yml` (I4).** The 25 step-level copies are gone. `adopter.yml` runs no `go test` and does not carry it. No step needs the switch absent: `internal/hostprobe/probe_test.go` strips it from its own child and passes with the parent set to `1` or `0`.
- **`TestWorkflowLevelProbeEnv` replaces the step-level shell model.** This reverses the 2026-10-13 approval of that model. It required every `go test` step to see exactly `1` through `export`, `unset`, prefixes and `env(1)`, in 563 lines of `tests/ci/workflow_supplychain_test.go`. The new rule needs no shell parsing: the entry equals `'1'` in the top-level `env:` of every workflow that runs `go test`, is absent from a workflow that runs none, and the name appears on no other line of any workflow, comments included. Twenty planted violations fail it, among them a step-level override, a job-level env, a shell `export`/`unset`/prefix, a missing workflow env, values other than `'1'`, a duplicate entry and flow style. On the pre-change workflows it reports all 25 step-level copies and the three missing workflow entries. Net: 118 lines added and 530 removed in that file.
- Not modeled: a Go test reached only through `make` or a script (every workflow that runs one also runs `go test` directly, and the test pins those three), and `env -i`, which clears the variable without naming it.

#### Tests
- **`hostprobe.Skip` states its contract.** It is only for a condition a quiet CI runner never meets: real host load or noise. It is never for a deterministic platform or runtime property, which needs a fix or a platform gate. `TestSerialCall_LeavesAPForOtherGoroutines` broke it: its idle baseline sat on Go's Linux 1 ms netpoller floor (`epoll_wait(1)`) and fired on every Linux runner until caebc32. The nine call sites were checked against the contract. All nine pass, with no SKIP, on every Linux and macOS lane of Matrix 40070336468 (96ddc24) and 40068268871 (ec86583), with the switch on. Eight compare a probe against a 250 ms to 2.5 s job that ignores cancellation. The ninth (the bystander baseline) has a 100 µs bound against a 0.5-17 µs baseline measured on GitHub runners (run 40058658715). None needed a fix.

### 2026-10-23 · Nightly failures report themselves and keep their evidence

From `ft-987c804c`. Test and workflow changes only: no export signature, ABI or public Go API change. GitHub Actions has not run this tree. The criteria that need three consecutive green nightlies are open.

#### CI
- **Miri is bounded at 240 minutes (I4).** It had no `timeout-minutes` and was cancelled at GitHub's 6 h default on 2026-10-15..17 and 2026-10-23, before the ring seed sweep ran. `one_cut_evicts_what_line_by_line_eviction_does` took 3 h 30 m of job 117993651499. It now runs 20 rounds under `cfg(miri)` and 2000 elsewhere, as `pool::ring`'s handshake test does. Under Miri on darwin the 20 rounds took 206 s. On 2026-10-19 the rest of the lib suite and the seed sweep took under 2 minutes in CI. Since then every Miri run was cancelled inside this test, and no run has reached the tests added after 10-19. One of those, `pool::tests::open_refuses_a_descriptor_that_is_not_a_pipe_write_end` (924479b), failed under Miri on darwin with `unsupported operation: fcntl ... F_GETFL`. Like its fd-checking neighbours, it is now `#[cfg_attr(miri, ignore)]`.
- **`ffi_exports` runs as five 60 s processes instead of one 300 s run.** Under ASan, each handle open/close keeps about 78 KB of RSS that is never returned. RSS stays flat at 26 MB without a sanitizer, so this is not a Gusset leak. A 300 s run crossed the 2048 MB limit with a 26 MB live heap, the second cause of the nightly OOM after the 1 GiB lengths. Each 60 s process stays well under the limit, which still catches a real out-of-memory. Total time and corpus are unchanged.
- **A cancelled or timed-out nightly job files an issue.** A job that hits its timeout concludes `cancelled`, and `failure()` is false for that (run 38710039955: Miri cancelled, report skipped). The report job runs on `always()` when any needed job failed, was cancelled or was skipped. Its body names jobs by result. If every job succeeded, it files nothing.
- **cargo-fuzz uploads `fuzz/artifacts/` on failure**, with the upload-artifact SHA go-fuzz pins. The `ffi_exports` OOM input from 39930332461 was lost because nothing uploaded it.
- **`tests/ci/workflow_failure_evidence_test.go` holds these rules.** Every job that runs `cargo fuzz` or Go fuzzing has an on-failure upload covering the tree that fuzzer writes to. The report job's `needs` list every other nightly job. Its `if:` covers `'failure'` and `'cancelled'`, and its body names cancelled jobs. Miri's bound is 1..240. Each rule fails on the pre-change `nightly.yml` and on planted fixtures. It is a new file, so PR #5's edits to `workflow_supplychain_test.go` do not collide with it.

#### Harness
- **The hammer's abandon oracle no longer reports a correct result as corruption (I4).** `opAbandon` sent `[9, k]` with an empty body 1 time in 6000. Mode 9 echoes the whole input when nothing follows its prefix, so the engine returned `09 0k` and the oracle wanted 0 bytes. That is the `abandon: corrupted result: got 2 bytes, want 0` of nightlies 39575563846 and 39754401724. The body is now 1+ bytes, as the other mode-9 builders' are. `TestHammer_AbandonInputNeverHasAnEmptyBody` pins the echo. It draws from a seed whose fifth draw was empty under the old builder, so it fails there and takes under a second with `-race`. The three recorded seeds replay green for 30 s each on darwin, which has no heap-check leg.
- **`FuzzDiagnosticEngineNeverAborts` never leaves a closed handle for the next input.** When Close or the reopen failed after a poisoning input, the target Fatalf'd with the closed handle still in place. The minimizer then reported `gusset: handle is closed` for everything (39112666661, GitHub issue #4). The poisoned handle now leaves the slot first and is always closed, and the replacement is opened before any report. A failed Open is retried, and reported as itself, on the next input. A planted Close failure and a planted reopen failure each report their own message; the old ordering reports `handle is closed`.
- **The 2026-10-18 crash file is committed as recovered, not reconstructed.** `tests/pitfalls/testdata/fuzz/FuzzDiagnosticEngineNeverAborts/582528ddfad69eb5` is the file from artifact 12130769765, byte for byte: `[]byte("0")`. The minimizer produced it against the closed handle, and the artifact did not keep the original 31-byte input, so that first failure's cause is still unknown. The file passes on a fresh handle. On darwin, 4000 poison-and-reopen cycles left descriptors at 5 and OS threads at 11 (from 6), with flat cycle time, and a 240 s local fuzz run never fell below 52 execs/s. Neither supports descriptor or thread exhaustion there. Linux was not measured.

### 2026-10-23 · Isolate keeps finished results and drains on progress

From the 2026-10-23 audit of `gs-isolate-corrupt-frame-hang`, items B-2 to B-6 and the criterion 11 amendment. Everything is in `internal/isolate`, which has no importer outside its package. No runtime, export, ABI or public Go API change, and no hot path, so no benchmark delta is claimed. Run on darwin/arm64 only; GitHub Actions has not run this tree. Each new test fails on 2ed9a29's `proc.go`.

#### Isolate
- **A result stored before the worker died is returned by `Wait` (I2, I4).** The reader's exit used to delete every ticket, so `Wait` returned the worker's poison for a job that had already finished. A stored result now stays until `Wait`, `Discard` or `Close` takes it. In-flight tickets and every later `Submit` still get `ErrPoisoned`. `TestProc_StoredResultSurvivesWorkerDeath` fails before the fix.
- **The drain windows restart on progress (I3).** After the worker exits, the reaper's 200 ms window restarts while frames are still delivered. The reader's 50 ms probe is re-armed after each frame instead of once. Before the fix, frames arriving 50 ms apart after the exit lost 6 of 10, and frames 5 ms apart behind the probe lost 12 or 13 of 20.
- **`Submit` refuses once the worker has exited (I4).** It used to write to the dead worker's stdin and wait for the reader to latch. That bounds the drain: only tickets in flight at the exit can extend it.
- **Stale code.** `ProtocolOut` loses a nil check that could not fire. The package comment says responses run on the `ExtraFiles` protocol pipe, not stdout.

#### Tests
- **`TestProc_CancelSendDoesNotPinTheWriteLock` closes with a 100 ms budget (I4).** The deaf worker ignores stdin, so the public `Close` waited out its 30 s budget before the kill. The test took 33.10 s and now takes 3.14 s.
- **The grandchild fixture starts its grandchild before hello.** A kill right after `Start` could beat the grandchild to the pipe, and the test then never opened the drain window it was about.

#### Decisions
- Four rows dated 2026-10-23 in DECISIONS:
  - stored results survive worker death;
  - a write deadline poisons even when 0 bytes were written (kept fail-closed; a counting writer was rejected);
  - the progress-based drain, with the limits that stay wall-clock;
  - the typed-nil trace-carrier recover duplicated in `readContext`, so the public surface stays at 13 Go entry points.

### 2026-10-23 · Main green again: the two Linux test bugs, new benchmarks, masked steps

From the 2026-10-23 audit: tracker `ft-97faf4f4`, plus `ft-c689767b`, `gs-isolate-corrupt-frame-hang` and `ft-1f4a1228`. No runtime, export, ABI or public Go API change. Nothing in this set touches a hot path, so no benchmark delta is claimed. 320c56c was pushed directly to main on 2026-10-19 and Matrix 39270656716 was red in 8 jobs; this set fixes all three causes. Linux evidence is from the throwaway branch `probe/w1-linux-evidence` (run 40058658715).

#### Tests
- **The bystander wake test no longer fails on every Linux host (I4).** Its baseline timed naps in an idle process. On Linux an idle 200 µs nap wakes about 860 µs late, because the netpoller rounds a sub-millisecond wait up to `epoll_wait(1)`. The test skipped as "too noisy", and `GUSSET_REQUIRE_PROBES` made that a failure. The baseline now runs with a goroutine cycling the scheduler, which is the regime of a correct Call: 2–5 µs on Linux runners. On Linux the pre-fix gate wakes 6.3–14 ms late and fails 20 of 20 runs; the fixed build wakes 1.5–5 µs late.
- **The close-on-exec test no longer mistakes Go's cgroup fd for a leak (I1).** On Linux the Go runtime holds the cgroup `cpu.max` at the fd number exec just freed. The exec probe now compares descriptor identity (`dev:ino` of the protocol pipe) instead of asking whether fd 3 is open. Removing `CloseOnExec` from `ProtocolOut` fails it.

#### CI
- **A benchmark the change adds is a `::notice`, not a failure.** Base-only rows (dropped or renamed benchmarks) still fail, and a rename still fails. Allow-list names match with or without the `-N` suffix.
- **Independent steps run after an unrelated red.** These steps now carry `if: success() || failure()`, and `tests/ci` pins them:
  - the simd suite;
  - GOGC=1;
  - the allocator interop race and fallback steps;
  - the bench allocation gate.

#### Process
- **`main` is protected.** A PR is required, and the 23 Matrix and adopter checks are required, strict and enforced for admins. See DECISIONS 2026-10-23, which also records keeping `gusset_buf_alloc` uncapped and splitting iceoryx2-go PR #1.

### 2026-10-23 · The step-5 check reads the umbrella's own output, and gussetvet takes build tags

From `ft-0f6a723d` (residuals C-8, C-9 and C-10 of the 2026-10-23 audit). No export signature, ABI or public Go API change. `gussetvet` gains a `-tags` flag. GitHub Actions has not run this tree.

#### Adopter
- **"Step 5 snippet compiled" comes from the umbrella's build script only (I4).** The walkthrough grepped every build-script output under the umbrella's `target/release/build`, and gusset's own allocator probe prints the same `cargo::rustc-cfg=gusset_allocator_api` line. With the probe forced on (the nightly `step5-snippet` job), a document `build.rs` that set nothing still printed "compiled", so `GUSSET_REQUIRE_STEP5=1` could not fail. The check reads only the `[package]` named in the document's step-1 block, in both cargo layouts: `build/<package>-<hash>/output` (stable 1.99) and `build/<package>/<hash>/run/stdout` (nightly-2026-10-05). No output, or two, is a failure: the check did not run. A copy of the document whose `build.rs` reads `DEP_GUSSET_ALLOCATOR_API_WRONG` now fails under `GUSSET_REQUIRE_STEP5=1`.
- **A leftover `internal/ffi/libgusset.a` no longer fails a local run.** The file is ignored, and the walkthrough asserted it did not exist after the installs. A dev checkout that already held one failed with "an install wrote libgusset.a". It now takes the file's state (absent, or its `cksum`) before the installs and fails only if `make install` or `install.sh` creates, rewrites or removes it. CI checkouts are clean, so CI behaviour is unchanged.
- **The walkthrough also runs the installed `gussetvet` with `-tags gusset_pkgconfig`** on the adopter's shim, the tag Option A builds with.

#### gussetvet
- **`-tags` selects files the way `go build -tags` does, and R1 reads the same files as R5 (I4).** R5 matched files with `build.Default`, with no tags, so a `//go:build gusset_pkgconfig` shim (Option A) was never checked. R1 walked every `.go` file under the directory. A `C.<export>` reference in a `//go:build ignore` file, another GOOS's file or a `_test.go` file therefore counted as a wrapper that no build compiles. Both rules now read one list: the non-test `.go` files that `MatchFile` accepts under the default context plus `-tags` (comma-separated). A tag that is not a build-constraint identifier, such as go build's legacy space-separated form, is a usage error. R4 and the `//export` scan still walk every file in the module whatever its constraints.

### 2026-10-23 · The hashed archive is the linked archive

From `ft-1a7e8a99`. No export signature, ABI or public Go API change. `ffi.go`'s LDFLAGS and every workflow are unchanged. GitHub Actions has not run this tree. No benchmark baseline was recorded.

#### Build
- **`make build` refuses a redirected cargo target directory (I6).** With `CARGO_TARGET_DIR=/x`, cargo built `/x/release/libgusset.a`, while cgo linked and `gen.go` hashed `./target/release/libgusset.a`. That was an old archive, linked silently, or no archive and a failed link. `build`, `test`, `bench`, `bench-crossover` and `bench-scaling` now stop before running anything, naming both archives. `ci-local` stops through `build`. The check is a `$(error)`, so `make -n` refuses too. `CARGO_TARGET_DIR=target` and the absolute path of `./target` still pass. `install` still honours the variable, because it does not link Go against the archive. A `build.target-dir` in a cargo config file is not detected: the Makefile reads only the environment, as `install` does.
- **`go generate ./internal/ffi` fails when there is no archive.** It used to write a placeholder hash over the tracked file and exit 0. It now exits 1 and lists each path it searched, which are the `-L` directories in `ffi.go`, in the same order. It writes nothing. When `CARGO_TARGET_DIR` is set, it says that cargo writes there and that cgo does not link from it. An empty or unreadable archive is also an error. It is not a reason to try the next path, because the linker would stop at that file too. The write is a rename, and an unchanged hash is not rewritten.
- **The archive hash is untracked.** `gen.go` writes `internal/ffi/zz_archive_hash.go`, which is gitignored. This follows `bench/r14/host/zz_archives.go`. `internal/ffi/archive_hash.go` stays tracked as a stub that declares nothing and that nothing rewrites. The stub is kept because it carries no build tag, which `unsupported.go` relies on, and because the adopter walkthrough checksums it. A checkout without the generated file builds the same. Running `go generate` no longer dirties the tree or needs an "update archive hash" commit. The mechanism chosen is the ignored file with a committed stub. A runtime build identity checked in `init` would widen the ABI and was not added.
- **AGENTS.md no longer says CI uses `go build -a`.** No workflow does. The row now says that `make build` runs `go generate` and that only some CI jobs do.

#### Tests
- `internal/ffi/gen_test.go` runs a temp copy of `gen.go`. With no archive, it checks the non-zero exit, that every searched path is named, and that the stub and any previous hash are byte-identical. It checks that release wins over debug, as it does for the linker, and that the hash lands in the ignored file in gofmt form. It checks that an empty archive fails, that the hash file is ignored and untracked, that the stub declares nothing, and that the placeholder string is gone from the tree. The searched paths are compared against the `-L` list parsed from `ffi.go`, so the two cannot drift.
- `tests/ci/makefile_target_dir_test.go` runs `make -n` for each guarded target with a temporary `CARGO_TARGET_DIR`. It checks that each target refuses, names both archives and prints no command first. It checks that `unset`, `target` and the absolute `./target/` are accepted, and that `install` still takes the redirected archive.

### 2026-10-23 · The ffi_exports fuzz target commits at most 1 MiB per buffer

From `gs-boundary-defence-in-depth` (audit 2026-10-23, A-4 and B-7). No export signature, ABI, default or public Go API change. Raw `gusset_buf_alloc` stays uncapped, as the owner decided on 2026-10-23. GitHub Actions has not run this tree, so the nightly cargo-fuzz job has not gone green.

#### Fuzz
- **Op 2 of `ffi_exports` never commits more than 1 MiB per allocation (I1, I2).** Its hostile case passed `c.u64() as usize` straight through, so any valid length up to `MAX_BUFFER_BYTES` (1 GiB) was allocated. `BUFFER_ALIGN` is 64, so `alloc_zeroed` writes every byte. A crafted 11-byte input reproduces it: `libFuzzer: out-of-memory (malloc(1073741824))` from `RawBuffer::allocate_with`. The hostile case now draws only from `MAX_BUFFER_BYTES + 1` up to `usize::MAX`, which is refused before the allocator runs. The valid case is at most `MAX_REAL_BUFFER`.
- **The bound has its own file and tests.** `crates/gusset/tests/ffi_exports_bound.rs` holds `MAX_OPS`, `MAX_REAL_BUFFER`, `MAX_JOB_OUTPUT`, `MAX_LIVE_BYTES` (64 MiB) and `buf_alloc_len`. The fuzz target includes it by path, and `cargo test --workspace` runs its tests. They sweep every selector against the library's own `check_buffer_len` without allocating. Both fail on the old length choice (`selector 3, raw 0x20000000: length 536870912 would be committed`). A `const` assertion fails the build if `MAX_OPS` × (`MAX_REAL_BUFFER` + `MAX_JOB_OUTPUT`) exceeds `MAX_LIVE_BYTES`. While it runs, the target asserts both bounds after every call.
- **Still red: RSS grows on the handle open/close path under ASan.** A 300 s local run with the nightly flags (nightly-2026-10-05, macOS arm64, `-malloc_limit_mb=2` added) still reached `out-of-memory (used: 2300Mb; limit: 2048Mb)` after about 29k execs, with a live heap of 26 MB. An empty input only opens and closes a two-worker handle. Peak RSS for it rose from 240 MB at 2,000 runs to 1,644 MB at 20,000 under ASan. Rebuilt without a sanitizer (`-s none`), it was 26 MB at both counts. With a one-worker handle it was 1,199 MB at 20,000, so part of the growth scales with worker threads and part does not. That split is inferred from one run each. `quarantine_size_mb=0`, `detect_stack_use_after_return=0` and `malloc_context_size=0` did not remove it. Linux was not measured. The green-gate criterion stays open.

#### Boundary docs
- **`Handle::open` names the one refusal that changes the caller's descriptor (I1).** On Darwin, if `F_SETNOSIGPIPE` fails and the `F_SETFL` that restores the saved flags also fails, `O_NONBLOCK` stays set. The error names both failures.
- **`gusset.h` states what `gusset_buf_alloc` commits.** It commits `len` zero-filled bytes per call, for any `len` up to `GUSSET_MAX_BUFFER_BYTES` (1 GiB). It has no lower cap and no cross-call budget (`WithBufferBudget` is Go-only and opt-in), so a C host must cap the length itself. This is a comment only. The default and the allocator are unchanged (DECISIONS 2026-09-24, 2026-09-25, 2026-10-23).

### 2026-10-13 · Unsupported targets fail with the named error

From `ft-9977d2b2`. No export signature, ABI or public Go API change. GitHub Actions has not run this tree. No benchmark baseline was recorded.

#### Platform
- **A target outside linux and macOS fails in `lib.rs` (I4).** The gate is an allow-list (`target_os` linux or macos), and the rest of the crate is cfg'd out with it, so rustc does not also typecheck `pool::sys`. FreeBSD and illumos stop at the existing `compile_error!` text. They do not fail later with E0425 on `pthread_getattr_np`. OpenBSD's standard library is not published for this host, so that target was not compiled; its `target_os` is outside the allow-list.
- **A Go build without cgo, and a Windows Go build, fail in `internal/ffi`.** `unsupported.go` is compiled only for `!cgo` or `windows`. The compiler prints `gusset requires cgo on a unix target` and `docs/platforms.md`. A cgo build on darwin still compiles. The cross job asserts both refusals, and a workflow comment that only contains the phrase does not satisfy the test. `docs/platforms.md` states that cgo is required.

### 2026-10-13 · Caller fd flags survive a refused Darwin open, and the recorder's lock cannot fail open

From `gs-boundary-defence-in-depth`, `ft-0f6a723d` and `ft-568a30e2`. No export signature, ABI or public Go API change. GitHub Actions has not run this tree. No quiet-host benchmark baseline was recorded.

#### Boundary
- **A failed `F_SETNOSIGPIPE` restores the caller's status flags (I1).** `set_nonblocking` saved `F_GETFL`, set `O_NONBLOCK`, then on Apple set `F_SETNOSIGPIPE`. If that second call failed, `O_NONBLOCK` stayed on a descriptor `open` had not taken. The flags captured before the call are written back with `F_SETFL`. If the restore itself fails, the error names both failures and `O_NONBLOCK` may remain. Success still sets `O_NONBLOCK` and, on Apple, `NOSIGPIPE`. A pre-set `O_APPEND` is kept.
- **`gusset.h` says what `CallHeader.reserved` is.** The field name stays (it is ABI). The comment states it is the engine dispatch opcode, with 0 meaning the global handler, and that unknown flag bits are refused. A unit test fails if either sentence disappears. The `ffi_exports` fuzz target sets `reserved` from its input on every submit.
- **`ffi_exports` stays at 15 of 17 exports.** DECISIONS 2026-10-13: `gusset_init` and `gusset_shutdown` stay out of that target. They are process-global. No lifecycle harness was added.

#### Adopter
- **The step-5 snippet is compiled where the compiler has the Allocator API (I4).** On a compiler whose probe is off, the walkthrough emits a GitHub `::warning::` and continues. `GUSSET_REQUIRE_STEP5=1` turns that skip into a failure. Nightly job `step5-snippet` runs the walkthrough with the probe forced on. A copy of the snippet whose output type does not exist fails `cargo check`, and the compiler output must name that type. Nothing broken is left in the tree.
- **Adopter prose uses the step-1 crate name.** `register_engine`, `BufferAlloc` and `ALLOCATOR_API` say `gusset_core::`. The quoted runtime error still says `gusset::set_engine_handler()`, which is what the library prints.
- **gussetvet matches the files cgo would compile.** `#cgo noescape` and `#cgo nocallback` are one set for the package, required only for an export a file references as `C.<name>`. A second file whose only `#cgo` line is `LDFLAGS` is not a violation. Directives in a file the default build skips (`//go:build ignore`, another GOOS) do not satisfy a reference in a file the build compiles. Each physical line of a `/* */` preamble is reported at its own line.
- **`gusset-example` is an rlib.** `crate-type` no longer includes `staticlib`. `libc` is a dev-dependency. No new `extern "C"` export. `docs/adoption.md` describes it that way.

#### Bench
- **The recorder no longer signals its own process group (I4).** Interrupt and terminate kill the child process group that run started, wait for it, and exit 130. The EXIT trap then removes that run's lock and temporary directory. The lock directory is renamed into place only after `owner` and `pgid` exist. A lock with no pgid is held for 2 seconds instead of being stolen. Variant rounds alternate: odd rounds in listed order, even rounds reversed, and the provenance header records that order.
- **Dispatch benches use the existing API.** A Rust bench binary compares the diagnostic engine with `set_engine_handler` (opcode 0, which `register_engine` refuses) and with `register_engine(7)`. Go benches cover an already-expired `Call`, `Discard`, and `Stats`. No new C export and no second staticlib. A quiet-host baseline was not recorded.

### 2026-10-13 · A required probe fails when it does not run, and a split isolate frame is kept

From `ft-1f4a1228` (CI gates) and `gs-isolate-corrupt-frame-hang` (isolate spike), plus the LiveBytes hygiene fix on the same branch (`ft-8a7cd8ef`). No export signature, ABI or public Go API change. `internal/hostprobe` is internal. The isolate work stays in `internal/isolate`. GitHub Actions has not run this tree.

#### CI
- **A host-load skip fails when the workflow requires the probe (I4).** Those skips called `t.Skip`, so a job that set out to measure a live job passed when the probe landed after the job ended. They now go through `internal/hostprobe.Skip`. `GUSSET_REQUIRE_PROBES=1` turns the same message into `t.Fatalf`; any other value still skips. The race step and the `GOGC=1` step set that variable and include `./internal/...`. `tests/shutdown` is not added to those steps. The matrix and nightly fuzz steps set the same variable; `make fuzz` inherits it.
- **The workflow checker follows the environment the `go test` process sees.** YAML `env` is the initial exported value. `export` sets and exports, and the last assignment on that command wins. A bare `NAME=1` changes the shell value and does not export, so it does not arm the gate. A prefix on `go test` applies only to that process. A step is armed only when every `go test` in it would see exactly `1`. A comment, an `echo` or `printf` of the assignment, and `unset` do not arm it. `gofmt` is `gofmt -l --`, so a tracked file named `-ok.go` is a path. benchgate treats a short sec/op row as a failure and keeps reading the table; `-allow-one-sided` defaults to empty. The shell forms still outside the model (`export -n`, `+=`, short-circuit, `eval`, `env -i`, and the rest named above `TestHostileLaunderDoesNotSetProbe`) are residuals, not covered gates.
- **LiveBytes hygiene compares a quiesced baseline with `<=` (I1).** A process-wide drop below the snapshot, from an earlier test's `AddCleanup`, made `==` fail a clean run. Two consecutive stable samples are the baseline. A held buffer still fails. The 500 ms and 1 s bounds that could only false-fail are removed; the error and hang assertions stay.

#### Isolate
- **A frame the drain deadline split is delivered (I3).** `io.ReadFull` copies a prefix that `detachBufio` cannot see, so the one-shot probe parsed the remainder at the wrong offset and both waiters failed (a mid-header read as an oversize length, a mid-body read as a timeout). An incomplete read is a `frameError` carrying those bytes, prepended after detach. A deadline that consumed nothing still probes once, for 50 ms. Oversize of a complete header, invalid JSON, and an unknown id stay fatal and are not retried.
- **A deadline between the length write and the body write poisons the process.** The stream is marked poisoned and stdin is closed, so a writer blocked on the permit unblocks. `Wait`'s cancel runs on its own goroutine with a one-second budget. If the context ends while that goroutine is still waiting for the write lock, it returns without poisoning.
- **One goroutine owns `cmd.Wait` after a successful `Start`.** The handshake-failure path still waits itself, before a `Proc` exists. After the process exits, `waitProc` gives the reader the 200 ms protocol drain, then sets a read deadline so a grandchild holding the write end cannot stall frames that are already readable. `read` does not call `cmd.Wait`.

### 2026-10-12 · WaitBuffer no longer writes freed memory; the boundary refuses what it used to trust

From the GitPulse board (`gs-boundary-defence-in-depth`, `ft-3d60ee16`, `ft-1f4a1228`, `ft-415ddf1f`, `ft-0f6a723d`, `ft-86b4ae63`, `gs-isolate-corrupt-frame-hang`, `gs-flaky-isolate-sigabrt-cgocheck2`, `ft-c689767b`) and what verifying them turned up. No export signature or ABI change. One Go behaviour change (`WaitBuffer` under a racing `Close`, below).

#### Fixed
- **`WaitBuffer` wrote an inline result into freed memory when `Close` raced it (I1).** It allocated a Rust buffer, took the view with `Bytes` (which drops `cgoMu` on return) and copied with no lock held; a `Close` in that gap freed every buffer of the handle, so up to 4 KiB landed in a freed C-heap chunk. `allocBuffer` now fills the buffer before releasing `cgoMu`. A `Close` that wins now yields a buffer whose `Bytes` is nil, as the large-result path already did, instead of `ErrClosed`. `TestWaitBuffer_CloseCannotFreeTheResultBeingFilled` holds the window open with a test hook and fails every run with the copy outside the lock; under libgmalloc the old code faults at the copy. Plausibly the unidentified writer behind the 2026-09-30 hammer SIGSEGV (a 973-byte `gusset_take` finding a non-canonical tcache pointer in the 992-byte class) — inferred, not reproduced. A read-only sweep of the other 34 sites that view, free or keep alive Rust memory found no second instance.
- **Concurrent respawns could leave a pool with no workers that still accepted work (I4).** Found in review of the sigaltstack change below before it landed; `respawn_lock` makes the slow path of `ensure_workers` single-flight, and `close` never takes it. `concurrent_respawn_does_not_lose_a_failed_worker` fails without it.
- **`gusset_handle_open` refuses a completion descriptor that is not the write end of a pipe (I1).** It checked only that the fd was open, then set `O_NONBLOCK` on it and later closed it. Read ends, regular files and sockets are refused; the caller keeps the fd, flags unchanged.
- **A worker that cannot install its `sigaltstack` fails `open` (or the respawning submit) instead of running unprotected (I5).** Workers report ready over a channel and are awaited against one shared deadline, outside the `workers` lock: `open` at pool size 1024 measured 12.6-18.8 ms against 13.1 ms before (a serial handshake had been 157 ms).
- **A panic after the engine returned no longer kills the worker silently (I2).** `deliver` guards storing and publishing the result: a panic while storing completes the ticket as a panic and poisons the handle; a panic while publishing poisons and logs, and does not publish twice. The outer guard now logs, poisons and releases the cancel flag. DECISIONS 2026-10-12 records what is still accepted.
- **A torn completion write gives up after 2 s without progress, not 10,000 spins.** Under load a live, slow reader was classified as stalled. Unreachable on a pipe with records far below `PIPE_BUF`, which `open` now guarantees.
- **Go: an error code with an OK or nil status is an error (`statusToError`)**, not a zero value with a nil error.
- **`make install` (and `install.sh`) parse under macOS's make 3.81.** A literal `#` inside `$(shell …)` stopped it with "unterminated call to function 'shell'".
- **gussetvet scans what Go builds.** It skips a subdirectory only when it has its own `go.mod` (as `./...` does), not because it holds a `.git` — an empty `.git` file would otherwise have hidden an R4 violation that still compiled — and `target`/`vendor` only at the module root.

#### CI and supply chain
- staticcheck pinned at v0.8.2 (reads Go 1.27.2 export data); the Go patch pin is gone and the lint log prints both versions.
- Every cargo invocation in workflows, the Makefile, `tools/ci/lint.sh` and `bench/r14/run.sh` is `--locked`; cargo-fuzz installs at a pinned version and nightly proves `fuzz/Cargo.lock` current. `tests/ci` enforces locked cargo, pinned installs, read-only default permissions, and untrusted expressions reaching a shell only through `env:` — each rule with a planted violation that it catches.
- The bench job fails when there was no base to compare against, unless the `allow_no_base` dispatch input says that is intended.
- Soak runs every Soak test on Linux and macOS; memlimit also runs `TestMemLimit` under `GOMEMLIMIT`; nightly takes a `hammer_seed` input and fuzzes the new `ffi_exports` target, which drives 15 of the 17 raw C exports with adversarial arguments.
- The lint gofmt step checks the files git would commit, not everything on disk.

#### Tests
- Invariant gaps closed with mutation proofs: the init ABI check is asserted behaviourally (`mustCheckABI` returns the verified report), R6 has `TestR6_SubmitCopiesTheInputBeforeItReturns`, Go's poison latch is asserted at the public surface, isolate frames (oversize, truncated, stalled, duplicate, unknown, out of order) are exercised against a live worker.
- Load-robust timing: tests that failed on a correct build at load 100-400 now separate "correct" from "broken" by a wide margin (`Discard` instead of a racing deadline, relative-to-baseline scheduling checks, best-of-N, a 1 s engine against a 500 ms detach bound) rather than by a host-speed guess.
- The adopter walkthrough gives each pkg-config build its own `GOCACHE` (Go caches pkg-config link flags without keying on `PKG_CONFIG_PATH`; `docs/adoption.md` says so) and tests gussetvet's "no export list" refusal for real.

### 2026-10-10 · The hammer's seed replays, and a nightly leg watches the heap

From `ft-94a2f74b` (the 2026-09-30 nightly SIGSEGV in `gusset_take`, run 36702124084, seed 1790763872967289580). No library, ABI or Go API change. Tests and CI only. **The crash is not fixed and its cause is not known.**

- **A recorded seed can be replayed.** `TestHammer_BoundaryUnderChaos` drew its seed from `time.Now()` with no override, so the crash's seed could not be rerun. `GUSSET_HAMMER_SEED` now sets it, and a value that does not parse as a uint64 fails the test instead of quietly running a different seed. The seed goes to stderr before the lanes start: without `-v`, `t.Logf` output is lost when a cgo call crashes the process. Every failing run ends with `replay: GUSSET_HAMMER_SEED=… GUSSET_STRESS=…`. A seed fixes each lane's operations, sizes and deadlines. It does not fix how the lanes and the Rust workers interleave. `TestHammerSeed_ReplaysTheRecordedSeed` checks the recorded seed, the uint64 bounds and the rejected forms.
- **What the dump shows.** The faulting PC is in glibc. The registers match `_mid_memalign` walking the thread's tcache chain for a 64-byte-aligned (`rbx=0x40`) 973-byte (`rsi=0x3cd`) request, bin 60 (`rdi=0x3c`, the 992-byte class), whose decoded `next` was non-canonical (`rax=rdx=0x5abce2e8632f6205`). The 973-byte request is `RawBuffer::from_bytes` in `gusset_take`. So the first 8 bytes of a freed 992-class chunk on that thread had already been overwritten. `drainPipe` found the damage. It did not cause it. The writer is unidentified.
- **Replay, bounded (no repro).** 30 runs on the crash seed in an arm64 Linux container (Debian, glibc 2.36, Go 1.27.1, 4 CPUs, 60 s each, `GOGC=1`), all passed: 10 with `GLIBC_TUNABLES=glibc.malloc.tcache_count=0:glibc.malloc.perturb=165 GOTRACEBACK=crash`, 10 with Gusset built `-Zsanitizer=address` (nightly-2026-10-05) in a `go test -asan` binary, and 10 under `-race`. The 2026-10-09 audit ran 125 more, all passing. CI's x86-64 ubuntu-24.04 (glibc 2.39), where the crash happened, has not run either new leg yet.
- **New nightly job `hammer-heap`.** It runs those two legs for 5 minutes each on ubuntu-latest, where a stray heap write faults at the write rather than in a later allocation on another thread. The ASan leg checks that `gusset_take` in the linked binary calls into ASan. Without that check, a link against the plain archive, or a cached link of it, would pass unexamined. `binutils`' `nm` lists no symbols for rustc's LTO objects, so the check disassembles the function. The job is in `report.needs`, so a failure opens an issue.

### 2026-10-10 · gussetseal accepts what Apple's ar writes

From the 2026-10-09 audit (`gs-gussetseal-darwin-ar-padding`). No library, ABI or Go API change; `tools/gussetseal` only.

- **A sealed object whose length is not a multiple of 8 is no longer refused on macOS (R14).** `archiveHoldsExactly` compared the archive's member to the verified object with `bytes.Equal`. Apple's `ar` pads a `#1/len` member with `\n` to a multiple of 8 inside the size it declares, so a 529-byte object comes back as a 536-byte member. Every unaligned `ld -r` output was refused and its archive deleted. CI runs 37944107330 and 37855657375 failed with "its member (2346576 bytes) differs from …libengine_a.o (2346575 bytes)", and the `R14 sealed staticlibs (macos-latest)` job had failed on every main push since 97947d0. A BSD member is now accepted when it is exactly the object followed by `\n` up to the next multiple of 8, and nothing else: its length must equal `roundup8(len(obj))`. NUL bytes, any other byte, a shorter or longer tail, or the same tail on a GNU-format member are still refused, and a GNU-format member must still match exactly. `TestArchiveOfAnUnalignedObjectIsAccepted` archives objects 1, 3 and 7 bytes past a multiple of 8 with the host `ar`. It fails on the old code on darwin, and on Linux it passes either way because GNU `ar` pads outside the size. `TestArchiveRefusesWhatIsNotPadding` covers the refusals on hand-built archives. The audit traced why R14 passed locally: the local `ld -r` happened to write 8-aligned output, so the failure depended on the toolchain and showed up only in CI.
- **A negative BSD name length is refused instead of panicking.** A member named `#1/-5` sliced its data with a negative bound. `TestNegativeBSDNameLengthIsRefused` panicked on the old code.

### 2026-10-10 · The log ring keeps its lines under contention

From the 2026-10-09 audit (`gs-log-ring-quadratic-eviction`). No export signature, ABI or Go API change; `gusset::ffi::log_dropped_total` is a new Rust diagnostic.

- **Two threads logging onto a full ring no longer drop lines (I2, I3, I5).** `log_event` tried the ring lock 16 times, a `spin_loop` hint apart, then dropped the line. One eviction from a full ring outlasts that, and the ring stays full in normal use because Go drains it only on `DrainLogs`. With one thread writing short lines and another writing "completion write failed", "drain budget expired" and "WITHOUT sigaltstack" lines, about 15,000 of 16,200 lines were dropped per run (3 runs on the old code). Every caller now waits for the ring. The hold is bounded: nothing under the ring lock blocks, calls out or logs. The try-lock was there so a panic hook firing inside the critical section could not deadlock. `LOG_OWNER` now records which thread holds the ring (`thread_local!` is ruled out in `ffi/` by R7), and only a call re-entered on that thread drops its line. `log_ring_contention` asserts that no line is dropped and that the last critical line is in the ring. With a backported drop counter it fails 3/3 on the old code. `logging_while_holding_the_ring_drops_instead_of_deadlocking` covers re-entry.
- **Eviction is one cut and one shift.** `append_line` found one newline per pass and shifted the rest of the ring each time, all under the ring lock. `evict_cut` scans forward once for the cut point, and `append_line` drains up to it once. Release build, best of 5: a 60 KiB line onto 8000 eight-byte lines went from 2,820 µs to 1.5-1.8 µs. Onto three large lines it went from 16.8 µs to 2.0 µs. Instrumented, the old loop shifted the ring 7,489 times for that line. `one_cut_evicts_what_line_by_line_eviction_does` compares `evict_cut` against the line-by-line loop on 2000 seeded rings. `a_long_line_onto_many_short_lines_evicts_in_one_cut` pins the 59,912-byte cut. `log_ring_matches_its_model` now runs that case through `check_log_ring` against the line-by-line model first. The unit tests call `evict_cut`, which does not exist on the old code, so there they fail to compile rather than fail.

### 2026-10-10 · Submit sends without a sender mutex

From GP-FEAT-001. No export signature, ABI or Go API change.

- **Parallel submits no longer convoy on one Rust mutex (I3, I4).** `Handle::sender` was a `Mutex<Option<QueueSender>>` held across the cancel-flag lock and the queue's push lock; a native sample of `BenchmarkGussetCallParallel` put 986 of 1116 Rust mutex-wait samples in it. `close` now closes the queue directly and `submit` sends without it: the flag goes in before the send and comes out when the send is refused, and `close` closes the queue before `cancel_all`, so every queued unit's flag is cancelled and a refused submit leaves none. Interleaved A/B over 10 rounds against `23e925b`: CallParallel -6.6% (p=0.012), min 3325 -> 2707 ns; CallNoop unchanged (p=0.90). `submits_racing_close_leave_no_flag_and_none_is_accepted_after` covers the race. A performance change, so no test fails on the old code; the A/B is the evidence.
- **Benchmarks re-recorded on this tree.** The main suite (`bench/results/darwin-arm64-go1.27.2-rust1.99.0.txt`) and the crossover and thread-pressure files replace the 2026-09-20 recordings, and `make docs` regenerated the README table and charts. The README's CallNoop `3.000 ± 33%` allocs/op was that old data: a memory profile puts today's single allocation at `drain.go:88`, the result bytes `Call` returns (1 alloc/op, 1 B/op). `docs/choosing.md`'s prose now matches the new crossover: about 1.24x raw cgo serial at ~7 µs jobs, 3.1x at ~0.7 µs, and 6x at ~7 µs under 18 goroutines. The 1.5x bar on 1-10 µs jobs is not met; `bench/results/gp-feat-001/README.md` has the profiles naming the contended locks and what comes next.

### 2026-10-09 · The quiet handoff leaves a P for everyone else

From the 2026-10-09 audit (`gs-perf-quiet-handoff-gomaxprocs2`). No ABI or Go API change.

- **A lone call's handoff runs quietly only at GOMAXPROCS >= 3 (I4).** `a4d4cd8` let the waiter poll its result channel and the drain reader poll the ring without yielding whenever GOMAXPROCS was above one. Neither loop passes through the scheduler, so at GOMAXPROCS=2 they held both Ps: a goroutine sleeping 200 µs beside serial Calls woke 2.2-3.5 ms late at the median on an M5 Pro (max 21-25 ms), against 2-4 µs with `a4d4cd8` reverted, and under a Linux 2-CPU quota serial Calls themselves slowed, the two pollers and the Rust worker contending for two CPUs. The shared gate (`multiP`, now `quietProcs` behind `quietMinProcs` in `drain.go`) leaves one P for everything else; below it the handoff is the one before `a4d4cd8`. `TestSerialCall_LeavesAPForOtherGoroutines` fails on the code before this and passes after; `TestQuietProcs_FollowsGOMAXPROCS` covers both sides of the threshold, including a GOMAXPROCS change while a handle is open.
- **The first certified record of `a4d4cd8`'s effect** (`bench/results/handoff-gomaxprocs/`): head, `a4d4cd8` reverted and the fix, interleaved over 10 rounds at GOMAXPROCS 2, 3, 4 and 8. At 2 the fix is no different from reverted (p = 0.97 on `GussetCallNoop`; minima within 3%); at 3, 4 and 8 it is no different from head and 37-42% faster than reverted. On this 18-core host head is faster at 2 because the Rust worker has a core of its own; that is given up for the goroutines it starved.
- **`bench/record.sh` takes `RECORD_VARIANTS` and `RECORD_CPU`.** The first records several builds of the same benchmarks in one recording, interleaved by round and labelled with a `variant:` line for `benchstat -col variant`; the second passes `-cpu`. Both are off by default.

### 2026-10-09 · The hygiene tests' baseline waits for the idle thread count

No library, ABI or Go API change. Tests only.

- **Resource-hygiene failures on Linux were a measurement race, not a leak (I4, I5).** CI run 37944107330 failed `resource_hygiene` with threads 3 then 2 and descriptors and mappings exact. The baseline was the first reading after the warm-up, kept once a second matched it. `/proc/self/task` lists a thread for a moment after `join` returns, so a just-joined worker or `gusset-close` joiner got counted in the baseline, and the next reading was one lower. A baseline that is too high would also hide a real one-thread leak. Both tests now record the thread count before any handle exists and take the baseline once `/proc/self/task` is back to it. If it is not back within 5 s, the baseline panics instead of measuring against a busy process. `respawn_and_failed_open_release_threads_and_mappings` had the same bug (the audit traces run 37860820816 to it). With a detached canary thread exiting 1.5 s after the warm-up, both tests fail 10/10 on the old baseline and pass 10/10 on the new one. `hygiene_baseline` (new binary) keeps that canary as a test.
- **One owner for the `/proc/self` readers.** `count_dir`, `maps`, `mapped_bytes`, `Usage`, `baseline` and `settled` live in `tests/common/procfs.rs` (Linux only). Before, `resource_hygiene.rs` and the pool unit tests each had a copy. `drain_logs_boundaries.rs` now uses the log-ring drains in `tests/props/mod.rs` instead of its own `drain`/`drain_all`.
- **`write_completion_gives_up_on_a_full_pipe_once_the_handle_closes` starts its clock before it spawns the 200 ms closer.** The closer could begin sleeping before `spawn` returned, so a slow spawn measured less than 200 ms even though the writer waited the whole time. The 2026-10-09 audit reports one such failure under TSan; a 150 ms delay injected after the spawn fails the old order and passes the new one.

### 2026-10-09 · Buffer ids are unique across handles

From the 2026-10-09 audit. No export signature, ABI or Go API change.

- **An engine can no longer return another handle's buffer as its output (I4).** Buffer ids came from a per-handle counter starting at 1, but the engine registry is one per process, so a Rust host's engine could hold handle A's buffer id and return it while running on handle B. B usually had a buffer under the same number: B claimed its own buffer as the output, B's caller received B's bytes with no error, and A's buffer was never handed back. Buffer ids now come from one process-wide counter (`NEXT_BUFFER_ID`), the same way tickets already did (`NEXT_TICKET`), so a foreign id is unknown on B and the call fails with "no such live buffer". Go hosts could not reach this: their engines never see a `Handle`. `a_foreign_handles_buffer_id_is_refused_as_an_output` fails on the code before it. Allocation now takes one shared atomic, as submission already did for tickets. Nothing was measured.
- **One ceiling test for both id counters.** `buffer_ids_stop_at_the_ceiling_instead_of_wrapping` pushed the handle's own counter to `1 << 63` and expected a handle's first id to be 1. Neither holds now, and a process-wide counter cannot be pushed to the ceiling while other tests run in parallel. That test and the ticket ceiling test are now `ids_stop_at_the_ceiling_instead_of_wrapping`, which checks `reserve_id` on a local counter. Both counters reserve ids through `reserve_id`.

### 2026-10-08 · Every certified benchmark is charted

No library, ABI or Go API change. `DECISIONS.md` 2026-10-08 records the rule.

- **`tools/benchplot` draws the rest of the committed results (`gallery.go`).** Seven new charts in `docs/img/`: Gusset's own suite on darwin (`darwin-suite.svg`) and, before and after, on the Linux VM (`vm-suite.svg`); the VM's threads, resident memory and wall time under concurrency (`vm-threads.svg`, `vm-memory.svg`, `vm-scaling-time.svg`); and peak threads at 512 in flight on both hosts (`thread-pressure.svg`). Every file goes through `mustLoad`, so one without a provenance header is fatal, and `make docs-check` fails when a chart drifts. The three existing charts regenerate byte-identical.
- **`docs/benchmarks.md` (new)** shows the charts and lists every `Benchmark*` function in the tree against the chart that covers it, or the reason none does: refused by a check, no provenance header, or never recorded. It is linked from the README and `docs/choosing.md`.
- **`linux-amd64-vm/transport-after.txt` is not charted, and the check was not loosened.** Its arms disagree by 11% about how long the same Rust loop takes, over the 10% `calibrationTolerance`. The README had quoted ratios from it as hand-typed figures; those bullets are replaced by the charts and a pointer, and the VM README now marks that table as not certified. Re-record with `make bench-crossover` on that host.
- **`ThreadPressure` bars are single runs** (`-count=1`, because a thread count is only attributable to the first transport sampled in a process). The chart and `docs/benchmarks.md` say a gap of a few threads is not resolved.

### 2026-10-08 · Tests and CI that measured nothing now measure

- **`internal/isolate` (new, internal; the Go entry-point count stays 13).** A spike of whether one `Handle` contract can front both the in-process and an out-of-process mode: `Serve` runs a `*gusset.Handle` in a worker process, `Proc` is the host side over length-prefixed JSON frames, and both satisfy one `Caller` interface with parity tests. A worker killed by SIGKILL or SIGABRT fails its in-flight tickets and every later call with an error matching `ErrPoisoned`. The `Buffer` surface is not carried (`ErrBufferUnsupported`). `DECISIONS.md` and `docs/ipc.md` record the finding.
- **The churn tests count threads and descriptors on darwin.** They read only `/proc`, so on macOS every thread and descriptor comparison was -1 against -1, a pass that measured nothing. `countOSThreads` now uses `ps -M` and `countFDs` falls back to `/dev/fd`; a platform that still cannot count skips instead of passing. A new churn test poisons a handle, closes it and opens a fresh one repeatedly, and requires goroutines, threads, descriptors, the Go heap and Rust live bytes back at a warm baseline.
- **One set of Rust test helpers (`tests/common/mod.rs`) replaces per-file copies** (eight `make_pipes`, four ticket readers, three `Must` traits). `read_ticket` waits at most five seconds, so a lost ticket fails with a message rather than hanging the binary until CI's job timeout.
- **CI:** the interleaved benchmark loop's unused counter is named `_` (actionlint SC2034).

### 2026-10-08 · Repo tooling checks the tree and the file it was given

From the 2026-10-07 audit. Each fix has a test that fails on the code before it. No library, ABI or README change.

- **gussetvet scans the module that holds its target, not the working directory.** R4 and the R5 callback scan walked `"."`, so `gussetvet /repo/internal/ffi` run from anywhere else — `install.sh` does exactly that — checked an unrelated tree and reported both as passed. Both scans now walk the nearest `go.mod` at or above the target, and a target outside any module is an error. `main` is now a testable `run`, and the `walkGo` doc no longer claims test files are skipped.
- **benchdoc takes a results file: `benchdoc [-check] <results-file>`.** With more than one file in `bench/results` it asked for one to be passed explicitly, but read no arguments, so it could not run at all.
- **benchdoc no longer drops benchmarks.** Rows whose names did not start with `Gusset` or `Channel` were skipped without a word, despite the promise that an unmapped benchmark shows up under its raw name. Only a numeric `-N` suffix is stripped now (`size-large` was cut to `size`), and a section for a custom metric no longer inherits the previous unit and overwrites the allocs/op column. `parseBenchstat` has table tests.
- **docblock refuses a BEGIN or END marker that appears more than once.** Only the first block was rewritten, leaving a second copy of the numbers stale.

### 2026-10-08 · A serial call's handoff no longer wakes a thread

- **A lone in-flight call reaches its caller without the scheduler (I4).** The drain reader yielded with `runtime.Gosched` on every poll of the completion ring, and each yield with a P idle and no thread spinning woke a thread with a system call (`pthread_cond_signal` on darwin); the waiter then parked on its channel, and the delivery that readied it paid another. Polled through a job, the wakes grew a serial `Call`'s overhead with the job's length: profiled over 200k serial calls on an M5 Pro, `runtime.wakep` cost 1.5 µs per call at ~0.7 µs jobs and 5.9 µs at ~7 µs jobs, 78% of it from the reader's own yields, while a Rust-only round trip stayed flat at ~3 µs. With at most one call in flight and more than one P, the waiter now polls its buffered result channel for up to 50 µs before parking, and the reader polls without yielding, yielding once after a delivery that readied a goroutine. With one P, or more than one call in flight, both behave as before. `TestSerialCall_HandsOffWithoutScheduling` counts sampled scheduler transitions over 4000 serial calls: 16k–30k before, 0–10 after. Exploratory interleaved min-of-5 on a loaded M5 Pro (not recorded through `bench/record.sh`; the machine was below its 60% idle floor): serial ~0.7 µs jobs −42%, ~7 µs jobs −30% (1.89x → 1.36x raw cgo), `CallNoop` −10% to −20%, `CallParallel` and the parallel crossover unchanged.

### 2026-10-08 · R14: a second Rust staticlib is sealed, not forbidden

No export, Go entry point or ABI change. `DECISIONS.md` 2026-10-08 and `docs/rfc-r14.md` carry the evidence.

- **R14 rewritten.** One *unsealed* Rust staticlib per binary (`libgusset.a`); any other is sealed. The old rule's only guard was the duplicate-`rust_eh_personality` link error, and that fires only under fat LTO: built without LTO by one toolchain, a second archive links cleanly and silently shares std with the first, panic hook and global allocator included (`bench/r14` `nolto` scenario).
- **`tools/gussetseal` (new).** Seals a staticlib into one object with only its prefixed C ABI global, renames every ELF COMDAT group and common symbol for it, drops `.llvmbc`/`.llvmcmd`, then re-reads the object with `debug/elf`/`debug/macho` and refuses to write the archive on a leaked global (a Mach-O private external counts), a shared COMDAT group or common, no exports, or a `gusset`/Rust prefix; after `ar` it re-reads the archive and deletes it unless its member is byte-identical to the verified object. On darwin the architecture comes from the archive (`-arch` was missing, and an x86_64 archive sealed on arm64 came out empty); universal archives are refused. ASan's `___asan_globals_registered` is the one common left to merge: one flag per engine re-registered every instrumented global in the binary and ASan aborted with an `odr-violation` on Gusset's `LIVE_BYTES`. A localize-only seal, the usual recipe, crashes on ELF the first time the second engine panics. Each of these has a test that fails without its fix.
- **`bench/r14/` harness, `r14` and `r14-musl` CI jobs.** Two independent engines sealed beside `libgusset.a`, driven concurrently with panics in all three; asserts per-engine panic hooks, allocators and thread-locals, under `-race`, `cgocheck2`, `-asan`, and with Gusset and both engines built `-Zsanitizer=address` (glibc), without LTO and across a 1.97/stable toolchain skew. Covers every supported target: linux x86-64 and arm64 (glibc), linux x86-64 musl, darwin arm64 and x86-64 (Rosetta). `run.sh build|check` splits cargo from the rest for emulated checks, and each link stamps the archives' hash into a generated file so a cached link is never reused. A skipped test or scenario fails the job.
- **`docs/adoption.md` §1b** covers when to seal instead of joining the umbrella, and what a sealed engine gives up (Gusset's firewall and memory accounting).

### 2026-10-08 · Structure, folded duplicates, retired Rust items

- **`pool/mod.rs` and `handle.go` are split along their seams; no public path moves.** `pool` keeps the shared types and adds private `worker`, `completion`, `buffers`, `engine`, `diagnostic` and `lifecycle` modules whose public items are re-exported, so rustdoc's item list is unchanged; the tests move to `pool/tests/`. `handle.go` becomes `handle.go`, `options.go`, `submit.go`, `wait.go`, `drain.go`, `ticket_reader.go` and `tickets.go`; `go doc -all .` is byte-identical.
- **Near-duplicates have one owner.** The two caught-panic arms on a worker, the three buffer registrations, the eleven argument refusals and seven result tails of the handle exports, `queue::lock` (now `lock_recover`), the error-only cgo epilogues (`checkStatus`) and the pipe reader's second record parser (`nextPipe` now uses `parseBuffered`) are each one function. No behaviour changes. Kept, and still reported by `devmap clones`: `Cancel`/`BufFree` and `HandleClose`/`CancelAll` in `internal/ffi/ffi.go`, two-line cgo wrappers that differ only in the export they call (a shared caller moves the status onto the heap, one allocation per large-result `Wait`; the one shape that does not bypasses R5's `#cgo noescape` lines and DevMap still pairs it; see `checkStatus` and `DECISIONS.md` 2026-10-08), and the `cancel`/`buf_free` adapters in `ffi/mod.rs`'s test table, one per export by design.
- **Removed from the Rust crate's public surface (no C ABI or Go change).** None had a caller outside its own tests, here or in the known adopters; `DECISIONS.md` 2026-10-08 has the evidence. `pool::sys::write_ticket` and `write_ticket_attempt` (a bounded writer production had replaced with `write_completion`; their two tests now test `write_completion`), `pool::sys::RawBuffer::to_vec`, `ffi::guard::ffi_guard` (the `Option` form of `ffi_guard_code`), `alloc::record_alloc` and `record_dealloc` (superseded by `count_buffer_alloc`/`count_buffer_dealloc`), and `JobContext::mark_finished` and `compute_duration` (the finish instant was set after the engine returned, so no reader ever saw it). `JobContext::total_duration` stays and always measures to now, which is what an engine mid-job already saw; it drops one `Instant::now()` per job from the worker. The public surface in the sense of AGENTS.md rule 4 (17 C exports, 13 Go entry points) is unchanged: these are rustdoc items of a 0.0.x crate that is not on crates.io, used by no adopter.
- **gussetvet's R5 directive check reads the `import "C"` preamble only.** It treated every comment group mentioning `#cgo` as a preamble, so a doc comment naming a directive failed every export, and a preamble with no directives at all passed whenever another comment spelled them out. A directory with no preamble carrying `#cgo` lines is now an error, not a pass.

### 2026-10-07 · Errors keep their meaning across the boundary

From the 2026-10-07 audit. Each fix has a test that fails on the code before it; no export signature changed, so the ABI version and header signatures are unchanged.

- **An engine that notices its own deadline reports a deadline (I3).** `execute_unit` mapped every engine `Err` to a generic error, and Go recognises a cancel only by the runtime's exact `cancelled: …` text, so the documented pattern (the example engine's opcode 10 returned `"deadline exceeded"`) never matched `context.DeadlineExceeded`. An engine `Err` returned after `ctx.check()` fails is now reported as `Cancelled(reason)`. `CancelReason` implements `Display` and converts into `String` with the canonical text, so `ctx.check()?` works in an engine; the example engine, the diagnostic engine and `docs/adoption.md` use it. `an_engine_stopping_on_its_own_deadline_reports_deadline_exceeded` fails before the fix with `Err("deadline exceeded")`.
- **A panic inside `gusset_shutdown` is not reported as budget expiry (I2).** The unwind arm returned `FFI_ERR`, the expiry code, and Go mapped every non-OK code to `ErrShutdownIncomplete`. It returns `FFI_PANIC` now, which Go reports as `ErrPanic`; `gusset_init`'s unwind arm does the same, and both log the payload instead of dropping it in `extern "C"`.
- **The completion reader's exit cause is kept (I4).** A corrupt record or unexpected read error ended `drainPipe` on an open handle and the error was dropped. It is logged with `slog.Warn`, stored on the handle, and carried by the waiter's error and every later refusal; all still match `ErrClosed` and keep their historical text.
- **No more made-up `file:line`.** Engine errors, bad arguments and poison stamped `gusset.rs`, `ffi.rs` or `handle.rs` (none exist) with the constructor's own line, and Go printed `(at gusset.rs:204)`. Only a caught panic carries a location now.
- **Boundary refusals are sentinels.** `ErrNilHandle`, `ErrNilContext`, `ErrNilBuffer` and `ErrInputTooLarge` are exported beside `ErrClosed`, with unchanged texts; `ErrUnknownTicket` and `ErrTicketBusy` moved there.
- **`gusset_cancel` / `gusset_cancel_all` are documented as they behave.** They return `FFI_OK` without reporting whether a ticket was found or how many were cancelled; `gusset.h`, the Rust docs and the Go wrappers now say so.
- **Two races the new tests exposed.** `install_panic_hook` set its flag before installing the hook, so a concurrent caller (another handle opening, `gusset_init`) could return early and a panic in that window lost its location; it uses `Once` now. In tests only, the fault injector held one armed thread, so parallel tests disarmed each other (for `gusset_shutdown` that would have run a real shutdown); it keeps a set now.

### 2026-10-07 · Bounds and validation at the public API

- **`Handle.Discard(ticket)` (new, the thirteenth Go entry point; I1, I4).** A result nobody Waits for was kept until `Close`, and since a finished `Submit` returns its permit nothing slowed a fire-and-forget caller down: results over 4 KiB piled up as Rust memory the Go GC cannot see. `Discard` frees a stored result at once and a running job's result when it lands, and the permit still comes back only when the work stops. It does not cancel the work. `Submit`'s doc now states the retention. `TestPitfall_DiscardReleasesUncollectedResults` measures the growth and the release.
- **`WithPoolSize(0)` and `WithPoolSize(-1)` say what is wrong (I4).** They were refused as "pool_size exceeds maximum 1024"; the error is now "pool_size must be at least 1 (got N)", and an over-ceiling value names itself. A dead `> MaxPoolSize` check in `Open` is gone.
- **A negative `WithBufferBudget` is refused by `Open`.** It was silently read as 0, which means unlimited.
- **A refused `Handle::open` leaves the caller's descriptor alone (I4).** It set O_NONBLOCK (NOSIGPIPE on Darwin) before checking the pool size, so a refused open still changed the caller's fd. Arguments are now all checked first and the descriptor's mode changes only once the pool is up. An unreachable `pipe_write_fd < 0` branch is removed.

### 2026-10-02 · Docs catch up with Close

- **`Close` is documented as bounded.** README, `docs/adoption.md` and `docs/choosing.md` still said `Close` has no budget. They now describe the 30-second join, the error when a worker outlasts it, and that the Rust pool stays allocated until that worker exits.
- **`docs/adoption.md` covers engine registration and idle permits.** `register_engine` collisions and opcode 0 are refused; a `Submit` that nobody waits on returns its permit.
- **`AGENTS.md` notes that GC-backstop closes are limited to four at a time.**

### 2026-09-30 · Close, engine registration, and idle permits

- **`Close` is bounded (I3, I5).** It joins workers for at most 30 seconds. If one is still inside an engine call, `Close` returns an error and a background thread holds the pool until that worker exits, so the caller is not wedged and the pool is not freed under it. A close whose workers do exit still joins them and closes the pipe before returning. `close_returns_while_a_worker_ignores_cancel` fails against an unbounded join.
- **`register_engine` reports collisions and refuses opcode 0.** A second registration of the same opcode returns an error and leaves the first handler. Opcode 0 is not inserted: dispatch never consults the registry for it. `register_engine_does_not_overwrite_an_opcode` and `register_engine_rejects_opcode_zero` fail against the silent insert.
- **A `Submit` that nobody waits on returns its permit when the reader stores the result (I4).** The result stays until `Wait` or `Close`. `TestPitfall_SubmitWithoutWaitDoesNotWedgeThePool` fails while the permit stays held.
- **Engine hooks stay process-global.** A panic poisons the handle and leaves the hook. `clear_engine_handlers` drops it, and the next handle does not run until the adopter registers again. `AdviseMemoryLimit` still does not see Metal or mmap memory; that limit is documented on the function, with no new API. `write_ticket` remains the test helper that gives up after 10s; production completion writes go through `write_completion`, which retries.

### 2026-09-28 · Four audits of the Go/Rust boundary

Each fix below has a test that fails on the code before it; each commit names the invariant it protects.

- **A waiter no longer reads freed Rust memory as its result (I1, I4).** `Close` and the completion reader's error exit reset the take-buffer map for every ticket, including results already handed to a waiter, which then returned a view of freed memory: right length, wrong bytes. Only unclaimed results are freed now.
- **A second `Wait` no longer parks forever on a delivered ticket (I4).** Delivery and collection now happen in one critical section, so a second `Wait` gets `ErrTicketBusy`.
- **No permit can be stranded.** A trace carrier that panics, or calls `runtime.Goexit` (a `t.Fatal` in a test carrier), used to run after the permit was taken; carriers are read before it. Submitters parked when the completion reader dies are refused instead of hanging or being admitted.
- **Any panic caught at a handle export poisons the handle (I2),** not only an engine panic. With a NULL status, exports now return the guard's code rather than `FFI_BAD_ARG`.
- **Rust pool:** a push during a worker's fast take now wakes a sleeper (a lost wake-up serialised work), and ring overflow is counted only when the spill reached the pipe (a phantom overflow kept the reader spinning).
- **Boundary text:** engine error messages are capped on a character boundary like panic payloads; `gusset_drain_logs` hands over whole lines and never splits a UTF-8 character (call until it returns 0); a leaked destructor payload is freed.
- **`ErrClosed` (new, exported).** Every closed-handle error matches `errors.Is(err, gusset.ErrClosed)`; the historical texts are unchanged, so substring matchers keep working.
- **ABI check:** `tests/header_field_types.rs` compares every struct field's C type with Rust's; a same-width signedness flip passed every earlier check.
- **Evidence:** a chaos hammer (`stress_internal_test.go`, `GUSSET_STRESS`), a single-handle bookkeeping soak (`GUSSET_SOAK`), a 20k-handle churn test (`GUSSET_CHURN`) that returns goroutines, fds and Rust bytes to baseline, 13 boundary fuzz targets (Go native and `fuzz/` cargo-fuzz), and CI that runs them: the root package under `-race` and `GOGC=1`, a nightly long hammer and fuzz, Miri seed sweeps, ASan over the new tests, and `cargo test --release`.
- **CI:** the Lint job passes (staticcheck's own suppression syntax; `deny.toml` on the cargo-deny v2 schema).
- **A burst of handles dropped to the GC backstop no longer keeps a thread per handle.** Each backstop close joined its workers in cgo on its own goroutine, and Go never returns the Ms those calls used: 48 handles dropped in one GC left 46 threads behind for the life of the process. Backstop closes now take one of four slots first. The cleanup callback still only spawns a goroutine, so Go's single cleanup queue never waits; the goroutine waits, parked, holding no M. Explicit `Close` is unchanged. `TestBackstop_ABurstOfDroppedHandlesRetainsBoundedThreads` fails against the unbounded closer (+46 threads, bound 16).
- **`cargo deny` checks transitive crates for maintenance too** (`unmaintained = "all"`, was `"workspace"`): gusset is linked into every host, so a dependency's dependency ships in them.

### 2026-09-27 · One allocation per Call

- **Result channels are reused.** Each wait used to create a `chan callResult` with a buffer of one. Because `callResult` holds pointers, that is two allocations, the channel header and its buffer, and they were the only two a `Call` made. A per-handle free list, guarded by the mutex these paths already hold, hands them out again. A channel returns to the list only once it is empty and out of `pending`: every send happens under that mutex and removes the channel first, so no late send can reach a reused one. A channel that still holds a value is dropped rather than reused. The list never exceeds the pool size, because each waiter holds a permit.
  - Allocations: `Call`, `Submit` + `Wait` and a 64 KiB copy go from 2 to 1 per op. The one left is the result slice handed to the caller. The zero-copy path goes from 4 to 3.
  - Bytes per `Call`: 160 → 1.
  - Time (interleaved, n=8): serial `Call` −7.6%, parallel −12%, `Submit` + `Wait` −6.9%. The buffer paths are unchanged within noise.
  - Tests:
    - `Call` allocates only its result.
    - The free list refuses a channel that holds a value and never grows past the pool size.
    - Both deadline branches (a completion racing the deadline, and abandonment) are driven deterministically and recycle an empty channel, with the correct permit handling.
    - A 32-goroutine stress with deadlines firing mid-flight gets every caller its own result. Coverage confirms it reaches all three recycle sites.
    - Two mutants (recycling a channel that holds a value, and skipping the recycle on abandon) are each caught.

### 2026-09-27 · Ring hardening, and where Gusset fits the other apps

- **Both completion-record parsers are fuzzed.** `FuzzTicketReaderPipeBytes` feeds arbitrary bytes to the pipe reader. `FuzzRingSlotDecode` puts arbitrary words into a published ring slot. Both run in `make fuzz` and in the CI fuzz job, which grows from about 6 to about 10 minutes (5 targets × 120 s).
- **Close while the reader is parked on the ring is tested.** `TestCloseWhileReaderIsParkedOnTheRing` runs 40 rounds, idle and with a job in flight. `Close` returns, the waiter gets an answer, and the drain goroutine exits.
- **`docs/integrations.md`.** DevCouncil's existing integration (the dc-glob fnmatch engine) was checked against this Gusset: the umbrella carries all 17 exports, and `gusset-check` reports ok. The page measures `devmap` at 11–26 ms per query when each query starts a process, against 0.8–18 ms from a warm one. Tracing every Go caller showed that none of them repeats queries: `dcmap` calls `status` once or twice, and the rest run once. Agents already use the warm `devmap mcp`. So neither a warm-process pool nor an in-process Gusset engine would speed anything up there. The first version of the page recommended a pool, and that has been corrected. GitPulse and Manvi could not be inspected in this session.

## [0.0.2] - 2026-09-26

Release since v0.0.1. The C ABI is 17 exports. Go `Open` publishes completions into a shared-memory ring and uses the pipe as a doorbell; a success of at most 48 bytes is carried in the record. Rust 1.100+ engines can return `Vec<u8, BufferAlloc>` without a copy. On the recorded Linux VM, serial no-op `Call` is 3.73 µs with the ring (`bench/results/linux-amd64-vm/ring-*.txt`).

### 2026-09-26 · Completion ring: no system calls per round trip

Measured on one Linux VM. The raw data and tables are in `bench/results/linux-amd64-vm/`
(`ring-*.txt`).

- **Completions travel through shared memory.** A worker publishes each completion record into a ring of 128-byte slots (`pool::ring`, a bounded multi-producer queue with a Vyukov-style sequence word per slot). The Go reader polls the ring with atomic loads. The pipe only wakes a reader that has parked: before parking, the reader sets a `waiting` flag, and the next worker to publish writes one 8-byte wake token (ticket 0). In steady state a round trip makes no system call.
  - Serial `Call` 7.5 → 3.7 µs (−50%). Parallel 3.2 → 2.4 µs (−23%).
  - Against the raw-cgo sweep: serial 1 µs jobs −39%, parallel 10 µs −18%, parallel 100 µs −10%. The 1 ms rows are unchanged.
  - Allocations are unchanged. An idle handle costs no measurable CPU.
- **Two new C exports, `gusset_handle_ring` and `gusset_ring_release` (17 in total).** The ring is opt-in. A C host that never attaches one keeps the pipe protocol unchanged. The layout and the reader's protocol are in `gusset.h` (`GUSSET_RING_*`). `constants_match` checks the offsets and `header_match` the signatures. The ring's memory outlives `gusset_handle_close` until the reader releases it. If the ring is ever full (Go's permits rule that out), a record goes through the pipe and an overflow counter tells the reader to look there.
- **The reader's poll window for a lone job adapts.** It is twice a moving average of recent completion gaps, clamped to 50–200 µs. A flat 200 µs made the reader spin through GC pauses on 64 KiB results (+11%, through extra scavenger and page-fault work in the runtime). Adaptive, those results are at parity and ~115 µs jobs keep the full window.
- **Work-queue pollers read an atomic length before taking the lock, and do not yield for their first 5 µs.** Measured alone: small parallel jobs −3 to −6%.
- **The diagnostic SIMD kernel picks its vector width at run time.** It is the same loop compiled under `#[target_feature]` for AVX2 and AVX-512BW, dispatched with `is_x86_feature_detected!`. The crate is built for baseline x86-64 (SSE2), which capped it at 148 µs per MiB. It now runs 1 MiB in 46 µs, beating both a `target-cpu=native` build (57 µs) and Go SIMD on the same host (69 µs). `docs/adoption.md` shows the pattern for engine kernels, and `docs/choosing.md` has the new SIMD crossover (about 170 KB on that host).
- **`crates/gusset/examples/rt_latency.rs` reports percentiles** and polls a non-blocking pipe, like the Go reader.
- **The architecture docs describe this path.** README, `AGENTS.md`, `docs/PLAN.md`, `docs/why.md`, `docs/platforms.md`, `docs/choosing.md`, and `docs/adoption.md` had still called the pipe the only completion path and the ABI 15 exports. They now describe the 17-export surface, the ring, inline records of at most 48 bytes, and the pipe as the doorbell.

### 2026-09-25 · Performance and the last open gaps

Measured before and after on one Linux VM; the raw data and tables are in
`bench/results/linux-amd64-vm/`.

- **Serial calls are about 9× faster.** Each call slept twice, once in a Rust worker's `recv` and once in the Go reader's netpoll. On a virtualized host a wake-up costs tens of microseconds. A new `pool::queue` lets workers poll briefly, yielding the CPU, before parking, with no lock held while waiting. The drain reader polls the pipe the same way, reads up to 64 tickets per syscall, and polls longer when one job is in flight. An idle handle costs no CPU.
  - `Call` no-op: 90 → 10.6 µs.
  - Parallel: 24.5 → 5.9 µs.
  - Against a blocking cgo call, for 10 µs of work and up: 1.1–2.2×, down from as much as 8.6×.
  - Threads stay flat at the pool size.
- **Instruction-level cuts on the submit and complete path**, guided by callgrind on a Rust-only round trip (`crates/gusset/examples/rt_latency.rs`): 78.3M → 56.3M instructions for 20k calls, `Handle::submit` about 1,025 → 596 instructions per call. Id maps hash with a Fibonacci multiply instead of SipHash (ids are Rust-assigned, never attacker-chosen). Inputs up to 56 bytes travel inside the work unit instead of a heap `Vec`. `ensure_workers` is one atomic load per submit instead of a mutex, a scan and two allocations. `Call` no-op 10.6 → 7.7 µs, parallel 5.9 → 3.8 µs.
- **Small results ride in the completion record.** With the new `GUSSET_FLAG_INLINE_COMPLETION` (Go always sets it), a success of up to 48 bytes is written into the pipe with its ticket: `[ticket | 1<<63][len][bytes padded to 8]`, at most 64 bytes, one atomic write. The Go reader hands it to the waiter directly. That removes, per small call, the `gusset_take` and `gusset_buf_free` cgo calls, a registry buffer allocation, the results-map insert and remove, and the `cgoMu` read lock. Everything else (larger results, errors, panics, a C host that leaves the bit clear) is a bare ticket as before. If the pipe cannot grow to hold `pool_size` records, the handle falls back to tickets for every job instead of refusing the pool. Parallel `Call` 3.97 → 3.24 µs (−18%); serial unchanged within noise (it is bound by thread wake-up); 2 allocs/op.
- **The Go 1.26 floor is tested.** `go.mod` declares 1.26 but every CI lane ran 1.27, and on 1.26 the goroutine-leak gate failed outright (the profile is 1.27-only). A new `go-floor` lane vets and tests on 1.26. The gate now skips with its reason on toolchains without the profile, and fails as before wherever `GUSSET_REQUIRE_LEAK_PROFILE=1` is set, which the 1.27 unit lane and the Go tip soak now do.
- **The ticket-only fallback is tested.** The pipe-sizing decision is a function of its own, tested for records, the fallback to tickets, and refusal. A handle forced to ticket-only mode is also tested end to end: bare tickets even when inline records are requested.
- **Allocations are back to main's.** Boxing a `[]byte` through `submit(any)`, and a `*Buffer` escaping through a mutex on the `Submit` path, added one allocation to every `Call` and `Submit` on this branch. Both are gone: 2 allocs/op, as on main.
- **`NewBuffer` memory is zero-filled.** It returned stale process memory.
- **`WithBufferBudget` / `ErrBufferBudget`:** an opt-in per-handle cap on live `NewBuffer` bytes.
- **Gusset buffers come from `System` and are always counted by Gusset.** Accounting no longer depends on an inferred flag.
- **The allocator probe retries for the host** when the target compile fails (custom target specs, `-Zbuild-std`).
- **Opcode context values accept any integer kind.**
- **The BufferLarge benchmarks initialize their input.** Uninitialized input made them run random diagnostic modes.

### 2026-09-25 · Third audit: lock order, state machine, regressions

Each fix is backed by a test that fails against the code before it, unless marked otherwise.

- **A ticket waited on the wrong handle returned another caller's result.** Every handle numbered its tickets from 1. `Wait` on handle B with A's ticket found B's own ticket of that number, returned B's result with a nil error, and left B's real waiter with `ErrUnknownTicket`. Tickets now come from one process-wide counter, so a foreign ticket is `ErrUnknownTicket`.
- **`Close` could hang forever.** It waited for EOF on the completion pipe, and a surviving copy of the write end prevents EOF: a child forked outside Go's `ForkLock`, or a panic in `gusset_handle_close` before the descriptor was closed. Close now ends the reader with a read deadline once the workers are joined.
- **A completion whose write timed out could strand every permit.** It was retried only by a later submit or completion. When every permit was held by such a ticket, neither could happen. The worker now retries until the write lands or the handle closes. The completion fd is read and closed under the write lock, so a write never reaches a reused descriptor, and `close(2)` is never retried after EINTR.
- **A panic could be reported at the wrong location.** The panic hook records locations by thread, and `resume_unwind` skips the hook, so an earlier, already-handled panic's `file:line` was reported for the next failure. Stale entries are now cleared before every `catch_unwind`.
- **Errors now say what happened.**
  - A poisoned handle that has been closed reports "closed", not `ErrPoisoned`, and the poisoned error now has a message.
  - Work cancelled by `Shutdown` is `ErrShutdown`, not `context.Canceled`: the caller's context was live.
  - An expired drain budget is `ErrShutdownIncomplete`.
- **Runtime fixes without dedicated tests:**
  - `gusset_init` waits for a shutdown drain in progress.
  - `total_in_flight` no longer drops handles while holding the registry lock.
  - Finished workers are joined, not dropped.
  - Log lines dropped under contention are counted.
  - The signal-stack guard does not unmap a stack the kernel may still use.
  - The Go drain reader does not block during `Close`.
- **Tooling and tests:**
  - `gussetvet` checks the syntax tree. It catches every reference to `C.free`, `free()` in cgo preambles, `//export` callbacks, and exports without a Go wrapper.
  - A `RawBuffer` releases exactly the bytes it counted.
  - Timing-dependent tests wait on observable state, not fixed sleeps.
  - `TestStress_ConcurrentZeroCopyEgressAndCloseRace` counted a withdrawn view (`Bytes` is `nil` after `Close`) as corruption. That is the documented contract, and it failed about 1 run in 10 under `-race`.

### 2026-09-25 · Go SIMD evaluation

- **Go 1.27's `simd` experiment was evaluated against the Rust path.** It does not touch Gusset's runtime: the Go path has no numeric kernel, and `copy()` is already vectorized. The suite passes under `GOEXPERIMENT=simd`, and CI now checks that. `bench/simd_crossover_test.go` measures one kernel three ways and checks that all three agree. `docs/choosing.md` records where pure-Go SIMD beats crossing into Rust.
- **Cancellation checks inside the hot loop blocked vectorization.** The diagnostic and example engines' sum-of-squares kernels called `ctx.check()` on every 1024th iteration inside the loop. That kept the loop scalar at about 1.4 GB/s. Checking once per 4 KiB chunk and summing each chunk in `u32` reaches about 6 GB/s, and a 1 MiB Gusset call dropped from 839 µs to 262 µs in the measurement sandbox. `docs/adoption.md` now shows the chunked pattern.

### 2026-09-24 · Allocator API and interop audit

Rust 1.100 stabilizes `std::alloc::Allocator`. Every fix below ships with a
test that fails against the code before it.

- **Zero-copy output with the stable `Allocator` trait.** `BufferAlloc` is a 64-byte aligned allocator counted exactly once. An engine returns `Vec<u8, BufferAlloc>` as `JobOutput::Allocated`, and the worker adopts the allocation as the result buffer. `Counting<A>` is also an `Allocator`. A build probe enables it per compiler, and `rust-version` stays 1.97. Dependents read `DEP_GUSSET_ALLOCATOR_API` (`links = "gusset"`). `JobOutput` is `#[non_exhaustive]`, because its variants depend on the compiler.
- **A completion write to a closed pipe killed the process.** On a thread Go did not create, SIGPIPE from `write(2)` is re-raised by Go's handler with the default action, which exits the process with status 141. Workers now block SIGPIPE, the write fails with EPIPE, and the handle refuses new work. `rust_sigpipe` dies of signal 13 against the pre-fix code.
- **A completion whose write timed out was dropped.** Its Go waiter and its pool permit were stranded forever, and after `pool_size` of them every Submit blocked. Such tickets are now kept and retried ahead of the next completion and on every submit. The unit also stays in flight until its ticket is written, so `gusset_shutdown` cannot report a clean drain while a worker sits in the write backoff.
- **Shutdown could miss a concurrent submission.** A submit that passed the shutdown check before `begin_shutdown` could insert its cancel flag after `cancel_all` ran, and then eat the whole drain budget. The flag is now re-checked under the cancel-flag lock.
- **The signal stack is guard-paged and sized from the kernel.** It is at least 64 KiB, and more when `AT_MINSIGSTKSZ` needs it (arm64 SME, AVX-512 plus AMX). The docs now say what it buys: Go's handler can run on a Rust thread, but a stack overflow is still fatal.
- **Go callers no longer park behind `Close`.** `Bytes`, `Free`, `NewBuffer`, `Submit`, `Wait` and the cancel path used to block on `cgoMu` for Close's whole worker join. They now fail fast with "closed". A forgotten Buffer's GC cleanup also parked there and stalled every cleanup queued behind it.
- **Other fixes:**
  - A second concurrent `Close` returns the first one's error.
  - `WaitBuffer` falls back to Go memory on any re-wrap failure, not only on poison, so it never loses a finished result.
  - Submitting a 0-id Buffer reads its data and liveness together.
  - `Counting<BufferAlloc>` no longer counts twice.
  - `FfiStatus::free_msg` is `unsafe`.
  - Adoption refuses capacity above 1 GiB.
- **Contracts and CI:**
  - `tests/constants_match.rs` checks the status codes, flags, limits and take-owned bit across Rust, `gusset.h` and Go. The header now names the limits a C host must honour.
  - `exports_match` honours `CARGO_TARGET_DIR`.
  - CI tests the fallback path on the new compiler, and ASan runs the allocator suite.
  - The lint job's gofmt step passes again: `disruptive_hardening_test.go` had trailing blank lines.

### 2026-09-22 · Named field layout

- **Equal-width fields could trade places without failing ABI init (I6).** `gusset_abi_layout` reports size and alignment. Swapping `CallHeader.flags` with `reserved` (both `u32`), `span_id` with `timeout_ns` (both 8 bytes), or any two `AllocStats` fields leaves both numbers unchanged, so version 2's check accepted a header that reads those fields from the wrong slots. `gusset_abi_fields` reports the offset and size of every named field and writes no more entries than the caller's `cap`, so the table is not appended to the 36-byte `AbiLayout` a version-2 caller already allocates. Go compares that report with cgo `Offsetof`/`Sizeof`. A planted swap of `flags` and `reserved` panics in `init` with `ABI field CallHeader.flags drifted: Rust offset 32 size 4, cgo offset 36 size 4`. `TestPitfall_EqualWidthFieldSwapKeepsSizeAndBreaksNamedOffsets` walks every equal-width pair and shows the offset multiset is unchanged, which is why a size check or a sorted-offset check cannot see the swap.

### 2026-09-22 · Caller-held buffers

- **An engine could return a buffer Go still holds (R16).** The alias check refused the unit's own input, id 0, a second claim, and a buffer another unit was reading. A buffer published through `gusset_buf_alloc` (`NewBuffer`) and then returned as `JobOutput::Buffer` was accepted. The waiter frees an output when it is done, which releases that memory under the caller's view. Published allocations are now marked `caller_held`, and `claim_output` refuses them. Engine-private `buf_alloc` may still be returned once. `engine_returning_a_caller_held_buffer_is_refused` fails against the pre-fix code (published buffer 1 was accepted as an output).
- **`Bytes` raced `Free` on the slice header.** `freed` is atomic, so the flag was synchronized and `b.data` was not: `-race` reported `Free` writing the header while `Bytes` read it. Both now take `Buffer.mu`, and `Bytes` holds `cgoMu` across that read so it cannot hand out a view `Close` is already freeing. `TestPitfall_BytesAndFreeDoNotRace` fails against the pre-fix code with a data race at `buffer.go`.
- **Engine dispatch held the registry read lock across execution.** `default_dispatch` called `engine(ctx, input)` while holding `ENGINE_REGISTRY.read()` or `GLOBAL_ENGINE.read()`. A concurrent call to `register_engine` or `clear_engine_handlers` took a write lock and queued behind a slow engine, starving all subsequent worker dispatches on POSIX `RwLock`. `EngineFn` is now `Arc<dyn ...>`, and `default_dispatch` clones the handler and releases the lock before invocation.

### 2026-09-22 · Containment audit

Every fix below ships with a test that fails against the pre-fix code, run from a
separate worktree of `7a56c3c` with only the new diagnostic mode added.

- **A panic payload whose destructor panicked killed the worker (I2, I4).** The firewall caught the engine's panic, then dropped the payload *after* `catch_unwind` returned. `panic_any(T)` where `T::drop` panics unwound the worker thread itself: the ticket was never stored or written, its cancel flag stayed registered, and a Go caller with no deadline parked forever holding a pool permit. Payloads are now disposed of under their own `catch_unwind` (a re-thrown payload is leaked, not recursed into), the whole per-unit path runs under a second firewall so any fault still produces a completion, and a dying worker's `join` payload is contained in `close`. Diagnostic mode 15 throws such a payload. `panicking_payload_destructor_does_not_kill_the_worker` and `TestPanicZoo_PanickingPayloadDestructor` fail against the pre-fix code (the Go call returned `context deadline exceeded` after its full 3 s).
- **A sibling's panic erased a finished result, but only a small one (I2).** `gusset_take` allocated the egress buffer for a result of 4 KiB or less through the public, poison-checked `buf_alloc`, so a job that finished after another job poisoned the handle lost its result and surfaced `FFI_ERR "handle is poisoned"` — neither the data nor `ErrPoisoned`. Larger results were already promoted on the worker and came back intact. Take now uses the internal egress copy; poison still refuses every new submission and `NewBuffer`. `TestPitfall_InFlightResultSurvivesSiblingPanic/small` fails against the pre-fix code.
- **`Close` waited for child processes.** The completion pipe's write end was duplicated with `syscall.Dup`, which does not set close-on-exec, so every subprocess started while a handle was open inherited it. Rust closing its copy then left the pipe with a writer, `drainPipe` never saw EOF, and `Close` blocked until the child exited. The dup now sets close-on-exec under `syscall.ForkLock`. `TestPitfall_CloseDoesNotWaitForChildProcesses` fails against the pre-fix code.
- **Two results could own one buffer (R4/R16).** The alias check refused only the unit's own input and id 0. An engine returning a captured, pre-allocated id — the opcode example's pattern — handed the same buffer to every call; the drain loop takes completions back to back, so both results resolved one pointer and the second was read after the first waiter freed it. Returning another in-flight unit's input was likewise accepted. A buffer may now become an output once per lifetime and never while another unit holds it as input, checked under the registry lock. `one_buffer_is_returned_as_an_output_at_most_once` and `a_buffer_in_use_as_another_units_input_is_refused_as_an_output` fail against the pre-fix code.
- **An overwritten error status leaked its message.** `gusset_submit` and `gusset_buf_alloc`, on discovering poison after the guarded call failed, wrote `FFI_POISONED` over the status `ffi_guard` had filled without freeing its boxed message. `FfiStatus::overwrite` frees first. The poison flip that reaches this branch is a race, so `overwriting_an_error_status_releases_its_message` pins the replacement under the counting allocator, with a control arm showing the old pattern's leak is measurable.
- **Containment stress test.** `TestStress_ContainmentUnderLoad` runs deadline-free callers against every failure above at once — destructor bombs direct and delayed, delayed panics beside slow successes, large zero-copy round trips, mid-flight `Close`, and continuous fork/exec — under a watchdog, and checks typed errors, byte-exact results, and that goroutines and Rust live bytes return to baseline. Against the pre-fix code it hangs until the watchdog fires. `GUSSET_STRESS_ROUNDS` scales it.

### 2026-09-21 · Boundary hardening

- **Allocator live bytes wrapped near the top of `usize`.** `fetch_add` plus `wrapping_add` turned a saturated counter into a small total, so `AdviseMemoryLimit` would raise the Go heap limit while Rust was still holding the memory. The counter now saturates. `accounting_is_balanced_saturating_and_peak_monotonic` fails against the pre-fix code (it observed live bytes `55` after adding 64 to `usize::MAX - 8`).
- **Ticket and buffer ids advanced through the 63-bit ceiling.** A refused `buf_alloc` still incremented the counter, and a ticket of `1<<63` was accepted. After wrap, the next id collides with a live buffer or an in-flight waiter. `reserve_id` refuses without advancing. `buffer_ids_stop_at_the_ceiling_instead_of_wrapping` and `ticket_ids_stop_at_the_ceiling_instead_of_wrapping` fail against the pre-fix code.
- **A full worker queue blocked inside `send`.** Holding the sender lock across that block stops `close` from disconnecting workers; cloning the sender and sending after the lock drops keeps the channel open across `join`. `try_send` refuses while the lock is held and does not leak the cancel flag. `submit_on_a_full_queue_fails_without_blocking_or_leaking` fails against the pre-fix code (the extra submit blocked, then returned ticket 4).
- **Completion-write backoff held every worker behind one lock.** The pipe lock now covers a single syscall attempt; the sleep runs outside it.
- **A context opcode wider than `uint32` collapsed to the diagnostic engine.** `uint(1<<32)` truncated to 0, and a payload whose first byte is 1 then panicked the handle. The value is refused. `TestHardening_OversizedOpcodeDoesNotCollapseToDiagnostic` fails against the pre-fix code.
- **Exported ABI functions lacked catch_unwind guards (R2).** `gusset_init`, `gusset_shutdown`, `gusset_drain_logs`, `gusset_alloc_stats`, `gusset_status_free`, and `gusset_abi_layout` were not protected by `catch_unwind`, risking process abort if a panic occurred across `extern "C"`. All exported entry points are now wrapped with `catch_unwind(AssertUnwindSafe(...))` and return safe zero/default values on unwind.
- **Lock inversion between `drainPipe` and `Handle.close`.** `drainPipe` called `bufFree` while holding `s.mu.Lock()`, which interacted with `close()` also holding `s.mu.Lock()` while calling `bufFree`. Buffer cleanup is now decoupled: take buffer IDs are collected under `s.mu.Lock()` and freed after releasing the mutex.
- **`CallBuffer` missed `KeepAlive` on input buffer.** If GC ran during `CallBuffer`, `runtime.AddCleanup` could trigger on the Go `*Buffer` wrapper while the FFI call was in flight. Wrapped with `defer runtime.KeepAlive(in)`.
- **Disallowed methods lint bypassed by fully-qualified paths (R3).** Added `core::option::Option` and `core::result::Result` entries to `clippy.toml` alongside `std::*` to ensure no `unwrap`/`expect` can bypass the lint.
- **Test profile lacked overflow checks.** Explicitly added `[profile.test] overflow-checks = true` to workspace `Cargo.toml` so test builds enforce the same arithmetic safety invariants as dev builds.
- **Adversarial disruption & chaos test suite.** Added `tests/pitfalls/disruptive_hardening_test.go` covering cross-handle buffer theft, concurrent poison churn assault, raw buffer boundary violations, and non-cooperative engine worker abandonment under deadline expiration.

### 2026-09-21 · Go tip bootstrap

- **The weekly tip job could not build Go (run 35594759554).** `Set up Go Tip` cloned tip and ran `./make.bash` with `GOROOT_BOOTSTRAP` unset, so the build used the runner image's Go: `Building Go cmd/dist using /opt/hostedtoolcache/go/1.24.13/x64. (go1.24.13 linux/amd64)` then `found packages main (build.go) and building_Go_requires_Go_1_26_0_or_later (notgo126.go)`. Go tip (1.28) requires a bootstrap >= Go 1.26.0 (`src/make.bash` `bootgo=1.26.0`, `src/cmd/dist/notgo126.go` `//go:build !go1.26`). The job now installs the matrix pin (`1.27.x`) and passes that tree as `GOROOT_BOOTSTRAP`. The suite's `go` floor stays 1.26; this is the compiler that builds tip, not the compiler Gusset ships against. `TestTipWorkflowBootstrapsFromGo1_26OrNewer` fails against the pre-fix workflow.

### 2026-09-20 · Audit and hardening

Findings from a full audit of the v0.0.1 tree, validated by adopting Gusset in a
second codebase (DevCouncil's `go_orchestrator` driving its `dc-glob` crate). Every
fix below ships with a test that fails against the pre-fix code.

### Security and correctness (protecting I2, I3, I4, I6)

- **A caller's deadline did not bound the caller (I3).** `waitInternal`'s `ctx.Done` branch issued a best-effort `gusset_cancel` and then did an unconditional `<-ticketCh`. Cancellation is cooperative, so against an engine that never calls `JobContext::check` that receive parked until the job finished on its own: measured, an **80 ms deadline on a 1.5 s job returned after 1.503 s**. Every real adopter engine is non-cooperative — a tokenizer, a regex scan, a proof verifier and a SIMD transform all run tight loops that never ask whether they should stop — so the context deadline was decorative for exactly the workloads Gusset exists to host. The waiter now detaches: it returns `ctx.Err()` at its deadline and `drainPipe` disposes of the result when it lands. The permit deliberately stays behind, because the worker is still executing and handing it back would let a further submission run alongside — in-flight work above the pool size, which is the OS-thread bound I4 sells. `TestDeadline_NonCooperativeEngineStillReleasesTheCaller`, `TestDeadline_ExplicitCancelReleasesTheCallerFromANonCooperativeEngine` and `TestDeadline_AbandonedJobKeepsHoldingItsPoolPermit` all fail against the pre-fix code.
  - The existing deadline coverage could not have caught this: `TestPitfall_DeadlineEnforcement` and `TestAdversarial_DeadlineStormDoesNotLeakPermits` both drive diagnostic **mode 5**, which checks between every iteration. A cooperative engine cancels itself, Rust posts a `Cancelled` completion, and *that* is what wakes the Go caller — so those tests pass identically against a Gusset that ignores the deadline entirely. Diagnostic **mode 9** is mode 5 with the `ctx.check()` removed and nothing else changed, which isolates the single variable.
- **`gusset_shutdown` was unreachable from Go.** Rust implements a budgeted drain, `tests/rust_shutdown.rs` covers it, `internal/ffi.Shutdown` binds it, the README advertises it — and no exported Go function called it. The only shutdown a Go adopter had was `Handle.Close`, which joins its worker threads and so takes however long the engine still needs: measured at 653 ms for a 700 ms non-cooperative job. Now exported as `gusset.Shutdown(drain time.Duration)`, with the budget clamped rather than wrapped through `uint32` milliseconds. `tests/shutdown` is its own binary because the refusal latch is process-global and one-way.
- **`Call` could not carry a `*Buffer`.** `Submit` accepts `[]byte` or `*Buffer`; `Call` took `[]byte` only, and a `[]byte` over 4 KiB is refused — so the payload sizes zero-copy exists for were exactly the ones the one-line API rejected. Every adopter on the documented zero-copy path hand-rolled `Submit` plus `WaitBuffer`, including the `runtime.KeepAlive` that stops the input `Buffer`'s `AddCleanup` backstop from freeing Rust memory while Rust is still resolving its id. Added `(*Handle).CallBuffer(ctx, *Buffer) (*Buffer, error)` — typed rather than widening `Call` to `any`.
- **An engine could free the caller's input buffer (R4/R16).** An engine returning `JobOutput::Buffer(id)` was trusted with any id, including its own *input* id — the obvious way to write an echo. Go frees a take buffer once it has copied the result out, so that id being the input meant the caller's live `*Buffer` was released underneath it and the slice `Bytes()` had already handed out pointed at freed pages. `large_ok_result_is_promoted_off_the_cgo_thread` asserted that Gusset's *own* echo does not alias, so the hazard was understood — but nothing refused it, and the assertion covered one engine rather than the contract. The worker now refuses an output id equal to the input id, and an id of 0 (ids start at 1, so a zero is a default rather than an allocation). `crates/gusset-example/tests/alias.rs` fails against the pre-fix code.
- **A retired mechanism was still being read.** `callResult.buffer` was assigned at HEAD; the `takeIDs` rewrite replaced it and left the field plus every read site behind, so `discardResult`, `wait` and `waitBuffer` each carried a branch on a field that was by then always nil — including the comment explaining the `GOGC=1` race that branch used to guard, now guarding nothing. Field and dead branches removed; the race is still guarded, by the live `takeID` path that does the same `cgoMu`-held copy. `drainPipe`'s two duplicated delivery blocks were unified into one `deliver`.
- **`ErrTicketBusy` released the owner's semaphore permit (I4).** `waitInternal` deferred `releaseSem` on every return, so a second `Wait` on a live ticket consumed the slot while the job was still running and a third `Submit` could enter the pool. The permit now stays with the owner; `TestPitfall_DoubleWaitDoesNotReleaseSemaphore` fails against the pre-fix code.
- **A failed `submit` leaked `in_flight`.** The cancel flag was inserted before the sender was looked up. If the sender was already gone, shutdown waited on a flag no worker would ever clear. The sender is locked first; `submit_does_not_leave_a_cancel_flag_when_the_sender_is_gone` fails against the pre-fix code.
- **`WaitBuffer` copied `JobResult::Ok` onto the Go heap.** `drainPipe` copied take()'s Rust buffer into a Go slice and freed it, then `WaitBuffer` allocated a second buffer and copied again (~66 KiB/op for a 64 KiB result). Results larger than 4 KiB now keep the take buffer until a waiter consumes it: `WaitBuffer` wraps with no Go copy, `Wait` copies out and frees. Wrapping in `drainPipe` itself taxed `Wait` with a `*Buffer` it immediately destroyed (6 allocs/op vs 3). `TestPitfall_WaitBufferDoesNotCopyTakeAllocation` and `TestPitfall_WaitOfLargeTakeBufferDoesNotPayWrapperAllocs` fail against the pre-fix code.
- **FFI cancel did not satisfy `errors.Is(..., context.DeadlineExceeded)`.** A worker that finished with `cancelled: DeadlineExceeded` before Go's `ctx.Done` won the select returned `*Error` only; `GOGC=1` `TestStress_AdversarialContextCancelStorm` treated that as unexpected. `Error.Is` now maps the two Rust cancel reasons; `TestPitfall_FFICancelMapsToContextErrors` fails against the pre-fix code.
- **`Wait` copied a `take()` view while `Close` freed it.** Large results stay in Rust memory until a waiter copies them. Without `cgoMu`, `HandleClose` raced the copy and `GOGC=1` `TestStress_ConcurrentCallAndCloseRace` saw a torn first cache line. The copy now holds `cgoMu.RLock` so close waits; a closed handle returns an error instead of a partial slice.
- **The panic hook ate Rust test failures.** `install_panic_hook` replaced libtest's hook and recorded the location without printing, so `cargo test` reported FAIL with no message. It now chains the previous hook after recording.
- **A nil `context.Context` panicked the caller.** `Call`/`Submit`/`Wait` selected on `ctx.Done()` and aborted the goroutine. They now return `gusset: nil context`; `TestPitfall_NilContextIsRejected` fails against the pre-fix code.
- **`WithPoolSize` truncated through `uint32` (I4).** `WithPoolSize(1<<40)` became pool size 0 (a 0-capacity semaphore that hung every `Call`); `WithPoolSize(1<<32+4)` silently became 4 workers. Values outside `1..=1024` are refused before conversion; `TestPitfall_PoolSizeOverflowIsRefusedNotTruncated` fails against the pre-fix code.
- **R16 was documented, not enforced.** `Handle::submit` memcpy'd every `[]byte` on the cgo thread, including a 1 MiB payload. Inline input above 4 KiB is now refused on both sides; larger payloads use `NewBuffer`. `TestPitfall_InlineSliceOver4KiBIsRefused` fails against the pre-fix code.
- **`NewBuffer` ignored the poison latch (R10).** After a caught panic, allocations still entered Rust. They now return `ErrPoisoned`; `TestPitfall_NewBufferRefusesPoisonedHandle` fails against the pre-fix code.
- **`WaitBuffer` of a large take result raced `Close` (R16).** `Wait` already copied take views under `cgoMu`. `WaitBuffer` wrapped the same view after `waitInternal` returned, so `HandleClose` could free the pages between the view being published and `newBufferFromRaw`. The existing close-race used 3-byte payloads, which `drainPipe` copies onto the Go heap — it never took the take-buffer path. Wrap now holds `cgoMu.RLock`; a closed handle returns an error rather than a `*Buffer` over freed memory. `TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose` fails against the pre-fix code.
- **Large `JobResult::Ok` was memcpy'd on the cgo thread.** `gusset_take` allocated and copied every `Ok` payload while an M was pinned. Workers now promote results above 4 KiB to `JobResult::Buffer` before the ticket is written, so take is a pointer return (R16 egress). `large_ok_result_is_promoted_off_the_cgo_thread` fails against the pre-fix code.
- **`NewBuffer` / `buf_alloc` had no size ceiling.** A caller-controlled length went straight into `posix_memalign`. 1 GiB is now refused, not clamped, matching `WithPoolSize`; `TestPitfall_BufferSizeIsBounded` and `allocate_refuses_above_maximum_without_touching_the_allocator` fail against the pre-fix code.
- **`timeout_ns = u64::MAX` meant "run forever".** `Instant::checked_add` returns `None` when the duration cannot be represented, and that `None` was treated as "no deadline". Overflow now expires immediately (`submit_instant`). `nonzero_timeout_always_installs_a_deadline` fails against the pre-fix code.
- **`Error.Is` prefix-matched cancel reasons.** A message that merely contained `DeadlineExceeded` satisfied `errors.Is(..., context.DeadlineExceeded)`. Matching is now exact (`cancelled: DeadlineExceeded` / `cancelled: Explicit`).
- **`ensure_workers` could spawn onto a closing handle.** The closed check ran before the workers lock, and `close` released that lock before `join`. The check is now under the lock, and `close` holds it across join.
- **Untrusted input could select a Rust panic.** The built-in diagnostic engine
  chooses behaviour from `input[0]` — bytes 1, 2 and 3 are panics — and ran by
  default whenever no adopter engine was registered. Since no C export registers
  one, that was every process before its Rust engine had installed itself. A
  submission with no registered engine is now refused; the diagnostic engine is
  reachable only through `gusset.WithDiagnosticEngine()`, and a registered engine
  always outranks it.
- **ABI version 2.** `AllocStats` crossed the boundary unverified: version 1
  checked three `#[repr(C)]` types while `gusset_alloc_stats` wrote a fourth
  straight into Go memory. `AbiLayout` now covers four types (28 → 36 bytes), and
  Go additionally cross-checks Rust's reported layout against what cgo compiled,
  because `internal/ffi/gusset.h` is hand-maintained rather than generated.
- **`Wait` ignored its context.** A ticket that was never submitted, or a second
  concurrent `Wait` on one ticket, parked the caller forever. Now `ErrUnknownTicket`
  and `ErrTicketBusy`.
- **Completion-pipe descriptor could be closed twice.** `Handle` took the write fd
  at construction, so an `open` that failed after the `Arc` existed closed a
  descriptor the caller still owned. Ownership now transfers only on success.
- **`gusset_shutdown` was a stub** that returned `FFI_OK` without draining,
  cancelling or refusing anything. It now latches a shutdown flag, cancels every job
  on every live handle, waits up to `drain_ms`, and returns `FFI_ERR` with the
  residual count when cooperative work outlives the budget.
- **Unbounded resources bounded:** pool size capped at 1024 and refused rather than
  clamped; the completion-write EAGAIN path backs off and gives up instead of
  spinning a core forever; the panic-location map is capped at 256 entries; the log
  ring evicts oldest lines instead of clearing itself, and truncates a line larger
  than the whole budget (it previously stored 500,001 bytes in a 64 KiB ring).
- **Fail-silent locks removed.** Poisoned mutexes no longer skip the guarded work:
  a dropped result hung the Go caller, a skipped `sender.take()` hung `close` in
  `join`, and a lost cancel flag disabled cancellation without telling anyone.
- **Allocator double counting fixed.** `RawBuffer` counted its allocations by hand
  *and* through `Counting` when an adopter installed it, reporting every buffer at
  twice its size to `AdviseMemoryLimit`.
- Unknown `CallHeader.flags` bits are rejected; a failed `sigaltstack` install is
  logged rather than silently degrading I5; trace context moved off the bare
  `"spanContext"` string key to `gusset.SpanContextKey`.

### Gates that were not gating

- **The lint job could never pass.** Its forbidden-pattern audit grepped the whole
  tree and matched the rules as written in `AGENTS.md`, `docs/PLAN.md` and the
  workflow file itself. Now scoped to tracked `*.go`/`*.rs`, verified to pass clean
  and to still detect a planted match.
- **Clippy skipped all test code** (no `--all-targets`), so R3's disallowed-method
  rules never saw it — and `tests/exports_match.rs` was violating them.
- **The export check never inspected the shipped archive.** It preferred
  `target/debug` while cgo links `target/release`, and it ignored `nm`'s stderr —
  with `lto = "fat"` the release archive is LLVM bitcode that a system `nm` cannot
  read, so it printed an error, exited 0, and produced an empty symbol list
  indistinguishable from a clean one. It now uses the toolchain's `llvm-nm`, checks
  every archive present, and fails when a reader cannot parse one.
- **Miri ran zero tests.** The library had no unit tests at all; `header` and
  `ffi::alloc` now have six, and the job fails if the count is zero.
- **The ASan job only built.** It now runs the Rust suite under the sanitizer.
- Added the `cgocheck2` job (R6 and R16 named it as their enforcer; it did not
  exist) and a `memlimit` job. `AGENTS.md`'s testing table now carries a status
  column.
- **`make bench-scaling` did not exist.** `tools/benchplot` requires
  `scaling-rawcgo-*.txt` and `scaling-gusset-*.txt` and names that target in
  three separate error messages telling the reader to run it. There was no such
  rule in the `Makefile`, so the committed chart data had no documented way to be
  regenerated, and the one-transport-per-process isolation its validity depends
  on was enforced by nothing. Added, mirroring `bench-crossover`'s isolation.
- **`tools/benchplot` was never run by anything.** It ships a `-check` staleness
  flag, and no target and no CI job invoked either the tool or the flag — every
  reference to it in the tree was a comment. `docs/img/*.svg` were likewise
  referenced by no document, so three generated charts sat in the repository
  with nothing regenerating them and nothing checking them. `make docs` now runs
  it and `make docs-check` runs it with `-check`, alongside `benchdoc`; `ci-local`
  depends on `docs-check`, so the charts are now gated the way the numbers are.
  `README.md` embeds all three.
- **The committed `crossover.svg` plotted data its own file no longer
  supported.** It was drawn from transport samples taken before the per-arm
  process isolation landed — samples in which serial Gusset degraded 94 µs →
  340 µs → 591 µs → 663 µs down its last four repetitions as the process
  accumulated Ms it never destroys. A generated artifact nothing regenerates and
  nothing checks is a claim, not a measurement.

### Every remaining MISSING gate, implemented

The status column introduced above listed eight gates that did not exist. All of
them now do. Where the table's original wording named a tool that would have meant
a new dependency, the implementation differs and the table says why.

- **`musl` (R8's enforcer).** Runs the suite in `golang:1.27-alpine`, where the
  128 KiB default thread stack is actually in play. R8 also gained a gate that works
  everywhere: `pool::sys::current_thread_stack_size` reads the size back off the
  running worker, so `worker_stack_is_explicitly_sized_not_inherited` fails anywhere
  the explicit sizing is dropped. A deep-recursion probe cannot do that — on glibc
  and darwin it passes whether or not Gusset set the size, because the platform
  default already covers it. Workers also log a shortfall instead of degrading
  quietly, as they already did for `sigaltstack`.
- **`cross`.** `zig`/QEMU dropped. Gusset is unix-only by construction — POSIX pipe,
  `sigaltstack`, pthread stack accounting — so `lib.rs` now fails a non-unix build
  with a named `compile_error!` instead of a `libc` symbol cascade, `make cross`
  compile-checks the supported set (including musl), and CI asserts the Windows
  build *refuses* with that error. `docs/platforms.md` records what a port would
  have to add.
- **`bench` (R15).** `tools/benchdoc` regenerates the README table from benchstat;
  `make docs-check` fails when it drifts. The old `make docs` printed "Documentation
  up to date" and regenerated nothing, while the README's figures disagreed with
  benchstat over the very file they cited (parallel read 5.07 µs against a 5.237 µs
  median) and rested on five samples where benchstat needs six before it will quote
  a confidence interval. Numbers are now generated from a ten-sample run and carry
  their intervals.
- **`fuzz`.** Three Go native fuzz targets over the FFI boundary — no dependency,
  where `cargo fuzz` would have meant `libfuzzer-sys` and a nightly-only build.
  4.56M executions on the refusal path, 178K against the deliberately-panicking
  diagnostic engine, 45s on the buffer lifecycle: no crashers. Diagnostic mode 6's
  recursion depth is now capped, since it came from the payload and `[6, 0xFF ×4]`
  would recurse 4.29 billion frames and overflow even an 8 MiB stack.
- **Header diff.** `tests/header_match.rs` compares the hand-written `gusset.h`
  against the Rust exports *by type*, not just by name — no cbindgen dependency.
  Verified against three planted mutations: a widened parameter, a dropped `const`,
  and a removed parameter.
- **R4's cross-free.** `TestR4_CrossFreeIsDetectedUnderASan` cross-frees a Gusset
  buffer and then lets the owner release it. ASan accepts the libc free — a
  64-byte-aligned block comes from `posix_memalign`, and freeing that with `free` is
  legal C — and reports the owner's release as `attempting double-free`. R4 also
  gained an always-on static gate: `gussetvet` now rejects `C.free` outside the one
  package allowed to hold the deliberate violation.
- **The `soak` goroutine-leak assertion.** Uses Go 1.27's real `goroutineleak`
  profile rather than counting goroutines, and fails if the profile is unavailable
  instead of skipping.
- **The `memlimit` 200 MiB / 1% assertion.** Measured drift across a 200 MiB Rust
  allocation is 0.0000%.
- **The `cgocheck2` retained-pointer test.** Stores a Go pointer into C memory in a
  subprocess and asserts it is caught *with* the experiment and allowed *without*
  it, so the job cannot go green while the experiment is off. The obvious probe —
  passing a Go pointer to an unpinned Go pointer — is caught by baseline cgo
  checking in every build and would have proved nothing.
- **`tip`'s issue-on-failure.** Now exists, and reuses an open `toolchain` issue
  rather than filing one per week.

### Found by the new gates

- **A use-after-free of Rust memory in the close-race stress test.** It called
  `buf.Bytes()` and indexed the result while a concurrent `Close` was in flight.
  Before `Bytes()` was hardened to return nil after close, it got a live slice onto
  memory Rust had already released and wrote three bytes into it — invisible to the
  Go race detector, because the memory is not Go's. The nil return turned that
  silent corruption into a panic, which is how it was found.
- **R5's enforcer could be evaded by a name prefix.** `gussetvet` matched each
  required directive with `strings.Contains` over the whole cgo preamble, so
  `#cgo noescape gusset_cancel_all` satisfied the check for `gusset_cancel`.
  Deleting *both* directives for `gusset_cancel` left it reporting "R5 checks
  passed" — verified, then fixed by matching whole directive lines. That is a live
  pair in `exports.txt`, and R5 is the rule that stops cgo treating Go pointers as
  escaping into C, so losing it on one export is silent.
- **`staticcheck ./...` exited 1 on every commit**, so the `lint` job could never
  pass — the same shape as the forbidden-pattern audit fixed earlier in this
  release, found the same way: by running the gate instead of reading it.
  `tools/gussetvet` used `go/parser.ParseDir`, deprecated since Go 1.25 (SA1019).
  Replaced with a per-file `parser.ParseFile` walk, which is what the check
  actually needs — it reads cgo preambles and never asks which package a file
  belongs to — rather than adding `golang.org/x/tools/go/packages`. It now also
  fails if it parsed zero files, instead of reporting success having inspected
  nothing.
- **A checklist item claimed the header was generated by `build.rs` and diffed in
  CI, and had been ticked.** `build.rs` only enforces `panic = "unwind"` and has
  never run cbindgen. A `cbindgen.toml` sat in the repository root for a tool
  nothing invoked, which is what made the claim look plausible; it was deleted.

### Adoption (validated against DevCouncil)

- **Gusset could not be linked by any external Go module.** `#cgo LDFLAGS` resolve
  `target/release` inside a source checkout; `.gitignore` keeps it out of the module
  zip and the module cache is read-only. Adopters now build with
  `-tags gusset_pkgconfig` against the installed `gusset.pc`, or pass
  `CGO_LDFLAGS=-L…`. Both verified against a module-cache layout.
- `docs/adoption.md` documents the full recipe, including the two requirements
  nothing stated before: an umbrella crate must set `[lib] name = "gusset"` so its
  output is `libgusset.a`, and the adopter must export and call its own engine
  registration entry point.
- `lto` is no longer prescribed. No invariant depends on it, a real adopter had
  measured `thin` as the better trade, and `fat` is what made the archive opaque to
  `nm`.
- `crates/gusset-example` is now actually exercised. `init_example_engine` had zero
  callers anywhere in the repository, and the Go test named `TestPhase2_CpuBoundEngine`
  drove Gusset's built-in dispatcher — which carried its own near-copy of the
  example's opcode 10 — so the Phase 2 "second-app validation" had never run the
  adopter path it claimed to validate.

### Tests

- New: `tests/rust_runtime.rs` (7), `tests/rust_shutdown.rs` (1),
  `crates/gusset-example/tests/adopter.rs` (1), lib unit tests now 14,
  `tests/pitfalls/hardening_test.go` (5), `tests/pitfalls/adversarial_test.go` (13).
  Rust workspace tests: 27.
- Green under `-race` and `GOGC=1 -count=3` this pass, including the 8 KiB
  `WaitBuffer`/`Close` race. `GOEXPERIMENT=cgocheck2` and `GOMEMLIMIT=256MiB`
  were recorded in the prior audit on this tree, not re-run here. Rust workspace
  is 27 tests. A 3-sample bench smoke on darwin/arm64 kept the committed alloc
  counts (noop 2–3 allocs/op, large `Wait` 65696 B/op, `WaitBuffer` 256 B/op).
- New for the deadline-detach work: `tests/pitfalls/deadline_detach_test.go` (8),
  `tests/pitfalls/detach_stress_test.go` (3), `tests/shutdown/` (2, own binary).
  Diagnostic modes 9 (non-cooperative sleep, a delay prefix in front of any other
  mode) and 11 (fixed-cost CPU work) exist to make those assertions reachable at
  all: an instant result is collected by its caller before it can be abandoned,
  so the abandoned large-result and abandoned-panic paths had no way to be
  entered. Both are capped like mode 6's recursion depth, since the count is
  input-derived and the fuzz targets reach them.
- The three regression guards most at risk from the detach were mutation-tested
  rather than assumed. Removing `deliver`'s permit return fails
  `TestDeadline_AbandonedJobKeepsHoldingItsPoolPermit`; removing the poison latch
  in `deliver` failed *nothing*, which is how that latch was found to be a
  fourth redundant copy — Rust's worker sets `Handle.poisoned` inside its
  `catch_unwind` and is the authority — and removed.

### Benchmarks

- **`make bench-crossover`**: raw blocking cgo versus Gusset on byte-identical
  work (`rs_spin` and diagnostic mode 11 run the same loop), so the difference is
  transport alone. Every other benchmark here measures a noop, which is where any
  coordination layer looks worst and which is not why anyone embeds Rust in Go.
  On darwin/arm64 (Apple M5 Pro, n=10, medians over the committed raw data):
  Gusset's fixed overhead is **13.0 µs** serial and **4.1 µs** parallel. Serially
  it costs **+16 %** at 74 µs of work and **+1.0 %** at 711 µs; under full
  parallelism it costs **+4.9 %** at 711 µs.
  - An earlier revision of this line claimed Gusset was **24 % faster** than
    blocking cgo at ~1 ms under parallelism. It does not reproduce, and it was
    never true: it came from a recording whose blocking-cgo arm was measured on a
    busier machine than its Gusset arm — the two arms' own calibration of the
    identical loop disagreed by 20 % (823 µs against 684 µs), and the arm that
    wraps the call came out 2.5x faster than the call. Gusset does not beat the
    cgo call it wraps; the case for it is the thread bound and the firewall, and
    a coordination cost that falls to about 1 % once a call does real work.
    `checkTransportOrdering` now refuses to chart that shape.
- **Thread pressure** is the measurement the design rests on, and it needs
  512 concurrent callers — `RunParallel` uses exactly `GOMAXPROCS` goroutines
  against Ms the runtime already has, so it reports `Δthreads = 0` for blocking
  cgo and proves nothing. It also needs one transport per process at `-count=1`:
  Go never destroys an M, so sharing a process makes the second measurement
  inherit the first's threads. Isolated, at 512 callers × 7 ms of work:
  blocking cgo peaked at **185–298 OS threads**, Gusset at **20**, at
  indistinguishable throughput.
  - The memory cost of those threads was first written up as "~1.5–2.4 GiB of
    reserved address space against ~160 MiB", which was **derived rather than
    measured** — thread count multiplied by an assumed 8 MiB stack.
    `BenchmarkThreadScaling` now samples RSS and VSZ directly. Four recordings
    of the same 32 → 2048 sweep put blocking cgo's VSZ delta at **2.3, 14.4,
    19.8 and 21.0 GiB**, which read as an unmeasurable quantity until the
    recordings were certified idle and exclusive: the three that disagreed were
    taken while other benchmark processes shared the machine. On the certified
    recording the two arms **independently agree on the per-thread division** —
    blocking cgo grew 21.0 GiB over 335 extra threads, Gusset 578 MiB over 9,
    both ~64 MiB per thread. Two arms landing on the same figure is what makes
    it a measurement rather than a number.
  - That figure is **not 8 MiB**, so "threads × the platform's default stack" is
    the wrong estimate on darwin/arm64 — it predicts about an eighth of what is
    actually reserved. `docs/choosing.md` now generates the per-thread division
    from the committed data instead of asserting the arithmetic.
  - The disputed part is **resident** memory, and that measures stably: over the
    same sweep it grows by roughly **28 MiB** for blocking cgo and **13 MiB**
    for Gusset, a gap of **10–15 MiB** across recordings. Address space is not RAM.
    The thread bound is worth having for scheduler pressure and the
    10,000-thread ceiling; it is not worth having for the memory, and every
    informal retelling of the gigabyte figure dropped the word "virtual" and so
    implied otherwise.
  - Recorded here because it took three tries to get right: the first correction
    claimed the derived estimate was two orders of magnitude too high, which was
    itself an artifact of a contended run, and the second claimed the direct
    measurement confirmed the estimate, which was one clean run landing near the
    physically expected value by luck. A quantity that varies tenfold between
    honest recordings does not confirm or refute anything.
- **An unattributable thread count is now withheld rather than printed.**
  `BenchmarkThreadScaling` reported only an absolute `peakThreads`, and Go never
  destroys an M, so a second transport in the same process inherited every thread
  the first created. Run in one process the two transports reported an identical
  figure at every concurrency level — 232, 232, 232, 232, 232, 232, 308, 308 —
  and a chart drawn from that shows two flat, equal lines, which reads as "cgo is
  fine" from data that measured nothing. `reportPeakThreads` now publishes
  `peakDthreads` always and the absolute figure only for the first transport to
  sample threads in that process, logging the reason when it withholds. A missing
  metric fails `benchplot` loudly; a wrong one becomes a chart.
- **`benchplot` refuses to draw a contaminated run.** The transport arms run as
  separate processes, which is what makes the thread numbers honest and also what
  lets an unrelated load spike land on one arm and not the other. That failure is
  invisible in the raw file: every sample in the affected arm is uniformly wrong,
  so the spread stays tight and the median looks authoritative — a spread check
  cannot catch it. `checkTransportOrdering` compares the arms against an ordering
  the transport guarantees instead: serial Gusset runs the identical Rust loop
  *plus* the boundary, so it cannot be meaningfully faster than the blocking cgo
  call it wraps. Observed during this audit, which is why it exists: a
  regeneration overlapping an unrelated fuzzer reported blocking cgo at 2.98 ms
  against Gusset's 764 µs for the same loop.
- **The seed table had never been re-measured across its toolchain bump.**
  `bench/seed/README.md` carries an explicit "re-run on every toolchain bump
  (R15)" and its figures were from Go 1.24.7 / Rust 1.95 on x86-64 Linux, while
  the project had moved to Go 1.27.1 / Rust 1.98.0 on darwin/arm64. Both columns
  are now kept side by side. The raw cgo no-op is **18.43 ns** here against
  **64 ns** there, so the commonly quoted "~40 ns cgo call" brackets the range
  rather than describing it. `#cgo noescape` still removes the same allocation
  and about a third of the call on both machines, and batching still falls ~27x
  per item from 16 to 4096 — but "C is cheaper than Rust across cgo" does not
  survive the move: the inline-C no-op was slower on x86-64 Linux (76 vs 64 ns)
  and is faster here (14.93 vs 18.43 ns).
- **Two recording runs could tee into one results file, and nothing noticed.**
  `bench-crossover` appends four arms into a single file through `tee -a`, so a
  second `make bench-crossover` beside the first interleaves into it *and*
  competes for the same cores. Observed: one file held 561 ns, 653 ns and
  5289 ns for the same sub-benchmark as a parallel cgo arm ran beside it, its
  `Serial/Gusset/1000it` iteration count collapsing from 2.4M to 222K mid-arm;
  one line carried a `Serial/Gusset` name with `Parallel/RawCgo`'s metric shape,
  two `write(2)` calls having torn it in half. Nothing about those rows looks
  malformed and the medians would have gone straight into a committed chart.
  Every recording now goes through `bench/record.sh`, which holds a lock across
  the whole run, refuses to start beside another benchmark process or on a
  machine below 60% CPU idle, writes per-arm temporaries and assembles the
  published file only once every arm has passed — so interleaving is structurally
  impossible rather than merely detected. `tools/internal/benchfile` rejects what
  is already on disk: `go test` emits its lines sequentially, so an arm re-entered
  after another intervened cannot come from one producer. Both were exercised for
  real during this work — the preflight refused a regeneration run while an
  unrelated session held the machine at 98% CPU, and the structural check
  refused the contaminated file rather than charting it.
- **An honest recording could not pass its own calibration check**, for three
  compounding reasons. The arms name themselves with a measured duration for the
  shared deterministic loop, and `benchfile` compares those across arms to detect
  a contaminated recording — so anything that makes one arm measure the loop
  differently fails a run taken on an idle machine. All three are recorded here
  because the first diagnosis was wrong and the fix for it exposed the next one:
  - **Timing a single ~700 ns call.** `calibrate` timed one `Spin(1000)` and
    took the minimum of five, which is five fragile samples rather than one
    good one. Each attempt now times a batch against a fixed budget and divides,
    putting the timer and a stray preemption orders of magnitude below the
    signal.
  - **Calibrating inside the `b.Run` loop.** `b.Run` is synchronous, so each
    size was measured on a process the previous sub-benchmark had already run
    in — and `RunParallel` over a blocking cgo call leaves a few hundred idle Ms
    behind it. Calibration is now hoisted above the loop.
  - **A cold core** — which hoisting *caused*, and which is why it is listed
    last. With calibration moved to the top of the process, the first arm
    measures the machine before the CPU has ramped and possibly on an efficiency
    core. Recorded that way, `CrossoverSerial/RawCgo` calibrated
    **1 µs / 11 µs / 113 µs / 1048 µs** where the later arms reported
    **709 ns / 6 µs / 69 µs / 696 µs** for the identical loop: uniformly ~50%
    high across every size at once, which is a clock artifact rather than the
    sub-microsecond sampling noise it was first mistaken for. What settled it
    was the *order*: one recording calibrated 1000, 917, 833 and 709 ns for the
    identical loop in exactly the order `record.sh` runs the arms, and a
    monotonic decay in arm order is a machine warming up, not noise.
    `bench/record.sh` now discards a warm-up arm before the first real one, and
    `burnIn` probes inside each arm's own process until two consecutive probes
    agree within 2% — adaptive rather than a fixed duration, because fixed ones
    were tried and were not enough: 100 ms left the first arm 24% out.
  - Together these take the 1000-iteration spread across arms from **35% to
    ~3%**, and the 1,000,000-iteration spread from **51% to ~5%**, so a
    recording taken on an idle machine now passes its own comparison instead of
    failing it.

### Documentation and landscape (verified 2026-09-20)

- **The landscape table had no Wasm row**, despite Wasm sandboxes being the most
  common answer to the question Gusset exists to answer. Added, with 2026
  figures: **Wasmtime at 2.41x native, wazero at 4.72x** on compute-bound work,
  plus a linear-memory copy per payload. It is the honest alternative to Gusset's
  fault-domain trade-off and the reason that is a trade-off rather than a win —
  Wasm buys an in-process fault domain and pays native speed for it; Gusset takes
  the inverse trade. An engine that can genuinely fault wants one of those or
  Phase 4.
- **iceoryx2's Go binding is unstarted work, not integration work.** `DECISIONS.md`
  commits Phase 4 to contributing it upstream, and an adopter reading "thin
  adapter over iceoryx2" could reasonably assume there is a binding to adapt.
  Checked against iceoryx2 v0.10.0: the upstream bindings are C, C++, Rust,
  Python and C#, with Go still listed as planned. Recorded as its own decision
  row so the commitment's actual size is visible where decisions are read.
- **Go 1.27 changes nothing Gusset depends on** — no scheduler, sysmon,
  `runtime/metrics` or `GOMAXPROCS` change — so the design's assumptions hold. Its
  goroutine-leak profiling is already the basis of
  `TestSoak_GoroutineLeakProfileIsEmptyAfterDrain`.
- `AGENTS.md` pointed at `bench/channel_hop`, which does not exist; the
  measurement lives in `BenchmarkChannelHop` in `bench/seed/go/ffi_test.go`. Its
  conclusion survived the re-measurement and got stronger: a channel send and a
  cgo call stayed within 5% of each other across both an architecture and a
  toolchain change (56 vs 64 ns on x86-64 Linux, 17.62 vs 18.43 ns here).
- **`docs/why.md` named a test that does not exist** as the verifier of I4, the
  library's central claim — `TestSoak_ThreadCapUnderTenThousandCalls`, asserting a
  bound of `pool_size + GOMAXPROCS + 8` "under ten thousand calls". The real test
  is `TestPitfall_ThreadCapBoundedSoak`: 500 concurrent callers, a 4-worker pool,
  and a bound of `poolSize + GOMAXPROCS + 16` above the count it started from. All
  three numbers and the name were wrong, which is the failure mode a citation
  exists to prevent. A sweep of every test name cited anywhere in the docs — 31 Go
  names and 13 Rust ones — found no other false citation.
- **Nothing told a prospective adopter when *not* to use Gusset.** The README
  explains what it does and `docs/adoption.md` how to wire it, but neither
  answers the first question a team actually asks. `docs/choosing.md` does: the
  two measurements that settle it (per-call Rust work, and in-flight concurrency
  against `GOMAXPROCS`), what an average, commercial or enterprise adopter each
  has to do differently, and the six situations where the answer is raw cgo, a
  separate process or Wasm. It states plainly that Gusset cannot beat a raw cgo
  call on one call — it caps the aggregate cost rather than reducing the
  per-call one — because "saves overhead" is the thing adopters most often
  expect and it is not what Gusset sells.
- **The memory case for bounded threads was overstated wherever it was made
  informally** — in the quantity it named and, it turns out, in its arithmetic
  too. A per-thread stack figure estimates *reserved address space*, which is
  not RAM, and every informal retelling dropped the word "virtual" and so turned
  it into a saving that does not exist. Resident memory over 32 → 2048 in-flight
  calls grows by roughly **28 MiB** for blocking cgo and **13 MiB** for Gusset,
  a **10–15 MiB** gap across recordings. `docs/choosing.md` now *generates* both
  the resident column and the per-thread address-space division from the
  committed data rather than asserting either, and says the real cost of a few
  hundred threads is scheduler pressure, thread-creation latency on the request
  path and the 10,000-thread cliff.
- **`docs/why.md` claimed a burst of 1,000 goroutines spawns 1,000 OS threads.**
  It does not: the count grows sub-linearly, at roughly **0.17 threads per
  in-flight call** for ~7 ms calls (33/90/224/368 threads at 32/128/512/2048),
  because Go reuses idle Ms and `sysmon` only retakes a P from a call that has
  already blocked ~20 µs. The ratio approaches 1:1 only as calls get long. Both
  Mermaid diagrams in that section carried the same overstatement — one showing
  1,000 goroutines reaching the 10,000-thread ceiling, and the Gusset side
  claiming "OS threads stay <= 10" against a measured 21 — and now carry the
  measured figures instead.

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
