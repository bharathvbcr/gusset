//! Gusset: The Go-Rust runtime contract.
//!
//! Provides the panic firewall, bounded concurrency, deadlines, poisoned handles,
//! ABI layout verification, allocator accounting, and cross-boundary signal safety.

#![deny(
    unsafe_code,
    unsafe_op_in_unsafe_fn,
    improper_ctypes_definitions,
    missing_docs
)]

// Supported platform set, enforced rather than described.
//
// The completion path is a POSIX pipe, I5's signal protection is `sigaltstack`,
// and the worker stack read-back is pthread. None of the three has a Windows
// counterpart in this crate, so a Windows build would fail deep inside `pool::sys`
// with errors about `libc::sigaltstack` and leave an adopter guessing whether the
// port was intended. Failing here says what is actually true: Gusset is unix-only
// today, and `docs/platforms.md` records what a Windows port would have to add.
#[cfg(not(unix))]
compile_error!(
    "gusset supports unix targets only (linux-gnu, linux-musl, macos). The completion \
     pipe, sigaltstack and pthread stack accounting have no Windows implementation; \
     see docs/platforms.md."
);

pub mod alloc;
pub mod ffi;
pub mod header;
pub mod pool;

// Convenient re-exports
pub use alloc::{get_alloc_stats, AllocStats, Counting};
pub use ffi::status::{FfiStatus, FFI_BAD_ARG, FFI_ERR, FFI_OK, FFI_PANIC, FFI_POISONED};
pub use ffi::{gusset_abi_layout, AbiLayout, GUSSET_ABI_VERSION};
pub use header::{CallHeader, CancelReason, JobContext};
pub use pool::{set_engine_handler, Handle};
