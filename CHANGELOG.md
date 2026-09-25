# Changelog

## [Unreleased] - 2026-09-25 · Performance and the last open gaps

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

## [Unreleased] - 2026-09-25 · Third audit: lock order, state machine, regressions

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

## [Unreleased] - 2026-09-25 · Go SIMD evaluation

- **Go 1.27's `simd` experiment was evaluated against the Rust path.** It does not touch Gusset's runtime: the Go path has no numeric kernel, and `copy()` is already vectorized. The suite passes under `GOEXPERIMENT=simd`, and CI now checks that. `bench/simd_crossover_test.go` measures one kernel three ways and checks that all three agree. `docs/choosing.md` records where pure-Go SIMD beats crossing into Rust.
- **Cancellation checks inside the hot loop blocked vectorization.** The diagnostic and example engines' sum-of-squares kernels called `ctx.check()` on every 1024th iteration inside the loop. That kept the loop scalar at about 1.4 GB/s. Checking once per 4 KiB chunk and summing each chunk in `u32` reaches about 6 GB/s, and a 1 MiB Gusset call dropped from 839 µs to 262 µs in the measurement sandbox. `docs/adoption.md` now shows the chunked pattern.

## [Unreleased] - 2026-09-24 · Allocator API and interop audit

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

## [Unreleased] - 2026-09-22 · Named field layout

- **Equal-width fields could trade places without failing ABI init (I6).** `gusset_abi_layout` reports size and alignment. Swapping `CallHeader.flags` with `reserved` (both `u32`), `span_id` with `timeout_ns` (both 8 bytes), or any two `AllocStats` fields leaves both numbers unchanged, so version 2's check accepted a header that reads those fields from the wrong slots. `gusset_abi_fields` reports the offset and size of every named field and writes no more entries than the caller's `cap`, so the table is not appended to the 36-byte `AbiLayout` a version-2 caller already allocates. Go compares that report with cgo `Offsetof`/`Sizeof`. A planted swap of `flags` and `reserved` panics in `init` with `ABI field CallHeader.flags drifted: Rust offset 32 size 4, cgo offset 36 size 4`. `TestPitfall_EqualWidthFieldSwapKeepsSizeAndBreaksNamedOffsets` walks every equal-width pair and shows the offset multiset is unchanged, which is why a size check or a sorted-offset check cannot see the swap.

## [Unreleased] - 2026-09-22 · Caller-held buffers

- **An engine could return a buffer Go still holds (R16).** The alias check refused the unit's own input, id 0, a second claim, and a buffer another unit was reading. A buffer published through `gusset_buf_alloc` (`NewBuffer`) and then returned as `JobOutput::Buffer` was accepted. The waiter frees an output when it is done, which releases that memory under the caller's view. Published allocations are now marked `caller_held`, and `claim_output` refuses them. Engine-private `buf_alloc` may still be returned once. `engine_returning_a_caller_held_buffer_is_refused` fails against the pre-fix code (published buffer 1 was accepted as an output).
- **`Bytes` raced `Free` on the slice header.** `freed` is atomic, so the flag was synchronized and `b.data` was not: `-race` reported `Free` writing the header while `Bytes` read it. Both now take `Buffer.mu`, and `Bytes` holds `cgoMu` across that read so it cannot hand out a view `Close` is already freeing. `TestPitfall_BytesAndFreeDoNotRace` fails against the pre-fix code with a data race at `buffer.go`.
- **Engine dispatch held the registry read lock across execution.** `default_dispatch` called `engine(ctx, input)` while holding `ENGINE_REGISTRY.read()` or `GLOBAL_ENGINE.read()`. A concurrent call to `register_engine` or `clear_engine_handlers` took a write lock and queued behind a slow engine, starving all subsequent worker dispatches on POSIX `RwLock`. `EngineFn` is now `Arc<dyn ...>`, and `default_dispatch` clones the handler and releases the lock before invocation.

## [Unreleased] - 2026-09-22 · Containment audit

Every fix below ships with a test that fails against the pre-fix code, run from a
separate worktree of `7a56c3c` with only the new diagnostic mode added.

- **A panic payload whose destructor panicked killed the worker (I2, I4).** The firewall caught the engine's panic, then dropped the payload *after* `catch_unwind` returned. `panic_any(T)` where `T::drop` panics unwound the worker thread itself: the ticket was never stored or written, its cancel flag stayed registered, and a Go caller with no deadline parked forever holding a pool permit. Payloads are now disposed of under their own `catch_unwind` (a re-thrown payload is leaked, not recursed into), the whole per-unit path runs under a second firewall so any fault still produces a completion, and a dying worker's `join` payload is contained in `close`. Diagnostic mode 15 throws such a payload. `panicking_payload_destructor_does_not_kill_the_worker` and `TestPanicZoo_PanickingPayloadDestructor` fail against the pre-fix code (the Go call returned `context deadline exceeded` after its full 3 s).
- **A sibling's panic erased a finished result, but only a small one (I2).** `gusset_take` allocated the egress buffer for a result of 4 KiB or less through the public, poison-checked `buf_alloc`, so a job that finished after another job poisoned the handle lost its result and surfaced `FFI_ERR "handle is poisoned"` — neither the data nor `ErrPoisoned`. Larger results were already promoted on the worker and came back intact. Take now uses the internal egress copy; poison still refuses every new submission and `NewBuffer`. `TestPitfall_InFlightResultSurvivesSiblingPanic/small` fails against the pre-fix code.
- **`Close` waited for child processes.** The completion pipe's write end was duplicated with `syscall.Dup`, which does not set close-on-exec, so every subprocess started while a handle was open inherited it. Rust closing its copy then left the pipe with a writer, `drainPipe` never saw EOF, and `Close` blocked until the child exited. The dup now sets close-on-exec under `syscall.ForkLock`. `TestPitfall_CloseDoesNotWaitForChildProcesses` fails against the pre-fix code.
- **Two results could own one buffer (R4/R16).** The alias check refused only the unit's own input and id 0. An engine returning a captured, pre-allocated id — the opcode example's pattern — handed the same buffer to every call; the drain loop takes completions back to back, so both results resolved one pointer and the second was read after the first waiter freed it. Returning another in-flight unit's input was likewise accepted. A buffer may now become an output once per lifetime and never while another unit holds it as input, checked under the registry lock. `one_buffer_is_returned_as_an_output_at_most_once` and `a_buffer_in_use_as_another_units_input_is_refused_as_an_output` fail against the pre-fix code.
- **An overwritten error status leaked its message.** `gusset_submit` and `gusset_buf_alloc`, on discovering poison after the guarded call failed, wrote `FFI_POISONED` over the status `ffi_guard` had filled without freeing its boxed message. `FfiStatus::overwrite` frees first. The poison flip that reaches this branch is a race, so `overwriting_an_error_status_releases_its_message` pins the replacement under the counting allocator, with a control arm showing the old pattern's leak is measurable.
- **Containment stress test.** `TestStress_ContainmentUnderLoad` runs deadline-free callers against every failure above at once — destructor bombs direct and delayed, delayed panics beside slow successes, large zero-copy round trips, mid-flight `Close`, and continuous fork/exec — under a watchdog, and checks typed errors, byte-exact results, and that goroutines and Rust live bytes return to baseline. Against the pre-fix code it hangs until the watchdog fires. `GUSSET_STRESS_ROUNDS` scales it.

## [Unreleased] - 2026-09-21 · Boundary hardening

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

## [Unreleased] - 2026-09-21 · Go tip bootstrap

- **The weekly tip job could not build Go (run 35594759554).** `Set up Go Tip` cloned tip and ran `./make.bash` with `GOROOT_BOOTSTRAP` unset, so the build used the runner image's Go: `Building Go cmd/dist using /opt/hostedtoolcache/go/1.24.13/x64. (go1.24.13 linux/amd64)` then `found packages main (build.go) and building_Go_requires_Go_1_26_0_or_later (notgo126.go)`. Go tip (1.28) requires a bootstrap >= Go 1.26.0 (`src/make.bash` `bootgo=1.26.0`, `src/cmd/dist/notgo126.go` `//go:build !go1.26`). The job now installs the matrix pin (`1.27.x`) and passes that tree as `GOROOT_BOOTSTRAP`. The suite's `go` floor stays 1.26; this is the compiler that builds tip, not the compiler Gusset ships against. `TestTipWorkflowBootstrapsFromGo1_26OrNewer` fails against the pre-fix workflow.

## [Unreleased] - 2026-09-20 · Audit and hardening

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
