//! An engine must not return its own input buffer as its output.
//!
//! Go frees a take buffer once it has copied the result out of it. If the id it
//! frees is the *input* buffer, the caller's live `*Buffer` is released
//! underneath it and the slice `Bytes()` already handed out points at freed
//! pages — a use-after-free that an adopter opens by writing the obvious echo:
//!
//!     register_engine(op, |_ctx, _input| Ok(JobOutput::Buffer(input_id)))
//!
//! `large_ok_result_is_promoted_off_the_cgo_thread` in pool::tests asserts that
//! Gusset's own echo does not alias, which says the hazard was understood — but
//! nothing refused it, so the assertion covered one engine rather than the
//! contract. The worker now turns it into a returned error.

use gusset::header::CallHeader;
use gusset::pool::{Handle, JobOutput, JobResult};

mod common;
use common::{make_pipe, read_ticket};

/// Opcodes of their own, so registration cannot disturb a parallel test in this
/// binary the way a global `set_engine_handler` would.
const OPCODE_ALIAS_INPUT: u32 = 9101;
const OPCODE_RETURN_ZERO: u32 = 9102;

#[test]
fn engine_returning_its_input_buffer_is_refused_not_freed_under_the_caller() {
    // The engine echoes whatever id it is told to, via the payload's first 8
    // bytes. Reading the id from the input is what lets one engine cover both
    // the aliasing case and the control case below.
    gusset::pool::register_engine(OPCODE_ALIAS_INPUT, |_ctx, input: &[u8]| {
        let mut id = [0u8; 8];
        let n = input.len().min(8);
        id[..n].copy_from_slice(&input[..n]);
        Ok::<_, String>(JobOutput::Buffer(u64::from_le_bytes(id)))
    });

    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    // A real input buffer, holding its own id — so the engine returns exactly
    // the buffer the caller still owns.
    let (input_id, ptr) = match handle.buf_alloc(64) {
        Ok(v) => v,
        Err(e) => panic!("buf_alloc failed: {}", e),
    };
    // SAFETY: `ptr` is a live 64-byte buffer this handle just allocated.
    unsafe {
        let bytes = input_id.to_le_bytes();
        for (i, b) in bytes.iter().enumerate() {
            std::ptr::write(ptr.add(i), *b);
        }
    }

    let header = CallHeader {
        reserved: OPCODE_ALIAS_INPUT,
        ..Default::default()
    };
    let ticket = match handle.submit(header, &[], input_id) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    assert_eq!(read_ticket(r), ticket);

    match handle.take(ticket) {
        Ok(JobResult::Err(msg)) => {
            assert!(
                msg.contains("input buffer"),
                "the refusal must name the aliasing, got: {}",
                msg
            );
        }
        Ok(JobResult::Buffer(id)) => panic!(
            "engine aliased the input buffer ({}) as its output and was accepted; \
             the caller's buffer would be freed underneath it",
            id
        ),
        Ok(other) => panic!(
            "expected a refusal, got {:?}",
            std::mem::discriminant(&other)
        ),
        Err(e) => panic!("take failed: {}", e),
    }

    // The input buffer must still be alive: the point of refusing is that the
    // caller's memory is untouched, so it is still there to be read and freed.
    match handle.buf_get(input_id) {
        Ok((p, len)) => {
            assert_eq!(len, 64);
            // SAFETY: buf_get just confirmed this buffer is live.
            let first = unsafe { std::ptr::read(p) };
            assert_eq!(
                first,
                input_id.to_le_bytes()[0],
                "the input buffer's contents were disturbed"
            );
        }
        Err(e) => panic!("the input buffer was freed despite the refusal: {}", e),
    }

    let _ = handle.buf_free(input_id);
    handle.close();
    // SAFETY: the read end is still owned by this test; close() took the write end.
    unsafe {
        libc::close(r);
    }
}

/// A buffer id of 0 is never live — ids start at 1 — so an engine returning one
/// is returning a default, not a result.
#[test]
fn engine_returning_buffer_id_zero_is_refused() {
    gusset::pool::register_engine(OPCODE_RETURN_ZERO, |_ctx, _input: &[u8]| {
        Ok::<_, String>(JobOutput::Buffer(0))
    });

    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    let header = CallHeader {
        reserved: OPCODE_RETURN_ZERO,
        ..Default::default()
    };
    let ticket = match handle.submit(header, &[1, 2, 3], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    assert_eq!(read_ticket(r), ticket);

    match handle.take(ticket) {
        Ok(JobResult::Err(msg)) => assert!(
            msg.contains("never a live buffer"),
            "the refusal must name why 0 is invalid, got: {}",
            msg
        ),
        Ok(other) => panic!(
            "buffer id 0 must be refused, got {:?}",
            std::mem::discriminant(&other)
        ),
        Err(e) => panic!("take failed: {}", e),
    }

    handle.close();
    // SAFETY: the read end is still owned by this test; close() took the write end.
    unsafe {
        libc::close(r);
    }
}
