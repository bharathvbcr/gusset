//! A closed handle never closes its descriptor number again after the number
//! has been given to something else.
//!
//! `Handle::close` releases the completion pipe's write end. The failure this
//! guards against is a second release, from a second `close`, from `Drop`, or
//! from a join path, that lands after the process has reused the number for
//! an unrelated file and closes that file instead.
//!
//! Deterministic by construction rather than by luck: this is its own test
//! binary with one test, so no other thread opens or closes descriptors, and
//! the reuse is arranged with `dup2` onto the exact number the handle held
//! instead of hoping the next `open` receives it.

#![allow(unsafe_code)]

#[path = "../../../tests/common/mod.rs"]
mod common;

use common::{make_pipe, Must};
use gusset::pool::Handle;

/// True when `write_fd` is still the write end of the pipe whose read end is
/// `read_fd`: a byte written to one comes out of the other.
fn same_pipe(write_fd: i32, read_fd: i32) -> bool {
    let out = [0xA5u8];
    // SAFETY: one byte from a valid stack buffer to a candidate descriptor.
    if unsafe { libc::write(write_fd, out.as_ptr() as *const libc::c_void, 1) } != 1 {
        return false;
    }
    let mut back = [0u8; 1];
    // SAFETY: one byte into a valid stack buffer; the pipe has a byte waiting.
    let n = unsafe { libc::read(read_fd, back.as_mut_ptr() as *mut libc::c_void, 1) };
    n == 1 && back[0] == 0xA5
}

#[test]
fn closing_a_handle_never_closes_a_reused_descriptor_number() {
    // The handle's pipe, then a second, unrelated pipe standing in for "the
    // file the process opened next".
    let (handle_r, handle_w) = make_pipe();
    let (other_r, other_w) = make_pipe();

    let handle = Handle::open(2, handle_w).must("open");
    handle.close().must("first close");

    // The handle has released `handle_w`. Give that exact number to the other
    // pipe's write end.
    // SAFETY: both descriptors are open and owned by this test.
    let placed = unsafe { libc::dup2(other_w, handle_w) };
    assert_eq!(placed, handle_w, "dup2 onto the released number failed");
    // SAFETY: the original is now redundant.
    unsafe { libc::close(other_w) };
    assert!(
        same_pipe(handle_w, other_r),
        "test setup: the reused number must be the other pipe"
    );

    // Everything that can release a handle again, in the order a caller can
    // reach it: a repeated close, then a close racing nothing, then Drop.
    handle.close().must("second close");
    handle.close().must("third close");
    drop(handle);

    assert!(
        same_pipe(handle_w, other_r),
        "a closed handle closed descriptor {handle_w} again after it was reused for another file"
    );

    // SAFETY: every remaining descriptor is owned by this test.
    unsafe {
        libc::close(handle_w);
        libc::close(handle_r);
        libc::close(other_r);
    }
}
