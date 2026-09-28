//! With `Counting` as the `#[global_allocator]`, a Rust-owned buffer is counted
//! exactly once, on every toolchain.
//!
//! `rust_allocator_counting` proves this for `BufferAlloc`, but it is gated on
//! `cfg(gusset_allocator_api)` (Rust 1.100+) and runs zero tests on the
//! supported 1.97/1.98 toolchains. Buffers Go allocates (`gusset_buf_alloc`)
//! and large results promoted on the worker exist on every toolchain, and an
//! adopter umbrella crate that installs `Counting` is the documented setup, so
//! the "counted once whether or not Counting is global" claim needs a test
//! that actually runs there. A double count subtracts every buffer twice from
//! the Go heap budget in `AdviseMemoryLimit`.
//!
//! Separate binary because it installs the global allocator.

#![allow(unsafe_code)]

use gusset::ffi::status::FFI_OK;
use gusset::ffi::{gusset_buf_alloc, gusset_buf_free, gusset_handle_close, gusset_handle_open};
use gusset::{get_alloc_stats, Counting, Handle};
use std::alloc::System;
use std::ptr;

#[global_allocator]
static ALLOC: Counting<System> = Counting::new(System);

const MIB: usize = 1 << 20;
const SLACK: usize = 256 * 1024;

fn live() -> isize {
    get_alloc_stats().live_bytes as isize
}

fn check(what: &str, delta: isize, want: usize) {
    let want = want as isize;
    assert!(
        (delta - want).unsigned_abs() <= SLACK,
        "{what}: live moved by {delta}, expected ~{want} (2x means double counting)"
    );
}

#[test]
fn a_go_visible_buffer_is_counted_once_under_a_counting_global_allocator() {
    let mut fds = [0i32; 2];
    assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
    let mut handle: *mut Handle = ptr::null_mut();
    assert_eq!(
        unsafe { gusset_handle_open(1, fds[1], &mut handle, ptr::null_mut()) },
        FFI_OK
    );

    let before = live();
    let mut id = 0u64;
    let mut p: *mut u8 = ptr::null_mut();
    let code = unsafe { gusset_buf_alloc(handle, 8 * MIB, &mut id, &mut p, ptr::null_mut()) };
    assert_eq!(code, FFI_OK);
    assert!(!p.is_null());
    check("gusset_buf_alloc", live() - before, 8 * MIB);

    assert_eq!(
        unsafe { gusset_buf_free(handle, id, ptr::null_mut()) },
        FFI_OK
    );
    check("gusset_buf_free", live() - before, 0);

    assert_eq!(
        unsafe { gusset_handle_close(handle, ptr::null_mut()) },
        FFI_OK
    );
    unsafe { libc::close(fds[0]) };
}
