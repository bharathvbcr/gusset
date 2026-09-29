//! A NULL `status` must not change the code an export returns (I2).
//!
//! Every export documents `status` as optional ("if non-null"). Each one used to
//! end in `if ok { FFI_OK } else if !status.is_null() { (*status).code } else {
//! FFI_BAD_ARG }`: with no status to read the code back from, every failure
//! collapsed to `FFI_BAD_ARG`. A C host that passes NULL then saw a panicking
//! job's `gusset_take` as a bad argument, not `FFI_PANIC`, and never learned
//! the handle was poisoned — the one signal I2 exists to deliver.
//!
//! Each case runs twice, with and without a status, and the codes must match.
//!
//! Separate binary: the diagnostic engine is used, which requires that no
//! process-global engine handler is registered.

#![allow(unsafe_code)]

use gusset::ffi::status::{FfiStatus, FFI_ERR, FFI_OK, FFI_PANIC};
use gusset::ffi::{
    gusset_handle_close, gusset_handle_open, gusset_status_free, gusset_submit, gusset_take,
};
use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::Handle;
use std::ptr;

fn make_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    if unsafe { libc::pipe(fds.as_mut_ptr()) } != 0 {
        panic!("pipe failed");
    }
    (fds[0], fds[1])
}

fn read_ticket(fd: i32) -> u64 {
    let mut buf = [0u8; 8];
    let mut got = 0usize;
    while got < 8 {
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        if unsafe { libc::poll(&mut pfd, 1, 5000) } <= 0 {
            panic!("no completion within 5s");
        }
        let n = unsafe {
            libc::read(
                fd,
                buf.as_mut_ptr().add(got) as *mut libc::c_void,
                buf.len() - got,
            )
        };
        if n <= 0 {
            panic!("completion pipe read failed");
        }
        got += n as usize;
    }
    u64::from_ne_bytes(buf)
}

/// Runs `call` with a real status, frees it, then with NULL; returns both codes.
fn both(call: impl Fn(*mut FfiStatus) -> i32) -> (i32, i32) {
    let mut st = FfiStatus::ok();
    let with = call(&mut st);
    unsafe { gusset_status_free(&mut st) };
    let without = call(ptr::null_mut());
    (with, without)
}

struct Open {
    handle: *mut Handle,
    read_fd: i32,
}

fn open(pool: u32) -> Open {
    let (r, w) = make_pipe();
    let mut handle: *mut Handle = ptr::null_mut();
    let mut st = FfiStatus::ok();
    let code = unsafe { gusset_handle_open(pool, w, &mut handle, &mut st) };
    if code != FFI_OK {
        panic!("open failed with {code}");
    }
    Open { handle, read_fd: r }
}

fn close(o: Open) {
    let code = unsafe { gusset_handle_close(o.handle, ptr::null_mut()) };
    assert_eq!(code, FFI_OK);
    unsafe { libc::close(o.read_fd) };
}

fn submit(o: &Open, input: &[u8]) -> u64 {
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let mut ticket = 0u64;
    let code = unsafe {
        gusset_submit(
            o.handle,
            &header,
            input.as_ptr(),
            input.len(),
            0,
            &mut ticket,
            ptr::null_mut(),
        )
    };
    if code != FFI_OK {
        panic!("submit failed with {code}");
    }
    ticket
}

#[test]
fn handle_open_failure_reports_the_same_code_without_a_status() {
    let (with, without) = both(|st| {
        let mut h: *mut Handle = ptr::null_mut();
        // A negative fd is refused by Handle::open, after the null checks.
        unsafe { gusset_handle_open(1, -1, &mut h, st) }
    });
    assert_eq!(with, FFI_ERR, "control: the status path reports FFI_ERR");
    assert_eq!(
        without, with,
        "gusset_handle_open with a NULL status returned {without}, with a status {with}"
    );
}

#[test]
fn take_of_an_unknown_ticket_reports_the_same_code_without_a_status() {
    let o = open(1);
    let (with, without) = both(|st| {
        let mut id = 0u64;
        let mut p: *mut u8 = ptr::null_mut();
        let mut len = 0usize;
        unsafe { gusset_take(o.handle, 0xDEAD_BEEF, &mut id, &mut p, &mut len, st) }
    });
    assert_eq!(with, FFI_ERR, "control: the status path reports FFI_ERR");
    assert_eq!(
        without, with,
        "gusset_take with a NULL status returned {without}, with a status {with}"
    );
    close(o);
}

#[test]
fn take_of_a_panicked_job_reports_ffi_panic_without_a_status() {
    let o = open(1);
    // Diagnostic mode 1 panics. The pipe carries the bare ticket.
    let ticket = submit(&o, &[1]);
    assert_eq!(read_ticket(o.read_fd), ticket);

    let mut id = 0u64;
    let mut p: *mut u8 = ptr::null_mut();
    let mut len = 0usize;
    let code = unsafe { gusset_take(o.handle, ticket, &mut id, &mut p, &mut len, ptr::null_mut()) };
    assert_eq!(
        code, FFI_PANIC,
        "a C host that passes a NULL status must still learn the job panicked (I2); got {code}"
    );
    close(o);
}
