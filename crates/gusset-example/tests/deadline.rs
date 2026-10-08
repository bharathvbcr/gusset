//! An engine that notices its own deadline is reported as a deadline (I3).
//!
//! The documented adopter pattern stops on a failed `ctx.check()` and returns
//! `Err`. The runtime mapped every engine `Err` to `JobResult::Err`, and Go
//! recognises a cancel only by the runtime's exact message, so with the
//! example engine's own wording ("deadline exceeded") `errors.Is(err,
//! context.DeadlineExceeded)` was false. The runtime now re-checks the context
//! when an engine returns `Err` and reports `Cancelled(reason)`, whose message
//! Go maps (`TestPitfall_FFICancelMapsToContextError` covers that hop).
//!
//! Its own test binary: `set_engine_handler` is process-global.

use gusset::ffi::status::FfiStatus;
use gusset::ffi::{gusset_status_free, gusset_take};
use gusset::header::CallHeader;
use gusset::pool::{Handle, JobResult};
use gusset::{CancelReason, FFI_ERR};
use gusset_example::init_example_engine;

mod common;
use common::{make_pipe, read_ticket};

/// Large enough that summing it takes far longer than [`TIMEOUT_NS`] even in a
/// release build, so the deadline lands while the engine is running rather
/// than before it is dequeued. If dequeue ever took longer than the timeout,
/// the runtime's own pre-check would report `Cancelled` and the engine path
/// would go untested — on an idle one-worker pool that is implausible.
const PAYLOAD: usize = 128 << 20;
const TIMEOUT_NS: u64 = 1_000_000;

/// Submits `input` as a Buffer under a 1 ms deadline, waits for the ticket and
/// frees the input buffer once the job has completed.
fn submit_under_deadline(handle: &Handle, read_fd: i32, opcode: u32, input: &[u8]) -> u64 {
    let (id, ptr) = match handle.buf_alloc(input.len()) {
        Ok(v) => v,
        Err(e) => panic!("buf_alloc failed: {e}"),
    };
    // SAFETY: `ptr` is a fresh allocation of `input.len()` bytes.
    unsafe { std::ptr::copy_nonoverlapping(input.as_ptr(), ptr, input.len()) };
    let header = CallHeader {
        timeout_ns: TIMEOUT_NS,
        reserved: opcode,
        ..Default::default()
    };
    let ticket = match handle.submit(header, &[], id) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {e}"),
    };
    assert_eq!(read_ticket(read_fd), ticket);
    assert!(handle.buf_free(id).is_ok());
    ticket
}

#[test]
fn an_engine_stopping_on_its_own_deadline_reports_deadline_exceeded() {
    init_example_engine();
    // An engine with its own wording, registered by opcode: the runtime must not
    // depend on an engine choosing the canonical message.
    const OWN_WORDING: u32 = 77;
    let registered = gusset::register_engine(OWN_WORDING, |ctx, input| {
        for chunk in input.chunks(4096) {
            if ctx.check().is_err() {
                return Err("deadline exceeded".to_string());
            }
            std::hint::black_box(chunk.iter().map(|&b| b as u64).sum::<u64>());
        }
        Ok(Vec::new())
    });
    assert!(registered.is_ok());

    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {e}"),
    };

    // Opcode 10 is selected by the first payload byte.
    let mut payload = vec![7u8; PAYLOAD];
    payload[0] = 10;
    let ticket = submit_under_deadline(&handle, r, 0, &payload);
    match handle.take(ticket) {
        Ok(JobResult::Cancelled(CancelReason::DeadlineExceeded)) => {}
        other => {
            panic!("opcode 10 under a deadline: want Cancelled(DeadlineExceeded), got {other:?}")
        }
    }

    let ticket = submit_under_deadline(&handle, r, OWN_WORDING, &payload);
    match handle.take(ticket) {
        Ok(JobResult::Cancelled(CancelReason::DeadlineExceeded)) => {}
        other => panic!(
            "an engine's own deadline wording: want Cancelled(DeadlineExceeded), got {other:?}"
        ),
    }

    // Across the boundary: the status message is the exact string Go's
    // Error.Is maps to context.DeadlineExceeded, with no fabricated location.
    let ticket = submit_under_deadline(&handle, r, 0, &payload);
    let raw = std::sync::Arc::as_ptr(&handle) as *mut Handle;
    let (mut buf_id, mut out_ptr, mut out_len) = (0u64, std::ptr::null_mut(), 0usize);
    let mut st = FfiStatus::ok();
    // SAFETY: `raw` is a live handle and every out pointer is a valid local.
    let rc = unsafe {
        gusset_take(
            raw,
            ticket,
            &mut buf_id,
            &mut out_ptr,
            &mut out_len,
            &mut st,
        )
    };
    assert_eq!(rc, FFI_ERR);
    let msg = unsafe { std::slice::from_raw_parts(st.msg, st.msg_len) };
    assert_eq!(msg, b"cancelled: DeadlineExceeded");
    assert!(
        st.file.is_null() && st.line == 0,
        "a cancel has no source location"
    );
    // SAFETY: `st` was initialised by gusset_take.
    unsafe { gusset_status_free(&mut st) };

    assert!(!handle.is_poisoned());
    assert!(handle.close().is_ok());
    // SAFETY: `r` is the read end this test opened.
    unsafe { libc::close(r) };
}
