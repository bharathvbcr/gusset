# Gusset

The runtime contract for running a Rust engine inside a Go service: panic firewall, bounded concurrency, deadlines, poisoned handles, ABI verification, allocator accounting, and the CI matrix that proves them.

Gusset is **not** a binding generator, **not** a cgo-free calling path, and **not** an IPC transport. It is the hardening plate between Go and Rust in production.

---

## Architecture

```
Go Application
       │
       ▼
Gusset Go Package (github.com/bharathvbcr/gusset)
 [Bounded Handles, Semaphore, Deadlines, Netpoller Completion]
       │
       ▼  (cgo with #cgo noescape and #cgo nocallback)
C ABI Boundary (internal/ffi/gusset.h)
       │
       ▼  (pub unsafe extern "C" - strictly 14 exports)
Gusset Rust Crate (crates/gusset)
 [Panic Firewall, 8 MiB Worker Threads, sigaltstack, Allocator Stats]
       │
       ▼
Adopter Engine (crates/gusset-example / tessl / sparsl)
```

---

## Core Invariants

1. **(I1) Memory Ownership:** Memory is freed by the allocator that created it via exported `*_free` functions. Go never calls `C.free` on Rust memory.
2. **(I2) Panic Firewall:** No Rust panic crosses the FFI boundary. A caught panic poisons the handle; subsequent calls fail fast with `ErrPoisoned`.
3. **(I3) Deadline & Cancellation:** Deadlines and cancellations are enforced inside Rust between work units using relative `timeout_ns` and per-job `AtomicBool` flags.
4. **(I4) Bounded Concurrency:** In-flight calls per handle never exceed the configured pool size. Callers park on the Go semaphore, never on an OS thread in cgo.
5. **(I5) Stack & Signal Safety:** Heavy work runs on Rust-spawned threads with 8 MiB explicit stack size and 64 KiB `sigaltstack`.
6. **(I6) ABI Verification:** Go `init()` verifies ABI version, struct sizes, and alignments against Rust before the process starts serving.

---

## The Public Surface

### 14 Exported Rust Functions
Gusset exports strictly 14 C ABI functions from `libgusset.a` (enforced by `tests/exports_match.rs`):
- `gusset_abi_layout(out)`: Layout and struct size/alignment verification.
- `gusset_init()`: Global runtime initialization and panic hook installation.
- `gusset_shutdown(drain_ms)`: Graceful shutdown and worker drain.
- `gusset_handle_open(pool_size, pipe_write_fd, out_handle, status)`: Opens bounded worker pool.
- `gusset_handle_close(handle, status)`: Closes handle, cancels tasks, and closes write fd.
- `gusset_submit(handle, header, input_ptr, input_len, buffer_id, out_ticket, status)`: Submits work unit.
- `gusset_take(handle, ticket, out_buf_id, out_ptr, out_len, status)`: Moves result out once.
- `gusset_cancel(handle, ticket, status)`: Cancels specific job ticket.
- `gusset_cancel_all(handle, status)`: Cancels all pending jobs on handle.
- `gusset_status_free(status)`: Frees Rust-allocated error message.
- `gusset_alloc_stats(out)`: Non-allocating allocator accounting export.
- `gusset_drain_logs(buf, len, out_written)`: Drains log ring buffer.
- `gusset_buf_alloc(handle, len, out_id, out_ptr, status)`: Allocates 64-byte aligned Rust buffer.
- `gusset_buf_free(handle, id, status)`: Frees Rust-owned buffer.

### 10 Go Public Entry Points
- `gusset.Open(opts ...Option) (*Handle, error)`
- `(*Handle).Close() error`
- `(*Handle).Call(ctx context.Context, in []byte) ([]byte, error)`
- `(*Handle).Submit(ctx context.Context, in any) (uint64, error)`
- `(*Handle).Wait(ctx context.Context, ticket uint64) ([]byte, error)`
- `(*Handle).NewBuffer(n int) (*Buffer, error)` (with `(*Buffer).Free() error`)
- `gusset.Stats() AllocStats`
- `gusset.AdviseMemoryLimit(total int64) int64`
- `gusset.Threads() int64`
- `gusset.DrainLogs(buf []byte) int`

---

## Benchmark Results

Measured on `darwin/arm64` (Apple M5 Pro, Go 1.27.1, Rust 1.98.0). Committed in `bench/results/darwin-arm64-go1.27.1-rust1.98.0.txt`:

| Benchmark | Latency / Throughput | Allocations |
| :--- | :--- | :--- |
| **Gusset Call Parallel** | **5.07 µs / op** | 161 B/op (3 allocs/op) |
| **Gusset Call No-op** | **20.38 µs / op** | 161 B/op (3 allocs/op) |
| **Gusset Submit & Wait** | **23.68 µs / op** | 160 B/op (2 allocs/op) |
| **Large Buffer (64 KiB)** | **32.19 µs / op** | 65,696 B/op (3 allocs/op) |
| **Channel Hop Baseline** | **18.32 ns / op** | **0 B/op (0 allocs/op)** |

---

## Panic Zoo & Firewall Guarantees

The audited document's firewall contained a fatal vulnerability: converting panic messages via `CString::new(msg).unwrap()`. A panic message with an embedded NUL byte panicked a second time inside the unwinding path, causing an uncatchable `SIGABRT` that destroyed the Go process.

Gusset's panic firewall passes all 5 cases in `tests/panic_zoo/`:
1. `&str` panic payload
2. `String` panic payload
3. Non-string arbitrary payload (`panic_any(42)`)
4. **Embedded NUL byte** in panic message (survives without SIGABRT)
5. Panic inside the error's `Display` implementation

---

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/bharathvbcr/gusset"
)

func main() {
	// 1. Open bounded handle with 4 worker threads
	h, err := gusset.Open(gusset.WithPoolSize(4))
	if err != nil {
		panic(err)
	}
	defer h.Close()

	// 2. Execute call with deadline
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result, err := h.Call(ctx, []byte{0, 1, 2, 3})
	if err != nil {
		fmt.Printf("Call error: %v\n", err)
		return
	}

	fmt.Printf("Call result: %v\n", result)
}
```

---

## License

`MIT OR Apache-2.0`
