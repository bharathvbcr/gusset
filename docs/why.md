# Why Gusset is Needed: The Runtime Impedance Mismatch

"Bindings exist" is not "this runs in production without taking the Go process down."

Foreign Function Interface (FFI) generators like `cbindgen`, `cgo`, and `uniffi-bindgen-go` solve **type marshalling** and **function calling conventions**. They do not solve—and explicitly do not own—what happens when Go and Rust disagree on thread management, signal handling, memory limits, panic recovery, stack sizing, and monotonic clocks inside a single process.

Gusset is the hardened plate between the two runtimes in production.

---

## 1. The Naive FFI Illusion

In a development environment, calling a Rust function from Go through cgo looks straightforward:

```go
// Naive FFI call
result := C.my_rust_engine_process(ptr, len)
```

In production under load, this naive boundary crashes the service across six distinct failure modes:

```mermaid
flowchart TD
    subgraph Naive ["Naive cgo / FFI Binding Under Production Load"]
        N1["Rust Panic!"] -->|"Unwinds across extern 'C'"| F1["SIGABRT: Process Crashed"]
        N2["1,000 Concurrent Calls"] -->|"Goroutines pin OS threads (P handoff)"| F2["Thread Exhaustion: Fatal Error >10,000 Threads"]
        N3["Containerized Execution (musl)"] -->|"Worker runs on 128 KiB pthread stack"| F3["SIGSEGV: Stack Overflow Crash"]
        N4["context.WithTimeout Expiration"] -->|"Goroutine blocked inside cgo"| F4["Uninterruptible Hang: Request Leaks"]
        N5["Large Rust Allocations"] -->|"GOMEMLIMIT blind to Rust heap"| F5["OOM Killer (SIGKILL from cgroups)"]
        N6["Panic Midway in Engine"] -->|"Next caller re-uses corrupted state"| F6["Mutex Poisoning & Cascading Failures"]
    end

    subgraph Gusset ["Gusset Runtime Contract"]
        G1["ffi_guard: catch_unwind + Safe FfiStatus"] --> S1["Panic Caught: Process Stays Alive"]
        G2["Go Semaphore + Netpoller Pipe Completion"] --> S2["Thread Cap Bounded (<= PoolSize <= 1024)"]
        G3["Rust Worker Pool with Explicit 8 MiB Stacks"] --> S3["Deterministic Stack & sigaltstack Protection"]
        G4["Relative timeout_ns + AtomicBool Flag"] --> S4["Cooperative Cancellation between Work Units"]
        G5["Counting Allocator + AdviseMemoryLimit"] --> S5["Go GC Triggered Before Container OOM"]
        G6["Bulkhead Pattern: Permanent Handle Poisoning"] --> S6["Immediate ErrPoisoned Fast-Fail"]
    end
```

---

## 2. The Six Runtime Collisions

### Collision 1: Failure Models & Double-Panic SIGABRT (Invariant I2)

**The Problem:**
- In Rust 1.81+, a panic escaping an `extern "C"` boundary causes an immediate, uncatchable `std::process::abort()`.
- Standard Rust code attempts to catch this with `std::panic::catch_unwind(|| ...)`.
- The catastrophic vulnerability lies in converting the panic message into an FFI-compatible string. Common implementations use `CString::new(msg).unwrap()`. If the panic payload contains an embedded `NUL` byte (e.g. raw binary bytes, slice fragments, or malformed UTF-8), `CString::new` returns an `Err`, causing `.unwrap()` to panic a *second* time inside the unwinding landing pad.
- A panic during panic unwinding is fatal: the operating system immediately aborts the process with `SIGABRT`. The entire Go backend goes down.

**How Gusset Solves It:**
- Rule R3 forbids `CString::new`, `unwrap`, and `expect` in the boundary crate.
- `ffi_guard` null-checks pointers, transfers messages as raw `(ptr, len)` without NUL-termination, and wraps error `Display` formatting in its own nested `catch_unwind`.
- Verified by Gusset's 5-case Panic Zoo suite (`tests/panic_zoo/`), which tests `&str`, `String`, non-string payloads (`panic_any(42)`), panic inside `Display`, and embedded NUL bytes.

---

### Collision 2: Concurrency & Thread Exhaustion (Invariant I4)

**The Problem:**
- Go goroutines are lightweight M:N green threads (millions can exist concurrently).
- To the Go scheduler, a cgo call is treated as a blocking system call. The operating system thread (M) is pinned to the goroutine. If the call does not return within ~20 µs, the scheduler hands off the logical processor (P) and spawns or wakes a new OS thread to service remaining goroutines.
- If a burst of 1,000 goroutines calls a Rust engine simultaneously (e.g. incoming HTTP/gRPC requests), the Go runtime spawns 1,000 OS threads.
- When the thread count reaches Go's hard limit (default 10,000 threads), the Go runtime fatally terminates the process:
  ```
  fatal error: runtime: program exceeds 10000-thread limit
  ```

```mermaid
sequenceDiagram
    autonumber
    participant HTTP as 1,000 Inbound Requests
    participant GoSched as Go Runtime Scheduler
    participant OS as OS Threads (M)
    participant Rust as Native CGO Call

    rect rgb(255, 235, 235)
        Note over HTTP,Rust: Naive cgo: Thread Storm
        HTTP->>GoSched: 1,000 Concurrent Goroutines
        GoSched->>OS: Pin M0..M999 to foreign calls
        OS->>Rust: Block in native execution (>20 µs)
        GoSched->>OS: Scheduler hands off P, creating new OS threads
        Note over OS: Thread count hits 10,000 ceiling
        OS-->>HTTP: fatal error: program exceeds 10000-thread limit
    end

    rect rgb(235, 255, 235)
        Note over HTTP,Rust: Gusset: Bounded Concurrency
        HTTP->>GoSched: 1,000 Concurrent Goroutines
        GoSched->>GoSched: Park on Go Semaphore (permit queue)
        GoSched->>Rust: Submit work unit to bounded pool (e.g. 4 workers)
        Rust-->>GoSched: Return immediately (submit < 5 µs)
        Note over GoSched: Callers wait on netpoller pipe (OS threads stay <= 10)
    end
```

**How Gusset Solves It:**
- Callers acquire a permit from a Go channel semaphore before entering cgo.
- Bounded concurrency: In-flight calls per handle can never exceed the configured pool size, capped at `gusset.MaxPoolSize` (1024). Requests exceeding the ceiling are refused, not clamped.
- Submissions are strictly non-blocking (`gusset_submit` copies or references the input and returns a ticket ID in `< 5 µs`, well below the 20 µs P-handoff threshold).
- Waiters park on Go's Netpoller via an `os.Pipe`, consuming 0 OS threads while awaiting completion.
- Verified by the soak test (`TestSoak_ThreadCapUnderTenThousandCalls`), asserting that `/sched/threads:threads` stays under `pool_size + GOMAXPROCS + 8`.

---

### Collision 3: Stack Sizing & The musl 128 KiB Trap (Invariant I5, Rule R8)

**The Problem:**
- Go goroutines run on dynamic, growable stacks that start at 2 KiB and expand on demand.
- A cgo call switches the goroutine to the host OS thread stack.
- On glibc (standard Linux) and macOS, default pthread stacks are 8 MiB.
- However, **`musl libc`** (the standard C library in Alpine Linux, used in millions of containerized Go deployments) specifies a default thread stack of only **128 KiB**!
- Any non-trivial Rust engine (parsers, AST traversal, deep recursion, regex compilation, or large array allocations) will exhaust 128 KiB in microseconds.
- Because static libraries do not run `std::rt::init`, Rust's stack overflow handler is absent. The process dies instantly with `SIGSEGV` before any Go recovery can intercept it.

**How Gusset Solves It:**
- Heavy work never runs on the cgo caller's thread stack.
- Gusset handles open a dedicated worker pool where every thread is spawned explicitly with `thread::Builder::new().stack_size(8 * 1024 * 1024)` (8 MiB).
- Each worker thread installs a 64 KiB `sigaltstack` in its entry function so stack overflows trigger Go's signal handler cleanly rather than crashing the thread silently.
- Verified by:
  1. `pool::sys::current_thread_stack_size`: runtime inspection on every platform that proves Gusset sized the stack rather than relying on OS defaults.
  2. Dedicated Alpine Linux (`musl`) CI container running the deep-recursion test suite.

---

### Collision 4: Deadlines, Cancellation & Monotonic Clocks (Invariant I3, Rule R9)

**The Problem:**
- In Go, cancellations are propagated cooperatively through `context.Context`.
- A goroutine blocked inside foreign C/Rust code **cannot be cancelled or preempted from Go**. If a Rust job takes 30 seconds and the HTTP client timed out after 500 ms, the server continues wasting CPU cycles until completion.
- Furthermore, monotonic clocks do not cross language boundaries:
  - Go's `time.Now()` monotonic reading is an internal process offset that is not wall-clock time.
  - macOS monotonic time uses `mach_absolute_time()` with Mach timebase conversions.
  - Linux uses `CLOCK_MONOTONIC`.
  - Comparing a Go monotonic timestamp to a Rust `std::time::Instant` produces garbage results.

**How Gusset Solves It:**
- Submit calls compute a **relative** `timeout_ns` from `ctx.Deadline() - time.Now()` at the exact moment of submission.
- The 40-byte `CallHeader` carries `timeout_ns`. Rust reads this relative offset and instantiates its own `Instant::now() + Duration::from_nanos(timeout_ns)`.
- Each job owns an `AtomicBool` cancellation flag in Rust memory. If the Go context expires, Go executes `gusset_cancel(handle, ticket)` (a sub-microsecond call) to flip the flag.
- The adopter engine checks `ctx.check()` between work units. If either the relative deadline passed or the cancellation flag was set, the engine halts immediately.
- Rust never reads Go-owned memory, avoiding cross-language memory model races.

---

### Collision 5: Two Heaps, One Memory Limit (Invariant I1, Rules R6 & R16)

**The Problem:**
- In Kubernetes, containers are assigned hard memory limits (e.g. 4 GiB). If memory exceeds the limit, the Linux OOM-killer immediately issues `SIGKILL` to the process.
- Go applications use `GOMEMLIMIT` (or `debug.SetMemoryLimit`) to ensure the garbage collector triggers aggressively before the container limit is reached.
- **Go's GC cannot see Rust allocations.** If Go has allocated 1 GiB and Rust allocates 2.8 GiB, Go believes it is well within its budget and delays GC. The container is OOM-killed while Go was completely idle.
- Furthermore, passing Go pointers to Rust that outlive the call violates Go's runtime pointer-passing contract (R6), resulting in unrecoverable `cgocheck` panics.

**How Gusset Solves It:**
- **Dual-Path Memory Model (R6/R16):** Small inputs (`<= 4 KiB`) are copied during `gusset_submit`. Larger payloads are allocated in 64-byte aligned Rust memory via `gusset_buf_alloc` and wrapped in Go as `[]byte` via `unsafe.Slice`. Go pointers never outlive the FFI call.
- **Allocator Accounting:** Gusset provides `gusset::alloc::Counting<A>`, a non-allocating wrapper around the global allocator that tracks live and peak bytes with relaxed atomics.
- **Dynamic Limit Advisory:** Go exposes `gusset.AdviseMemoryLimit(containerBudget)`, which reads live Rust memory and updates Go's `debug.SetMemoryLimit(max(budget - rustLive, floor))`, ensuring Go collects garbage proactively before the container limit is breached.

---

### Collision 6: Handle Poisoning & Mutex State Corruption (Invariant I2, Rule R10)

**The Problem:**
- If a Rust function panics midway through updating an in-memory B-tree, graph, or tensor buffer, internal state invariants are broken and standard library `Mutex` locks become poisoned.
- If the Go service simply catches the panic and continues submitting new requests to that same engine instance, subsequent calls either crash on poisoned locks (`PoisonError`), produce corrupted data, or deadlock.

**How Gusset Solves It:**
- Gusset applies the **Bulkhead Pattern** from distributed systems to in-process memory.
- If any job on a handle returns `FFI_PANIC`, the handle's atomic poison flag is latched permanently.
- All subsequent calls to that handle fail-fast with `ErrPoisoned` in Go **without ever entering native code**.
- The only remedy is for the Go caller to explicitly `Close()` the poisoned handle and open a fresh one, ensuring no corrupted state survives.

---

## 3. Comparison Matrix

| Failure Mode | Naive cgo / bindgen | uniffi-bindgen-go | rust2go | Gusset Runtime Contract |
| :--- | :--- | :--- | :--- | :--- |
| **Rust Panic Recovery** | `SIGABRT` / Crash | `catch_unwind` (vulnerable to NUL-byte abort) | `catch_unwind` | **Guaranteed**: 5-case Panic Zoo verified; `FfiStatus` ptr+len |
| **Thread Scaling** | Unbounded OS thread spawning | Unbounded OS thread spawning | Async, but disables `cgocheck` | **Bounded**: Go semaphore queue (`<= pool_size <= 1024`) |
| **musl / Docker Stack** | Crash on 128 KiB default | Crash on 128 KiB default | Dependent on host stack | **Guaranteed**: Explicit 8 MiB worker stack + `sigaltstack` |
| **Deadline Cancellation** | Hangs until native return | Hangs until native return | Async callback | **Cooperative**: Relative `timeout_ns` + `AtomicBool` flag |
| **Memory Accounting** | Go blind to Rust heap | Go blind to Rust heap | Pre-computed buffers | **Integrated**: `Counting<A>` + `AdviseMemoryLimit` |
| **GC Pointer Safety** | Easy to violate R6 | Manages copies | Forces `GODEBUG=cgocheck=0` | **Strict**: Dual-path buffer model (R6 & R16 verified) |
| **State Corruption** | Cascades or deadlocks | Unhandled | Unhandled | **Bulkhead**: Atomic handle poisoning (`ErrPoisoned`) |
| **ABI Drift Detection** | Undetected heap corruption | Checked via hash | Unchecked | **ABI v2**: Verifies version, sizes & alignments at `init()` |

---

## 4. When You Need Gusset

### You Need Gusset When:
1. **You run high-performance Rust in a Go service**: Search engines, regex/glob matching, vector indexing, parsers, cryptographic verification, compression, or ML inference embedded directly in Go HTTP/gRPC services.
2. **You deploy to containers (Docker/Kubernetes)**: You require protection against musl's 128 KiB thread stack trap and container OOM-killer termination under tight memory budgets.
3. **You serve concurrent traffic**: You cannot allow a burst of inbound requests to spawn thousands of OS threads and crash Go with `program exceeds 10000-thread limit`.
4. **Reliability is non-negotiable**: A panic in foreign code must degrade gracefully to a clean error response rather than terminating the entire binary.

### You Do Not Need Gusset When:
1. **Stateless math operations**: Trivial, microsecond C calculations that never allocate, never block, never recurse, and never panic.
2. **Separate microservices**: Workloads where network latency (`> 500 µs`) is acceptable and components run in separate processes communicating via gRPC, HTTP, or Unix domain sockets.
3. **Pure Go implementations**: Where Go's native standard library or third-party packages already meet performance requirements.
