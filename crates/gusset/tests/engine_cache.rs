//! The per-worker engine cache (R7) against real workers: a worker that has
//! run an engine keeps it, and must stop the moment the registry changes.
//!
//! `src/pool/tests/engine_cache.rs` drives the cache directly, on nonzero
//! opcodes. This binary is the other half: opcode 0, a clear with jobs in
//! flight on several workers at once, and a concurrent register/clear stress.
//! It is a separate binary because it installs a global (opcode 0) handler,
//! which every diagnostic-engine unit test sharing its process would run
//! instead. The tests here share the registry with each other, so `REGISTRY`
//! serialises them.
//!
//! The guarantee under test is the one the registry's lock gave before the
//! cache: a job submitted after `clear_engine_handlers` (or
//! `set_engine_handler`) returned runs no handler that call removed. A job a
//! worker was already running is not interrupted.

#![allow(unsafe_code)]

use gusset::header::{CallHeader, JobContext, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::{clear_engine_handlers, register_engine, set_engine_handler, Handle, JobResult};
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

#[path = "../../../tests/common/mod.rs"]
mod common;
use common::{make_pipe, read_ticket, Must};

/// Serialises the tests in this binary: the registry is one map for the process.
static REGISTRY: Mutex<()> = Mutex::new(());

fn registry_lock() -> std::sync::MutexGuard<'static, ()> {
    REGISTRY.lock().unwrap_or_else(|e| e.into_inner())
}

/// Workers in the multi-worker tests: enough that "every worker has the handler
/// cached" is a claim about several threads, not one.
const POOL: usize = 4;

/// How long a gate waits before giving up, so a broken test fails with a
/// message instead of hanging the binary.
const GATE_WAIT: Duration = Duration::from_secs(5);

type Handler = Box<dyn Fn(&JobContext, &[u8]) -> Result<Vec<u8>, String> + Send + Sync>;

/// Installs `handler` for `opcode`: the global slot for 0, the registry otherwise.
fn install(opcode: u32, handler: Handler) {
    if opcode == 0 {
        set_engine_handler(move |ctx: &JobContext, input: &[u8]| handler(ctx, input));
    } else {
        register_engine(opcode, move |ctx: &JobContext, input: &[u8]| {
            handler(ctx, input)
        })
        .must("register");
    }
}

/// A handler that answers with one fixed byte, so a result names its handler.
fn answer(byte: u8) -> Handler {
    Box::new(move |_ctx, _input| Ok(vec![byte]))
}

fn wait_for(what: &str, mut ready: impl FnMut() -> bool) {
    let started = Instant::now();
    while !ready() {
        assert!(
            started.elapsed() < GATE_WAIT,
            "timed out waiting for {what}"
        );
        thread::sleep(Duration::from_millis(1));
    }
}

/// What the gated handler's inputs make it do.
#[derive(Default)]
struct Gate {
    /// Workers currently inside a `rendezvous` job.
    entered: AtomicUsize,
    /// Workers currently inside a `hold` job.
    holding: AtomicUsize,
    /// Lets `hold` jobs finish.
    release: AtomicBool,
}

/// A handler answering `tag`, except that `rendezvous` blocks until every worker
/// of the pool is inside one (so every worker has run, and cached, this
/// handler), and `hold` blocks until `gate.release` (so a job is in flight
/// while the test clears the registry).
fn gated(tag: u8, gate: &Arc<Gate>) -> Handler {
    let gate = Arc::clone(gate);
    Box::new(move |_ctx, input| {
        let wait = |ready: &dyn Fn() -> bool| {
            let started = Instant::now();
            while !ready() {
                if started.elapsed() > GATE_WAIT {
                    return Err("gate timed out".to_string());
                }
                thread::sleep(Duration::from_micros(200));
            }
            Ok(())
        };
        match input {
            b"rendezvous" => {
                gate.entered.fetch_add(1, Ordering::SeqCst);
                wait(&|| gate.entered.load(Ordering::SeqCst) >= POOL)?;
            }
            b"hold" => {
                gate.holding.fetch_add(1, Ordering::SeqCst);
                wait(&|| gate.release.load(Ordering::Acquire))?;
            }
            _ => {}
        }
        Ok(vec![tag])
    })
}

/// One handle with its own completion pipe.
struct Session {
    handle: Arc<Handle>,
    read_fd: i32,
    /// Results taken off the handle that a caller has not asked for yet: with
    /// several workers a later batch can finish while an earlier one is read.
    stash: Mutex<HashMap<u64, JobResult>>,
}

impl Session {
    fn open(workers: usize) -> Self {
        let (read_fd, write_fd) = make_pipe();
        Session {
            handle: Handle::open(workers as u32, write_fd).must("open"),
            read_fd,
            stash: Mutex::new(HashMap::new()),
        }
    }

    fn submit_with(&self, header: CallHeader, input: &[u8]) -> u64 {
        self.handle.submit(header, input, 0).must("submit")
    }

    fn submit(&self, opcode: u32, input: &[u8]) -> u64 {
        let header = CallHeader {
            reserved: opcode,
            ..Default::default()
        };
        self.submit_with(header, input)
    }

    /// The results of `tickets`, in the order given. Completions arrive in
    /// whatever order the workers finish.
    fn results(&self, tickets: &[u64]) -> Vec<JobResult> {
        let mut stash = self.stash.lock().unwrap_or_else(|e| e.into_inner());
        tickets
            .iter()
            .map(|wanted| loop {
                if let Some(result) = stash.remove(wanted) {
                    break result;
                }
                let ticket = read_ticket(self.read_fd);
                stash.insert(ticket, self.handle.take(ticket).must("take"));
            })
            .collect()
    }

    /// `count` jobs, submitted `POOL` at a time: a handle queues twice its
    /// worker count and refuses a submit past that.
    fn run_many(&self, opcode: u32, count: usize, input: &[u8]) -> Vec<JobResult> {
        let mut all = Vec::with_capacity(count);
        let mut left = count;
        while left > 0 {
            let batch = left.min(POOL);
            let tickets: Vec<u64> = (0..batch).map(|_| self.submit(opcode, input)).collect();
            all.extend(self.results(&tickets));
            left -= batch;
        }
        all
    }

    fn run_with(&self, header: CallHeader, input: &[u8]) -> JobResult {
        let ticket = self.submit_with(header, input);
        self.results(&[ticket]).remove(0)
    }

    fn run(&self, opcode: u32, input: &[u8]) -> JobResult {
        let ticket = self.submit(opcode, input);
        self.results(&[ticket]).remove(0)
    }

    fn close(self) {
        self.handle.close().must("close");
        // SAFETY: `read_fd` is the read end `make_pipe` returned for this
        // session, and nothing else closes it.
        unsafe {
            libc::close(self.read_fd);
        }
    }
}

fn bytes(result: &JobResult) -> Vec<u8> {
    match result {
        JobResult::Ok(bytes) => bytes.clone(),
        other => panic!("expected a result, got {other:?}"),
    }
}

/// Asserts `result` is the refusal for `opcode`, and not a handler's answer.
fn assert_refused(result: &JobResult, opcode: u32) {
    match result {
        JobResult::Err(msg) if opcode == 0 => assert!(
            msg.contains("no engine handler registered; call"),
            "opcode 0 refusal: {msg}"
        ),
        JobResult::Err(msg) => assert!(
            msg.contains(&format!("no engine handler registered for opcode {opcode}")),
            "opcode {opcode} refusal: {msg}"
        ),
        JobResult::Ok(bytes) => {
            panic!("opcode {opcode} ran a cleared handler, answering {bytes:?}")
        }
        other => panic!("opcode {opcode}: unexpected result {other:?}"),
    }
}

/// Every worker has run the handler (so each holds it cached), then a clear
/// lands while `POOL` jobs are running it and `POOL` more are queued behind
/// them. The running jobs finish: the clear does not interrupt a call. The
/// queued ones are dequeued after the clear returned, so every one is refused.
/// Then a new handler is registered and every worker runs that one.
fn clear_with_jobs_in_flight(opcode: u32) {
    let _registry = registry_lock();
    clear_engine_handlers();
    let gate = Arc::new(Gate::default());
    install(opcode, gated(1, &gate));
    let session = Session::open(POOL);

    // Each `rendezvous` job blocks its worker until all POOL are inside, so the
    // POOL jobs land on POOL distinct workers.
    let warm: Vec<u64> = (0..POOL)
        .map(|_| session.submit(opcode, b"rendezvous"))
        .collect();
    for result in session.results(&warm) {
        assert_eq!(bytes(&result), vec![1]);
    }

    let held: Vec<u64> = (0..POOL).map(|_| session.submit(opcode, b"hold")).collect();
    wait_for("every worker to be inside a hold job", || {
        gate.holding.load(Ordering::SeqCst) == POOL
    });
    let queued: Vec<u64> = (0..POOL)
        .map(|_| session.submit(opcode, b"queued"))
        .collect();

    clear_engine_handlers();
    gate.release.store(true, Ordering::Release);

    for result in session.results(&held) {
        assert_eq!(
            bytes(&result),
            vec![1],
            "a job already running when the registry was cleared must finish"
        );
    }
    for result in session.results(&queued) {
        assert_refused(&result, opcode);
    }

    // Re-register after the clear: the new handler runs, on every worker.
    install(opcode, answer(2));
    for result in session.run_many(opcode, POOL * 4, b"x") {
        assert_eq!(
            bytes(&result),
            vec![2],
            "the re-registered handler must run"
        );
    }

    session.close();
    clear_engine_handlers();
}

#[test]
fn a_clear_with_jobs_in_flight_on_every_worker_refuses_the_queued_ones_for_a_nonzero_opcode() {
    clear_with_jobs_in_flight(9421);
}

#[test]
fn a_clear_with_jobs_in_flight_on_every_worker_refuses_the_queued_ones_for_opcode_zero() {
    clear_with_jobs_in_flight(0);
}

/// `set_engine_handler` replaces the global slot with no clear in between. Every
/// worker that cached the old handler must run the new one.
#[test]
fn replacing_the_global_handler_reaches_every_worker() {
    let _registry = registry_lock();
    clear_engine_handlers();
    let gate = Arc::new(Gate::default());
    install(0, gated(1, &gate));
    let session = Session::open(POOL);

    let warm: Vec<u64> = (0..POOL)
        .map(|_| session.submit(0, b"rendezvous"))
        .collect();
    for result in session.results(&warm) {
        assert_eq!(bytes(&result), vec![1]);
    }

    install(0, answer(2));
    for result in session.run_many(0, POOL * 4, b"x") {
        assert_eq!(bytes(&result), vec![2], "a worker ran the replaced handler");
    }

    session.close();
    clear_engine_handlers();
}

/// A worker that has found no opcode 0 handler remembers that, and must forget
/// it when one is installed: the diagnostic path it was taking gives way to the
/// handler (which outranks the flag), and returns when the handler is cleared.
/// One worker, so the same thread sees every step.
#[test]
fn a_remembered_absence_gives_way_to_a_later_install() {
    let _registry = registry_lock();
    clear_engine_handlers();
    let session = Session::open(1);
    let diagnostic = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };

    for _ in 0..8 {
        assert_eq!(bytes(&session.run_with(diagnostic, &[0])), vec![0]);
    }
    // Without the flag the same absence fails closed, and stays so.
    for _ in 0..2 {
        assert_refused(&session.run(0, b"x"), 0);
    }

    install(0, answer(3));
    assert_eq!(
        bytes(&session.run_with(diagnostic, &[0])),
        vec![3],
        "the installed handler must outrank the diagnostic engine"
    );
    assert_eq!(bytes(&session.run(0, b"x")), vec![3]);

    clear_engine_handlers();
    assert_eq!(bytes(&session.run_with(diagnostic, &[0])), vec![0]);
    assert_refused(&session.run(0, b"x"), 0);

    session.close();
}

/// Opcode 0 and the registry stay apart on a worker that has cached both: the
/// global handler never serves a nonzero opcode (flag or not), a registry entry
/// never serves opcode 0, and a clear takes both away.
#[test]
fn opcode_zero_and_the_registry_stay_isolated_on_a_warm_worker() {
    let _registry = registry_lock();
    const OPCODE: u32 = 9422;
    const OTHER: u32 = 9423;
    clear_engine_handlers();
    install(0, answer(10));
    install(OPCODE, answer(20));
    let session = Session::open(1);
    let flagged = |opcode: u32| CallHeader {
        reserved: opcode,
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };

    for _ in 0..3 {
        assert_eq!(bytes(&session.run(0, b"x")), vec![10]);
        assert_eq!(bytes(&session.run(OPCODE, b"x")), vec![20]);
        assert_refused(&session.run(OTHER, b"x"), OTHER);
        assert_refused(&session.run_with(flagged(OTHER), &[0]), OTHER);
        // The flag does not displace the global handler.
        assert_eq!(bytes(&session.run_with(flagged(0), &[0])), vec![10]);
    }

    clear_engine_handlers();
    assert_refused(&session.run(0, b"x"), 0);
    assert_refused(&session.run(OPCODE, b"x"), OPCODE);
    // With the global handler gone the flag reaches the diagnostic engine for
    // opcode 0 only.
    assert_eq!(bytes(&session.run_with(flagged(0), &[0])), vec![0]);
    assert_refused(&session.run_with(flagged(OPCODE), &[0]), OPCODE);

    // A reinstall of only the registry entry does not revive opcode 0.
    install(OPCODE, answer(21));
    assert_eq!(bytes(&session.run(OPCODE, b"x")), vec![21]);
    assert_refused(&session.run(0, b"x"), 0);

    session.close();
    clear_engine_handlers();
}

/// Several opcodes cached on one worker go together on a clear; only what is
/// reinstalled comes back.
#[test]
fn several_opcodes_on_one_worker_are_all_dropped_by_a_clear() {
    let _registry = registry_lock();
    const BASE: u32 = 9430;
    clear_engine_handlers();
    for n in 0..5u8 {
        install(BASE + u32::from(n), answer(n + 1));
    }
    let session = Session::open(1);

    for _ in 0..2 {
        for n in 0..5u8 {
            assert_eq!(bytes(&session.run(BASE + u32::from(n), b"x")), vec![n + 1]);
        }
    }

    clear_engine_handlers();
    for n in 0..5u32 {
        assert_refused(&session.run(BASE + n, b"x"), BASE + n);
    }

    install(BASE + 1, answer(11));
    install(BASE + 3, answer(13));
    for n in 0..5u32 {
        let result = session.run(BASE + n, b"x");
        match n {
            1 => assert_eq!(bytes(&result), vec![11]),
            3 => assert_eq!(bytes(&result), vec![13]),
            _ => assert_refused(&result, BASE + n),
        }
    }

    session.close();
    clear_engine_handlers();
}

/// The version a handler installed under `version` answers with.
fn versioned(version: u64) -> Handler {
    Box::new(move |_ctx, _input| Ok(version.to_le_bytes().to_vec()))
}

/// What one submitter saw.
#[derive(Default)]
struct Seen {
    ran: [u64; 2],
    refused: [u64; 2],
    violations: Vec<String>,
}

/// A controller registers, replaces and clears handlers as fast as it can while
/// submitters run jobs on both opcodes, and no job may run a handler that was
/// removed before the job was submitted.
///
/// Every handler answers with the version it was installed as, and versions only
/// grow. The controller stores a version into `installed` after the
/// `set_engine_handler` that installed it returned, and into `cleared` after the
/// `clear_engine_handlers` that removed it returned. A submitter reads both
/// before submitting, so the job is submitted after those calls returned and
/// can only be dequeued after them. Then:
///
/// - a job answered by version `x` with `x <= cleared` ran a cleared handler;
/// - an opcode 0 job answered by `x < installed` ran a handler that
///   `set_engine_handler` had already replaced.
///
/// A cache that kept a handler across a clear, or across a replacement, breaks
/// one of the two on the first stale hit.
#[test]
fn concurrent_install_replace_and_clear_never_run_a_handler_removed_before_submission() {
    let _registry = registry_lock();
    const OPCODE: u32 = 9441;
    const EPOCHS: u64 = 250;
    const SUBMITTERS: usize = 4;
    clear_engine_handlers();

    let installed = AtomicU64::new(0);
    let cleared = AtomicU64::new(0);
    let finished = AtomicBool::new(false);

    let seen: Vec<Seen> = thread::scope(|scope| {
        let submitters: Vec<_> = (0..SUBMITTERS)
            .map(|_| {
                scope.spawn(|| {
                    let session = Session::open(2);
                    let mut seen = Seen::default();
                    while !finished.load(Ordering::Acquire) {
                        for (slot, opcode) in [0, OPCODE].into_iter().enumerate() {
                            let cleared_before = cleared.load(Ordering::Acquire);
                            let installed_before = installed.load(Ordering::Acquire);
                            match session.run(opcode, b"x") {
                                JobResult::Ok(answer) => {
                                    let version = match <[u8; 8]>::try_from(answer.as_slice()) {
                                        Ok(raw) => u64::from_le_bytes(raw),
                                        Err(_) => {
                                            seen.violations
                                                .push(format!("answer of {} bytes", answer.len()));
                                            continue;
                                        }
                                    };
                                    seen.ran[slot] += 1;
                                    if version <= cleared_before {
                                        seen.violations.push(format!(
                                            "opcode {opcode} ran version {version}, cleared \
                                             through {cleared_before} before the submit"
                                        ));
                                    }
                                    if opcode == 0 && version < installed_before {
                                        seen.violations.push(format!(
                                            "opcode 0 ran version {version}, replaced by \
                                             {installed_before} before the submit"
                                        ));
                                    }
                                }
                                JobResult::Err(msg)
                                    if msg.contains("no engine handler registered") =>
                                {
                                    seen.refused[slot] += 1;
                                }
                                other => seen
                                    .violations
                                    .push(format!("opcode {opcode}: unexpected {other:?}")),
                            }
                        }
                    }
                    session.close();
                    seen
                })
            })
            .collect();

        let mut version = 0u64;
        for _ in 0..EPOCHS {
            // Two installs of opcode 0 with no clear between: a replacement.
            for _ in 0..2 {
                version += 1;
                set_engine_handler({
                    let handler = versioned(version);
                    move |ctx: &JobContext, input: &[u8]| handler(ctx, input)
                });
                installed.store(version, Ordering::Release);
                // Long enough for jobs to run the handler just installed, so the
                // replacement that follows has a cached handler to displace.
                thread::sleep(Duration::from_micros(150));
            }
            version += 1;
            let handler = versioned(version);
            register_engine(OPCODE, move |ctx: &JobContext, input: &[u8]| {
                handler(ctx, input)
            })
            .must("register");
            thread::sleep(Duration::from_micros(300));
            clear_engine_handlers();
            cleared.store(version, Ordering::Release);
            thread::sleep(Duration::from_micros(100));
        }
        finished.store(true, Ordering::Release);

        submitters
            .into_iter()
            .map(|s| match s.join() {
                Ok(seen) => seen,
                Err(_) => panic!("a submitter panicked"),
            })
            .collect()
    });

    let violations: Vec<&String> = seen.iter().flat_map(|s| &s.violations).collect();
    assert!(
        violations.is_empty(),
        "{} violation(s), first: {:?}",
        violations.len(),
        violations.first()
    );
    // Not vacuous: both opcodes ran handlers, and both were refused in the gaps.
    for (slot, name) in ["opcode 0", "the registry opcode"].into_iter().enumerate() {
        let ran: u64 = seen.iter().map(|s| s.ran[slot]).sum();
        let refused: u64 = seen.iter().map(|s| s.refused[slot]).sum();
        assert!(ran > 0, "{name}: no job ever ran a handler");
        assert!(refused > 0, "{name}: no job was ever refused");
    }
    clear_engine_handlers();
}
