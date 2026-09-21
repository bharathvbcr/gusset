//! Allocator accounting wrapper and stats export (R4).
#![allow(unsafe_code)]

use std::alloc::{GlobalAlloc, Layout};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

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

/// Set the first time a `Counting` wrapper services an allocation.
///
/// Gusset declares no `#[global_allocator]`, so `Stats()` would read zero for an
/// adopter who never installs `Counting`. `RawBuffer` therefore records its own
/// allocations by hand. When the adopter *does* install `Counting`, those same
/// bytes already went through it — `RawBuffer` allocates from the global allocator
/// — and counting them again reports every buffer at twice its size, which
/// `AdviseMemoryLimit` then subtracts twice from the Go heap budget.
///
/// A global allocator services the process's first allocation, long before any
/// buffer exists, so this flag is always settled by the time a manual record runs.
static COUNTING_ACTIVE: AtomicBool = AtomicBool::new(false);

#[inline]
fn mark_counting_active() {
    if !COUNTING_ACTIVE.load(Ordering::Relaxed) {
        COUNTING_ACTIVE.store(true, Ordering::Relaxed);
    }
}

/// Reports whether a `Counting` wrapper is installed as the global allocator.
#[inline]
pub fn counting_is_active() -> bool {
    COUNTING_ACTIVE.load(Ordering::Relaxed)
}

/// Adds `size` to the live counter without wrapping.
///
/// `fetch_add` plus `wrapping_add` turns a counter near `usize::MAX` into a
/// small total. `AdviseMemoryLimit` would then treat a saturated Rust heap as
/// nearly empty and raise the Go limit. Saturation sticks at the top.
#[inline]
fn add_live_bytes(size: usize) -> usize {
    if size == 0 {
        return LIVE_BYTES.load(Ordering::Relaxed);
    }
    let mut current = LIVE_BYTES.load(Ordering::Relaxed);
    loop {
        let next = current.saturating_add(size);
        match LIVE_BYTES.compare_exchange_weak(current, next, Ordering::Relaxed, Ordering::Relaxed)
        {
            Ok(_) => {
                update_peak(next);
                return next;
            }
            Err(actual) => current = actual,
        }
    }
}

/// Advances the allocation count, saturating at `usize::MAX`.
#[inline]
fn add_alloc_count() {
    let mut current = ALLOC_COUNT.load(Ordering::Relaxed);
    loop {
        let next = current.saturating_add(1);
        match ALLOC_COUNT.compare_exchange_weak(current, next, Ordering::Relaxed, Ordering::Relaxed)
        {
            Ok(_) => return,
            Err(actual) => current = actual,
        }
    }
}

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

#[inline]
fn dec_live_bytes(size: usize) {
    let mut current = LIVE_BYTES.load(Ordering::Relaxed);
    while let Err(actual) = LIVE_BYTES.compare_exchange_weak(
        current,
        current.saturating_sub(size),
        Ordering::Relaxed,
        Ordering::Relaxed,
    ) {
        current = actual;
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
        mark_counting_active();
        let size = layout.size();
        let ptr = unsafe { self.inner.alloc(layout) };
        if !ptr.is_null() {
            add_live_bytes(size);
            add_alloc_count();
        }
        ptr
    }

    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        mark_counting_active();
        let size = layout.size();
        unsafe { self.inner.dealloc(ptr, layout) };
        dec_live_bytes(size);
    }

    unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
        mark_counting_active();
        let size = layout.size();
        let ptr = unsafe { self.inner.alloc_zeroed(layout) };
        if !ptr.is_null() {
            add_live_bytes(size);
            add_alloc_count();
        }
        ptr
    }

    unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
        mark_counting_active();
        let old_size = layout.size();
        let new_ptr = unsafe { self.inner.realloc(ptr, layout, new_size) };
        if !new_ptr.is_null() {
            if new_size > old_size {
                add_live_bytes(new_size - old_size);
            } else if old_size > new_size {
                let diff = old_size - new_size;
                dec_live_bytes(diff);
            }
            add_alloc_count();
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

/// Records an allocation that bypassed the counting wrapper.
///
/// A no-op once `Counting` is installed, because the global allocator has already
/// counted these bytes. See [`COUNTING_ACTIVE`].
#[inline]
pub fn record_alloc(size: usize) {
    if counting_is_active() {
        return;
    }
    add_live_bytes(size);
    add_alloc_count();
}

/// Records a deallocation that bypassed the counting wrapper.
///
/// Paired with [`record_alloc`]: both consult the same flag, and the flag cannot
/// change between a buffer's allocation and its free, so the pair never goes
/// asymmetric and drives `live_bytes` toward a phantom balance.
#[inline]
pub fn record_dealloc(size: usize) {
    if counting_is_active() {
        return;
    }
    dec_live_bytes(size);
}

#[cfg(test)]
mod tests {
    use super::*;

    /// One test, not four: the counters are process globals, so parallel tests in
    /// this binary would read each other's deltas. Everything here is expressed as a
    /// delta for the same reason.
    #[test]
    fn accounting_is_balanced_saturating_and_peak_monotonic() {
        // Gusset installs no global allocator, so unless an adopter installed
        // Counting the manual recorders are live. If some other test in this binary
        // ever installs one, this test would be measuring nothing; assert instead of
        // silently passing.
        assert!(
            !counting_is_active(),
            "the library under test must not install a global allocator"
        );

        let before = get_alloc_stats();

        record_alloc(4096);
        let after_alloc = get_alloc_stats();
        assert_eq!(
            after_alloc.live_bytes,
            before.live_bytes + 4096,
            "an allocation must raise live bytes by exactly its size"
        );
        assert_eq!(
            after_alloc.alloc_count,
            before.alloc_count + 1,
            "allocation count must advance once per allocation"
        );
        assert!(
            after_alloc.peak_bytes >= after_alloc.live_bytes,
            "peak must never read below live"
        );

        record_dealloc(4096);
        let after_free = get_alloc_stats();
        assert_eq!(
            after_free.live_bytes, before.live_bytes,
            "alloc then free must return live bytes to where they started"
        );
        assert!(
            after_free.peak_bytes >= after_alloc.peak_bytes,
            "peak is a high-water mark and must not fall when memory is released"
        );

        // A free larger than the live total must saturate at zero rather than wrap.
        // `live_bytes` is a usize read by AdviseMemoryLimit as a signed budget; an
        // underflow there would subtract roughly 16 EiB from the Go heap limit.
        let live_now = get_alloc_stats().live_bytes;
        record_dealloc(live_now.saturating_add(1 << 20));
        assert_eq!(
            get_alloc_stats().live_bytes,
            0,
            "over-release must saturate at zero, never wrap around"
        );

        // `fetch_add` + `wrapping_add` turns a counter near usize::MAX into a
        // small live total. AdviseMemoryLimit then treats a saturated Rust heap
        // as nearly empty and raises the Go limit. Saturation must stick at
        // the top, and this probe must restore the globals before it returns:
        // they are process-wide and a later test reads the delta.
        let saved_live = LIVE_BYTES.swap(usize::MAX - 8, Ordering::Relaxed);
        let saved_peak = PEAK_BYTES.swap(0, Ordering::Relaxed);
        let saved_count = ALLOC_COUNT.load(Ordering::Relaxed);
        record_alloc(64);
        let saturated = LIVE_BYTES.load(Ordering::Relaxed);
        LIVE_BYTES.store(saved_live, Ordering::Relaxed);
        PEAK_BYTES.store(saved_peak.max(saved_live), Ordering::Relaxed);
        ALLOC_COUNT.store(saved_count, Ordering::Relaxed);
        assert_eq!(
            saturated,
            usize::MAX,
            "live bytes must saturate at usize::MAX; wrapping reports a tiny heap"
        );
    }
}
