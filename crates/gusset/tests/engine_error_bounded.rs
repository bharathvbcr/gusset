//! An engine's error text crosses the boundary bounded and whole.
//!
//! Panic payloads are capped at 32 KiB on a character boundary before they
//! reach `FfiStatus`. An engine's `Err(String)` was not capped at all: a
//! parser echoing its input into the error handed Go a status message as large
//! as the input, and Go's reader (`statusToError` in internal/ffi/ffi.go)
//! copies at most 65536 bytes with no regard for character boundaries. Past
//! that size every error string Go produced ended in a broken UTF-8 sequence.
//!
//! Separate binary: registers a process-global engine handler.

#![allow(unsafe_code)]

use gusset::ffi::status::{FfiStatus, FFI_ERR, FFI_OK};
use gusset::ffi::{
    gusset_handle_close, gusset_handle_open, gusset_status_free, gusset_submit, gusset_take,
};
use gusset::header::CallHeader;
use gusset::{set_engine_handler, Handle};
use std::ptr;

#[path = "../../../tests/common/mod.rs"]
mod common;
use common::{make_pipe, read_ticket};

/// The most bytes Go's `statusToError` reads from a status message.
const GO_STATUS_MSG_CAP: usize = 65536;

#[test]
fn a_huge_engine_error_is_capped_on_a_character_boundary() {
    // 3-byte characters: 65536 is not a multiple of 3, so Go's cap splits one.
    set_engine_handler(|_ctx, input: &[u8]| -> Result<Vec<u8>, String> {
        Err("€".repeat(input.len() * 1000))
    });

    let (r, w) = make_pipe();
    let mut handle: *mut Handle = ptr::null_mut();
    let code = unsafe { gusset_handle_open(1, w, &mut handle, ptr::null_mut()) };
    assert_eq!(code, FFI_OK);

    let header = CallHeader::default();
    let input = [0u8; 40]; // 40_000 characters, 120_000 bytes
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

    let mut st = FfiStatus::ok();
    let mut id = 0u64;
    let mut p: *mut u8 = ptr::null_mut();
    let mut len = 0usize;
    let code = unsafe { gusset_take(handle, ticket, &mut id, &mut p, &mut len, &mut st) };
    assert_eq!(code, FFI_ERR);
    assert_eq!(st.code, FFI_ERR);

    let msg = unsafe { std::slice::from_raw_parts(st.msg, st.msg_len) }.to_vec();
    unsafe { gusset_status_free(&mut st) };

    assert!(
        msg.len() <= GO_STATUS_MSG_CAP,
        "status message is {} bytes; Go reads at most {} and cuts wherever that lands",
        msg.len(),
        GO_STATUS_MSG_CAP
    );
    let text = match std::str::from_utf8(&msg) {
        Ok(t) => t,
        Err(e) => panic!("status message is not valid UTF-8: {e}"),
    };
    assert!(text.starts_with("€€€"), "the message must keep its head");
    assert!(
        text.ends_with("[truncated]"),
        "a capped message must say it was capped"
    );

    let code = unsafe { gusset_handle_close(handle, ptr::null_mut()) };
    assert_eq!(code, FFI_OK);
    unsafe { libc::close(r) };
}
