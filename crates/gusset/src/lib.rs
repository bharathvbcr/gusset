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
