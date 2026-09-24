//! Allocator accounting wrapper and stats export.

pub use crate::ffi::alloc::{
    get_alloc_stats, record_alloc, record_dealloc, AllocStats, BufferAlloc, Counting,
    ALLOCATOR_API, BUFFER_ALIGN,
};
