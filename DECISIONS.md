# Decisions

Append-only. An agent does not reopen a decided item without a new entry here.

| Date | Decision | Choice | Why | Rejected |
| --- | --- | --- | --- | --- |
| 2026-09-20 | Allocator | Gusset declares no `#[global_allocator]`. It ships `gusset::alloc::Counting<A>`, a wrapper the app installs around whatever allocator it already uses, plus `gusset_alloc_stats()` exporting live bytes and peak | Only one global allocator per binary; a framework that claims it cannot be adopted by apps that already picked mimalloc or jemalloc | `mimalloc` feature flag (kept only in the example engine) |
| 2026-09-20 | Completion wakeup | Go creates the pipe with `os.Pipe()` and hands the write fd to `gusset_handle_open`; a worker writes the 8-byte ticket id when a job finishes; the reader goroutine parks on the netpoller. Windows v0.1 uses the blocking call under the semaphore | `os.Pipe` is pollable with no runtime internals; writes of at most `PIPE_BUF` bytes are atomic; bounded in-flight means the pipe never fills | `eventfd` (counter, no ticket ids), kqueue user events (macOS-only), Go callbacks from Rust threads |
| 2026-09-20 | License | `MIT OR Apache-2.0` | Rust ecosystem default, superset of the Apache-2.0 used by sparsl and tessl | Apache-2.0 alone |
| 2026-09-20 | Out-of-process (Phase 4) | Contribute the Go binding upstream to iceoryx2; `gusset-ipc` is a thin adapter, not a transport | Distribution and maintenance from an existing project | Standalone shm bus |
| 2026-09-20 | Version floor | Latest-minus-one for Go and Rust, bumped on a schedule | Develop on latest and nightly; adopters lag by one release at most | Fixed MSRV years back |
| 2026-09-20 | Input ownership | Inputs up to 4 KiB copied during `gusset_submit`; larger inputs in Rust-owned `Buffer`s filled by Go | Submit-and-return plus `#cgo noescape` forbids Rust touching Go memory after the call returns | Pinning Go memory for the job's lifetime |
| 2026-09-20 | Cancellation | Per-job `AtomicBool` in Rust memory, set by `gusset_cancel(handle, ticket)`; `gusset_cancel_all` on `Close` | Rust may never read a Go-owned atomic; per-job matches `context.Context` semantics | Generation counter on the Go handle; absolute deadlines |
| 2026-09-20 | Clock domain | Header carries relative `timeout_ns` computed at submit | Go's monotonic reading is process-internal and macOS/Linux sources differ from Rust `Instant` | Absolute `deadline_ns` |
| 2026-09-20 | Worker pool scope | One pool per handle, sized at `gusset_handle_open` | Engines differ in parallelism; per-handle keeps the semaphore and pool the same size | Process-global pool |
| 2026-09-20 | ABI surface | 14 exports, three `#[repr(C)]` types, one `gusset_abi_layout()` for version/size/align | Small enough to list in `exports.txt` and diff against `nm` | Per-type `gusset_sizeof_*` functions |
