use super::{drain_ticket, make_pipe, Must, REGISTRY_TEST_LOCK};
use crate::header::CallHeader;
use crate::pool::{
    clear_engine_handlers, has_engine_handler, lock_recover, register_engine, Handle, JobResult,
};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::thread;

/// Opcode 0 never reaches the registry. Registering it must fail, and the
/// handler must not become the one opcode 0 actually runs.
#[test]
#[cfg_attr(miri, ignore)]
fn register_engine_rejects_opcode_zero() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    let marker = Arc::new(AtomicBool::new(false));
    let seen = Arc::clone(&marker);
    let err = match register_engine(0, move |_ctx, _input: &[u8]| {
        seen.store(true, Ordering::Release);
        Ok(Vec::<u8>::new())
    }) {
        Ok(()) => panic!("opcode 0 was reported as registered"),
        Err(e) => e,
    };
    assert!(
        err.contains("opcode 0"),
        "refusal must name opcode 0, got: {err}"
    );

    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");
    let header = CallHeader::default();
    let ticket = handle.submit(header, b"nope", 0).must("submit");
    assert_eq!(drain_ticket(r), ticket);
    match handle.take(ticket).must("take") {
        JobResult::Err(msg) => assert!(
            msg.contains("no engine handler registered"),
            "opcode 0 must not run the refused handler, got: {msg}"
        ),
        other => panic!("opcode 0 ran a handler: {other:?}"),
    }
    assert!(
        !marker.load(Ordering::Acquire),
        "the refused opcode-0 handler ran"
    );
    handle.close().must("close");
    unsafe {
        libc::close(r);
    }
}

/// A second `register_engine` for the same opcode must not replace the first.
#[test]
#[cfg_attr(miri, ignore)]
fn register_engine_does_not_overwrite_an_opcode() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    const OPCODE: u32 = 9301;
    // A previous run of this test in the same process (no process-per-test)
    // already holds the opcode. `clear_engine_handlers` is the documented
    // replacement, and this lock keeps that from racing other tests.
    clear_engine_handlers();
    register_engine(OPCODE, |_ctx, _input: &[u8]| Ok(vec![1u8])).must("first");
    let second = register_engine(OPCODE, |_ctx, _input: &[u8]| Ok(vec![2u8]));
    match second {
        Ok(()) => panic!("the second registration replaced the first"),
        Err(e) => assert!(e.contains("already registered"), "got: {e}"),
    }

    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");
    let header = CallHeader {
        reserved: OPCODE,
        ..Default::default()
    };
    let ticket = handle.submit(header, b"x", 0).must("submit");
    assert_eq!(drain_ticket(r), ticket);
    match handle.take(ticket).must("take") {
        JobResult::Ok(bytes) => assert_eq!(bytes, vec![1], "the first handler must still run"),
        other => panic!("unexpected result: {other:?}"),
    }
    handle.close().must("close");
    unsafe {
        libc::close(r);
    }
}

/// Poison is per handle. The engine hook is process-global and stays
/// installed; a second handle still runs it. Clearing the hook (the
/// existing API) drops that state, and the next handle does not work
/// until the adopter registers again. The poisoned handle stays poisoned.
#[test]
#[cfg_attr(miri, ignore)]
fn a_panic_poisons_one_handle_and_leaves_the_global_engine() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    const OPCODE: u32 = 9303;
    clear_engine_handlers();
    register_engine(OPCODE, |_ctx, input: &[u8]| {
        if input == b"poison-this-handle" {
            panic!("adopter engine fault");
        }
        Ok(input.to_vec())
    })
    .must("register");

    let (r1, w1) = make_pipe();
    let poisoned = Handle::open(1, w1).must("open poisoned");
    let header = CallHeader {
        reserved: OPCODE,
        ..Default::default()
    };
    let ticket = poisoned
        .submit(header, b"poison-this-handle", 0)
        .must("submit");
    assert_eq!(drain_ticket(r1), ticket);
    assert!(
        matches!(poisoned.take(ticket).must("take"), JobResult::Panic { .. }),
        "the engine panic must come back as a panic result"
    );
    assert!(
        poisoned.is_poisoned(),
        "the panicking handle must be poisoned"
    );
    assert!(
        has_engine_handler(),
        "the process-global engine must still be installed after a handle is poisoned"
    );

    let (r2, w2) = make_pipe();
    let live = Handle::open(1, w2).must("open live");
    let ticket = live.submit(header, b"still-here", 0).must("submit live");
    assert_eq!(drain_ticket(r2), ticket);
    match live.take(ticket).must("take live") {
        JobResult::Ok(bytes) => assert_eq!(bytes, b"still-here"),
        other => {
            panic!("the global engine did not survive the other handle's panic: {other:?}")
        }
    }

    clear_engine_handlers();
    assert!(
        !has_engine_handler(),
        "clear_engine_handlers must drop the hook the adopter installed"
    );
    let ticket = live.submit(header, b"again", 0).must("submit after clear");
    assert_eq!(drain_ticket(r2), ticket);
    match live.take(ticket).must("take after clear") {
        JobResult::Err(msg) => assert!(
            msg.contains("no engine handler registered"),
            "the adopter must reinstall explicitly, got: {msg}"
        ),
        other => panic!("cleared engine still ran: {other:?}"),
    }

    register_engine(OPCODE, |_ctx, input: &[u8]| Ok(input.to_vec())).must("reinstall");
    let ticket = live.submit(header, b"back", 0).must("submit reinstalled");
    assert_eq!(drain_ticket(r2), ticket);
    match live.take(ticket).must("take reinstalled") {
        JobResult::Ok(bytes) => assert_eq!(bytes, b"back"),
        other => panic!("reinstalled engine did not run: {other:?}"),
    }
    match poisoned.submit(header, b"no", 0) {
        Err(e) => assert!(e.contains("poisoned"), "got: {e}"),
        Ok(t) => panic!("poisoned handle accepted ticket {t}"),
    }

    poisoned.close().must("close poisoned");
    live.close().must("close live");
    unsafe {
        libc::close(r1);
        libc::close(r2);
    }
}

/// `close` must return while a worker is still inside an engine that
/// ignores cancellation, and must not report that the join succeeded.
#[test]
#[cfg_attr(miri, ignore)]
fn close_returns_while_a_worker_ignores_cancel() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    const OPCODE: u32 = 9302;
    clear_engine_handlers();
    let entered = Arc::new(AtomicBool::new(false));
    let release = Arc::new(AtomicBool::new(false));
    let entered_c = Arc::clone(&entered);
    let release_c = Arc::clone(&release);
    register_engine(OPCODE, move |_ctx, _input: &[u8]| {
        entered_c.store(true, Ordering::Release);
        while !release_c.load(Ordering::Acquire) {
            thread::sleep(std::time::Duration::from_millis(10));
        }
        Ok(Vec::<u8>::new())
    })
    .must("register");

    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");
    let header = CallHeader {
        reserved: OPCODE,
        ..Default::default()
    };
    handle.submit(header, b"stuck", 0).must("submit");
    let started = std::time::Instant::now();
    while !entered.load(Ordering::Acquire) {
        assert!(
            started.elapsed() < std::time::Duration::from_secs(2),
            "the engine never started"
        );
        thread::sleep(std::time::Duration::from_millis(5));
    }

    let closing = Arc::clone(&handle);
    let (tx, rx) = std::sync::mpsc::channel();
    thread::spawn(move || {
        let _ = tx.send(closing.close_within(std::time::Duration::from_millis(200)));
    });
    let outcome = rx.recv_timeout(std::time::Duration::from_secs(2));
    // Unblock the worker even when close failed to return, so this test
    // cannot leave a thread behind for the rest of the binary.
    release.store(true, Ordering::Release);
    match outcome {
        Ok(Err(msg)) => assert!(
            msg.contains("workers still running"),
            "close must report the expired budget, got: {msg}"
        ),
        Ok(Ok(())) => panic!("close joined a worker that was still inside the engine"),
        Err(_) => panic!("close did not return within 2s; the join is unbounded"),
    }

    let settle = std::time::Instant::now();
    while handle.in_flight() != 0 && settle.elapsed() < std::time::Duration::from_secs(2) {
        thread::sleep(std::time::Duration::from_millis(10));
    }
    handle.close().must("second close");
    unsafe {
        libc::close(r);
    }
}
