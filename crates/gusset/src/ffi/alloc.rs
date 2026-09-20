//! Allocator accounting wrapper and stats export (R4).
#![allow(unsafe_code)]

use std::alloc::{GlobalAlloc, Layout};
use std::sync::atomic::{AtomicUsize, Ordering};

/// Global allocator statistics for cross-language memory management.
#[repr(C)]
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct AllocStats {
    /// Currently allocated bytes across the Rust runtime.
    pub live_bytes: usize,
    /// Peak allocated bytes since process start.
    pub peak_bytes: usize,
    /// Total number of allocations made.
    pub alloc_count: usize,
}

static LIVE_BYTES: AtomicUsize = AtomicUsize::new(0);
static PEAK_BYTES: AtomicUsize = AtomicUsize::new(0);
static ALLOC_COUNT: AtomicUsize = AtomicUsize::new(0);

#[inline]
fn update_peak(current_live: usize) {
    let mut peak = PEAK_BYTES.load(Ordering::Relaxed);
    while current_live > peak {
        match PEAK_BYTES.compare_exchange_weak(
            peak,
            current_live,
            Ordering::Relaxed,
            Ordering::Relaxed,
        ) {
            Ok(_) => break,
            Err(actual) => peak = actual,
        }
    }
}

/// GlobalAlloc wrapper that tracks live bytes, peak bytes, and total allocations.
pub struct Counting<A> {
    inner: A,
}

impl<A> Counting<A> {
    /// Wraps an existing allocator with memory tracking.
    pub const fn new(inner: A) -> Self {
        Self { inner }
    }
}

unsafe impl<A: GlobalAlloc> GlobalAlloc for Counting<A> {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let size = layout.size();
        let ptr = unsafe { self.inner.alloc(layout) };
        if !ptr.is_null() {
            let live = LIVE_BYTES.fetch_add(size, Ordering::Relaxed) + size;
            update_peak(live);
            ALLOC_COUNT.fetch_add(1, Ordering::Relaxed);
        }
        ptr
    }

    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        let size = layout.size();
        unsafe { self.inner.dealloc(ptr, layout) };
        LIVE_BYTES.fetch_sub(size, Ordering::Relaxed);
    }

    unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
        let size = layout.size();
        let ptr = unsafe { self.inner.alloc_zeroed(layout) };
        if !ptr.is_null() {
            let live = LIVE_BYTES.fetch_add(size, Ordering::Relaxed) + size;
            update_peak(live);
            ALLOC_COUNT.fetch_add(1, Ordering::Relaxed);
        }
        ptr
    }

    unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
        let old_size = layout.size();
        let new_ptr = unsafe { self.inner.realloc(ptr, layout, new_size) };
        if !new_ptr.is_null() {
            if new_size > old_size {
                let diff = new_size - old_size;
                let live = LIVE_BYTES.fetch_add(diff, Ordering::Relaxed) + diff;
                update_peak(live);
            } else if old_size > new_size {
                let diff = old_size - new_size;
                LIVE_BYTES.fetch_sub(diff, Ordering::Relaxed);
            }
            ALLOC_COUNT.fetch_add(1, Ordering::Relaxed);
        }
        new_ptr
    }
}

/// Returns current allocator statistics without performing any allocations.
#[inline]
pub fn get_alloc_stats() -> AllocStats {
    AllocStats {
        live_bytes: LIVE_BYTES.load(Ordering::Relaxed),
        peak_bytes: PEAK_BYTES.load(Ordering::Relaxed),
        alloc_count: ALLOC_COUNT.load(Ordering::Relaxed),
    }
}

/// Manually records an allocation for test cases or manual buffers.
#[inline]
pub fn record_alloc(size: usize) {
    let live = LIVE_BYTES.fetch_add(size, Ordering::Relaxed) + size;
    update_peak(live);
    ALLOC_COUNT.fetch_add(1, Ordering::Relaxed);
}

/// Manually records a deallocation for test cases or manual buffers.
#[inline]
pub fn record_dealloc(size: usize) {
    LIVE_BYTES.fetch_sub(size, Ordering::Relaxed);
}
