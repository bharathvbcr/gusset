//! I3 with no help from Go, judged by what the engine did rather than by how
//! long the wall clock says it took.
//!
//! A bound such as "the job stopped within 400 ms of a 60 ms deadline" is a
//! statement about the host: a runner that stalls this process for longer than
//! the margin fails a correct implementation, and the margin has to be widened
//! each time a busier host is found. The engines here count their own progress
//! instead. A unit's sleep is at least [`UNIT`] on every host (it can only be
//! longer under load), so the number of units that can start before a deadline
//! is bounded above by a ratio that no stall can push past: load makes the count
//! smaller, never larger. A deadline Rust does not enforce between units, or
//! enforces late, runs more units than that and fails at any load.
//!
//! Its own test binary because the engines are registered in the process-global
//! opcode registry, which `rust_runtime.rs` promises never to touch. Each test
//! owns one opcode and one counter, so the tests in this binary do not observe
//! each other.

use gusset::header::{CallHeader, CancelReason};
use gusset::pool::{Handle, JobResult};
use gusset::register_engine;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

mod common;
use common::{make_pipe, read_ticket_within, Must};

/// One engine unit: the engine sleeps this long between `ctx.check()` calls.
const UNIT: Duration = Duration::from_millis(10);
/// Units in a full run of the engine: 2 s of work, far past any deadline here.
const FULL_RUN_UNITS: usize = 200;

/// Opcode of the engine that counts the units it runs.
const OPCODE_COUNTING: u32 = 0x5301;
/// Opcode of the engine that counts how often it is entered at all.
const OPCODE_ENTERED: u32 = 0x5302;

/// What an engine saw of one job.
#[derive(Default)]
struct Progress {
    /// Times the engine was entered.
    entered: AtomicUsize,
    /// `ctx.check()` calls that passed, each followed by one [`UNIT`] of sleep.
    units: AtomicUsize,
}

/// Registers an engine on `opcode` that runs [`FULL_RUN_UNITS`] units, checking
/// `ctx` before each one and recording what it did, and returns its record.
///
/// `ctx.check()?` is the documented I3 pattern: an `Err` ends the job, and the
/// runtime reports it as the cancel reason it was.
fn register_counting_engine(opcode: u32) -> Arc<Progress> {
    let progress = Arc::new(Progress::default());
    let seen = Arc::clone(&progress);
    register_engine(opcode, move |ctx, _input: &[u8]| {
        seen.entered.fetch_add(1, Ordering::SeqCst);
        for _ in 0..FULL_RUN_UNITS {
            ctx.check()?;
            seen.units.fetch_add(1, Ordering::SeqCst);
            std::thread::sleep(UNIT);
        }
        Ok(vec![5u8, 0u8])
    })
    .must("register engine");
    progress
}

/// Only `timeout_ns` is set and the cancel flag is never touched. Go's context
/// also sets the cancel flag when its deadline fires, which masks a Rust
/// deadline that does nothing; here the only thing that can stop a multi-unit
/// job early is `JobContext::check` reading the deadline Rust derived from the
/// header.
///
/// The engine passes at most `DEADLINE / UNIT` checks: check `k` runs at least
/// `(k - 1)` units after the job started, so it passes only while
/// `(k - 1) * UNIT < DEADLINE`. A host stall delays checks and lowers the count;
/// it cannot raise it. An engine that ran more units than that was not stopped
/// by the deadline, however fast the host is.
#[test]
fn a_deadline_stops_a_multi_unit_job_between_units() {
    const DEADLINE: Duration = Duration::from_millis(60);
    let progress = register_counting_engine(OPCODE_COUNTING);

    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");
    let header = CallHeader {
        timeout_ns: DEADLINE.as_nanos() as u64,
        reserved: OPCODE_COUNTING,
        ..Default::default()
    };

    let started = Instant::now();
    let ticket = handle.submit(header, &[], 0).must("submit");
    // A bound on the wait that keeps a lost ticket from hanging the binary, not
    // on the deadline: it is the full run, which a working deadline never needs.
    assert_eq!(
        read_ticket_within(r, Duration::from_secs(30)),
        Some(ticket),
        "no completion for a job whose full run is {:?}",
        UNIT * FULL_RUN_UNITS as u32
    );
    let elapsed = started.elapsed();

    match handle.take(ticket).must("take") {
        JobResult::Cancelled(CancelReason::DeadlineExceeded) => {}
        other => {
            panic!("want Cancelled(DeadlineExceeded) from the Rust deadline alone, got {other:?}")
        }
    }
    assert!(
        elapsed >= DEADLINE,
        "stopped after {elapsed:?}, before the {DEADLINE:?} deadline"
    );

    let max_units = (DEADLINE.as_nanos() / UNIT.as_nanos()) as usize;
    let units = progress.units.load(Ordering::SeqCst);
    assert!(
        units <= max_units,
        "the engine ran {units} of {FULL_RUN_UNITS} units against a {DEADLINE:?} deadline; \
         at most {max_units} can start before it, so the deadline is not enforced \
         between units in Rust (I3)"
    );

    assert!(handle.close().is_ok());
    unsafe { libc::close(r) };
}

/// A timeout that has already passed when the worker starts is refused before
/// the engine runs: the unit is cancelled in the pool, not handed to an engine
/// to notice on its own. Whether the engine was entered is the fact, so no
/// elapsed-time bound is needed to tell "refused at once" from "ran and then
/// stopped".
#[test]
fn an_expired_deadline_never_enters_the_engine() {
    let progress = register_counting_engine(OPCODE_ENTERED);

    let (r, w) = make_pipe();
    let handle = Handle::open(1, w).must("open");
    let header = CallHeader {
        timeout_ns: 1,
        reserved: OPCODE_ENTERED,
        ..Default::default()
    };

    let ticket = handle.submit(header, &[], 0).must("submit");
    assert_eq!(read_ticket_within(r, Duration::from_secs(30)), Some(ticket));
    assert!(
        matches!(
            handle.take(ticket).must("take"),
            JobResult::Cancelled(CancelReason::DeadlineExceeded)
        ),
        "a 1 ns timeout must report DeadlineExceeded"
    );
    assert_eq!(
        progress.entered.load(Ordering::SeqCst),
        0,
        "the engine was entered for a job whose deadline had passed before it started"
    );

    assert!(handle.close().is_ok());
    unsafe { libc::close(r) };
}
