//! Replacing an initialised `FfiStatus` must not leak the message it owns.
//!
//! `gusset_submit` and `gusset_buf_alloc` run the guarded call, and when it
//! fails and the handle turns out to be poisoned they report `FFI_POISONED`
//! instead. They used to `ptr::write` the new status over the one `ffi_guard_code`
//! had just filled, leaking its boxed message on every such failure.
//!
//! The poison flip that reaches that branch is a race and cannot be scheduled
//! deterministically, so this pins the replacement itself. The control arm
//! shows the measurement sees the old pattern's leak; the second arm shows
//! `FfiStatus::overwrite` does not have it.
//!
//! Separate binary because it installs the counting global allocator.

use gusset::ffi::status::{FfiStatus, FFI_ERR};
use gusset::{get_alloc_stats, Counting};
use std::alloc::System;

#[global_allocator]
static ALLOC: Counting<System> = Counting::new(System);

const ROUNDS: usize = 10_000;
const MSG: &[u8] = &[b'x'; 256];

/// The fixed arm may grow the counter by less than one byte per round. A leak
/// in the replacement costs at least one allocation per round, so it shows up
/// as ROUNDS bytes or more (2.5 MB for the guard's 256-byte message). One-time
/// allocations by the runtime or the test harness during the window do not
/// scale with ROUNDS. The old bound, under one message (256 bytes) over 1000
/// rounds, failed on a correct build when 956 bytes landed in the window
/// (Allocator API macos-latest, job 120787865307).
const MAX_FIXED_GROWTH: usize = ROUNDS;

fn guard_style_error() -> FfiStatus {
    FfiStatus::new_err(FFI_ERR, MSG, std::ptr::null(), 0, 0)
}

fn live() -> usize {
    get_alloc_stats().live_bytes
}

#[test]
fn overwriting_an_error_status_releases_its_message() {
    // Control: the pre-fix pattern leaks, and the counter can see it.
    let before = live();
    for _ in 0..ROUNDS {
        let mut st = guard_style_error();
        unsafe {
            std::ptr::write(&mut st, FfiStatus::poisoned("handle is poisoned"));
        }
        unsafe { st.free_msg() };
    }
    let leaked = live().saturating_sub(before);
    assert!(
        leaked >= ROUNDS * MSG.len(),
        "control arm: expected the plain overwrite to leak ~{} bytes, measured {}; the \
         counter cannot distinguish the fix from the bug",
        ROUNDS * MSG.len(),
        leaked
    );

    // Fixed: overwrite frees the guard's message first. One round outside the
    // window first, so anything the path allocates once is not counted.
    let mut warm = guard_style_error();
    unsafe {
        FfiStatus::overwrite(&mut warm, FfiStatus::poisoned("handle is poisoned"));
        warm.free_msg();
    }
    let before = live();
    for _ in 0..ROUNDS {
        let mut st = guard_style_error();
        unsafe {
            FfiStatus::overwrite(&mut st, FfiStatus::poisoned("handle is poisoned"));
        }
        unsafe { st.free_msg() };
    }
    let delta = live().saturating_sub(before);
    assert!(
        delta < MAX_FIXED_GROWTH,
        "FfiStatus::overwrite leaked {} bytes over {} rounds (bound {})",
        delta,
        ROUNDS,
        MAX_FIXED_GROWTH
    );
}
