//! A buffer counted by hand must be uncounted when it is freed, even if a
//! `Counting` was used directly (not as the global allocator) in between.
//!
//! `COUNTING_ACTIVE` flips on the first allocation any `Counting` services.
//! `RawBuffer` used to consult the flag at free time, so a buffer recorded
//! before the flip was never released: `live_bytes` stayed inflated for the
//! rest of the process and `AdviseMemoryLimit` shrank the Go heap for memory
//! that no longer existed.
//!
//! Separate binary: the flip is process-wide and permanent.

use gusset::pool::sys::RawBuffer;
use gusset::{get_alloc_stats, Counting};
use std::alloc::{GlobalAlloc, Layout, System};

#[test]
fn a_buffer_counted_by_hand_is_uncounted_after_the_flag_flips() {
    let before = get_alloc_stats().live_bytes;
    let buf = match RawBuffer::allocate(1 << 20) {
        Ok(b) => b,
        Err(e) => panic!("allocate: {e}"),
    };
    assert_eq!(get_alloc_stats().live_bytes, before + (1 << 20));

    // An adopter calling a non-global Counting directly flips the flag.
    let direct = Counting::new(System);
    let layout = Layout::from_size_align(64, 8).unwrap_or_else(|e| panic!("{e}"));
    unsafe {
        let p = direct.alloc(layout);
        assert!(!p.is_null());
        direct.dealloc(p, layout);
    }

    drop(buf);
    assert_eq!(
        get_alloc_stats().live_bytes,
        before,
        "the hand-counted megabyte was never released"
    );
}
