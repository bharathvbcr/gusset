//! `gusset_shutdown` drain semantics.
//!
//! Its own test binary: shutdown sets a process-global flag that refuses every
//! submission, so a parallel test in the same binary would see its work rejected.
//! The tests here share that flag, so each holds [`serial`] for its whole run and
//! leaves the runtime re-armed.
//!
//! Before this change `gusset_shutdown` was `{ FFI_OK }` — it returned success
//! without draining anything, cancelling anything, or refusing anything, while the
//! README advertised "graceful shutdown and worker drain".

use gusset::ffi::status::FfiStatus;
use gusset::ffi::{gusset_shutdown, gusset_status_free, gusset_take};
use gusset::header::{CallHeader, JobContext, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::{self, Handle};
use gusset::FFI_ERR;
use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Instant;

mod common;
use common::{make_pipe, read_ticket, Must};

static SERIAL: Mutex<()> = Mutex::new(());

/// Serialises the tests of this binary: they all flip the process-wide latch.
/// A test that failed while holding the lock, possibly with the latch still set,
/// must not fail the next one too: the runtime is re-armed on the way in.
fn serial() -> MutexGuard<'static, ()> {
    let guard = SERIAL.lock().unwrap_or_else(|e| e.into_inner());
    pool::rearm();
    guard
}

/// Diagnostic mode 18: the peak of the engine's in-flight gauge since the last
/// read. A read resets the peak to the current level, so a read taken while no
/// mode 17 job is in the engine zeroes it.
fn read_engine_gauge_peak() -> u64 {
    let ctx = JobContext::new(CallHeader::default(), Arc::new(AtomicBool::new(false)));
    pool::diagnostic_dispatch(&ctx, &[18u8])
        .must("read the engine gauge")
        .get(..8)
        .map(|b| u64::from_le_bytes(b.try_into().unwrap_or([0; 8])))
        .unwrap_or(0)
}

/// How long diagnostic mode 17 holds a job at least: `input[1]` is a u8, so 250
/// ms is the most one job can hold.
const HOLD: std::time::Duration = std::time::Duration::from_millis(250);

/// How long to wait for the engine to take a job before the test gives up.
const ENGINE_START_WAIT: std::time::Duration = std::time::Duration::from_secs(10);

/// Returns once a diagnostic mode 17 job is inside the engine.
///
/// Mode 18 reads the peak of the engine's process-wide in-flight gauge since the
/// last read, which only the engine itself raises, so a peak of 1 or more means
/// a worker has dequeued a job and entered the engine. It is called here on the
/// test thread, so it needs no free worker. Bounded: a job that never starts
/// fails the test with a message instead of hanging it.
fn wait_until_the_engine_is_running() {
    let deadline = Instant::now() + ENGINE_START_WAIT;
    loop {
        if read_engine_gauge_peak() >= 1 {
            return;
        }
        assert!(
            Instant::now() < deadline,
            "no worker entered the engine within {ENGINE_START_WAIT:?}: the job was never started"
        );
        std::thread::sleep(std::time::Duration::from_millis(1));
    }
}

/// What `gusset_take` reports for `ticket`: its return code and message.
fn take_failure(handle: &Arc<Handle>, ticket: u64) -> (i32, String) {
    let handle_ptr = Arc::as_ptr(handle) as *mut Handle;
    let (mut buf_id, mut out_ptr, mut out_len) = (0u64, std::ptr::null_mut(), 0usize);
    let mut st = FfiStatus::ok();
    // SAFETY: `handle_ptr` points at a live handle for the call and every out
    // pointer is a valid local.
    let rc = unsafe {
        gusset_take(
            handle_ptr,
            ticket,
            &mut buf_id,
            &mut out_ptr,
            &mut out_len,
            &mut st,
        )
    };
    let msg = if st.msg.is_null() {
        String::new()
    } else {
        // SAFETY: a non-null `msg` is `msg_len` bytes the export allocated.
        String::from_utf8_lossy(unsafe { std::slice::from_raw_parts(st.msg, st.msg_len) })
            .into_owned()
    };
    // SAFETY: `st` was initialised by gusset_take.
    unsafe { gusset_status_free(&mut st) };
    (rc, msg)
}

/// Drains the completion pipe so workers never block writing tickets.
fn spawn_drainer(fd: i32) -> std::thread::JoinHandle<()> {
    std::thread::spawn(move || {
        let mut buf = [0u8; 8];
        loop {
            let n = unsafe { libc::read(fd, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
            if n <= 0 {
                return;
            }
        }
    })
}

fn diagnostic_header() -> CallHeader {
    CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    }
}

#[test]
fn shutdown_drains_in_flight_work_and_then_refuses_submissions() {
    let _serial = serial();
    let (r, w) = make_pipe();
    let drainer = spawn_drainer(r);

    let handle = match Handle::open(4, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    // Mode 5 is the cooperative sleep loop: 8 iterations of 10 ms, checking
    // ctx.check() between units, so cancellation can actually land.
    for _ in 0..4 {
        let input = [5u8, 8u8];
        match handle.submit(diagnostic_header(), &input, 0) {
            Ok(_) => {}
            Err(e) => panic!("submit failed: {}", e),
        }
    }

    assert!(
        handle.in_flight() > 0,
        "work must be in flight before the drain is meaningful"
    );

    // A generous budget: these units cancel cooperatively and should drain well
    // inside it. A non-zero return would mean work outlived the budget.
    let remaining = pool::shutdown(std::time::Duration::from_secs(5));
    assert_eq!(
        remaining, 0,
        "cooperative work must drain within the budget, {} left in flight",
        remaining
    );

    assert!(
        pool::is_shutting_down(),
        "shutdown must latch, so nothing slips in behind the drain loop"
    );

    // New work is refused while shut down. Previously the stub accepted everything.
    let input = [0u8];
    match handle.submit(diagnostic_header(), &input, 0) {
        Ok(_) => panic!("submit must be refused while the runtime is shutting down"),
        Err(e) => assert!(
            e.contains("shutting down"),
            "refusal should say why, got: {}",
            e
        ),
    }

    // init/shutdown is a reversible pair, so a process can run a second cycle.
    pool::rearm();
    assert!(!pool::is_shutting_down());
    match handle.submit(diagnostic_header(), &input, 0) {
        Ok(_) => {}
        Err(e) => panic!("submit must work again after re-arming: {}", e),
    }

    assert!(handle.close().is_ok());
    let _ = drainer.join();
    unsafe {
        libc::close(r);
    }
}

/// A drain budget that expires with work still running is reported, not rounded
/// up to success.
///
/// Mode 17 holds without ever calling `ctx.check()`, so the cancel flag
/// `shutdown` sets is never read and the unit cannot be drained. That is the
/// shape of a real adopter engine, and the only way the non-zero branch of
/// `pool::shutdown` (and the `FFI_ERR` that Go reads as `ErrShutdownIncomplete`)
/// runs at all: every other test here drains cooperatively. A `shutdown` that
/// returned 0 with undrained work passed all of them.
#[test]
#[cfg_attr(miri, ignore)]
fn shutdown_reports_non_cooperative_work_that_outlives_its_budget() {
    let _serial = serial();
    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");

    // The job can only be reported as outliving the budget while it is in the
    // engine, and the engine holds it for `HOLD` at the very least (a sleep is
    // never shorter than asked), counted from some instant after the submit. So
    // a drain that returns before `submitted + HOLD` met a job that was still
    // running, on any host: if it reported none, shutdown is wrong. One that
    // returns later may have met a job a stalled host let finish, and says
    // nothing about shutdown. That attempt is discarded and run again (the
    // runtime re-arms, so this can be repeated), instead of failing a correct
    // shutdown or, by holding the budget wider, passing on a job that was gone.
    // Every attempt unproven is a failure: the check could not run.
    const MAX_ATTEMPTS: usize = 20;
    let mut observed = false;
    for attempt in 1..=MAX_ATTEMPTS {
        // A previous attempt's job left its peak in the gauge, which would pass
        // for this job having started. Reading it at rest zeroes it.
        read_engine_gauge_peak();

        // Mode 17 holds `HOLD` (the most its u8 allows) without ever calling
        // `ctx.check()`, and counts itself in the engine's in-flight gauge: far
        // past the 10 ms budget, short enough to wait out.
        let submitted = Instant::now();
        let ticket = handle
            .submit(diagnostic_header(), &[17u8, 250u8], 0)
            .must("submit");

        // Shutdown only meets a job the engine is running. A unit still queued
        // when it sets its cancel flag is cancelled before the engine runs and
        // drains at once, which would make this a test of queueing. Wait for the
        // engine's own signal rather than a guess.
        wait_until_the_engine_is_running();

        let remaining = pool::shutdown(std::time::Duration::from_millis(10));
        assert!(
            pool::is_shutting_down(),
            "an incomplete drain still latches"
        );

        // The export maps the same outcome to FFI_ERR, which Go reads as
        // ErrShutdownIncomplete. Nothing about the job has changed in between.
        // SAFETY: no pointer arguments.
        let rc = unsafe { gusset_shutdown(10) };
        // Taken after both drains returned, so each of them read the job before
        // this instant.
        let job_certainly_running = submitted.elapsed() < HOLD;

        pool::rearm();
        assert!(!pool::is_shutting_down());

        // The job was never killed; it runs to its end and completes normally.
        assert_eq!(read_ticket(r), ticket);

        if remaining > 0 && rc == FFI_ERR {
            observed = true;
            break;
        }
        assert!(
            !job_certainly_running,
            "a non-cooperative job outlived the budget and was still running when both drains \
             returned, so shutdown must report it: pool::shutdown left {remaining} in flight, \
             gusset_shutdown returned {rc} (want {FFI_ERR}, not FFI_OK)"
        );
        eprintln!(
            "attempt {attempt}/{MAX_ATTEMPTS}: the host stalled the test past the job's {HOLD:?} \
             hold, so the drain met no job and proves nothing; running it again"
        );
    }
    assert!(
        observed,
        "no attempt out of {MAX_ATTEMPTS} reached shutdown while its job was still running: \
         the host stalled this test past the job's hold every time, so the check could not run"
    );

    assert!(handle.close().is_ok());
    // SAFETY: `r` is the read end this test opened.
    unsafe { libc::close(r) };
}

/// The two cancel messages Go maps to `ErrShutdown` and `context.Canceled`,
/// pinned beside `cancelled: DeadlineExceeded`
/// (`crates/gusset-example/tests/deadline.rs`). Go's `Error.Is` matches these
/// strings exactly (`internal/ffi/ffi.go`), so rewording either one turns a
/// correctly cancelled job into an unrecognised error.
///
/// Shutdown cancels through the same flag an explicit cancel sets, so the engine
/// sees `Explicit` both times; the message differs only because `gusset_take`
/// asks whether the runtime is shutting down.
#[test]
#[cfg_attr(miri, ignore)]
fn cancel_messages_are_cancelled_explicit_and_cancelled_shutdown() {
    let _serial = serial();
    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");

    // Mode 5 checks between 10 ms units, so both cancels land.
    let input = [5u8, 100u8];

    let ticket = handle.submit(diagnostic_header(), &input, 0).must("submit");
    assert!(handle.cancel(ticket), "the job's cancel flag must be live");
    assert_eq!(read_ticket(r), ticket);
    assert_eq!(
        take_failure(&handle, ticket),
        (FFI_ERR, "cancelled: Explicit".to_string()),
        "an explicit cancel"
    );

    let ticket = handle.submit(diagnostic_header(), &input, 0).must("submit");
    assert_eq!(
        pool::shutdown(std::time::Duration::from_secs(5)),
        0,
        "cooperative work must drain"
    );
    assert_eq!(read_ticket(r), ticket);
    assert_eq!(
        take_failure(&handle, ticket),
        (FFI_ERR, "cancelled: Shutdown".to_string()),
        "work cancelled by shutdown"
    );

    pool::rearm();
    assert!(handle.close().is_ok());
    // SAFETY: `r` is the read end this test opened.
    unsafe { libc::close(r) };
}
