use super::{make_pipe, read_ticket, Must, INJECT_LOCK, REGISTRY_TEST_LOCK};
use crate::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use crate::pool::{
    lock_recover, register_engine, reserve_id, Handle, JobOutput, JobResult, ID_CEILING,
    MAX_INLINE_INPUT,
};
use std::sync::atomic::{AtomicU64, Ordering};

/// A submit that cannot queue the work unit must not leave a cancel flag
/// behind. `in_flight` is the flag count; `gusset_shutdown` waits on it, so a
/// leaked flag after a failed send makes a clean drain impossible.
///
/// The production path inserts the flag and then looks up the sender. If the
/// sender is already gone, that insert is a leak the worker will never clear.
#[test]
#[cfg_attr(miri, ignore)]
fn submit_does_not_leave_a_cancel_flag_when_the_sender_is_gone() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    lock_recover(&handle.sender).take();

    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    if let Ok(ticket) = handle.submit(header, &[0u8], 0) {
        panic!(
            "submit must fail once the sender is gone, queued ticket {}",
            ticket
        );
    }

    assert_eq!(
        handle.in_flight(),
        0,
        "a failed submit must not leak a cancel flag; shutdown drain waits on in_flight"
    );

    handle.close().must("close");
    // SAFETY: the read end is still owned by this test; close() took the write end.
    unsafe {
        libc::close(r);
    }
}

/// Advancing an id counter past the 63-bit ceiling wraps it onto ids that are
/// still live: a wrapped buffer id reuses buffer 1, a wrapped ticket aliases
/// an in-flight job and the completion pipe wakes the wrong waiter.
/// `fetch_add` wraps even when the call then returns an error.
///
/// Buffer ids and tickets both come from process-wide counters, which a
/// parallel test must not push to the ceiling, and both reserve through
/// `reserve_id`, so the ceiling rule is tested there on a local counter.
#[test]
fn ids_stop_at_the_ceiling_instead_of_wrapping() {
    let counter = AtomicU64::new(ID_CEILING);
    assert!(
        reserve_id(&counter).is_err(),
        "an id at the ceiling must be refused"
    );
    assert_eq!(
        counter.load(Ordering::Relaxed),
        ID_CEILING,
        "a refused id must not advance the counter; the next success would wrap onto id 1"
    );
    let counter = AtomicU64::new(ID_CEILING - 1);
    assert_eq!(reserve_id(&counter).ok(), Some(ID_CEILING - 1));
    assert!(
        reserve_id(&counter).is_err(),
        "the last id is below the ceiling"
    );
}

#[test]
#[cfg_attr(miri, ignore)]
fn tickets_are_unique_across_handles() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let (ra, wa) = make_pipe();
    let (rb, wb) = make_pipe();
    let a = match Handle::open(1, wa) {
        Ok(h) => h,
        Err(e) => panic!("open a: {e}"),
    };
    let b = match Handle::open(1, wb) {
        Ok(h) => h,
        Err(e) => panic!("open b: {e}"),
    };
    let mut seen = std::collections::HashSet::new();
    for _ in 0..8 {
        for (h, r) in [(&a, ra), (&b, rb)] {
            let t = match h.submit(header, &[0u8], 0) {
                Ok(t) => t,
                Err(e) => panic!("submit: {e}"),
            };
            assert_eq!(read_ticket(r), t);
            assert!(seen.insert(t), "ticket {t} was issued by two handles");
        }
    }
    a.close().must("close");
    b.close().must("close");
    unsafe {
        libc::close(ra);
        libc::close(rb);
    }
}

/// `SyncSender::send` blocks when the queue is full. Held across the sender
/// lock, that blocks `close` forever; released before the send, it reopens
/// the cancel-flag leak. `try_send` fails in microseconds and leaves
/// `in_flight` unchanged.
#[test]
#[cfg_attr(miri, ignore)]
fn submit_on_a_full_queue_fails_without_blocking_or_leaking() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    // Every accepted job sleeps. One worker cannot drain them before the
    // bounded queue fills, so a correct submit refuses instead of blocking
    // inside `send` until a worker finishes.
    let mut accepted = 0usize;
    let overall = std::time::Instant::now();
    loop {
        if overall.elapsed() > std::time::Duration::from_millis(500) {
            panic!("submit never refused a full queue; accepted {accepted}");
        }
        let started = std::time::Instant::now();
        match handle.submit(header, &[9, 30], 0) {
            Ok(_) => {
                let elapsed = started.elapsed();
                assert!(
                    elapsed < std::time::Duration::from_millis(200),
                    "submit blocked for {:?} instead of queueing or refusing",
                    elapsed
                );
                accepted += 1;
            }
            Err(e) => {
                let elapsed = started.elapsed();
                assert!(
                    elapsed < std::time::Duration::from_millis(200),
                    "a full queue must fail without blocking; took {:?}",
                    elapsed
                );
                assert!(
                    e.contains("full"),
                    "expected a full-queue refusal, got: {e}"
                );
                assert_eq!(
                    handle.in_flight(),
                    accepted,
                    "the refused submit must not leave a cancel flag"
                );
                break;
            }
        }
    }
    assert!(accepted > 0, "the queue should accept at least one job");

    handle.close().must("close");
    unsafe {
        libc::close(r);
    }
}

/// Large `JobResult::Ok` used to be copied into a fresh `RawBuffer` inside
/// `gusset_take` — on the cgo thread, with an M pinned for the memcpy.
/// Promoting on the worker keeps take() a pointer return (R16 egress).
#[test]
#[cfg_attr(miri, ignore)]
fn large_ok_result_is_promoted_off_the_cgo_thread() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    let len = MAX_INLINE_INPUT + 1;
    let (buf_id, ptr) = match handle.buf_alloc(len) {
        Ok(v) => v,
        Err(e) => panic!("buf_alloc failed: {}", e),
    };
    unsafe {
        std::ptr::write(ptr, 0u8);
        for i in 1..len {
            std::ptr::write(ptr.add(i), (i % 256) as u8);
        }
    }

    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let ticket = match handle.submit(header, &[], buf_id) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    assert_eq!(read_ticket(r), ticket);

    let result = match handle.take(ticket) {
        Ok(r) => r,
        Err(e) => panic!("take failed: {}", e),
    };
    match result {
        JobResult::Buffer(id) => {
            assert_ne!(id, buf_id, "echo must not alias the input buffer id");
            let (out_ptr, out_len) = match handle.buf_get(id) {
                Ok(v) => v,
                Err(e) => panic!("buf_get failed: {}", e),
            };
            assert_eq!(out_len, len);
            unsafe {
                assert_eq!(*out_ptr, 0);
                assert_eq!(*out_ptr.add(100), 100u8);
            }
            let _ = handle.buf_free(id);
        }
        JobResult::Ok(data) => panic!(
            "large JobResult::Ok must be promoted to Buffer on the worker, still Ok ({} bytes)",
            data.len()
        ),
        other => panic!(
            "large JobResult::Ok must be promoted to Buffer on the worker, got {:?}",
            std::mem::discriminant(&other)
        ),
    }

    let _ = handle.buf_free(buf_id);
    handle.close().must("close");
    unsafe {
        libc::close(r);
    }
}

/// `NewBuffer` reaches Rust through `gusset_buf_alloc`. That export, not
/// `Handle::buf_alloc`, is what must mark the slot caller-held: an engine
/// returning the id is otherwise accepted, and the waiter's free releases
/// memory Go still views.
#[test]
#[cfg_attr(miri, ignore)]
fn gusset_buf_alloc_marks_the_buffer_caller_held() {
    use crate::ffi::gusset_buf_alloc;
    use crate::ffi::status::{FfiStatus, FFI_OK};

    const OPCODE_RETURN_EXPORTED: u32 = 9201;
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    register_engine(OPCODE_RETURN_EXPORTED, |_ctx, input: &[u8]| {
        let mut raw = [0u8; 8];
        let n = input.len().min(8);
        raw[..n].copy_from_slice(&input[..n]);
        Ok::<_, String>(JobOutput::Buffer(u64::from_le_bytes(raw)))
    })
    .must("register opcode");

    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };
    // SAFETY: the Arc stays alive for the call. The export only reads the handle.
    let raw = std::sync::Arc::as_ptr(&handle) as *mut Handle;
    let mut id = 0u64;
    let mut ptr = std::ptr::null_mut();
    let mut status = FfiStatus::ok();
    // SAFETY: `raw` is a live handle; the out-params are local and writable.
    let rc = unsafe { gusset_buf_alloc(raw, 32, &mut id, &mut ptr, &mut status) };
    assert_eq!(
        rc, FFI_OK,
        "gusset_buf_alloc failed with code {}",
        status.code
    );
    assert!(!ptr.is_null());
    // SAFETY: the export just returned a 32-byte buffer.
    unsafe {
        std::ptr::write(ptr, 0x5A);
    }

    let header = CallHeader {
        reserved: OPCODE_RETURN_EXPORTED,
        ..Default::default()
    };
    let ticket = match handle.submit(header, &id.to_le_bytes(), 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    assert_eq!(read_ticket(r), ticket);

    match handle.take(ticket) {
        Ok(JobResult::Err(msg)) => assert!(
            msg.contains("caller still holds"),
            "refusal must name the caller's view, got: {}",
            msg
        ),
        Ok(JobResult::Buffer(got)) => {
            panic!("gusset_buf_alloc buffer {} was accepted as an output", got)
        }
        Ok(other) => panic!("unexpected result {:?}", std::mem::discriminant(&other)),
        Err(e) => panic!("take failed: {}", e),
    }

    match handle.buf_get(id) {
        Ok((p, len)) => {
            assert_eq!(len, 32);
            // SAFETY: buf_get just confirmed the allocation is live.
            let first = unsafe { std::ptr::read(p) };
            assert_eq!(first, 0x5A, "the exported buffer was disturbed");
        }
        Err(e) => panic!("exported buffer was freed despite the refusal: {}", e),
    }

    let _ = handle.buf_free(id);
    handle.close().must("close");
    unsafe {
        libc::close(r);
    }
}

/// The engine registry is one per process, so an engine can hold a buffer id
/// minted by another handle. With per-handle counters handle B also had a
/// buffer under that number: B accepted the foreign id as its own output, B's
/// caller received B's bytes with no error, and A's buffer was never returned.
/// A foreign id must be unknown on every handle but its own.
#[test]
#[cfg_attr(miri, ignore)]
fn a_foreign_handles_buffer_id_is_refused_as_an_output() {
    const OPCODE_RETURN_FOREIGN: u32 = 9202;
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    register_engine(OPCODE_RETURN_FOREIGN, |_ctx, input: &[u8]| {
        let mut raw = [0u8; 8];
        let n = input.len().min(8);
        raw[..n].copy_from_slice(&input[..n]);
        Ok::<_, String>(JobOutput::Buffer(u64::from_le_bytes(raw)))
    })
    .must("register opcode");

    let (ra, wa) = make_pipe();
    let (rb, wb) = make_pipe();
    let a = match Handle::open(1, wa) {
        Ok(h) => h,
        Err(e) => panic!("open a: {e}"),
    };
    let b = match Handle::open(1, wb) {
        Ok(h) => h,
        Err(e) => panic!("open b: {e}"),
    };
    let fill = |h: &Handle, byte: u8| match h.buf_alloc(4) {
        Ok((id, ptr)) => {
            // SAFETY: buf_alloc just returned a live 4-byte buffer.
            unsafe { std::ptr::write_bytes(ptr, byte, 4) };
            id
        }
        Err(e) => panic!("buf_alloc: {e}"),
    };
    let a_id = fill(&a, b'A');
    let b_id = fill(&b, b'B');

    let header = CallHeader {
        reserved: OPCODE_RETURN_FOREIGN,
        ..Default::default()
    };
    let ticket = match b.submit(header, &a_id.to_le_bytes(), 0) {
        Ok(t) => t,
        Err(e) => panic!("submit: {e}"),
    };
    assert_eq!(read_ticket(rb), ticket);

    let contents = |h: &Handle, id: u64| match h.buf_get(id) {
        // SAFETY: buf_get just confirmed the allocation is live.
        Ok((p, len)) => unsafe { std::slice::from_raw_parts(p, len) }.to_vec(),
        Err(e) => panic!("buffer {id} is gone: {e}"),
    };
    match b.take(ticket) {
        Ok(JobResult::Err(msg)) => assert!(
            msg.contains("no such live buffer"),
            "refusal must say the id is not this handle's, got: {msg}"
        ),
        Ok(JobResult::Buffer(got)) => panic!(
            "handle B accepted handle A's buffer id {a_id} as its output: B's caller \
             got buffer {got} holding {:?}",
            String::from_utf8_lossy(&contents(&b, got))
        ),
        Ok(other) => panic!("unexpected result {:?}", std::mem::discriminant(&other)),
        Err(e) => panic!("take: {e}"),
    }
    assert_eq!(contents(&a, a_id), b"AAAA", "A's buffer was disturbed");
    assert_eq!(contents(&b, b_id), b"BBBB", "B's buffer was disturbed");

    let _ = a.buf_free(a_id);
    let _ = b.buf_free(b_id);
    a.close().must("close a");
    b.close().must("close b");
    unsafe {
        libc::close(ra);
        libc::close(rb);
    }
}

/// R16: a raw inline slice above 4 KiB must be refused, not memcpy'd on the
/// submit path. The Go side parks an M for the whole cgo call; a 1 MiB copy
/// here is exactly the long cgo call the submit-and-return contract forbids.
#[test]
#[cfg_attr(miri, ignore)]
fn submit_rejects_inline_input_over_4kib() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let over = vec![0u8; 4096 + 1];
    match handle.submit(header, &over, 0) {
        Ok(ticket) => panic!(
            "inline submit of {} bytes must be refused, queued ticket {}",
            over.len(),
            ticket
        ),
        Err(e) => assert!(
            e.contains("copy limit"),
            "refusal must name the copy limit, got: {}",
            e
        ),
    }

    handle.close().must("close");
    // SAFETY: the read end is still owned by this test; close() took the write end.
    unsafe {
        libc::close(r);
    }
}
