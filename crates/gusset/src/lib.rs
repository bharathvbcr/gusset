//! Gusset: The Go-Rust runtime contract.
//!
//! Gusset provides the in-process runtime contract and hardening plate for running
//! native Rust engines inside Go services in production.
//!
//! It owns the zero-panic firewall (`catch_unwind` with embedded NUL safety),
//! bounded concurrency worker pool (explicit 8 MiB stacks, `sigaltstack` protection,
//! and SIGPIPE blocking), cooperative deadline and cancellation propagation,
//! poisoned handle state machine, ABI layout and named-field offset verification,
//! non-allocating allocator accounting (`Counting` and `AllocStats`), and
//! completion notification (shared-memory completion ring with non-blocking pipe wake tokens).
//!
//! Gusset is unix-only by construction (linux-gnu, linux-musl, macos darwin).

#![deny(
    unsafe_code,
    unsafe_op_in_unsafe_fn,
    improper_ctypes_definitions,
    missing_docs
)]

// Supported platform set, enforced rather than described.
//
// The completion path is a POSIX pipe, I5's signal protection is `sigaltstack`,
// and the worker stack read-back is pthread. Windows has none of the three.
// FreeBSD, OpenBSD and illumos are `unix` and do not provide `pthread_getattr_np`,
// so `cfg(not(unix))` never fired and `pool::sys` failed with E0425 instead.
// The gate is an allow-list (linux covers gnu and musl). The modules are cfg'd
// out with it: rustc still typechecks a crate that only also has `compile_error!`.
#[cfg(not(any(target_os = "linux", target_os = "macos")))]
compile_error!(
    "gusset supports Linux and macOS only (linux-gnu, linux-musl, macos); other targets, \
     including Windows and the other unix systems, are refused. The completion pipe, \
     sigaltstack and pthread stack accounting have no implementation there; \
     see docs/platforms.md."
);

#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod alloc;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod ffi;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod header;
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub mod pool;

// Convenient re-exports
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub use alloc::{get_alloc_stats, AllocStats, BufferAlloc, Counting, ALLOCATOR_API, BUFFER_ALIGN};
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub use ffi::status::{FfiStatus, FFI_BAD_ARG, FFI_ERR, FFI_OK, FFI_PANIC, FFI_POISONED};
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub use ffi::{gusset_abi_layout, AbiLayout, GUSSET_ABI_VERSION};
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub use header::{CallHeader, CancelReason, JobContext};
#[cfg(any(target_os = "linux", target_os = "macos"))]
pub use pool::{clear_engine_handlers, register_engine, set_engine_handler, Handle, JobOutput};
