//! Dispatch cost of the diagnostic engine versus a registered handler.
//!
//! Three comparisons, all through the public pool API (no new C export, and this
//! binary is not a staticlib):
//!
//! * opcode 0 on the diagnostic engine versus opcode 0 on a handler registered
//!   with [`set_engine_handler`]. [`register_engine`] refuses opcode 0 because
//!   dispatch never consults the registry for it.
//! * opcode 0 on the diagnostic engine versus a nonzero opcode registered with
//!   [`register_engine`]. A fourth arm, opcode 0 on the diagnostic engine while
//!   a nonzero opcode is registered, is the control for it: it separates "the
//!   registry being non-empty slows opcode 0" from "the registry lookup costs
//!   something".
//! * each of the engine states above again with several threads dispatching at
//!   once (the `_parallel` arms, below).
//!
//! # Parallel arms
//!
//! The serial arms run one submitter against one worker, so the engine
//! registries are never contended: they show what one dispatch costs, not what
//! it costs when every worker resolves its engine at the same moment. A
//! `_parallel` arm starts several lanes (default [`PARALLEL_THREADS`],
//! `--threads N`), each a thread with its own [`Handle`] of one
//! worker and its own completion pipe, and releases them together on a barrier.
//! Each thread keeps [`PARALLEL_DEPTH`] jobs in flight so its worker stays busy.
//! Separate handles on purpose: the only state the threads then share is the
//! process-global engine registry (and the allocator), which is what the arm
//! exists to load. One handle with several workers would add its single job
//! queue to the same measurement.
//!
//! A parallel arm's figure is the wall time of the slowest thread divided by the
//! round trips each thread ran, so it reads as one round trip's latency with
//! every thread running, comparable to the serial arm of the same engine state.
//! With no contention the two are equal; the gap is the cost of sharing.
//!
//! # Interleaving
//!
//! The arms run in rounds, not one after another. Every round runs each arm
//! once, and the order alternates by round (odd rounds as listed, even rounds
//! reversed: ABBA), so a clock ramp or load that arrives mid-run cannot land
//! only on the arm that always runs last. A discarded round 0 comes first, for
//! the frequency ramp. [`MIN_ROUNDS`] is a floor, not a default: benchstat will
//! not quote a confidence interval below 6 samples, and fewer rounds than this
//! cannot tell an order effect from a difference between arms.
//!
//! # Output
//!
//! One `BenchmarkDispatch/<arm>  <iterations>  <value> ns/op` line per round per
//! arm, which is the shape benchstat reads and `tools/internal/benchfile`
//! parses. The lines are grouped by arm, not in execution order: benchfile
//! rejects an arm that resumes after another one, because that is how two
//! processes writing one file looks. The execution order is kept in
//! `# gusset-bench-rust-round-N:` comment lines instead.
//!
//! ```text
//! cargo bench -p gusset --bench dispatch                  # 10 rounds
//! cargo bench -p gusset --bench dispatch -- --rounds 20
//! cargo bench -p gusset --bench dispatch -- --threads 8   # parallel arms only
//! ```
//!
//! Record it through `bench/record.sh` (`make bench-dispatch`), not by hand:
//! the recorder refuses a loaded or shared machine and stamps the file.

use gusset::header::{
    CallHeader, JobContext, GUSSET_FLAG_DIAGNOSTIC_ENGINE, GUSSET_FLAG_INLINE_COMPLETION,
};
use gusset::pool::{Handle, INLINE_RECORD_FLAG};
use gusset::{clear_engine_handlers, register_engine, set_engine_handler};
use std::collections::VecDeque;
use std::sync::Barrier;
use std::time::{Duration, Instant};

/// Round-trips timed per sample, unless `--iters` overrides it.
const DEFAULT_ITERS: u32 = 5000;

/// The fewest rounds a run may ask for; also the default.
const MIN_ROUNDS: u32 = 10;

/// The nonzero opcode the registry arms install.
const NONZERO_OPCODE: u32 = 7;

/// Threads a `_parallel` arm runs, unless `--threads` overrides it. Each is a
/// submitter plus a worker, so the default is eight busy threads: it fits an
/// 8-core host without oversubscribing it, and a smaller host should pass a
/// lower N.
const PARALLEL_THREADS: usize = 4;

/// The most threads `--threads` accepts; a typo of 400 would otherwise open 400
/// handles and report the scheduler's cost as the registry's.
const MAX_PARALLEL_THREADS: usize = 64;

/// Jobs each parallel thread keeps queued or running. A handle of one worker
/// queues two, so this stays inside that bound with a job always waiting.
const PARALLEL_DEPTH: usize = 2;

fn fail(msg: &str) -> ! {
    eprintln!("dispatch bench: {msg}");
    std::process::exit(1);
}

fn open_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    // SAFETY: pipe(2) writes two open descriptors into this array.
    let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
    if rc != 0 {
        fail("pipe failed");
    }
    // SAFETY: fds[0] is a read end this process just created.
    let flags = unsafe { libc::fcntl(fds[0], libc::F_GETFL) };
    if flags < 0 {
        fail("fcntl F_GETFL failed");
    }
    // SAFETY: same descriptor; only O_NONBLOCK is added.
    let set = unsafe { libc::fcntl(fds[0], libc::F_SETFL, flags | libc::O_NONBLOCK) };
    if set < 0 {
        fail("fcntl O_NONBLOCK failed");
    }
    (fds[0], fds[1])
}

fn read_full(fd: i32, buf: &mut [u8]) -> bool {
    let mut got = 0usize;
    let mut spins = 0u32;
    while got < buf.len() {
        // SAFETY: `buf[got..]` is a valid writable slice and `fd` is the pipe read end.
        let n = unsafe {
            libc::read(
                fd,
                buf[got..].as_mut_ptr().cast::<libc::c_void>(),
                buf.len() - got,
            )
        };
        if n > 0 {
            got += n as usize;
            spins = 0;
            continue;
        }
        if n == 0 {
            return false;
        }
        let err = std::io::Error::last_os_error().raw_os_error();
        if err == Some(libc::EINTR) || err == Some(libc::EAGAIN) || err == Some(libc::EWOULDBLOCK) {
            spins = spins.saturating_add(1);
            if spins > 100_000_000 {
                return false;
            }
            std::hint::spin_loop();
            continue;
        }
        return false;
    }
    true
}

fn drain_one(h: &Handle, fd: i32, ticket: u64) {
    let mut word = [0u8; 8];
    if !read_full(fd, &mut word) {
        fail("completion read failed");
    }
    let first = u64::from_ne_bytes(word);
    if first & INLINE_RECORD_FLAG == 0 {
        if first != ticket {
            fail("bare ticket mismatch");
        }
        if let Err(err) = h.take(ticket) {
            fail(&err);
        }
        return;
    }
    if first & !INLINE_RECORD_FLAG != ticket {
        fail("inline ticket mismatch");
    }
    if !read_full(fd, &mut word) {
        fail("inline length read failed");
    }
    let len = u64::from_ne_bytes(word) as usize;
    if len != 1 {
        fail("expected a 1-byte echo");
    }
    let mut body = [0u8; 8];
    if !read_full(fd, &mut body) {
        fail("inline body read failed");
    }
    if body[0] != 0 {
        fail("echo mismatch");
    }
}

fn echo(_ctx: &JobContext, input: &[u8]) -> Result<Vec<u8>, String> {
    Ok(input.to_vec())
}

fn time_roundtrips(h: &Handle, fd: i32, header: CallHeader, iters: u32) -> Duration {
    let payload = [0u8];
    for _ in 0..32 {
        let ticket = match h.submit(header, &payload, 0) {
            Ok(ticket) => ticket,
            Err(err) => fail(&err),
        };
        drain_one(h, fd, ticket);
    }
    let started = Instant::now();
    for _ in 0..iters {
        let ticket = match h.submit(header, &payload, 0) {
            Ok(ticket) => ticket,
            Err(err) => fail(&err),
        };
        drain_one(h, fd, ticket);
    }
    started.elapsed()
}

/// Submits `iters` jobs through `h` with [`PARALLEL_DEPTH`] always in flight and
/// drains them in order. A handle of one worker completes in submission order,
/// so `drain_one` keeps its exact-ticket check: an out-of-order completion fails
/// the run instead of being matched loosely.
fn pipelined_roundtrips(h: &Handle, fd: i32, header: CallHeader, iters: u32) {
    let payload = [0u8];
    let submit = |queue: &mut VecDeque<u64>| match h.submit(header, &payload, 0) {
        Ok(ticket) => queue.push_back(ticket),
        Err(err) => fail(&err),
    };
    let mut queue = VecDeque::with_capacity(PARALLEL_DEPTH);
    for _ in 0..PARALLEL_DEPTH.min(iters as usize) {
        submit(&mut queue);
    }
    let mut submitted = queue.len() as u32;
    while let Some(ticket) = queue.pop_front() {
        drain_one(h, fd, ticket);
        if submitted < iters {
            submit(&mut queue);
            submitted += 1;
        }
    }
}

/// One thread's (handle, completion read fd) per parallel thread.
type Lane = (std::sync::Arc<Handle>, i32);

fn open_lanes(threads: usize) -> Vec<Lane> {
    (0..threads)
        .map(|_| {
            let (read_fd, write_fd) = open_pipe();
            match Handle::open(1, write_fd) {
                Ok(handle) => (handle, read_fd),
                Err(err) => fail(&err),
            }
        })
        .collect()
}

fn close_lanes(lanes: Vec<Lane>) {
    for (handle, read_fd) in lanes {
        if let Err(err) = handle.close() {
            fail(&err);
        }
        // SAFETY: read_fd is the pipe read end from open_pipe, still open.
        unsafe {
            libc::close(read_fd);
        }
    }
}

/// Runs every lane at once and returns the slowest lane's time for `iters` jobs.
///
/// Each lane warms up for 32 jobs, then all lanes meet on a barrier, so the timed
/// loops overlap for their whole length instead of the first thread finishing
/// before the last has started. The slowest lane is the one that was contended
/// for longest, which is the figure a throughput claim has to answer to.
fn time_parallel(lanes: &[Lane], header: CallHeader, iters: u32) -> Duration {
    let start = Barrier::new(lanes.len());
    std::thread::scope(|scope| {
        let workers: Vec<_> = lanes
            .iter()
            .map(|(handle, fd)| {
                let start = &start;
                scope.spawn(move || {
                    pipelined_roundtrips(handle, *fd, header, 32);
                    start.wait();
                    let started = Instant::now();
                    pipelined_roundtrips(handle, *fd, header, iters);
                    started.elapsed()
                })
            })
            .collect();
        workers
            .into_iter()
            .map(|worker| match worker.join() {
                Ok(elapsed) => elapsed,
                Err(_) => fail("a parallel lane panicked"),
            })
            .max()
            .unwrap_or_default()
    })
}

/// One measured configuration. Each arm installs the engine state it needs from
/// scratch (the registries are process-global and dispatch prefers a registered
/// engine to the diagnostic flag), so an arm cannot inherit what the arm before
/// it left behind.
#[derive(Clone, Copy)]
enum Arm {
    DiagnosticOpcode0,
    RegisteredOpcode0,
    DiagnosticOpcode0RegistryLoaded,
    RegisteredNonzero,
    DiagnosticOpcode0Parallel,
    RegisteredOpcode0Parallel,
    RegisteredNonzeroParallel,
}

const ARMS: [Arm; 7] = [
    Arm::DiagnosticOpcode0,
    Arm::RegisteredOpcode0,
    Arm::DiagnosticOpcode0RegistryLoaded,
    Arm::RegisteredNonzero,
    Arm::DiagnosticOpcode0Parallel,
    Arm::RegisteredOpcode0Parallel,
    Arm::RegisteredNonzeroParallel,
];

impl Arm {
    fn name(self) -> &'static str {
        match self {
            Arm::DiagnosticOpcode0 => "diagnostic_opcode0",
            Arm::RegisteredOpcode0 => "registered_opcode0",
            Arm::DiagnosticOpcode0RegistryLoaded => "diagnostic_opcode0_registry_loaded",
            Arm::RegisteredNonzero => "registered_nonzero",
            Arm::DiagnosticOpcode0Parallel => "diagnostic_opcode0_parallel",
            Arm::RegisteredOpcode0Parallel => "registered_opcode0_parallel",
            Arm::RegisteredNonzeroParallel => "registered_nonzero_parallel",
        }
    }

    /// Whether the arm runs several threads at once instead of one round trip at
    /// a time.
    fn is_parallel(self) -> bool {
        matches!(
            self,
            Arm::DiagnosticOpcode0Parallel
                | Arm::RegisteredOpcode0Parallel
                | Arm::RegisteredNonzeroParallel
        )
    }

    /// Leaves exactly this arm's engines installed.
    fn install(self) {
        clear_engine_handlers();
        match self {
            Arm::DiagnosticOpcode0 | Arm::DiagnosticOpcode0Parallel => {}
            Arm::RegisteredOpcode0 | Arm::RegisteredOpcode0Parallel => set_engine_handler(echo),
            Arm::DiagnosticOpcode0RegistryLoaded
            | Arm::RegisteredNonzero
            | Arm::RegisteredNonzeroParallel => {
                if let Err(err) = register_engine(NONZERO_OPCODE, echo) {
                    fail(&err);
                }
            }
        }
    }

    fn header(self) -> CallHeader {
        let inline = GUSSET_FLAG_INLINE_COMPLETION;
        match self {
            // The diagnostic flag is what routes opcode 0 to the built-in engine;
            // a registered global handler would win over it, which is why
            // `install` clears the registries first.
            Arm::DiagnosticOpcode0
            | Arm::DiagnosticOpcode0RegistryLoaded
            | Arm::DiagnosticOpcode0Parallel => CallHeader {
                flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE | inline,
                ..CallHeader::default()
            },
            Arm::RegisteredOpcode0 | Arm::RegisteredOpcode0Parallel => CallHeader {
                flags: inline,
                ..CallHeader::default()
            },
            Arm::RegisteredNonzero | Arm::RegisteredNonzeroParallel => CallHeader {
                flags: inline,
                reserved: NONZERO_OPCODE,
                ..CallHeader::default()
            },
        }
    }
}

struct Config {
    rounds: u32,
    iters: u32,
    threads: usize,
}

fn positive(flag: &str, value: Option<String>) -> u32 {
    match value.as_deref().map(str::parse::<u32>) {
        Some(Ok(n)) if n > 0 => n,
        _ => fail(&format!("{flag} needs a positive integer")),
    }
}

/// Reads `--rounds N`, `--iters N` and `--threads N`. cargo appends `--bench` when it runs a
/// bench target, so that one is accepted and ignored; anything else is refused
/// rather than skipped, because a typo'd flag would otherwise run the defaults
/// and look like the run that was asked for.
fn parse_args() -> Config {
    let mut cfg = Config {
        rounds: MIN_ROUNDS,
        iters: DEFAULT_ITERS,
        threads: PARALLEL_THREADS,
    };
    let mut args = std::env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--bench" => {}
            "--rounds" => cfg.rounds = positive("--rounds", args.next()),
            "--iters" => cfg.iters = positive("--iters", args.next()),
            "--threads" => cfg.threads = positive("--threads", args.next()) as usize,
            other => fail(&format!(
                "unknown argument {other}; usage: dispatch [--rounds N] [--iters N] [--threads N]"
            )),
        }
    }
    if cfg.rounds < MIN_ROUNDS {
        fail(&format!(
            "--rounds {} is below the floor of {MIN_ROUNDS}: fewer rounds cannot separate an \
             order effect from a difference between arms",
            cfg.rounds
        ));
    }
    if cfg.threads > MAX_PARALLEL_THREADS {
        fail(&format!(
            "--threads {} is above the ceiling of {MAX_PARALLEL_THREADS}",
            cfg.threads
        ));
    }
    cfg
}

/// The arms in the order round `round` (1-based) runs them: as listed on odd
/// rounds, reversed on even ones.
fn round_order(round: u32) -> Vec<Arm> {
    let mut order = ARMS.to_vec();
    if round.is_multiple_of(2) {
        order.reverse();
    }
    order
}

fn main() {
    let cfg = parse_args();
    let (read_fd, write_fd) = open_pipe();
    let handle = match Handle::open(1, write_fd) {
        Ok(handle) => handle,
        Err(err) => fail(&err),
    };

    let lanes = open_lanes(cfg.threads);
    let measure = |arm: Arm| {
        if arm.is_parallel() {
            time_parallel(&lanes, arm.header(), cfg.iters)
        } else {
            time_roundtrips(&handle, read_fd, arm.header(), cfg.iters)
        }
    };

    // Round 0: every arm once, discarded. An idle machine is not yet at a steady
    // clock, and the first arm to run would otherwise pay for the ramp.
    for arm in ARMS {
        arm.install();
        let _ = measure(arm);
    }

    let mut samples: Vec<Vec<Duration>> = vec![Vec::new(); ARMS.len()];
    let mut orders: Vec<String> = Vec::new();
    for round in 1..=cfg.rounds {
        let order = round_order(round);
        let names: Vec<&str> = order.iter().map(|arm| arm.name()).collect();
        orders.push(format!(
            "# gusset-bench-rust-round-{round}: {}",
            names.join(" ")
        ));
        for arm in order {
            arm.install();
            let elapsed = measure(arm);
            samples[arm as usize].push(elapsed);
        }
    }
    clear_engine_handlers();

    close_lanes(lanes);
    if let Err(err) = handle.close() {
        fail(&err);
    }
    // SAFETY: read_fd is the pipe read end from open_pipe, still open.
    unsafe {
        libc::close(read_fd);
    }

    println!(
        "# gusset-bench-rust-order: {} rounds after a discarded round 0; each round ran every \
         arm once; order alternates (odd rounds as listed, even rounds reversed)",
        cfg.rounds
    );
    for line in &orders {
        println!("{line}");
    }
    for (arm, runs) in ARMS.iter().zip(&samples) {
        for elapsed in runs {
            let ns = elapsed.as_secs_f64() * 1e9 / f64::from(cfg.iters);
            println!(
                "BenchmarkDispatch/{}\t{}\t{ns:.1} ns/op",
                arm.name(),
                cfg.iters
            );
        }
    }
}
