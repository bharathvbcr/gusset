# Gusset: Agent Hand-off

As of 2026-09-20. Author: Bharath Chandra. Living copy: https://claude.ai/code/artifact/101473c0-1589-4016-bab8-721261a7912e

Working brief for any agent building Gusset, the Go–Rust runtime contract. The build plan and phases are in `docs/PLAN.md`; this file is the rulebook the agent reads before touching the repo. Resolved decisions are in `DECISIONS.md`.

## Mission and working rules

Gusset is the runtime contract for running a Rust engine inside a Go service: the layer between "bindings exist" and "this runs in production without taking the Go process down." It owns the panic firewall, bounded concurrency, deadlines, poisoned handles, ABI verification, allocator accounting and the CI matrix that proves them. It does not own type marshalling, cgo-free calling, or IPC transport.

How an agent works in this repo:

1. Every change is a merge with green CI on the full matrix; no "will fix in a follow-up" on a red gate.
2. A new failure mode gets a test in the panic zoo or pitfall suite before the fix lands; the test must fail on `main` first.
3. Numbers in the README come from `benchstat` output checked into `bench/results/<platform>-<go>-<rust>.txt`, never typed by hand.
4. Public surface stays at 14 exported Rust functions (the list in the hardening checklist is the whole ABI) and at most 10 Go entry points; adding one needs a line in `DECISIONS.md` saying why.
5. Anything that touches Go runtime internals (`//go:linkname`, `asmcgocall`, private symbols) is refused, whoever asks.
6. When a Go or Rust release changes boundary behaviour, the fix is a version-gated code path plus a test, not a raised floor.
7. Prefer deleting a feature over adding a knob; every config option must have a test that exercises both settings.
8. Commit messages state which invariant (I1–I6 in `docs/PLAN.md`) the change protects.

## Toolchain policy

Develop on the latest stable of both languages; support latest and latest-minus-one; use nightly and tip only for gates, never for shipped code. As of 2026-09-20 that is Go 1.27 (August 2026) and Rust 1.98.1 (3 September 2026).

| Channel | Go | Rust | Used for |
| --- | --- | --- | --- |
| Develop and release | 1.27.x | 1.98.x | Everything shipped; `go.mod` says `go 1.26`, `rust-version = "1.97"` |
| Supported floor | 1.26 | 1.97 | CI leg; a floor bump is a minor release with a changelog line |
| Gates only | gotip (weekly job) | nightly (pinned by date, bumped monthly) | Go tip catches boundary changes early; nightly runs Miri, `-Zsanitizer=address`, `cargo-fuzz` |
| Never | anything below 1.22 | anything below 1.81 | 1.22 introduced `#cgo noescape`/`nocallback`; 1.81 made panics escaping `extern "C"` abort |

Release-note facts to design around: Go 1.26 cut baseline cgo call overhead by about 30% and made the Green Tea GC default, so re-run `bench/` on every Go minor and never hard-code a nanosecond figure in docs; Go 1.26 randomizes the heap base on 64-bit builds, so any test that assumes pointer values is invalid; Go 1.26 added the `/sched/threads:threads` runtime metric, which is the thread-cap soak's assertion source; Go 1.27 made the `goroutineleak` pprof profile GA, which the completion-channel tests use to prove no waiter is leaked.

## Runtime differences and how Gusset handles each

The two runtimes disagree on lifetime, failure, threads, clocks, atomics and types. Four rows (lifetime, failure, cancellation, clocks) changed the design from the first draft.

| Difference | Go | Rust | Gusset handling |
| --- | --- | --- | --- |
| Object lifetime | GC-managed; `runtime.AddCleanup` (1.24+) runs later, on one goroutine, non-deterministically; an object whose last use is a cgo argument may be collected during the call | RAII: `Drop` runs deterministically at scope end | Explicit `Close` is the contract; `AddCleanup` is a leak backstop that logs and frees; `runtime.KeepAlive(h)` after every cgo call on the handle |
| Heap movement and input ownership | Heap objects never move; stacks grow and shrink, but not while the goroutine is in a cgo call | Nothing moves | Go pointers valid for the call only; submit copies inputs up to 4 KiB, larger inputs live in Rust-owned `Buffer`s that Go fills through `unsafe.Slice` (R16); anything else retained means copy or `runtime.Pinner` unpinned in the same function (R6) |
| Memory accounting | `GOMEMLIMIT` sees the Go heap only | Global allocator, no limit, no visibility | `Counting<A>` wrapper, `Stats()`, `AdviseMemoryLimit` |
| Failure model | `panic` recoverable per goroutine, no poisoning; runtime fatals (thread exhaustion, `cgocheck`) unrecoverable | Panic unwinds; escaping `extern "C"` aborts (1.81+); `Mutex` poisoning; `abort()` uncatchable | `catch_unwind` around each **work unit in the worker loop**, not only the submit call; the loop survives; a dead worker is respawned; a ticket whose result is `FFI_PANIC` poisons the handle; abort is out of scope (Phase 4 isolation) |
| Threads | Goroutines are M:N, migrate between OS threads, preempted by signal; cgo runs on the g0 stack | 1:1 OS threads, stable TLS, own stack size | Rust-owned pool per handle with explicit stacks; no thread-identity assumptions (R7); `LockOSThread` only in adopter code that needs it |
| Blocking | A cgo call is a syscall to the scheduler: M pinned, P handed off after ~20 µs, not counted in `GOMAXPROCS`, does not block STW; thread cap 10,000 | Blocking is just blocking | Submit-and-return keeps every call under the hand-off threshold; waits park on the netpoller via the completion pipe; semaphore bounds in-flight (R11) |
| Cancellation | `context.Context`, cooperative through channels | Dropped futures, or cooperative checks | Each job owns an `AtomicBool` cancel flag in Rust memory; Go sets it with `gusset_cancel(handle, ticket)` (one cgo call); `Close` sets all of them with `gusset_cancel_all`; Rust checks the flag between work units; Rust never reads Go-owned memory |
| Clocks | `time.Now()` monotonic reading is process-internal; macOS and Linux sources differ from Rust's | `Instant` from `CLOCK_MONOTONIC` or `mach_absolute_time` | Header carries a **relative** `timeout_ns` computed at submit; Rust builds its own `Instant`; absolute monotonic values never cross; wall-clock only for trace timestamps |
| Memory model and atomics | `sync/atomic` is sequentially consistent; 64-bit alignment rules on 32-bit targets | Acquire/Release orderings; no cross-language memory model exists | Nothing is shared by address except Rust-owned memory reached through exported functions; all cross-boundary values are copied into the header; the completion pipe is the only signal path |
| Types | `int` is platform-sized; no unions; strings immutable and not validated UTF-8; `error` is an interface | Fixed-width ints; only `#[repr(C)]` is stable; `u128`, fat pointers, most `Option<T>` are not FFI-safe; `str` must be valid UTF-8 | Header and status use `i32`, `u32`, `u64`, `usize` and thin pointers only; `#![deny(improper_ctypes_definitions)]`; `str::from_utf8` with `FFI_BAD_ARG` on failure, never `_unchecked`; Go exposes `*gusset.Error{Code, Msg, File, Line}` for `errors.Is` |
| Integer overflow | Wraps silently | Panics in `dev`, wraps in `release` | Explicit `wrapping_*` and `checked_*` in `ffi/`; panic zoo and pitfalls run in both profiles |
| Zero values | Everything zero-initialized | Uninitialized until written | Out params written with `ptr::write` only on `FFI_OK`; Go reads `out` only on `FFI_OK` |
| Async | Blocking-style goroutines | Poll-based Tokio | No runtime inside a cgo call; an async engine runs one Tokio runtime on a pool thread; submit = `spawn` + oneshot, completion through the same pipe |
| Signals | Go owns every handler and requires `SA_ONSTACK` from foreign code; a thread without an alternate stack dies on stack overflow before any handler runs | `std` installs neither handlers nor `sigaltstack` in a `staticlib` | Rust never installs handlers; each worker installs a 64 KiB `sigaltstack` in its entry function; `EINTR` retried in `ffi/` |
| Process exit | `os.Exit` skips defers and never runs Rust `Drop`; tests share one process | Statics never dropped; threads killed at exit | `gusset_shutdown(drain_ms)` from app shutdown and `TestMain`; nothing durable relies on `Drop` |
| Logging | `slog`/`fmt` on Go's side; Rust cannot call back | `log`/`tracing` on Rust's side | Rust subscriber writes into a bounded ring; Go drains it with `gusset_drain_logs` into `slog`; stderr fallback |
| Build profiles | One profile | `dev` has overflow checks and `debug_assertions` | CI runs the full pitfall suite in both `dev` and `release` |

## Hard rules for the boundary

Each rule names the test or lint that enforces it. A PR that cannot point at the enforcer for a rule it touches is not mergeable.

| # | Rule | Enforced by |
| --- | --- | --- |
| R1 | Every exported Rust function is `pub unsafe extern "C"`, does nothing but call `ffi_guard`, and is one of the 14 names in `ffi/exports.txt` | `tests/exports_match.rs` diffs `nm` output against the list |
| R2 | No panic crosses the boundary; the Cargo profile is `panic = "unwind"` for every profile including `test` and `bench` | `build.rs` reads `CARGO_CFG_PANIC` and fails the build on `abort`; panic zoo tests |
| R3 | Strings and buffers cross as `(ptr, len)`; no `CString`, no NUL termination, no `unwrap` in any error path | `clippy.toml` disallowed-methods: `CString::new`, `Option::unwrap`, `Result::unwrap`, `expect` in `ffi/` |
| R4 | Memory is freed by the side that allocated it, through an exported `*_free`; Go never calls `C.free` on Rust memory | ASan job with a deliberate cross-free test that must fail; `grep -r 'C.free' go/` is a CI lint |
| R5 | Every `#cgo` import of a Rust export carries `#cgo noescape` and `#cgo nocallback`; no export calls back into Go | `go vet` custom analyzer `gussetvet` in `tools/` |
| R6 | Go pointers passed to Rust are valid only for the duration of the call; anything Rust retains is copied or pinned with a `runtime.Pinner` that is unpinned in the same Go function | `GOEXPERIMENT=cgocheck2` job; the pinner test |
| R7 | Rust never stores a Go pointer, never calls a Go function, and never assumes two calls arrive on the same OS thread; no `thread_local!` in `ffi/` or job code | `clippy.toml` disallowed-macros: `thread_local!` in `ffi/` and `pool/`; the thread-migration test documents the hazard |
| R8 | Heavy work runs on Rust-spawned threads with explicit `stack_size`; the cgo call only submits and returns | musl job with the deep-recursion work unit |
| R9 | Every submission carries the 40-byte header (`trace_id[16]`, `span_id[8]`, `timeout_ns` relative to submit, `flags: u32`, `reserved: u32`); Rust turns `timeout_ns` into an `Instant` at submit and checks it and the job's cancel flag between work units | Deadline test and cancel test with a slow work unit; header size static-asserted at 40 |
| R10 | A handle whose ticket returned `FFI_PANIC` is poisoned; all later calls return `FFI_POISONED` without entering Rust | Poison test; `Handle.Close` is the only way out |
| R11 | In-flight calls per handle never exceed the pool size; the Go semaphore, not the OS, does the queuing | Thread-cap soak asserts `/sched/threads:threads` stays under pool size + `GOMAXPROCS` + 8 |
| R12 | Go `init()` compares `gusset_abi_layout()` (version, size and alignment of every `#[repr(C)]` type) against compiled-in constants and panics on mismatch | ABI-drift test bumps a field and expects the panic |
| R13 | No `//go:linkname`, no `asmcgocall`, no `purego` in this repo; the only calling path is cgo | CI grep; a PR touching it is closed |
| R14 | Exactly one Rust `staticlib` per Go binary; adopters with several engines build an umbrella crate | Documented in README; the example engine shows the pattern |
| R15 | Every number in docs is generated: `bench/` output via `benchstat`, committed per platform and toolchain version | `make docs` regenerates and CI diffs |
| R16 | Rust never retains Go memory after `gusset_submit` returns: inputs up to 4 KiB are copied during submit; larger inputs are written by Go into Rust-owned `Buffer`s (`gusset_buf_alloc`/`gusset_buf_free`, exposed as `[]byte` via `unsafe.Slice`) and submitted by id; results come back through `gusset_take` or as a `Buffer` | `cgocheck2` job; large-input test asserts 0 Go allocs and no extra copy; ASan job |

## Pitfall catalogue

Every row is a test name in `tests/pitfalls/`; the agent adds a row when it finds a new one, never fixes silently.

| Pitfall | Symptom | Cause | Guard |
| --- | --- | --- | --- |
| NUL byte in a panic message | `SIGABRT`, whole Go process gone | `CString::new(..).unwrap()` panics inside the `catch_unwind` `Err` arm; second panic unwinds out of `extern "C"` | R3; `panic_nul` test (reproduced in `bench/seed`) |
| `panic = "abort"` in a dependency or profile | Firewall silently becomes a no-op | `catch_unwind` cannot catch an abort | R2 `build.rs` check |
| `*out = value` on the Ok path | UB when `T: Drop` | Assignment drops the uninitialized old value | `ptr::write` only; clippy lint `gusset::assign_through_raw` |
| Thread exhaustion | Fatal `thread exhaustion`, not a recoverable error | Goroutines blocked in cgo each pin an M; cap is 10,000 | R11; soak test; `/sched/threads:threads` alert |
| Goroutine migrates between calls | `thread_local!` state vanishes; Tokio `Handle::enter` guard on the wrong thread; `errno` lost | Go schedules goroutines onto any M between cgo calls | R7; stateless submissions; `runtime.LockOSThread` only in the example that needs it |
| musl 128 KB thread stack | Stack overflow as a raw SIGSEGV in Go's handler | cgo-created threads inherit the pthread default; musl's is 128 KB | R8; musl job |
| Rust stack overflow reported by Go | `unexpected signal during runtime execution`, no Rust frame | A `staticlib` never runs `std::rt::init`, so Rust's overflow handler is absent | R8; Go owns SIGSEGV; each worker installs its own `sigaltstack` in its entry function, because std only does that for Rust binaries, not staticlibs |
| `SIGURG` preemption signal interrupts a Rust syscall | Sporadic `EINTR` from C deps inside Rust | Go 1.14+ async preemption signals hit the thread regardless of what it runs | Retry loop around any raw syscall in `ffi/`; `std` already retries |
| Two heaps, one limit | OOM-killed with a small Go heap | `GOMEMLIMIT` cannot see Rust allocations | `Counting<A>` + `Stats()` bridge; memory-limit test |
| Go pointer retained by Rust | `cgocheck` fatal, or silent corruption after GC | Rust stored a pointer past the call | R6, R16; `cgocheck2` job |
| Duplicate Rust `std` symbols | Link error `rust_eh_personality` defined twice | Two `staticlib`s in one binary | R14; umbrella crate |
| Windows toolchain mismatch | Unresolved symbols at link | cgo uses mingw; Rust built for `-msvc` | Target `x86_64-pc-windows-gnu`; CI leg |
| macOS dylib signing | `dyld` refuses to load under hardened runtime | Unsigned or ad-hoc dylib | Static link by default; dylib path only in docs |
| `uniffi-bindgen-go` needs `LD_LIBRARY_PATH` | Works in dev, fails in a container | Dynamic loading by default | Gusset example shows static link with generated bindings |
| rust2go's `GODEBUG=invalidptr=0,cgocheck=0` | Silent GC corruption under load | Disables the checks that make R6 enforceable | Never set these; CI fails if `GODEBUG` contains either |
| purego / `asmcgocall` trampolines | Works until the next Go minor; P pinned, GC STW blocked for the call | Runtime internals, no `entersyscall` | R13 |
| Batching through a channel | Amortization never materializes | A channel send costs about one cgo call (56 ns vs 64 ns measured) | Batch only slices that already exist at the call site; `bench/channel_hop` documents it |
| Pointer passed without `noescape` | 1 heap alloc per call, GC pressure | cgo assumes escape by default | R5; `-benchmem` gate at 0 allocs/op |
| `_test.go` cannot use cgo | Tests fail to compile | cgo forbidden in test files | All cgo lives in `internal/ffi`; tests call Go wrappers |
| Stale `.a` not rebuilt | Old Rust code linked | Go's build cache does not track the archive | `go generate` writes the archive hash into a Go file; CI uses `go build -a` |
| `unsafe.String` over mutable Rust memory | UB, corrupted strings | Go assumes string bytes are immutable | `unsafe.Slice` + copy to `string` when retaining |
| Deadline cannot cancel a running call | Caller hangs past its timeout | Nothing can interrupt a cgo call from Go | R9; deadline enforced between work units in Rust |
| Heap base randomization (Go 1.26) | Tests asserting pointer ranges flake | Security feature, on by default | Never assert on addresses |
| Green Tea GC timing (Go 1.26) | Race that only shows on new GC | Different marking order | `GOGC=1` job; `-race` job |

## Hardening checklist

Tick every box before a phase is called done. Each box maps to a rule (R) or pitfall row above.

### Rust crate (`gusset`)

- [ ] `#![deny(unsafe_code, unsafe_op_in_unsafe_fn, improper_ctypes_definitions, missing_docs)]` crate-wide, with `#[allow(unsafe_code)]` only on the `ffi` and `pool::sys` modules
- [ ] The whole ABI is 14 exports, nothing else is `#[no_mangle]`: `gusset_abi_layout`, `gusset_init`, `gusset_shutdown`, `gusset_handle_open`, `gusset_handle_close`, `gusset_submit`, `gusset_take`, `gusset_cancel`, `gusset_cancel_all`, `gusset_status_free`, `gusset_alloc_stats`, `gusset_drain_logs`, `gusset_buf_alloc`, `gusset_buf_free`
- [ ] Three `#[repr(C)]` types, all with `static_assertions::assert_eq_size!` and `assert_eq_align!` beside them: `CallHeader` (40 bytes), `FfiStatus { code: i32, msg: *mut u8, msg_len: usize, file: *const u8, file_len: usize, line: u32 }`, `AbiLayout { version: u32, sizes: [u32; 3], aligns: [u32; 3] }`
- [ ] `ffi_guard` null-checks both out pointers, uses `ptr::write`, wraps `Display` formatting in its own `catch_unwind`, maps `Ok(Err)` to `FFI_ERR` and `Err(payload)` to `FFI_PANIC`; `msg` comes from `Box<[u8]>` and is freed only by `gusset_status_free`
- [ ] `panic::set_hook` installed once in `gusset_init`, records location into a thread-local the worker loop reads; the hook is never replaced by adopters (documented)
- [ ] Worker pool per handle: fixed size from `gusset_handle_open`, threads from `Builder::stack_size` (default 8 MiB) named `gusset-w<N>`; each thread's entry installs a 64 KiB `sigaltstack`, then loops with `catch_unwind` around every job; a thread that dies is respawned by the next submit
- [ ] Every job receives `&CallHeader` and its own `AtomicBool` cancel flag; `check(&self) -> Result<(), Cancelled>` compares the `Instant` built from `timeout_ns` at submit and the flag
- [ ] `gusset_submit(handle, header, input_ptr, input_len, buffer_id, out_ticket, status)`: copies inputs up to 4 KiB, otherwise takes a `Buffer` id; returns a `u64` ticket; when a job finishes the worker writes the 8-byte ticket to the Go-owned pipe write fd with `EINTR`/`EAGAIN` retry; `gusset_take(handle, ticket, out, status)` moves the result out exactly once
- [ ] `Buffer`: `gusset_buf_alloc(handle, len, out_id, out_ptr)` returns 64-byte-aligned Rust-owned memory; freed only by `gusset_buf_free`; an engine may back a `Buffer` with device-shared memory (Metal shared buffers) for the GPU path
- [ ] `Counting<A: GlobalAlloc>` with relaxed atomics; `gusset_alloc_stats()` returns `{live, peak, allocs}`; no allocation inside the stats call
- [ ] `cbindgen.toml` with `language = "C"`, `include_guard`, `cpp_compat = true`; header generated by `build.rs`, committed, and diffed in CI
- [ ] `Cargo.toml`: `crate-type = ["staticlib"]`, `[profile.*] panic = "unwind"`, `rust-version`, `codegen-units = 1`, `lto = "fat"` in release, `-C force-frame-pointers=yes` in `.cargo/config.toml`
- [ ] `cargo deny` for licenses and advisories; `cargo semver-checks` on tags

### Go package (`gusset`)

- [ ] All cgo in `internal/ffi`; public API in the root package; `_test.go` files never import `C`
- [ ] `#cgo noescape` and `#cgo nocallback` on every import; `#cgo LDFLAGS` uses `${SRCDIR}` only, never absolute paths
- [ ] At most 10 entry points: `Open`, `Close`, `Call`, `Submit`, `Wait`, `NewBuffer` (with `Buffer.Free`), `Stats`, `AdviseMemoryLimit`, `Threads`, `DrainLogs`
- [ ] `Handle` fields: `ptr unsafe.Pointer`, `sem chan struct{}`, `poisoned atomic.Bool`, `pipe *os.File` (read end from `os.Pipe()`; the write fd went to `gusset_handle_open`), `mu sync.Mutex` + `pending map[uint64]chan result`
- [ ] `Call(ctx, in []byte)`: acquire `sem` with `ctx`; header from the remaining `ctx` deadline (as relative `timeout_ns`) and the OpenTelemetry `SpanContext` if present, zero otherwise; submit; wait on the ticket channel or `ctx.Done()`; on `ctx.Done()` call `gusset_cancel(handle, ticket)` and keep waiting for the ticket so the result is always drained; `runtime.KeepAlive(h)` after every cgo call
- [ ] `NewBuffer(n)` returns a Rust-owned `[]byte` (`unsafe.Slice` over `gusset_buf_alloc`) for inputs over 4 KiB; `Submit(ctx, buf)` sends it by id; `Buffer.Free` is explicit, with an `AddCleanup` backstop that logs
- [ ] One reader goroutine per handle drains the pipe and dispatches tickets; `Close`: `gusset_cancel_all`, `gusset_handle_close` (Rust closes the write fd), reader exits on EOF, then the Go side releases; `AddCleanup` on the handle logs and calls `Close` if the app forgot
- [ ] `init()` compares `gusset_abi_layout()` to compiled-in constants and calls `gusset_init` once; failure is `panic` with both version pairs in the message
- [ ] `Stats()` returns Rust allocator numbers; `AdviseMemoryLimit(total)` sets `debug.SetMemoryLimit(max(total - rustLive, floor))` when called from the app's ticker
- [ ] `runtime/metrics` reader for `/sched/threads:threads` exposed as `gusset.Threads()` for tests and dashboards
- [ ] `go vet` analyzer `gussetvet` in `tools/`; `staticcheck` and `govulncheck` in CI

### Build and link

- [ ] `make all` = `cargo build --release` then `go build`; `go generate` writes `internal/ffi/archive_hash.go` so Go's cache invalidates when the `.a` changes
- [ ] Cross builds via `cargo zigbuild` + `CC="zig cc -target <triple>"` for Go; matrix: `linux/amd64`, `linux/arm64`, `linux/arm64-musl`, `darwin/arm64`, `windows/amd64-gnu`
- [ ] Static link everywhere; the dylib path exists only in `docs/dylib.md` with the codesigning steps
- [ ] `-ldflags=-linkmode=external` pinned for macOS; internal linking of cgo is not exercised
- [ ] Container image: distroless with the static binary; no C toolchain at runtime

## Repo layout and build order

```
gusset/
  Cargo.toml              # workspace: gusset, gusset-example
  crates/gusset/          # runtime crate: ffi/, pool/, alloc/, header/
  crates/gusset-example/  # engine exercising every failure mode
  go.mod                  # module github.com/bharathvbcr/gusset
  gusset.go handle.go stats.go   # public API
  internal/ffi/           # all cgo; generated header lives here
  tools/gussetvet/        # go vet analyzer for R5
  bench/                  # Go benchmarks + committed benchstat results (bench/seed = Phase 0 repro)
  tests/pitfalls/         # one file per catalogue row
  tests/panic_zoo/
  .cargo/config.toml      # frame pointers, target flags
  cbindgen.toml  clippy.toml  deny.toml
  DECISIONS.md  CHANGELOG.md  AGENTS.md  docs/PLAN.md
  .github/workflows/      # matrix.yml, nightly.yml, tip.yml
```

Build order for Phase 1 (each step merges green; the next step starts only after):

1. Move the seed benchmark and the NUL-panic repro from `bench/seed` into `bench/` and `tests/panic_zoo/`; CI runs them on Linux and macOS. The document's original guard must fail here.
2. `FfiStatus`, `ffi_guard`, `gusset_status_free`, panic hook; Go error mapping with `FFI_OK/ERR/PANIC/POISONED/BAD_ARG`. Panic zoo green.
3. `gusset_abi_layout`; `static_assertions`; Go `init()` check; ABI-drift test.
4. `cbindgen` config, committed header, `go generate` archive hash; CI diff job.
5. Worker pool with explicit stack size and `sigaltstack`; `gusset_submit`/`gusset_take`; `os.Pipe` completion; Go reader goroutine; `goroutineleak` profile assertion.
6. `Handle` semaphore, header, timeout, per-job cancel flag, poisoning; deadline, cancel, poison and thread-migration tests.
7. `Buffer` (`gusset_buf_alloc`/`gusset_buf_free`, `NewBuffer`); large-input test.
8. `Counting<A>`, `gusset_alloc_stats`, `Stats()`, `AdviseMemoryLimit`; memory-limit test.
9. musl, cross-compile and Windows legs; thread-cap soak; `cgocheck2`, `-race`, ASan jobs.
10. Replace the boundary in the tessl/sparsl Go service; commit before/after `benchstat` to `bench/results/`.

## Testing plan

Three workflows: `matrix.yml` on every PR, `nightly.yml` daily, `tip.yml` weekly. A red job blocks merge; nightly and tip failures open an issue automatically.

| Job | Workflow | Platform | Command / setting | Must contain |
| --- | --- | --- | --- | --- |
| `unit` | matrix | linux, macos | `cargo test`, `go test ./...` | Panic zoo (5 cases), ABI drift, poison, deadline, cancel, thread migration, completion drain |
| `cgocheck2` | matrix | linux | `GOEXPERIMENT=cgocheck2 go test ./...` | Pinner test, retained-pointer test that must fail |
| `race-gc` | matrix | linux, macos | `go test -race`, then `GOGC=1 go test -count=20` | Completion reader vs `Close`; concurrent `Call` on one handle |
| `soak` | matrix | linux, macos | `go test -run Soak -timeout 20m` | 10,000 goroutines on a 4-worker pool; assert `/sched/threads:threads` under 4 + `GOMAXPROCS` + 8; `goroutineleak` profile empty after drain |
| `memlimit` | matrix | linux | `GOMEMLIMIT=256MiB` with 200 MiB allocated in Rust | `Stats().Live` within 1% of expected; `AdviseMemoryLimit` triggers GC |
| `musl` | matrix | linux (zig, `-musl`) | Static build, deep-recursion unit | Passes only because work runs on Rust threads with 8 MiB stacks |
| `cross` | matrix | linux | `cargo zigbuild` + `zig cc` for linux/arm64, darwin/arm64, windows/amd64-gnu | Binaries link; smoke test runs under QEMU for arm64 |
| `bench` | matrix | linux, macos | `go test -bench . -count=10 \| benchstat` vs `main` | `noescape` at 0 allocs/op; regression over 10% fails |
| `lint` | matrix | linux | `clippy -D warnings`, `gussetvet`, `staticcheck`, `govulncheck`, `cargo deny`, grep for `linkname`, `C.free`, `GODEBUG=` | Header diff clean; exports list matches `nm` |
| `asan` | nightly | linux | `RUSTFLAGS=-Zsanitizer=address cargo build` + `go test -asan` | Cross-free test fails as expected; everything else clean |
| `miri` | nightly | linux | `cargo miri test -p gusset --lib` (non-FFI modules) | Pool, header, alloc counting |
| `fuzz` | nightly | linux | `cargo fuzz run header -- -max_total_time=600`; Go `go test -fuzz=FuzzStatus` | Header decoder, status decoder |
| `tip` | tip | linux | `gotip` + Rust beta | Full `unit` + `soak`; failure opens an issue tagged `toolchain` |

Panic zoo cases (all must return `FFI_PANIC`, process alive, message and location populated): `&str` payload; `String` payload; non-string payload (`panic_any(42)`); NUL byte in message; panic inside the error's `Display` impl.

Property the whole suite proves: no test in this repo ever needs `GODEBUG=cgocheck=0` or `invalidptr=0`; CI greps for both and fails.

## Borrow and avoid

| Project | Borrow | Avoid |
| --- | --- | --- |
| rust2go (https://github.com/ihciah/rust2go) | The `drop_safe` idea for parameter ownership; precomputed buffers for nested types; its honesty about Go-side deep copies | Manual-assembly Go callbacks; `GODEBUG=invalidptr=0,cgocheck=0`; Rust-as-driver orientation |
| uniffi-bindgen-go (https://github.com/NordSecurity/uniffi-bindgen-go) | The interop target: Gusset's example shows generated bindings sitting on top of the runtime contract; their `RustBuffer` (ptr, len, capacity) shape | Dynamic loading default; tracking uniffi's version churn inside Gusset |
| purego (https://github.com/ebitengine/purego) | The `CGO_ENABLED=0` container story as a documented alternative for adopters who need it | Any dependency on it; runtime internals |
| Stoolap driver (https://stoolap.io/blog/2026/04/08/calling-a-rust-library-from-go-with-cgo-disabled/) | Their framing that the engine, not the boundary, dominates real workloads; benchmark discipline | `asmcgocall`; P pinned for the call; the `fakecgo` TLS shim |
| iceoryx2 (https://github.com/eclipse-iceoryx/iceoryx2) | Service discovery and lifecycle model for Phase 4; their tiered platform-support table as a README pattern | Reimplementing their transport |
| Gaultier's cgo notes (https://gaultier.github.io/blog/addressing_cgo_pains_one_at_a_time.html) | C in separate files so warnings work; `zig cc` static musl builds; getters instead of unions; `go build -a` when the archive changes | Inline C in Go comments |
| Arcjet (https://blog.arcjet.com/calling-rust-ffi-libraries-from-go/) | Wrapper crate over the real engine; `cbindgen` in `build.rs`; `cargo-zigbuild` | Their three-language maintenance drift; their wazero migration is the right call for them, not for GPU work |
| Tokio | Cancellation-by-flag instead of dropped futures across FFI; named worker threads | Running a Tokio runtime inside a cgo call (`block_on` trap) |
| Go `net` package | `os.Pipe` so waits park on the netpoller | Blocking reads on a raw fd |
| Hystrix / resilience4j | Bulkhead semantics for the semaphore; poison equals an open circuit | Retries at the boundary; the Rust side decides idempotency |

## Definition of done and hand-off

A phase is done when its checklist is ticked, its CI jobs are green on the supported floor and latest, and the hand-off note below is written into `CHANGELOG.md`.

| Phase | Done when | Hand-off note must state |
| --- | --- | --- |
| 0 Seed | The audited document's guard fails `panic_nul` in CI; hardened guard passes; `bench/results/` has one file per platform | Toolchain versions used; measured per-call and per-item numbers |
| 1 v0.1 | R1–R16 each have their enforcer; `matrix.yml` green on both floors; tessl/sparsl service runs on Gusset with before/after `benchstat` committed | Public surface list; any rule without an enforcer (there should be none) |
| 2 Second app | Second app adopted with zero public-surface changes, or the changes are logged in `DECISIONS.md` and both apps pass | Diff of the public surface between v0.1 and v0.2 |
| 3 Public | README numbers generated; uniffi-bindgen-go interop example builds; `cargo publish` and Go module tag; `cargo semver-checks` clean | Supported floors; known limitations (Windows completion path blocking) |
| 4 IPC | Upstream iceoryx2 Go binding PR merged or `gusset-ipc` adapter released; tessl behind the daemon survives an induced Metal fault | What the daemon supervisor does on crash; restart budget |

Hand-off checklist for any agent session:

- [ ] Read this file and `DECISIONS.md`; do not reopen a decided item without an entry
- [ ] Run `make ci-local` (unit, lint, bench smoke) before the first commit
- [ ] Every new failure mode becomes a `tests/pitfalls/` row and a catalogue row here
- [ ] Every number quoted comes from `bench/results/`
- [ ] End the session with `CHANGELOG.md` updated and the phase table above re-checked

## Sources

- Go 1.27 release notes (https://go.dev/doc/go1.27) and Go 1.26 release notes (https://go.dev/doc/go1.26): cgo overhead cut, Green Tea GC, heap base randomization, `/sched/threads:threads`, `goroutineleak`
- Rust release history (https://endoflife.date/rust): 1.98.1 stable as of 3 September 2026
- rust2go (https://github.com/ihciah/rust2go) and its design write-up (https://en.ihcblog.com/rust2go/)
- uniffi-bindgen-go (https://github.com/NordSecurity/uniffi-bindgen-go)
- iceoryx2 (https://github.com/eclipse-iceoryx/iceoryx2): bindings and platform tiers
- Stoolap: calling a Rust library from Go with cgo disabled (https://stoolap.io/blog/2026/04/08/calling-a-rust-library-from-go-with-cgo-disabled/)
- Addressing cgo pains, one at a time (https://gaultier.github.io/blog/addressing_cgo_pains_one_at_a_time.html)
- Arcjet: calling Rust FFI libraries from Go (https://blog.arcjet.com/calling-rust-ffi-libraries-from-go/)
- Seed measurements in `bench/seed/README.md`
