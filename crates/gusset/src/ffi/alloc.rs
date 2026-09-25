//! Allocator accounting wrapper and stats export (R4).
#![allow(unsafe_code)]

use std::alloc::{GlobalAlloc, Layout};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

#[cfg(gusset_allocator_api)]
use std::alloc::{AllocError, Allocator, Global, System};
#[cfg(gusset_allocator_api)]
use std::ptr::NonNull;

/// Whether this build of Gusset implements the stable `std::alloc::Allocator`
/// trait (Rust 1.100 and newer), making [`BufferAlloc`] usable with `Vec::new_in`
/// and `JobOutput::Allocated` available.
///
/// Decided at build time by probing the compiler, not by comparing versions; see
/// `crates/gusset/allocator_probe.rs`. On older toolchains everything else in the
/// crate is unchanged.
pub const ALLOCATOR_API: bool = cfg!(gusset_allocator_api);

/// Alignment of every Rust-owned buffer Go can see (R16).
///
/// `NewBuffer`, promoted results and [`BufferAlloc`] output all share it, so a Go
/// caller may rely on 64-byte alignment whichever path produced the buffer.
pub const BUFFER_ALIGN: usize = 64;

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
///
/// Inferred: the flag is set by the first allocation any `Counting` serves
/// through `GlobalAlloc`, so a non-global `Counting` called through
/// `GlobalAlloc` sets it too. Only [`record_alloc`]/[`record_dealloc`] consult
/// it; Gusset's own buffers never do (see [`count_buffer_alloc`]).
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

/// Records `size` freshly allocated bytes that bypassed the global allocator.
///
/// Unlike [`record_alloc`], never skipped: the caller knows these bytes did not
/// pass through an installed `Counting`, so nothing else will count them.
#[cfg(gusset_allocator_api)]
#[inline]
fn count_alloc_always(size: usize) {
    if size != 0 {
        add_live_bytes(size);
        add_alloc_count();
    }
}

/// Moves the live total from `old` to `new` bytes after a resize, with the same
/// no-op rule as [`record_alloc`] when `always` is false.
#[cfg(gusset_allocator_api)]
#[inline]
fn count_resize(old: usize, new: usize, always: bool) {
    if !always && counting_is_active() {
        return;
    }
    if new > old {
        add_live_bytes(new - old);
    } else if old > new {
        dec_live_bytes(old - new);
    }
    add_alloc_count();
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

/// Whether `A` is `std::alloc::Global`, whose allocations already pass through
/// an installed `Counting` global allocator.
#[cfg(gusset_allocator_api)]
#[inline]
fn is_global<A: 'static>() -> bool {
    std::any::TypeId::of::<A>() == std::any::TypeId::of::<Global>()
}

/// Whether `A` is [`BufferAlloc`], which counts every byte itself (by hand, or
/// through an installed `Counting` global allocator). Wrapping it in `Counting`
/// used to add the same bytes a second time.
#[cfg(gusset_allocator_api)]
#[inline]
fn counts_itself<A: 'static>() -> bool {
    std::any::TypeId::of::<A>() == std::any::TypeId::of::<BufferAlloc>()
}

/// `Counting` as a per-collection allocator (`Vec::new_in(Counting::new(System))`).
///
/// Counts into the same process-wide totals `gusset_alloc_stats` exports, so
/// memory an engine keeps in a local arena or a `System`-backed collection is
/// visible to `AdviseMemoryLimit` even though it never touches the global
/// allocator.
///
/// Wrapping `Global` while `Counting` is also the `#[global_allocator]` would count
/// every byte twice, as would wrapping [`BufferAlloc`] (which counts itself); both
/// cases are detected and forwarded uncounted. Any other
/// allocator that itself routes to the global allocator has the same problem and
/// cannot be detected — wrap `System`, an arena, or a pool, not a proxy for
/// `Global`.
///
/// Sizes are counted as the layouts the caller passes. A caller that deallocates
/// with a larger size than it requested (the Allocator contract allows up to the
/// returned block length) leaves the live total low, never wrapped: the
/// subtraction saturates.
#[cfg(gusset_allocator_api)]
unsafe impl<A: Allocator + 'static> Allocator for Counting<A> {
    fn allocate(&self, layout: Layout) -> Result<NonNull<[u8]>, AllocError> {
        let block = self.inner.allocate(layout)?;
        self.count_new(layout.size());
        Ok(block)
    }

    fn allocate_zeroed(&self, layout: Layout) -> Result<NonNull<[u8]>, AllocError> {
        let block = self.inner.allocate_zeroed(layout)?;
        self.count_new(layout.size());
        Ok(block)
    }

    unsafe fn deallocate(&self, ptr: NonNull<u8>, layout: Layout) {
        unsafe { self.inner.deallocate(ptr, layout) };
        if !self.forwards_to_counted_global() {
            dec_live_bytes(layout.size());
        }
    }

    unsafe fn grow(
        &self,
        ptr: NonNull<u8>,
        old: Layout,
        new: Layout,
    ) -> Result<NonNull<[u8]>, AllocError> {
        let block = unsafe { self.inner.grow(ptr, old, new) }?;
        self.count_moved(old.size(), new.size());
        Ok(block)
    }

    unsafe fn grow_zeroed(
        &self,
        ptr: NonNull<u8>,
        old: Layout,
        new: Layout,
    ) -> Result<NonNull<[u8]>, AllocError> {
        let block = unsafe { self.inner.grow_zeroed(ptr, old, new) }?;
        self.count_moved(old.size(), new.size());
        Ok(block)
    }

    unsafe fn shrink(
        &self,
        ptr: NonNull<u8>,
        old: Layout,
        new: Layout,
    ) -> Result<NonNull<[u8]>, AllocError> {
        let block = unsafe { self.inner.shrink(ptr, old, new) }?;
        self.count_moved(old.size(), new.size());
        Ok(block)
    }
}

#[cfg(gusset_allocator_api)]
impl<A: 'static> Counting<A> {
    #[inline]
    fn forwards_to_counted_global(&self) -> bool {
        counts_itself::<A>() || (is_global::<A>() && counting_is_active())
    }

    #[inline]
    fn count_new(&self, size: usize) {
        if !self.forwards_to_counted_global() {
            count_alloc_always(size);
        }
    }

    #[inline]
    fn count_moved(&self, old: usize, new: usize) {
        if !self.forwards_to_counted_global() {
            count_resize(old, new, true);
        }
    }
}

/// The allocator behind every Rust-owned buffer Go can see (R16).
///
/// With the stable `Allocator` trait (Rust 1.100+, see [`ALLOCATOR_API`]) an engine
/// builds its output directly in this memory and returns it without a copy:
///
/// ```ignore
/// let mut out: Vec<u8, BufferAlloc> = Vec::new_in(BufferAlloc);
/// out.try_reserve(n).map_err(|e| e.to_string())?;
/// out.extend_from_slice(&header);
/// Ok(JobOutput::from(out))
/// ```
///
/// Gusset adopts that allocation as the result buffer: no `memcpy` on the worker,
/// none on the cgo thread, and the pointer Go reads is the one the engine wrote.
/// A plain `Vec<u8>` result above 4 KiB is copied into a buffer instead, because
/// its memory is not 64-byte aligned and was not allocated with the layout the
/// buffer registry frees with.
///
/// Every block is 64-byte aligned ([`BUFFER_ALIGN`]) whatever the requested
/// alignment, comes from `System` (not the global allocator), and is counted in
/// [`get_alloc_stats`] exactly once, by Gusset, whether or not a `Counting`
/// global allocator is installed.
///
/// Growth that fails reports `AllocError`. Use `try_reserve` for sizes derived
/// from input: an infallible `Vec::push` that cannot allocate aborts the whole
/// process, Go included, and no panic firewall can intercept that. The 1 GiB
/// buffer ceiling is enforced when the output is adopted, not here, for the same
/// reason — refusing inside the allocator would turn an oversized result into an
/// abort instead of an error.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Hash)]
pub struct BufferAlloc;

impl BufferAlloc {
    /// The layout this allocator really uses for `layout`: same size, alignment
    /// raised to [`BUFFER_ALIGN`]. `None` when rounding would overflow `isize`.
    #[inline]
    pub fn buffer_layout(layout: Layout) -> Option<Layout> {
        layout.align_to(BUFFER_ALIGN).ok()
    }
}

#[cfg(gusset_allocator_api)]
unsafe impl Allocator for BufferAlloc {
    fn allocate(&self, layout: Layout) -> Result<NonNull<[u8]>, AllocError> {
        let real = Self::buffer_layout(layout).ok_or(AllocError)?;
        let block = System.allocate(real)?;
        count_buffer_alloc(real.size());
        Ok(block)
    }

    fn allocate_zeroed(&self, layout: Layout) -> Result<NonNull<[u8]>, AllocError> {
        let real = Self::buffer_layout(layout).ok_or(AllocError)?;
        let block = System.allocate_zeroed(real)?;
        count_buffer_alloc(real.size());
        Ok(block)
    }

    unsafe fn deallocate(&self, ptr: NonNull<u8>, layout: Layout) {
        // Cannot fail: the same size rounded successfully when it was allocated.
        // Leaking is the safe answer if it ever did, never a mismatched free.
        if let Some(real) = Self::buffer_layout(layout) {
            unsafe { System.deallocate(ptr, real) };
            count_buffer_dealloc(real.size());
        }
    }

    unsafe fn grow(
        &self,
        ptr: NonNull<u8>,
        old: Layout,
        new: Layout,
    ) -> Result<NonNull<[u8]>, AllocError> {
        let (o, n) = Self::pair(old, new)?;
        let block = unsafe { System.grow(ptr, o, n) }?;
        count_resize(o.size(), n.size(), true);
        Ok(block)
    }

    unsafe fn grow_zeroed(
        &self,
        ptr: NonNull<u8>,
        old: Layout,
        new: Layout,
    ) -> Result<NonNull<[u8]>, AllocError> {
        let (o, n) = Self::pair(old, new)?;
        let block = unsafe { System.grow_zeroed(ptr, o, n) }?;
        count_resize(o.size(), n.size(), true);
        Ok(block)
    }

    unsafe fn shrink(
        &self,
        ptr: NonNull<u8>,
        old: Layout,
        new: Layout,
    ) -> Result<NonNull<[u8]>, AllocError> {
        let (o, n) = Self::pair(old, new)?;
        let block = unsafe { System.shrink(ptr, o, n) }?;
        count_resize(o.size(), n.size(), true);
        Ok(block)
    }
}

#[cfg(gusset_allocator_api)]
impl BufferAlloc {
    #[inline]
    fn pair(old: Layout, new: Layout) -> Result<(Layout, Layout), AllocError> {
        let o = Self::buffer_layout(old).ok_or(AllocError)?;
        let n = Self::buffer_layout(new).ok_or(AllocError)?;
        Ok((o, n))
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
/// counted these bytes. See `COUNTING_ACTIVE`.
#[inline]
pub fn record_alloc(size: usize) {
    if counting_is_active() {
        return;
    }
    add_live_bytes(size);
    add_alloc_count();
}

/// Counts `size` bytes of Gusset buffer memory. Always counted here.
///
/// Buffer memory ([`BufferAlloc`] and every `RawBuffer`) comes from `System`,
/// never from the global allocator, so no installed `Counting` sees it and
/// nothing else counts it. That makes the count independent of
/// `COUNTING_ACTIVE`, which is inferred and can flip late (a non-global
/// `Counting` called through `GlobalAlloc`); consulting it at free time left
/// buffers counted forever once it flipped.
#[inline]
pub fn count_buffer_alloc(size: usize) {
    if size != 0 {
        add_live_bytes(size);
        add_alloc_count();
    }
}

/// Uncounts `size` bytes of Gusset buffer memory (see [`count_buffer_alloc`]).
#[inline]
pub fn count_buffer_dealloc(size: usize) {
    if size != 0 {
        dec_live_bytes(size);
    }
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
