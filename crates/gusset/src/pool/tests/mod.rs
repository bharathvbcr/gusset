//! Unit tests for the pool, split by concern. Shared fixtures and the
//! test-only injection points production code reaches as `tests::` live
//! here; `respawn_and_failed_open_release_threads_and_mappings` re-executes
//! itself as `pool::tests::...`, so it stays at this level too.

mod completion;
mod engine;
mod submit;

use super::*;
use crate::header::GUSSET_FLAG_DIAGNOSTIC_ENGINE;

trait Must<T> {
    fn must(self, msg: &str) -> T;
}
impl<T, E: std::fmt::Debug> Must<T> for Result<T, E> {
    fn must(self, msg: &str) -> T {
        match self {
            Ok(v) => v,
            Err(e) => panic!("{msg}: {e:?}"),
        }
    }
}
impl<T> Must<T> for Option<T> {
    fn must(self, msg: &str) -> T {
        match self {
            Some(v) => v,
            None => panic!("{msg}: None"),
        }
    }
}

/// Number of further worker spawns to allow before failing, or -1 to disable.
///
/// Compiled only under `cfg(test)`, so the injection point in
/// `spawn_workers_locked` does not exist in a shipped `libgusset.a`.
static SPAWN_FAIL_COUNTDOWN: std::sync::atomic::AtomicI64 = std::sync::atomic::AtomicI64::new(-1);

/// The thread that armed the injector. Only its spawns are failed.
///
/// The countdown is process-global and `cargo test` runs unit tests on
/// parallel threads. `INJECT_LOCK` serialised the tests that *arm* it, but a
/// test that merely opens a handle took no lock, so its `open` could consume
/// the armed failure and fail with "injected spawn failure" (roughly one run
/// in three under `--release`). Scoping the injection to the arming thread
/// removes the cross-talk rather than asking every future test to lock.
static ARMED_BY: Mutex<Option<thread::ThreadId>> = Mutex::new(None);

/// Serialises the tests that arm the injector.
static INJECT_LOCK: Mutex<()> = Mutex::new(());

/// Serialises tests that install or clear the process-global engine registry.
///
/// `cargo test` runs this module's tests in parallel, and the registry is
/// one map for the process. A test that calls `clear_engine_handlers`
/// would otherwise delete an opcode another test is still dispatching.
static REGISTRY_TEST_LOCK: Mutex<()> = Mutex::new(());

fn arm_spawn_failure(after: i64) {
    *lock_recover(&ARMED_BY) = Some(thread::current().id());
    SPAWN_FAIL_COUNTDOWN.store(after, Ordering::Release);
}

fn disarm_spawn_failure() {
    SPAWN_FAIL_COUNTDOWN.store(-1, Ordering::Release);
    *lock_recover(&ARMED_BY) = None;
}

pub(super) fn spawn_should_fail() -> bool {
    if *lock_recover(&ARMED_BY) != Some(thread::current().id()) {
        return false;
    }
    let remaining = SPAWN_FAIL_COUNTDOWN.load(Ordering::Acquire);
    if remaining < 0 {
        return false;
    }
    if remaining == 0 {
        return true;
    }
    SPAWN_FAIL_COUNTDOWN.store(remaining - 1, Ordering::Release);
    false
}

/// Opens a real pipe, returning `(read_fd, write_fd)`.
fn make_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    // SAFETY: `fds` is a valid two-element array for pipe(2) to fill.
    let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
    assert_eq!(rc, 0, "pipe() failed");
    (fds[0], fds[1])
}

/// True when `fd` is open *and still refers to the same pipe as `read_fd`*.
///
/// `fcntl(F_GETFD)` alone is not enough: a descriptor number that Gusset closed
/// can be handed straight back out by a later `open` in another thread, and the
/// check would then pass against exactly the bug it exists to catch. Writing a
/// sentinel and reading it off the far end tests identity, not just liveness.
fn still_the_same_pipe(write_fd: i32, read_fd: i32) -> bool {
    let out: [u8; 1] = [0xA5];
    // SAFETY: writing one byte from a valid stack buffer to a candidate fd.
    let n = unsafe { libc::write(write_fd, out.as_ptr() as *const libc::c_void, 1) };
    if n != 1 {
        return false;
    }
    let mut back = [0u8; 1];
    // SAFETY: reading one byte into a valid stack buffer.
    let n = unsafe { libc::read(read_fd, back.as_mut_ptr() as *mut libc::c_void, 1) };
    n == 1 && back[0] == 0xA5
}

/// An `open` that fails after the `Arc` exists must not close the caller's fd.
///
/// `Handle` used to take the write descriptor in its constructor, so the `Arc`
/// drop on a late failure path ran `close()` on a descriptor ownership had never
/// transferred for. The caller then closed it again, and in a process that had
/// meanwhile opened a file, the second close landed on an unrelated descriptor.
///
/// Miri cannot run pipe(2) or the worker threads, so this is skipped there; the
/// fd-ownership logic it covers is not the kind Miri checks for.
#[test]
#[cfg_attr(miri, ignore)]
fn failed_open_leaves_the_completion_fd_with_the_caller() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();

    // Allow two workers, then fail: the failure has to land *after* the Arc and
    // its Drop exist, which is the only window in which the old code could
    // close a descriptor it did not own.
    arm_spawn_failure(2);
    let result = Handle::open(4, w);
    disarm_spawn_failure();

    match result {
        Ok(_) => panic!("injected spawn failure did not fail the open"),
        Err(e) => assert!(
            e.contains("injected spawn failure"),
            "open failed for an unexpected reason: {}",
            e
        ),
    }

    assert!(
        still_the_same_pipe(w, r),
        "open() failed but closed the caller's completion descriptor: ownership \
             must transfer only once the pool is fully up"
    );
    assert_fd_flags_untouched(w, "an open that failed spawning its workers");

    // SAFETY: both descriptors are still owned by this test.
    unsafe {
        libc::close(w);
        libc::close(r);
    }
}

/// Fails if `fd` carries O_NONBLOCK, or (on Darwin) F_SETNOSIGPIPE: the two
/// flags `open` sets on a descriptor it takes ownership of.
fn assert_fd_flags_untouched(fd: i32, what: &str) {
    // SAFETY: F_GETFL on a descriptor the caller owns reads its status flags.
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
    assert!(flags >= 0, "F_GETFL failed on the caller's descriptor");
    assert_eq!(
        flags & libc::O_NONBLOCK,
        0,
        "{what} left the caller's descriptor O_NONBLOCK: a refused open must not \
             mutate a descriptor it never took"
    );
    #[cfg(any(
        target_os = "macos",
        target_os = "ios",
        target_os = "watchos",
        target_os = "tvos"
    ))]
    {
        // XNU's F_GETNOSIGPIPE (sys/fcntl.h); libc exports only SO_NOSIGPIPE.
        const F_GETNOSIGPIPE: libc::c_int = 74;
        // SAFETY: as above, a read of one descriptor flag.
        let nosigpipe = unsafe { libc::fcntl(fd, F_GETNOSIGPIPE) };
        assert_eq!(
            nosigpipe, 0,
            "{what} left the caller's descriptor NOSIGPIPE"
        );
    }
}

/// An `open` refused for its arguments leaves the caller's descriptor as it
/// was handed in.
///
/// `open` used to set O_NONBLOCK (and NOSIGPIPE on Darwin) before checking
/// the pool size, so a refused open still changed the blocking mode of a
/// descriptor whose ownership never transferred, and the caller went on
/// writing to it with EAGAIN it had not asked for.
#[test]
#[cfg_attr(miri, ignore)]
fn refused_open_does_not_mutate_the_callers_descriptor() {
    let (r, w) = make_pipe();

    let refused = Handle::open(MAX_POOL_SIZE as u32 + 1, w);
    match refused {
        Ok(_) => panic!("a pool above MAX_POOL_SIZE was accepted"),
        Err(e) => assert!(e.contains("exceeds maximum"), "unexpected refusal: {e}"),
    }
    assert_fd_flags_untouched(w, "an open refused for its pool size");
    assert!(
        still_the_same_pipe(w, r),
        "a refused open closed the caller's descriptor"
    );

    // SAFETY: both descriptors are still owned by this test.
    unsafe {
        libc::close(w);
        libc::close(r);
    }
}

/// A worker's stack is 8 MiB because Gusset asked for it (I5, R8).
///
/// Read back from the thread that actually ran the job, through the real pool.
/// A deep-recursion probe cannot distinguish this from the platform default on
/// glibc or darwin, which is why R8 previously had no gate outside a musl host.
#[test]
#[cfg_attr(miri, ignore)]
fn worker_stack_is_explicitly_sized_not_inherited() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let (r, w) = make_pipe();
    // R3 forbids `expect`, in test code too since clippy gained --all-targets.
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let ticket = match handle.submit(header, &[8], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };

    // Drain the completion ticket so the worker never blocks writing it.
    let mut buf = [0u8; 8];
    let mut got = 0usize;
    while got < buf.len() {
        // SAFETY: reading into a valid stack buffer from the pipe's read end.
        let n = unsafe {
            libc::read(
                r,
                buf.as_mut_ptr().add(got) as *mut libc::c_void,
                buf.len() - got,
            )
        };
        assert!(n > 0, "completion pipe read failed");
        got += n as usize;
    }
    assert_eq!(u64::from_ne_bytes(buf), ticket);

    let result = match handle.take(ticket) {
        Ok(r) => r,
        Err(e) => panic!("take failed: {}", e),
    };
    let reported = match result {
        JobResult::Ok(out) => {
            assert_eq!(out.len(), 8, "mode 8 returns a u64");
            let mut b = [0u8; 8];
            b.copy_from_slice(&out);
            u64::from_le_bytes(b) as usize
        }
        other => panic!("expected Ok from diagnostic mode 8, got {:?}", other),
    };

    assert_ne!(
        reported, 0,
        "the worker could not read its own stack size, so I5's 8 MiB guarantee \
             is unverified on this platform rather than confirmed"
    );
    assert!(
        reported >= WORKER_STACK_SIZE,
        "worker ran on a {} byte stack, below the {} bytes Gusset requests; on musl \
             the inherited default is 128 KiB (I5/R8)",
        reported,
        WORKER_STACK_SIZE
    );

    handle.close().must("close");
    // SAFETY: the read end is still owned by this test; close() took the write end.
    unsafe {
        libc::close(r);
    }
}

/// The thread whose `Handle::open` sizes the pipe for bare tickets only,
/// as when the pipe cannot grow to hold inline records. Scoped to one
/// thread like the spawn injector (R7 rules out `thread_local!`), so
/// concurrently running tests are unaffected.
static FORCE_TICKET_ONLY: Mutex<Option<std::thread::ThreadId>> = Mutex::new(None);

pub(super) fn force_ticket_only() -> bool {
    *lock_recover(&FORCE_TICKET_ONLY) == Some(std::thread::current().id())
}

/// True once the pipe has a completion to read, within `ms`.
fn completion_ready_within(read_fd: i32, ms: i32) -> bool {
    let mut pfd = libc::pollfd {
        fd: read_fd,
        events: libc::POLLIN,
        revents: 0,
    };
    // SAFETY: one valid pollfd on the stack.
    let n = unsafe { libc::poll(&mut pfd, 1, ms) };
    n == 1 && pfd.revents & libc::POLLIN != 0
}

/// Trace id that makes the worker running the unit exit after completing
/// it. Compiled only under `cfg(test)`, like the spawn injection.
pub(super) const KILL_TRACE: [u8; 16] = *b"gusset:kill-unit";

/// Kills `n` distinct workers: each runs one marked job, delivers its
/// completion and exits. Returns once all `n` are joinable.
///
/// The jobs sleep 20 ms, so every submit lands before the first death:
/// a submit after a death would reap and respawn it, and the exits
/// counted here would no longer be the ones this call caused.
fn kill_workers(handle: &Handle, read_fd: i32, n: usize) {
    let header = CallHeader {
        trace_id: KILL_TRACE,
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let before = handle.exited.load(Ordering::Acquire);
    for _ in 0..n {
        if let Err(e) = handle.submit(header, &[9, 2], 0) {
            panic!("submit to kill a worker failed: {e}");
        }
    }
    for _ in 0..n {
        assert!(
            completion_ready_within(read_fd, 5_000),
            "kill job never completed"
        );
        drain_ticket(read_fd);
    }
    let start = std::time::Instant::now();
    loop {
        // Counted and joinable: `exited` rises in the worker's last drop,
        // a moment before its JoinHandle reports finished, and a reap in
        // between would find nothing to reap yet.
        let (finished, len) = {
            let w = lock_recover(&handle.workers);
            (w.iter().filter(|h| h.is_finished()).count(), w.len())
        };
        let exited = handle.exited.load(Ordering::Acquire);
        if exited >= before + n && finished >= n {
            return;
        }
        if start.elapsed() > std::time::Duration::from_secs(5) {
            panic!(
                "marked workers never exited: exited {exited} (from {before}), \
                     {finished} of {len} finished, {n} expected"
            );
        }
        std::thread::yield_now();
    }
}

/// A respawn that fails must be retried by the next submit (I4, I5).
///
/// `ensure_workers` consumed the `exited` count before spawning the
/// replacements. When that spawn failed (the OS out of threads), the count
/// was already 0, so every later submit took the fast path and never
/// retried: the pool stayed shrunk for the handle's lifetime, and with no
/// worker left a submission was accepted, queued, and never run. Covers a
/// total failure (pool of 1) and a partial one (pool of 3, one of two
/// replacements spawned before the failure).
#[test]
#[cfg_attr(miri, ignore)]
fn failed_respawn_is_retried_by_the_next_submit() {
    let _serialise = lock_recover(&INJECT_LOCK);
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    // (pool size, workers to kill, spawns allowed before the failure)
    for (pool, kill, allow) in [(1usize, 1usize, 0i64), (3, 2, 1)] {
        let (r, w) = make_pipe();
        let handle = match Handle::open(pool as u32, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {e}"),
        };
        kill_workers(&handle, r, kill);

        arm_spawn_failure(allow);
        let refused = handle.submit(header, &[0, 1], 0);
        disarm_spawn_failure();
        match refused {
            Ok(_) => panic!("pool {pool}: injected respawn failure did not fail the submit"),
            Err(e) => assert!(
                e.contains("injected spawn failure"),
                "unexpected error: {e}"
            ),
        }

        // No further worker dies: the only thing that can bring the
        // pool back is the next submit retrying the respawn. Pool 1 has
        // no worker left at all, so its unit can only run on a retry.
        let alive = lock_recover(&handle.workers).len();
        assert_eq!(
            alive,
            pool - kill + allow as usize,
            "pool {pool}: survivors"
        );

        if let Err(e) = handle.submit(header, &[0, 1], 0) {
            panic!("pool {pool}: submit after the failed respawn refused: {e}");
        }
        assert!(
            completion_ready_within(r, 5_000),
            "pool {pool}: a submission after a failed respawn never ran; {} of {pool} \
                 workers alive",
            lock_recover(&handle.workers)
                .iter()
                .filter(|h| !h.is_finished())
                .count()
        );
        drain_ticket(r);
        assert_eq!(
            lock_recover(&handle.workers).len(),
            pool,
            "pool {pool}: the retry must restore the full pool"
        );

        handle.close().must("close");
        // SAFETY: the read end is still owned by this test.
        unsafe {
            libc::close(r);
        }
    }
}

/// Respawns and failed opens release every thread, descriptor and mapping
/// they create (I4, I5): the dead worker's stack and `sigaltstack`, the
/// workers a partial open spawned, and nothing of the caller's pipe.
///
/// Needs the test-only kill hook and spawn injection, so it lives here,
/// but the counts are process-wide (`/proc/self`) and every other unit
/// test runs in parallel. It therefore re-executes this test binary for
/// itself alone and measures there.
#[test]
#[cfg(target_os = "linux")]
#[cfg_attr(miri, ignore)]
fn respawn_and_failed_open_release_threads_and_mappings() {
    const CHILD: &str = "GUSSET_RESPAWN_HYGIENE_CHILD";
    if std::env::var_os(CHILD).is_none() {
        let exe = match std::env::current_exe() {
            Ok(p) => p,
            Err(e) => panic!("current_exe: {e}"),
        };
        let out = std::process::Command::new(exe)
            .args([
                "--exact",
                "pool::tests::respawn_and_failed_open_release_threads_and_mappings",
                "--nocapture",
                "--test-threads=1",
            ])
            .env(CHILD, "1")
            // glibc creates a malloc arena lazily, when a thread finds every
            // existing one locked, and never unmaps it: a 132 KiB heap and
            // the rest of its 64 MiB reservation, two lines of
            // /proc/self/maps. How many arenas exist after N lifetimes
            // depends on lock contention, up to 8 per CPU, so the count
            // drifted by exactly those two lines in some runs and not
            // others once Close gained its joiner thread. That is the
            // allocator's cache, not a pool resource. One arena keeps
            // malloc on the main heap, so every mmap left in the count is
            // one the pool made: thread stacks and sigaltstacks. musl
            // ignores the variable.
            .env("MALLOC_ARENA_MAX", "1")
            .output();
        let out = match out {
            Ok(o) => o,
            Err(e) => panic!("re-exec failed: {e}"),
        };
        let text = String::from_utf8_lossy(&out.stdout).into_owned()
            + &String::from_utf8_lossy(&out.stderr);
        eprintln!("{text}");
        assert!(out.status.success(), "isolated run failed");
        assert!(
            text.contains("1 passed"),
            "isolated run did not run the test"
        );
        return;
    }

    fn entries(path: &str) -> usize {
        match std::fs::read_dir(path) {
            Ok(d) => d.count(),
            Err(e) => panic!("{path}: {e}"),
        }
    }
    fn maps() -> Vec<String> {
        match std::fs::read_to_string("/proc/self/maps") {
            Ok(s) => s.lines().map(str::to_owned).collect(),
            Err(e) => panic!("/proc/self/maps: {e}"),
        }
    }
    // Bytes as well as lines. The kernel merges an anonymous mapping into
    // a neighbour with the same protection, so a leak shaped like its
    // neighbour adds no line at all: one leaked read-only page per
    // lifetime passed the line count alone. The brk heap is malloc's,
    // grows and trims with ordinary allocation, and is not a pool mapping.
    fn mapped_bytes(maps: &[String]) -> usize {
        maps.iter()
            .filter(|line| !line.ends_with("[heap]"))
            .map(|line| {
                let range = line.split(' ').next().unwrap_or("");
                let (lo, hi) = range.split_once('-').unwrap_or(("0", "0"));
                match (usize::from_str_radix(lo, 16), usize::from_str_radix(hi, 16)) {
                    (Ok(lo), Ok(hi)) if hi >= lo => hi - lo,
                    _ => panic!("unparsable /proc/self/maps line: {line}"),
                }
            })
            .sum()
    }
    let usage = || {
        let maps = maps();
        (
            entries("/proc/self/fd"),
            entries("/proc/self/task"),
            maps.len(),
            mapped_bytes(&maps),
        )
    };
    let settled = |base: (usize, usize, usize, usize)| {
        let mut now = usage();
        for _ in 0..200 {
            if now.0 <= base.0 && now.1 <= base.1 && now.2 <= base.2 && now.3 <= base.3 {
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(5));
            now = usage();
        }
        now
    };
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let rounds: usize = std::env::var("GUSSET_HYGIENE_ROUNDS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(250);

    let lifetime = || {
        let (r, w) = make_pipe();
        let handle = match Handle::open(4, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {e}"),
        };
        // Two workers die and are respawned by the next submit.
        kill_workers(&handle, r, 2);
        if let Err(e) = handle.submit(header, &[0, 1], 0) {
            panic!("submit failed: {e}");
        }
        assert!(completion_ready_within(r, 5_000));
        drain_ticket(r);
        // One more dies; its respawn fails once, then succeeds.
        kill_workers(&handle, r, 1);
        arm_spawn_failure(0);
        assert!(handle.submit(header, &[0, 1], 0).is_err());
        disarm_spawn_failure();
        if let Err(e) = handle.submit(header, &[0, 1], 0) {
            panic!("submit after a failed respawn failed: {e}");
        }
        assert!(completion_ready_within(r, 5_000));
        drain_ticket(r);
        assert_eq!(lock_recover(&handle.workers).len(), 4);
        handle.close().must("close");
        // SAFETY: the read end is still this test's.
        unsafe {
            libc::close(r);
        }
    };
    let failed_open = || {
        let (r, w) = make_pipe();
        arm_spawn_failure(2);
        let res = Handle::open(4, w);
        disarm_spawn_failure();
        assert!(res.is_err(), "injected spawn failure did not fail the open");
        assert!(
            still_the_same_pipe(w, r),
            "failed open closed the caller's fd"
        );
        // SAFETY: both ends are still this test's.
        unsafe {
            libc::close(w);
            libc::close(r);
        }
    };

    for _ in 0..50 {
        lifetime();
        failed_open();
    }
    let base = settled(usage());
    let base_maps = maps();
    for _ in 0..rounds {
        lifetime();
    }
    let after_respawns = settled(base);
    for _ in 0..rounds {
        failed_open();
    }
    let after_failed_opens = settled(base);
    // A count says that something leaked; the mapping says what. A thread
    // stack is a guard page and the stack below it, a sigaltstack the size
    // `sys::sigaltstack_size` reports.
    let new_maps: Vec<String> = maps()
        .into_iter()
        .filter(|line| !base_maps.contains(line))
        .collect();
    eprintln!(
        "respawn hygiene over {rounds} lifetimes (3 respawns each, 1 after a failed \
             spawn) and {rounds} partially spawned opens: (fds, threads, map lines, mapped \
             bytes) baseline {base:?}, after respawns {after_respawns:?}, after failed opens \
             {after_failed_opens:?}; mappings not in the baseline: {new_maps:#?}"
    );
    // Descriptors and threads return to exactly the baseline. Mappings may
    // end below it, never above: glibc evicting a cached thread stack is
    // a smaller address space, not a leak, and an equality check would
    // fail on it.
    let leaked = |after: (usize, usize, usize, usize)| {
        after.0 != base.0 || after.1 != base.1 || after.2 > base.2 || after.3 > base.3
    };
    assert!(
        !leaked(after_respawns),
        "respawned workers leaked resources"
    );
    assert!(
        !leaked(after_failed_opens),
        "partial opens leaked resources"
    );
}

/// Drain one 8-byte ticket from the completion pipe.
fn drain_ticket(read_fd: i32) -> u64 {
    let mut buf = [0u8; 8];
    let mut got = 0usize;
    while got < buf.len() {
        // SAFETY: reading into a valid stack buffer from the pipe's read end.
        let n = unsafe {
            libc::read(
                read_fd,
                buf.as_mut_ptr().add(got) as *mut libc::c_void,
                buf.len() - got,
            )
        };
        assert!(n > 0, "completion pipe read failed");
        got += n as usize;
    }
    u64::from_ne_bytes(buf)
}
