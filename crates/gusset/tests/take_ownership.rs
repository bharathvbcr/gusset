//! A result `gusset_take` copies into a registry buffer must be freed by the
//! caller, flagged or not.
//!
//! A result of 4 KiB or less is copied into a buffer and returned without
//! `GUSSET_TAKE_OWNED_FLAG`. A C host that read only the flagged-id sentence in
//! `gusset.h` would treat it as borrowed and leak one buffer per call until
//! `gusset_handle_close`. The header now says every nonzero id is freed with
//! `gusset_buf_free(id & ~FLAG)`; this pins that doing so is what releases it.
//!
//! The registry is private to the crate, so its size is read through
//! `get_alloc_stats`: buffer memory is counted by `count_buffer_alloc` alone
//! (it never passes through a global allocator), and this binary installs
//! none, so `live_bytes` and `alloc_count` move only with registry buffers.
//!
//! Separate binary: registers a process-global engine handler and reads
//! process-global counters, so it holds a single test.

#![allow(unsafe_code)]

use gusset::ffi::status::FFI_OK;
use gusset::ffi::{
    gusset_buf_free, gusset_handle_close, gusset_handle_open, gusset_submit, gusset_take,
};
use gusset::header::CallHeader;
use gusset::pool::TAKE_OWNED_FLAG;
use gusset::{get_alloc_stats, set_engine_handler, Handle};
use std::ptr;

#[path = "../../../tests/rust/common/mod.rs"]
mod common;
use common::{make_pipe, read_ticket};

const RESULT_LEN: usize = 10;

#[test]
fn an_unflagged_small_result_holds_one_buffer_until_gusset_buf_free() {
    set_engine_handler(|_ctx, _input: &[u8]| -> Result<Vec<u8>, String> {
        Ok(vec![7u8; RESULT_LEN])
    });

    let (r, w) = make_pipe();
    let mut handle: *mut Handle = ptr::null_mut();
    assert_eq!(
        unsafe { gusset_handle_open(1, w, &mut handle, ptr::null_mut()) },
        FFI_OK
    );

    // Flags 0: no GUSSET_FLAG_INLINE_COMPLETION, so the result waits for take.
    let header = CallHeader::default();
    let input = [0u8; 1];
    let mut ticket = 0u64;
    let code = unsafe {
        gusset_submit(
            handle,
            &header,
            input.as_ptr(),
            input.len(),
            0,
            &mut ticket,
            ptr::null_mut(),
        )
    };
    assert_eq!(code, FFI_OK);
    assert_eq!(read_ticket(r), ticket);

    let before = get_alloc_stats();
    let mut id = 0u64;
    let mut p: *mut u8 = ptr::null_mut();
    let mut len = 0usize;
    let code = unsafe { gusset_take(handle, ticket, &mut id, &mut p, &mut len, ptr::null_mut()) };
    assert_eq!(code, FFI_OK);
    assert_eq!(len, RESULT_LEN);
    assert!(!p.is_null());
    assert_ne!(id, 0, "a non-empty result carries a buffer id");
    assert_eq!(
        id & TAKE_OWNED_FLAG,
        0,
        "a small result is copied into a registry buffer and returned unflagged"
    );
    assert_eq!(
        unsafe { std::slice::from_raw_parts(p, len) },
        [7u8; RESULT_LEN]
    );

    let held = get_alloc_stats();
    assert_eq!(
        held.alloc_count - before.alloc_count,
        1,
        "take registers exactly one buffer"
    );
    assert_eq!(
        held.live_bytes - before.live_bytes,
        RESULT_LEN,
        "the registry holds that buffer until it is freed"
    );

    let code = unsafe { gusset_buf_free(handle, id & !TAKE_OWNED_FLAG, ptr::null_mut()) };
    assert_eq!(code, FFI_OK);
    assert_eq!(
        get_alloc_stats().live_bytes,
        before.live_bytes,
        "gusset_buf_free(id & ~FLAG) releases the unflagged result buffer"
    );

    assert_eq!(
        unsafe { gusset_handle_close(handle, ptr::null_mut()) },
        FFI_OK
    );
}
