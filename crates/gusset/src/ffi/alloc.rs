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
///
/// Each field saturates: once it reaches `i64::MAX` (`usize::MAX / 2`) it reads
/// `usize::MAX` and never wraps to a small value. No real heap or allocation count
/// gets near that, and the Go side clamps `live_bytes` to `math.MaxInt64` anyway.
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

/// A value that owns its cache lines, so nothing else shares one with it.
///
/// 128 bytes, as `pool/ring.rs` pads its `Slot`: Apple cores have 128-byte lines
/// and x86 prefetches lines in pairs. A static of this type is aligned to it.
#[repr(align(128))]
struct Padded<T>(T);

impl<T> Padded<T> {
    const fn new(value: T) -> Self {
        Self(value)
    }
}

impl<T> std::ops::Deref for Padded<T> {
    type Target = T;

    #[inline]
    fn deref(&self) -> &T {
        &self.0
    }
}

/// The counters every allocation and free passes through, process-wide.
///
/// # Layout
///
/// `live` and `count` share one line on purpose: an allocation updates both, so
/// one line moves between cores per allocation instead of two. `peak` is read on
/// every allocation and written only when a new high is reached, so it has a line
/// of its own: a core that reads it keeps it in cache until then.
///
/// # Saturation
///
/// None of the three wraps, and none reads small after reaching the top. A
/// wrapped live total would make `AdviseMemoryLimit` see a saturated Rust heap as
/// nearly empty and raise the Go limit. See [`add_saturating`] for how adds
/// avoid a compare-and-swap loop without giving that up.
///
/// # Reading
///
/// [`Counters::snapshot`] is three independent relaxed loads, live, then peak,
/// then count. It is not an atomic snapshot of the three, and never was.
/// `peak >= live` can fail in one window: an allocation has added to `live` but
/// not yet raised `peak` (the peak is raised after the add, exactly as before).
/// Once the allocating threads are quiescent, `peak` is at least every live total
/// any of them published.
struct Counters {
    hot: Padded<Hot>,
    peak: Padded<AtomicUsize>,
}

struct Hot {
    live: AtomicUsize,
    count: AtomicUsize,
}

static COUNTERS: Counters = Counters::new();

/// Counters stop counting here: a stored value at or above this reads as
/// `usize::MAX` (see [`reported`]), and a saturating add never writes a value
/// above it. `usize::MAX / 2` is `i64::MAX` on 64-bit, the same bound
/// `AdviseMemoryLimit` clamps to, so no consumer can tell a saturated counter
/// from one that reached the real `usize::MAX`.
const SATURATED: usize = usize::MAX >> 1;

/// The largest add that may take the `fetch_add` path (see [`add_saturating`]).
/// 256 GiB less one on 64-bit, far above any one allocation a process makes;
/// larger adds take the compare-and-swap path.
const FAST_ADD_MAX: usize = usize::MAX >> 26;

/// What a reader sees for a stored value: `usize::MAX` once it has saturated.
#[inline]
fn reported(stored: usize) -> usize {
    if stored >= SATURATED {
        usize::MAX
    } else {
        stored
    }
}

/// Adds `n` to `cell` without ever wrapping, and returns the value stored after
/// the add. A reader of `cell` never observes a wrapped value, between operations
/// or otherwise.
///
/// The add is a single `fetch_add` unless the cell has already reached
/// [`SATURATED`] or `n` is above [`FAST_ADD_MAX`]. Those take the saturating
/// compare-and-swap loop, which never writes a value above [`SATURATED`]. A
/// `fetch_add` that checks first and adds after is not enough on its own: another
/// thread can add between the check and the add, and two adders that each saw room
/// could together overflow. What makes it enough here is headroom.
///
/// # Why the `fetch_add` cannot wrap
///
/// Every `fetch_add` was preceded, on its own thread, by a load that saw a value
/// below `SATURATED`, and adds at most `FAST_ADD_MAX`. Take any instant at which
/// the stored value is above `SATURATED`, and the last earlier instant at which it
/// was not. Between the two it never stopped being above `SATURATED`, so no add
/// started after that earlier instant can pass the check. What can still land is an
/// add whose check came before it: one per thread, because a thread finishes one add
/// before starting the next. The stored value is therefore at most
/// `SATURATED + threads * FAST_ADD_MAX`. The compare-and-swap path cannot push it
/// higher (it never writes above `SATURATED`), and a decrement only lowers it. With
/// `FAST_ADD_MAX = usize::MAX >> 26` that stays below `usize::MAX` for up to 2^25
/// threads adding at once, eight times the 2^22 thread ids Linux hands out. A
/// signal handler that allocates while its thread is between a check and an add
/// counts as another adder.
///
/// A narrower `usize` (every supported target is 64-bit) changes the numbers, not
/// the argument: it saturates sooner and sends more adds down the slow path.
#[inline]
fn add_saturating(cell: &AtomicUsize, n: usize) -> usize {
    let seen = cell.load(Ordering::Relaxed);
    if seen < SATURATED && n <= FAST_ADD_MAX {
        return cell.fetch_add(n, Ordering::Relaxed).saturating_add(n);
    }
    let mut current = seen;
    loop {
        if current >= SATURATED {
            return current;
        }
        let next = current.saturating_add(n).min(SATURATED);
        match cell.compare_exchange_weak(current, next, Ordering::Relaxed, Ordering::Relaxed) {
            Ok(_) => return next,
            Err(actual) => current = actual,
        }
    }
}

impl Counters {
    const fn new() -> Self {
        Self {
            hot: Padded::new(Hot {
                live: AtomicUsize::new(0),
                count: AtomicUsize::new(0),
            }),
            peak: Padded::new(AtomicUsize::new(0)),
        }
    }

    /// Adds `size` to the live total and raises the peak to match. Returns the
    /// live value stored after the add.
    #[inline]
    fn add_live(&self, size: usize) -> usize {
        if size == 0 {
            return self.hot.live.load(Ordering::Relaxed);
        }
        let next = add_saturating(&self.hot.live, size);
        self.raise_peak(next);
        next
    }

    /// Advances the allocation count, saturating.
    #[inline]
    fn add_count(&self) {
        add_saturating(&self.hot.count, 1);
    }

    /// Takes `size` off the live total, flooring at zero.
    ///
    /// A compare-and-swap loop, not a `fetch_sub`: no headroom argument covers a
    /// subtraction, because nothing bounds how many frees run at once or how large
    /// one is (the `Allocator` contract lets a caller free with a size up to the
    /// block's length, and `count_buffer_dealloc` takes any size). A `fetch_sub`
    /// past zero would wrap to nearly `usize::MAX`, and `AdviseMemoryLimit` would
    /// pin the Go limit at its floor.
    #[inline]
    fn dec_live(&self, size: usize) {
        if size == 0 {
            return;
        }
        let mut current = self.hot.live.load(Ordering::Relaxed);
        while let Err(actual) = self.hot.live.compare_exchange_weak(
            current,
            current.saturating_sub(size),
            Ordering::Relaxed,
            Ordering::Relaxed,
        ) {
            current = actual;
        }
    }

    /// Raises the peak to `live` if it is higher. Read-only in the common case,
    /// where live oscillates below the peak, so it does not bounce the peak's line.
    #[inline]
    fn raise_peak(&self, live: usize) {
        if live > self.peak.load(Ordering::Relaxed) {
            self.peak.fetch_max(live, Ordering::Relaxed);
        }
    }

    /// Three independent loads (see the type's docs on what that does and does
    /// not promise), each reported as [`reported`] does.
    #[inline]
    fn snapshot(&self) -> AllocStats {
        AllocStats {
            live_bytes: reported(self.hot.live.load(Ordering::Relaxed)),
            peak_bytes: reported(self.peak.load(Ordering::Relaxed)),
            alloc_count: reported(self.hot.count.load(Ordering::Relaxed)),
        }
    }
}

/// Set the first time a `Counting` wrapper services an allocation through
/// `GlobalAlloc` (see [`counting_is_active`]).
///
/// `Counting`'s allocator-API path reads it to skip bytes that a counted global
/// allocator below it has already seen, so they are not reported twice. Gusset's
/// buffers do not read it: they allocate from `System` and are always counted
/// (see [`count_buffer_alloc`]).
///
/// Padded: it is read on the allocator-API paths and written once, so it must not
/// sit on a line the counters write on every allocation.
static COUNTING_ACTIVE: Padded<AtomicBool> = Padded::new(AtomicBool::new(false));

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
/// `GlobalAlloc` sets it too. Only `Counting`'s allocator-API path consults it,
/// to skip bytes a counted global allocator below it already saw; Gusset's own
/// buffers never do (see [`count_buffer_alloc`]).
#[inline]
pub fn counting_is_active() -> bool {
    COUNTING_ACTIVE.load(Ordering::Relaxed)
}

/// Adds `size` to the live counter without wrapping (see [`Counters`] and
/// [`add_saturating`]).
#[inline]
fn add_live_bytes(size: usize) -> usize {
    COUNTERS.add_live(size)
}

/// Advances the allocation count, saturating.
#[inline]
fn add_alloc_count() {
    COUNTERS.add_count();
}

/// Takes `size` off the live counter, flooring at zero.
#[inline]
fn dec_live_bytes(size: usize) {
    COUNTERS.dec_live(size);
}

/// Records `size` freshly allocated bytes that bypassed the global allocator.
///
/// Never skipped, whatever [`counting_is_active`] says: the caller knows these
/// bytes did not pass through an installed `Counting`, so nothing else will
/// count them.
#[cfg(gusset_allocator_api)]
#[inline]
fn count_alloc_always(size: usize) {
    if size != 0 {
        add_live_bytes(size);
        add_alloc_count();
    }
}

/// Moves the live total from `old` to `new` bytes after a resize. Unless
/// `always`, a no-op once [`counting_is_active`]: the counted global allocator
/// below has already seen the bytes.
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
#[allow(
    clippy::incompatible_msrv,
    reason = "compiled only where allocator_probe.rs found the stable Allocator API"
)]
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
// The cfg is the version gate (AGENTS.md rule 6): it is set only when
// allocator_probe.rs compiled a use of the stable `Allocator` trait with this
// compiler, without a feature gate. clippy's MSRV lint judges each item against
// `rust-version = "1.97"` and cannot see that gate, so it is told here, on the
// gated items only. Ungated code stays under the lint.
#[cfg(gusset_allocator_api)]
#[allow(
    clippy::incompatible_msrv,
    reason = "compiled only where allocator_probe.rs found the stable Allocator API"
)]
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
#[allow(
    clippy::incompatible_msrv,
    reason = "compiled only where allocator_probe.rs found the stable Allocator API"
)]
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
#[allow(
    clippy::incompatible_msrv,
    reason = "compiled only where allocator_probe.rs found the stable Allocator API"
)]
impl BufferAlloc {
    #[inline]
    fn pair(old: Layout, new: Layout) -> Result<(Layout, Layout), AllocError> {
        let o = Self::buffer_layout(old).ok_or(AllocError)?;
        let n = Self::buffer_layout(new).ok_or(AllocError)?;
        Ok((o, n))
    }
}

/// Returns current allocator statistics without performing any allocations.
///
/// The three fields are read one after another, not as one atomic snapshot, so
/// under concurrent allocation they can describe slightly different moments (and
/// `peak_bytes` can briefly trail `live_bytes`). Each field is exact until it
/// saturates and then reads `usize::MAX`.
#[inline]
pub fn get_alloc_stats() -> AllocStats {
    COUNTERS.snapshot()
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

#[cfg(test)]
mod tests {
    use super::*;

    /// One test, not four: the counters are process globals, so parallel tests in
    /// this binary would read each other's deltas. Everything here is expressed as a
    /// delta for the same reason.
    #[test]
    fn accounting_is_balanced_saturating_and_peak_monotonic() {
        // Gusset installs no global allocator (DECISIONS 2026-09-20); the
        // adopter does. A library that installed one would count every heap
        // allocation in this binary under the deltas below.
        assert!(
            !counting_is_active(),
            "the library under test must not install a global allocator"
        );

        let before = get_alloc_stats();

        count_buffer_alloc(4096);
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

        count_buffer_dealloc(4096);
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
        count_buffer_dealloc(live_now.saturating_add(1 << 20));
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
        let saved_live = COUNTERS.hot.live.swap(usize::MAX - 8, Ordering::Relaxed);
        let saved_peak = COUNTERS.peak.swap(0, Ordering::Relaxed);
        let saved_count = COUNTERS.hot.count.load(Ordering::Relaxed);
        count_buffer_alloc(64);
        let saturated = get_alloc_stats().live_bytes;
        COUNTERS.hot.live.store(saved_live, Ordering::Relaxed);
        COUNTERS
            .peak
            .store(saved_peak.max(saved_live), Ordering::Relaxed);
        COUNTERS.hot.count.store(saved_count, Ordering::Relaxed);
        assert_eq!(
            saturated,
            usize::MAX,
            "live bytes must saturate at usize::MAX; wrapping reports a tiny heap"
        );
    }

    // The tests below run on private `Counters`, never the process-wide one: the
    // other lib tests allocate buffers, and a test that parks the shared total near
    // usize::MAX or zero would corrupt theirs (and theirs, its).

    /// Threads and iterations for the concurrent tests. Miri runs them too
    /// (nightly.yml), a few orders of magnitude slower.
    const THREADS: usize = 4;
    const ITERS: usize = if cfg!(miri) { 40 } else { 20_000 };

    /// Runs `body` on `THREADS` threads while a reader samples `sample` until they
    /// finish, and returns every sample. The reader is how a value that is wrong
    /// only between two operations gets seen.
    fn race<S: Send>(body: impl Fn(usize) + Sync, sample: impl Fn() -> S + Sync) -> Vec<S> {
        let done = std::sync::atomic::AtomicBool::new(false);
        std::thread::scope(|scope| {
            let reader = scope.spawn(|| {
                let mut seen = Vec::new();
                while !done.load(Ordering::Relaxed) {
                    seen.push(sample());
                    std::thread::yield_now();
                }
                seen.push(sample());
                seen
            });
            let workers: Vec<_> = (0..THREADS)
                .map(|t| {
                    let body = &body;
                    scope.spawn(move || body(t))
                })
                .collect();
            // Every worker is joined and `done` set before anything can panic, so a
            // failing worker cannot leave the reader spinning and hang the scope.
            let joined: Vec<bool> = workers
                .into_iter()
                .map(|worker| worker.join().is_ok())
                .collect();
            done.store(true, Ordering::Relaxed);
            assert!(joined.iter().all(|&ok| ok), "a worker thread panicked");
            let Ok(seen) = reader.join() else {
                panic!("the reader thread panicked");
            };
            seen
        })
    }

    #[test]
    fn counters_do_not_share_a_cache_line() {
        assert_eq!(align_of::<Counters>(), 128);
        assert_eq!(align_of::<Padded<AtomicBool>>(), 128);
        assert!(size_of::<Hot>() <= 128);
        assert!(
            std::mem::offset_of!(Counters, peak) - std::mem::offset_of!(Counters, hot) >= 128,
            "peak is read on every allocation and must not sit on the line the \
             live total and the count are written on"
        );
        // The C ABI struct is untouched by any of this.
        assert_eq!(size_of::<AllocStats>(), 3 * size_of::<usize>());
    }

    #[test]
    fn live_never_wraps_when_adds_start_above_the_saturation_bound() {
        // A saturated heap: every add takes the compare-and-swap path and must leave
        // the total where it is. A plain `fetch_add` wraps it to a small number,
        // which AdviseMemoryLimit reads as an empty heap.
        let start = usize::MAX - 10_000;
        let c = Counters::new();
        c.hot.live.store(start, Ordering::Relaxed);
        let seen = race(
            |_| {
                for _ in 0..ITERS {
                    c.add_live(64);
                }
            },
            || c.hot.live.load(Ordering::Relaxed),
        );
        assert!(
            seen.iter().all(|&v| v >= start),
            "a reader saw the live total below where it started: it wrapped"
        );
        assert_eq!(c.snapshot().live_bytes, usize::MAX);
        assert_eq!(c.snapshot().peak_bytes, usize::MAX);
    }

    #[test]
    fn live_saturates_across_the_bound_while_adds_race() {
        // Adds that start below the bound and cross it together: the `fetch_add`
        // path, with room for the adds already in flight when it is crossed.
        let start = SATURATED - 1_000;
        let c = Counters::new();
        c.hot.live.store(start, Ordering::Relaxed);
        // Each thread has at most one 64-byte add in flight past the bound
        // (add_saturating's argument, with this test's add size for FAST_ADD_MAX).
        let ceiling = SATURATED + THREADS * 64;
        let seen = race(
            |_| {
                for _ in 0..ITERS {
                    c.add_live(64);
                }
            },
            || c.hot.live.load(Ordering::Relaxed),
        );
        assert!(
            seen.iter().all(|&v| v >= start),
            "a reader saw the live total fall below its start: it wrapped"
        );
        assert!(
            seen.iter().all(|&v| v <= ceiling),
            "the live total passed the headroom add_saturating's argument allows"
        );
        let stored = c.hot.live.load(Ordering::Relaxed);
        assert!(stored >= SATURATED, "adds past the bound must reach it");
        assert_eq!(c.snapshot().live_bytes, usize::MAX);
    }

    #[test]
    fn a_large_add_stops_at_the_saturation_bound() {
        // Headroom for the adds in flight only exists if the compare-and-swap path
        // never writes above the bound, whatever the size it is given.
        for size in [usize::MAX, usize::MAX / 2, FAST_ADD_MAX + 1] {
            let c = Counters::new();
            c.hot.live.store(100, Ordering::Relaxed);
            let after = c.add_live(size);
            let stored = c.hot.live.load(Ordering::Relaxed);
            assert!(stored <= SATURATED, "size {size}: stored {stored}");
            assert_eq!(after, stored);
            if size >= SATURATED {
                assert_eq!(c.snapshot().live_bytes, usize::MAX, "size {size}");
            }
        }
    }

    #[test]
    fn a_fast_add_is_exact_below_the_bound() {
        let c = Counters::new();
        assert_eq!(c.add_live(64), 64);
        assert_eq!(c.add_live(FAST_ADD_MAX), 64 + FAST_ADD_MAX);
        assert_eq!(c.add_live(0), 64 + FAST_ADD_MAX);
        assert_eq!(c.snapshot().live_bytes, 64 + FAST_ADD_MAX);
    }

    #[test]
    fn dec_floors_at_zero_while_frees_race() {
        // More bytes freed than were ever counted, from several threads. A
        // `fetch_sub` wraps the total to nearly usize::MAX, which AdviseMemoryLimit
        // reads as a saturated heap and answers by pinning the Go limit to its
        // floor.
        let start = 1_000;
        let c = Counters::new();
        c.hot.live.store(start, Ordering::Relaxed);
        let seen = race(
            |_| {
                for _ in 0..ITERS {
                    c.dec_live(64);
                }
            },
            || c.hot.live.load(Ordering::Relaxed),
        );
        assert!(
            seen.iter().all(|&v| v <= start),
            "a reader saw the live total above where it started: a free wrapped it"
        );
        assert_eq!(c.hot.live.load(Ordering::Relaxed), 0);
    }

    #[test]
    fn alloc_and_free_balance_exactly_under_contention() {
        let c = Counters::new();
        race(
            |_| {
                for _ in 0..ITERS {
                    c.add_live(64);
                    c.add_count();
                    c.dec_live(64);
                }
            },
            || (),
        );
        let stats = c.snapshot();
        assert_eq!(stats.live_bytes, 0);
        assert_eq!(stats.alloc_count, THREADS * ITERS);
    }

    #[test]
    fn peak_is_at_least_every_live_total_a_thread_published() {
        // `add_live` returns the total its own add produced. Once the threads are
        // done, the peak must cover the highest of those: it is raised after the
        // add, so only a quiescent read can promise it.
        let c = Counters::new();
        let highest: Vec<AtomicUsize> = (0..THREADS).map(|_| AtomicUsize::new(0)).collect();
        race(
            |t| {
                for _ in 0..ITERS {
                    let live = c.add_live(64);
                    highest[t].fetch_max(live, Ordering::Relaxed);
                    c.dec_live(64);
                }
            },
            || (),
        );
        let peak = c.peak.load(Ordering::Relaxed);
        for (t, h) in highest.iter().enumerate() {
            let h = h.load(Ordering::Relaxed);
            assert!(peak >= h, "thread {t} saw {h} live bytes, peak is {peak}");
        }
        assert!(peak >= 64, "peak must have moved off zero");
    }

    #[test]
    fn count_never_wraps_when_it_starts_above_the_saturation_bound() {
        let start = usize::MAX - 3;
        let c = Counters::new();
        c.hot.count.store(start, Ordering::Relaxed);
        let seen = race(
            |_| {
                for _ in 0..ITERS {
                    c.add_count();
                }
            },
            || c.hot.count.load(Ordering::Relaxed),
        );
        assert!(
            seen.iter().all(|&v| v >= start),
            "a reader saw the allocation count below where it started: it wrapped"
        );
        assert_eq!(c.snapshot().alloc_count, usize::MAX);
    }

    #[test]
    fn count_saturates_across_the_bound_while_increments_race() {
        let start = SATURATED - 5;
        let c = Counters::new();
        c.hot.count.store(start, Ordering::Relaxed);
        let seen = race(
            |_| {
                for _ in 0..ITERS {
                    c.add_count();
                }
            },
            || c.hot.count.load(Ordering::Relaxed),
        );
        assert!(seen.iter().all(|&v| v >= start));
        let stored = c.hot.count.load(Ordering::Relaxed);
        assert!(
            (SATURATED..=SATURATED + THREADS).contains(&stored),
            "stored {stored}: must stop within the adds already in flight of the bound"
        );
        assert_eq!(c.snapshot().alloc_count, usize::MAX);
    }
}
