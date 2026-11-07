//! Concurrent respawns (I4, I5): delay knobs widen the windows between a
//! worker's startup report, its removal from `workers`, and the clearing of
//! `respawn_pending`, so the races are deterministic. Compiled only under
//! `cfg(test)`.

use super::*;

pub(in crate::pool) type Knob = Mutex<Option<(thread::ThreadId, u64)>>;

/// Delay on the awaiting thread between a failed report and taking the lock.
pub(in crate::pool) static DELAY_BEFORE_REMOVE: Knob = Mutex::new(None);
/// Delay in `ensure_workers` between `await_ready` and clearing `respawn_pending`.
pub(in crate::pool) static DELAY_BEFORE_CLEAR: Knob = Mutex::new(None);
/// Delay inside a worker armed to fail its sigaltstack, before it reports.
pub(in crate::pool) static FAILING_WORKER_MS: AtomicU64 = AtomicU64::new(0);

/// Delay a worker sleeps after counting its own exit (`ExitMark::drop`) and
/// before its thread ends, so `exited` is raised while `is_finished` is not.
/// Armed on the spawning thread; a worker reads it once, at spawn.
pub(in crate::pool) static DELAY_AFTER_EXIT: Knob = Mutex::new(None);

/// The knob's delay if the calling thread armed it, else 0.
pub(in crate::pool) fn injected_delay_ms(knob: &Knob) -> u64 {
    match *lock_recover(knob) {
        Some((id, ms)) if id == thread::current().id() => ms,
        _ => 0,
    }
}

pub(in crate::pool) fn injected_delay(knob: &Knob) {
    let ms = injected_delay_ms(knob);
    if ms > 0 {
        thread::sleep(std::time::Duration::from_millis(ms));
    }
}

fn set(knob: &Knob, ms: u64) {
    *lock_recover(knob) = Some((thread::current().id(), ms));
}

fn header() -> CallHeader {
    CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    }
}

/// A concurrent `ensure_workers` that saw the pool "whole" (counting a worker
/// that has not reported yet) must not clear `respawn_pending` after the
/// awaiting thread found that worker failed and raised it. Before the lock was
/// released across the wait, the second submit blocked until the first had
/// removed the failed worker, then respawned it itself.
#[test]
#[cfg_attr(miri, ignore)] // pipe(2), worker threads and fcntl: not Miri-runnable
fn concurrent_respawn_does_not_lose_a_failed_worker() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");
    kill_workers(&handle, r, 1);

    FAILING_WORKER_MS.store(300, Ordering::Release);
    let h_a = Arc::clone(&handle);
    let a = thread::spawn(move || {
        arm_sigaltstack_failure();
        let res = h_a.submit(header(), &[0, 1], 0);
        disarm_sigaltstack_failure();
        res
    });
    thread::sleep(std::time::Duration::from_millis(100));
    let h_b = Arc::clone(&handle);
    let b = thread::spawn(move || {
        set(&DELAY_BEFORE_CLEAR, 600);
        h_b.submit(header(), &[0, 1], 0)
    });
    let ra = a.join().must("thread a");
    let rb = b.join().must("thread b");
    FAILING_WORKER_MS.store(0, Ordering::Release);
    *lock_recover(&DELAY_BEFORE_CLEAR) = None;
    assert!(ra.is_err(), "the armed respawn must fail");
    let tb = rb.must("b's submit");
    // A third submit must find the pool short and respawn it.
    let tc = handle.submit(header(), &[0, 1], 0).must("c's submit");
    let mut seen = Vec::new();
    for _ in 0..2 {
        if !completion_ready_within(r, 3_000) {
            break;
        }
        seen.push(read_ticket(r));
    }
    let _ = handle.close();
    // SAFETY: the read end is still this test's.
    unsafe {
        libc::close(r);
    }
    assert!(
        seen.contains(&tb) && seen.contains(&tc),
        "accepted work never ran: completions {seen:?}, expected {tb} and {tc}"
    );
}

/// Only the one that removes a failed worker from `workers` uncounts its
/// exit: when a concurrent reap took it first, `await_ready` must not
/// subtract again (that wraps `exited` to usize::MAX).
#[test]
#[cfg_attr(miri, ignore)] // pipe(2), worker threads and fcntl: not Miri-runnable
fn only_the_remover_uncounts_a_failed_worker() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");
    kill_workers(&handle, r, 1);

    let h_a = Arc::clone(&handle);
    let a = thread::spawn(move || {
        set(&DELAY_BEFORE_REMOVE, 400);
        arm_sigaltstack_failure();
        let res = h_a.submit(header(), &[0, 1], 0);
        disarm_sigaltstack_failure();
        res
    });
    // The armed worker has failed and exited; A is asleep before removing it.
    thread::sleep(std::time::Duration::from_millis(150));
    let tb = handle
        .submit(header(), &[0, 1], 0)
        .must("b's submit reaps and respawns");
    let ra = a.join().must("thread a");
    *lock_recover(&DELAY_BEFORE_REMOVE) = None;
    assert!(ra.is_err(), "the armed respawn must fail");
    assert!(completion_ready_within(r, 3_000), "b's unit never ran");
    assert_eq!(read_ticket(r), tb);
    let exited = handle.exited.load(Ordering::Acquire);
    let _ = handle.close();
    // SAFETY: the read end is still this test's.
    unsafe {
        libc::close(r);
    }
    assert_eq!(exited, 0, "a failed worker's exit was uncounted twice");
}

/// `respawn_lock` keeps a second respawn out while a failed worker is being
/// removed, so the only other party that can take that worker is `close`,
/// which drains `workers` and joins everything in it. When it got there first,
/// `await_ready` finds nothing to remove and must leave the worker's exit
/// counted: uncounting it as well would subtract an exit nobody reaped.
#[test]
#[cfg_attr(miri, ignore)] // pipe(2), worker threads and fcntl: not Miri-runnable
fn a_failed_worker_closed_away_is_not_uncounted() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");
    kill_workers(&handle, r, 1);

    let h_a = Arc::clone(&handle);
    let a = thread::spawn(move || {
        set(&DELAY_BEFORE_REMOVE, 400);
        arm_sigaltstack_failure();
        let res = h_a.submit(header(), &[0, 1], 0);
        disarm_sigaltstack_failure();
        res
    });
    // The armed worker has failed and exited; A is asleep before removing it.
    thread::sleep(std::time::Duration::from_millis(150));
    handle
        .close()
        .must("close drains and joins the failed worker");
    let ra = a.join().must("thread a");
    *lock_recover(&DELAY_BEFORE_REMOVE) = None;
    assert!(ra.is_err(), "the armed respawn must fail");
    assert_eq!(
        handle.exited.load(Ordering::Acquire),
        1,
        "a worker removed by close was uncounted by the respawn too"
    );
    // SAFETY: the read end is still this test's.
    unsafe {
        libc::close(r);
    }
}

/// A worker that has counted its exit but not yet ended must still be
/// replaced by the submit that notices it. `ensure_workers` reaped by
/// `is_finished`, which flips after `ExitMark` ran: that submit reaped
/// nothing, spawned nothing, and queued its unit with no thread to run it
/// until some later submit retried.
#[test]
#[cfg_attr(miri, ignore)] // pipe(2), worker threads and fcntl: not Miri-runnable
fn a_submit_replaces_a_worker_that_is_still_dying() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    set(&DELAY_AFTER_EXIT, 1_500);
    let opened = Handle::open(1, w);
    *lock_recover(&DELAY_AFTER_EXIT) = None;
    let handle = opened.must("open");

    let kill = CallHeader {
        trace_id: KILL_TRACE,
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    handle.submit(kill, &[9, 2], 0).must("kill submit");
    assert!(
        completion_ready_within(r, 5_000),
        "kill job never completed"
    );
    read_ticket(r);
    // Counted, and held in its drop by the injected delay: not finished.
    let start = std::time::Instant::now();
    while handle.exited.load(Ordering::Acquire) == 0 {
        assert!(
            start.elapsed() < std::time::Duration::from_secs(5),
            "worker never exited"
        );
        thread::yield_now();
    }
    assert!(
        lock_recover(&handle.workers)
            .iter()
            .all(|h| !h.is_finished()),
        "the window is not open: the worker already ended"
    );

    // One submit, no retry: the unit must run well inside the delay.
    let t = handle.submit(header(), &[0, 1], 0).must("submit");
    let ran = completion_ready_within(r, 1_000);
    let seen = if ran { Some(read_ticket(r)) } else { None };
    // Once the dying worker has ended, the next submit reaps it and the pool
    // settles at its size.
    thread::sleep(std::time::Duration::from_millis(1_700));
    handle.submit(header(), &[0, 1], 0).must("settling submit");
    assert!(completion_ready_within(r, 3_000), "settling unit never ran");
    read_ticket(r);
    let (len, exited) = (
        lock_recover(&handle.workers).len(),
        handle.exited.load(Ordering::Acquire),
    );
    let _ = handle.close();
    // SAFETY: the read end is still this test's.
    unsafe {
        libc::close(r);
    }
    assert_eq!(
        seen,
        Some(t),
        "a single submit left the queued unit without a running worker"
    );
    assert_eq!((len, exited), (1, 0), "the pool did not settle at its size");
}
