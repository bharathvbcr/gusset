//! Allocator accounting wrapper and stats export.

pub use crate::ffi::alloc::{
    count_buffer_alloc, count_buffer_dealloc, get_alloc_stats, record_alloc, record_dealloc,
    AllocStats, BufferAlloc, Counting, ALLOCATOR_API, BUFFER_ALIGN,
};
