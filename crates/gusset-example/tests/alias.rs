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

const OPCODE_RETURN_CAPTURED: u32 = 9103;
const OPCODE_HOLD_INPUT: u32 = 9104;
const OPCODE_RETURN_NAMED: u32 = 9105;

/// Two results must never share one buffer.
///
/// An engine returning a pre-allocated id it captured — the pattern the opcode
/// example teaches — hands that same id to every call. Go frees a take buffer
/// once it has copied a result out, so the second result was read from freed
/// memory: the drain loop takes both completions back to back, and both takes
/// resolved the same live pointer before either waiter freed it.
#[test]
fn one_buffer_is_returned_as_an_output_at_most_once() {
    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };
    let (shared, _) = match handle.buf_alloc(16) {
        Ok(v) => v,
        Err(e) => panic!("buf_alloc failed: {}", e),
    };
    gusset::pool::register_engine(OPCODE_RETURN_CAPTURED, move |_ctx, _input: &[u8]| {
        Ok::<_, String>(JobOutput::Buffer(shared))
    });

    let header = CallHeader {
        reserved: OPCODE_RETURN_CAPTURED,
        ..Default::default()
    };
    let t1 = match handle.submit(header, &[], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    let t2 = match handle.submit(header, &[], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    // Both complete before either is taken: the drain loop's real ordering.
    let _ = read_ticket(r);
    let _ = read_ticket(r);

    let mut owners = 0;
    let mut refusals = 0;
    for t in [t1, t2] {
        match handle.take(t) {
            Ok(JobResult::Buffer(id)) => {
                assert_eq!(id, shared);
                owners += 1;
            }
            Ok(JobResult::Err(msg)) => {
                assert!(
                    msg.contains("already"),
                    "refusal must say why, got: {}",
                    msg
                );
                refusals += 1;
            }
            Ok(other) => panic!("unexpected result {:?}", std::mem::discriminant(&other)),
            Err(e) => panic!("take failed: {}", e),
        }
    }
    assert_eq!(
        (owners, refusals),
        (1, 1),
        "buffer {} was handed out as the output of two calls; freeing one leaves the \
         other reading freed memory",
        shared
    );

    let _ = handle.buf_free(shared);
    handle.close();
    // SAFETY: the read end is still owned by this test; close() took the write end.
    unsafe {
        libc::close(r);
    }
}

/// A buffer another unit is still reading as its input is not an output.
///
/// The input-alias check covers only the unit's own input. A second unit could
/// return the first unit's input, and the caller would free it through the
/// result while the first engine was still reading it.
#[test]
fn a_buffer_in_use_as_another_units_input_is_refused_as_an_output() {
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;
    use std::time::{Duration, Instant};

    let release = Arc::new(AtomicBool::new(false));
    let entered = Arc::new(AtomicBool::new(false));
    {
        let release = Arc::clone(&release);
        let entered = Arc::clone(&entered);
        gusset::pool::register_engine(OPCODE_HOLD_INPUT, move |_ctx, input: &[u8]| {
            entered.store(true, Ordering::Release);
            let until = Instant::now() + Duration::from_secs(5);
            while !release.load(Ordering::Acquire) && Instant::now() < until {
                std::thread::sleep(Duration::from_millis(1));
            }
            Ok::<_, String>(vec![input.len() as u8])
        });
    }
    gusset::pool::register_engine(OPCODE_RETURN_NAMED, |_ctx, input: &[u8]| {
        let mut id = [0u8; 8];
        let n = input.len().min(8);
        id[..n].copy_from_slice(&input[..n]);
        Ok::<_, String>(JobOutput::Buffer(u64::from_le_bytes(id)))
    });

    let (r, w) = make_pipe();
    let handle = match Handle::open(2, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };
    let (held, _) = match handle.buf_alloc(32) {
        Ok(v) => v,
        Err(e) => panic!("buf_alloc failed: {}", e),
    };

    let hold = CallHeader {
        reserved: OPCODE_HOLD_INPUT,
        ..Default::default()
    };
    let t_hold = match handle.submit(hold, &[], held) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    let spin_until = Instant::now() + Duration::from_secs(5);
    while !entered.load(Ordering::Acquire) {
        assert!(Instant::now() < spin_until, "holding engine never started");
        std::thread::sleep(Duration::from_millis(1));
    }

    let steal = CallHeader {
        reserved: OPCODE_RETURN_NAMED,
        ..Default::default()
    };
    let t_steal = match handle.submit(steal, &held.to_le_bytes(), 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    assert_eq!(read_ticket(r), t_steal);
    match handle.take(t_steal) {
        Ok(JobResult::Err(msg)) => assert!(
            msg.contains("input"),
            "refusal must name the in-flight input, got: {}",
            msg
        ),
        Ok(JobResult::Buffer(id)) => panic!(
            "buffer {} was accepted as an output while another unit reads it as input",
            id
        ),
        Ok(other) => panic!("unexpected result {:?}", std::mem::discriminant(&other)),
        Err(e) => panic!("take failed: {}", e),
    }

    release.store(true, Ordering::Release);
    assert_eq!(read_ticket(r), t_hold);
    match handle.take(t_hold) {
        Ok(JobResult::Ok(out)) => assert_eq!(out, vec![32]),
        Ok(other) => panic!("unexpected result {:?}", std::mem::discriminant(&other)),
        Err(e) => panic!("take failed: {}", e),
    }

    let _ = handle.buf_free(held);
    handle.close();
    // SAFETY: the read end is still owned by this test; close() took the write end.
    unsafe {
        libc::close(r);
    }
}
