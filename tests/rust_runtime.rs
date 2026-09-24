//! Runtime hardening regression tests for the Gusset Rust crate.
//!
//! Every test here pins a defect found in the v0.0.1 audit. Each one fails against
//! the pre-fix code; the comment on each names the failure it reproduces.
//!
//! These run in one test binary and share process globals, so no test here touches
//! the shutdown flag or registers an engine handler. Those live in their own test
//! binaries (`rust_shutdown.rs`, and the adopter test in `crates/gusset-example`)
//! precisely because a global that one test mutates is a global every parallel test
//! in the same binary observes.

use gusset::ffi::guard::panic_location_count;
use gusset::ffi::log_event;
use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::sys::write_ticket;
use gusset::pool::{Handle, MAX_POOL_SIZE};
use std::time::{Duration, Instant};

/// Creates a pipe, returning (read_fd, write_fd).
fn make_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
    assert_eq!(rc, 0, "pipe() failed");
    (fds[0], fds[1])
}

/// Reports whether a descriptor number is currently open in this process.
fn fd_is_open(fd: i32) -> bool {
    unsafe { libc::fcntl(fd, libc::F_GETFD) != -1 }
}

fn set_nonblocking(fd: i32) {
    unsafe {
        let flags = libc::fcntl(fd, libc::F_GETFL);
        assert_ne!(flags, -1, "F_GETFL failed");
        assert_ne!(
            libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK),
            -1,
            "F_SETFL O_NONBLOCK failed"
        );
    }
}

/// Pool size came straight from a caller argument with no ceiling. Each worker is an
/// OS thread with an 8 MiB stack, so `WithPoolSize(1 << 20)` was a request for a
/// million threads and 8 TiB of stack address space.
#[test]
fn open_rejects_pool_size_above_maximum() {
    let (r, w) = make_pipe();

    let over = (MAX_POOL_SIZE + 1) as u32;
    match Handle::open(over, w) {
        Ok(_) => panic!("pool size {} must be refused, not spawned", over),
        Err(e) => {
            assert!(
                e.contains("exceeds maximum"),
                "error should name the ceiling, got: {}",
                e
            );
        }
    }

    // The refusal must leave the caller's descriptors untouched.
    assert!(fd_is_open(w), "refused open must not close the write fd");
    assert!(fd_is_open(r), "refused open must not close the read fd");

    unsafe {
        libc::close(r);
        libc::close(w);
    }
}

/// `Handle` used to take the write descriptor at construction, so an `open` that
/// failed after the `Arc` existed ran `Drop` -> `close` -> `close(fd)` on a
/// descriptor the caller still owned and would close itself. Double-closing an fd
/// number shuts whatever unrelated file has since been handed that number.
///
/// The descriptor is now published only once `open` has fully succeeded, and `close`
/// takes it back with a swap, so it is released exactly once and only by the owner.
#[test]
fn descriptor_ownership_transfers_only_on_successful_open() {
    // A rejected open leaves the descriptor with the caller.
    let (r1, w1) = make_pipe();
    assert!(Handle::open((MAX_POOL_SIZE + 1) as u32, w1).is_err());
    assert!(
        fd_is_open(w1),
        "a handle that never opened must not close the caller's descriptor"
    );
    unsafe {
        libc::close(r1);
        libc::close(w1);
    }

    // A successful open takes ownership, and close releases it exactly once.
    let (r2, w2) = make_pipe();
    let handle = match Handle::open(2, w2) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };
    assert!(fd_is_open(w2), "an open handle owns a live descriptor");

    handle.close();
    assert!(
        !fd_is_open(w2),
        "close must release the completion pipe descriptor"
    );

    // Second close, and the Drop that follows, must not close it again.
    handle.close();
    drop(handle);
    assert!(
        !fd_is_open(w2),
        "descriptor must stay closed, not be closed twice"
    );

    unsafe {
        libc::close(r2);
    }
}

/// `CallHeader::flags` was read for nothing and unknown bits were silently ignored,
/// so a caller built against a newer header would believe a flag took effect against
/// an older library. Unknown bits are now refused.
#[test]
fn submit_rejects_unknown_header_flags() {
    let (r, w) = make_pipe();
    let handle = match Handle::open(2, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    // No such bit in this ABI version.
    let header = CallHeader {
        flags: 1 << 31,
        ..Default::default()
    };
    match handle.submit(header, &[0u8], 0) {
        Ok(_) => panic!("an unknown flag bit must be refused, not ignored"),
        Err(e) => assert!(
            e.contains("unknown CallHeader flag"),
            "error should name the offending bits, got: {}",
            e
        ),
    }

    // The known bit is accepted, so the check rejects the unknown rather than all.
    let known = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    match handle.submit(known, &[0u8], 0) {
        Ok(_) => {}
        Err(e) => panic!("known flag bit must be accepted: {}", e),
    }

    handle.close();
    unsafe {
        libc::close(r);
    }
}

/// A full completion pipe used to spin `yield_now()` forever, burning a core and
/// hanging `Handle::close` in `join`. It now backs off and gives up.
#[test]
fn write_ticket_gives_up_on_a_permanently_full_pipe() {
    let (r, w) = make_pipe();
    set_nonblocking(w);

    // Fill the pipe. Nothing ever reads it.
    let filler = vec![0u8; 4096];
    loop {
        let n = unsafe { libc::write(w, filler.as_ptr() as *const libc::c_void, filler.len()) };
        if n < 0 {
            break;
        }
    }

    let started = Instant::now();
    let result = write_ticket(w, 42);
    let elapsed = started.elapsed();

    assert!(
        result.is_err(),
        "a permanently full pipe must produce an error, not an infinite spin"
    );
    assert!(
        elapsed < Duration::from_secs(30),
        "write_ticket must be bounded; took {:?}",
        elapsed
    );

    unsafe {
        libc::close(r);
        libc::close(w);
    }
}

/// A slow-but-live reader must still be served: the retry path has to make progress,
/// not just time out. This is the other half of the bound above.
#[test]
fn write_ticket_succeeds_once_a_stalled_reader_drains() {
    let (r, w) = make_pipe();
    set_nonblocking(w);

    let filler = vec![0u8; 4096];
    loop {
        let n = unsafe { libc::write(w, filler.as_ptr() as *const libc::c_void, filler.len()) };
        if n < 0 {
            break;
        }
    }

    // Drain the pipe shortly after the write starts backing off.
    let reader = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(200));
        let mut sink = vec![0u8; 65536];
        unsafe {
            libc::read(r, sink.as_mut_ptr() as *mut libc::c_void, sink.len());
        }
        r
    });

    let result = write_ticket(w, 7);
    assert!(
        result.is_ok(),
        "write must succeed once the reader drains, got {:?}",
        result.err()
    );

    match reader.join() {
        Ok(fd) => unsafe {
            libc::close(fd);
        },
        Err(_) => panic!("reader thread panicked"),
    }
    unsafe {
        libc::close(w);
    }
}

/// The log ring cleared its entire contents on overflow, throwing away every
/// diagnostic collected so far at exactly the moment they mattered, and a single
/// oversized line grew the buffer past its cap without limit.
#[test]
fn log_ring_evicts_oldest_and_stays_bounded() {
    const CAP: usize = 65536;

    fn drain() -> Vec<u8> {
        let mut out = vec![0u8; 1 << 21];
        let mut n = 0usize;
        unsafe {
            gusset::ffi::gusset_drain_logs(out.as_mut_ptr(), out.len(), &mut n);
        }
        out.truncate(n);
        out
    }

    // Part 1: a line larger than the whole ring is truncated, not stored whole.
    // Previously nothing capped a single line, so one oversized message grew the
    // buffer past its budget without limit.
    let _ = drain();
    log_event(&"X".repeat(500_000));
    let stored = drain();
    assert!(
        stored.len() <= CAP,
        "an oversized line must be truncated to the ring budget, stored {} bytes",
        stored.len()
    );

    // Part 2: overflow evicts the oldest lines and keeps the newest. The old
    // implementation called buf.clear() instead, discarding every diagnostic
    // collected so far at exactly the moment they were worth having.
    const LINES: usize = 20_000;
    for i in 0..LINES {
        log_event(&format!("line-{}", i));
    }

    let out = drain();
    assert!(
        out.len() <= CAP,
        "log ring must stay within its 64 KiB budget, drained {} bytes",
        out.len()
    );
    assert!(
        !out.is_empty(),
        "overflow must evict, not clear: the ring should still hold recent lines"
    );

    let text = String::from_utf8_lossy(&out);
    assert!(
        text.contains(&format!("line-{}", LINES - 1)),
        "the newest line must survive eviction"
    );
    assert!(
        !text.contains("line-0\n"),
        "the oldest lines must have been evicted"
    );

    // Every retained line must be intact: eviction drops whole lines, so no
    // fragment of a half-evicted line may be left at the front.
    for line in text.lines() {
        assert!(
            line.starts_with("line-"),
            "eviction must drop whole lines, found fragment: {:?}",
            line
        );
    }
}

/// The panic-location map was an unbounded `Vec<(ThreadId, PanicLocation)>` that only
/// shrank when the panicking thread itself took its entry back. Panics on threads
/// that never take — adopter code, threads Gusset does not own — left entries
/// behind, and `ThreadId`s are never reused, so it grew forever and made every panic
/// an O(n) scan of it.
#[test]
fn panic_locations_stay_bounded_across_unclaimed_panics() {
    // Each thread panics and exits without anyone taking its recorded location,
    // which is exactly the leak shape.
    for _ in 0..600 {
        let t = std::thread::spawn(|| {
            let _ = std::panic::catch_unwind(|| panic!("unclaimed"));
        });
        let _ = t.join();
    }

    let count = panic_location_count();
    assert!(
        count <= 256,
        "panic location map must stay bounded, holds {} entries",
        count
    );
}

/// Reads until EOF or `deadline`, returning the bytes seen and whether EOF came.
fn read_until_eof(fd: i32, deadline: Duration) -> (Vec<u8>, bool) {
    let start = Instant::now();
    let mut bytes = Vec::new();
    while start.elapsed() < deadline {
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        if unsafe { libc::poll(&mut pfd, 1, 200) } <= 0 {
            continue;
        }
        let mut buf = [0u8; 4096];
        let n = unsafe { libc::read(fd, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
        if n == 0 {
            return (bytes, true);
        }
        if n > 0 {
            bytes.extend_from_slice(&buf[..n as usize]);
        }
    }
    (bytes, false)
}

/// A worker publishing a completion upgrades its `Weak<Handle>`. If the caller
/// drops the last `Arc` in that window, the worker's upgrade is the last owner
/// and `Handle::drop` → `close` runs *on that worker*, which then joined its own
/// thread: EDEADLK, a std panic outside every firewall, the rest of the pool
/// detached and the completion pipe never closed. Now the worker skips itself,
/// the others are joined, and the pipe reaches EOF.
///
/// The window is widened deterministically: the pipe is filled first, so the
/// worker sits in its completion-write backoff holding the upgraded `Arc` while
/// the caller drops its own; draining the pipe then lets it finish.
#[test]
fn a_worker_holding_the_last_reference_closes_the_handle_cleanly() {
    let (r, w) = make_pipe();
    let h = match Handle::open(3, w) {
        Ok(h) => h,
        Err(e) => panic!("open: {e}"),
    };
    // open made the write end non-blocking; fill it until it refuses.
    let junk = [0xEEu8; 4096];
    let mut filled = 0usize;
    loop {
        let n = unsafe { libc::write(w, junk.as_ptr() as *const libc::c_void, junk.len()) };
        if n <= 0 {
            break;
        }
        filled += n as usize;
    }
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    // Mode 9, 2 units: sleeps 20 ms without checking, then echoes.
    let ticket = match h.submit(header, &[9, 2], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit: {e}"),
    };
    std::thread::sleep(Duration::from_millis(300));
    drop(h);
    let (bytes, eof) = read_until_eof(r, Duration::from_secs(8));
    unsafe { libc::close(r) };
    assert!(
        eof,
        "the completion pipe never closed: close deadlocked on its own worker"
    );
    assert_eq!(
        bytes.len(),
        filled + 8,
        "expected the filler plus exactly one ticket"
    );
    let t = &bytes[filled..];
    assert_eq!(
        u64::from_ne_bytes([t[0], t[1], t[2], t[3], t[4], t[5], t[6], t[7]]),
        ticket,
        "the stalled completion must still be delivered"
    );
}

/// `gusset_submit` built its input slice before validating it. A length past
/// `isize::MAX` made `slice::from_raw_parts` undefined behaviour (an abort in
/// debug builds, before any firewall), and a null pointer with a length was
/// silently treated as empty input.
#[test]
fn submit_validates_inline_input_before_building_a_slice() {
    use gusset::ffi::status::{FfiStatus, FFI_BAD_ARG, FFI_OK};
    use gusset::ffi::{gusset_buf_alloc, gusset_buf_free, gusset_status_free, gusset_submit};

    let (r, w) = make_pipe();
    let h = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open: {e}"),
    };
    let raw = std::sync::Arc::as_ptr(&h) as *mut Handle;
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let mut ticket = 0u64;
    let mut st = FfiStatus::ok();

    let bogus = std::ptr::NonNull::<u8>::dangling().as_ptr();
    for (ptr, len, what) in [
        (std::ptr::null(), 16usize, "null pointer with a length"),
        (bogus as *const u8, usize::MAX, "length past isize::MAX"),
        (bogus as *const u8, 4097, "length past the inline limit"),
    ] {
        let rc = unsafe { gusset_submit(raw, &header, ptr, len, 0, &mut ticket, &mut st) };
        assert_eq!(rc, FFI_BAD_ARG, "{what} must be refused");
        unsafe { gusset_status_free(&mut st) };
    }

    // A buffer submission never reads the inline pair, however bogus it is.
    let mut id = 0u64;
    let mut p = std::ptr::null_mut();
    let rc = unsafe { gusset_buf_alloc(raw, 8, &mut id, &mut p, &mut st) };
    assert_eq!(rc, FFI_OK);
    unsafe { std::ptr::write_bytes(p, 0, 8) };
    let rc = unsafe { gusset_submit(raw, &header, bogus, usize::MAX, id, &mut ticket, &mut st) };
    assert_eq!(rc, FFI_OK, "buffer submission must ignore the inline pair");
    let mut b = [0u8; 8];
    let n = unsafe { libc::read(r, b.as_mut_ptr() as *mut libc::c_void, 8) };
    assert_eq!(n, 8);
    let _ = h.take(ticket);
    let rc = unsafe { gusset_buf_free(raw, id, &mut st) };
    assert_eq!(rc, FFI_OK);
    h.close();
    unsafe { libc::close(r) };
}

/// Bounded concurrency only rules out a full completion pipe if the pipe holds
/// `pool_size` tickets. Linux hands out one-page pipes (512 tickets) under
/// `pipe-user-pages-soft` pressure; open now grows the pipe to fit.
#[cfg(target_os = "linux")]
#[test]
fn open_grows_a_one_page_completion_pipe_to_fit_the_pool() {
    let (r, w) = make_pipe();
    let page = unsafe { libc::fcntl(w, libc::F_SETPIPE_SZ, 4096) };
    assert!(page >= 4096, "could not shrink the test pipe");
    let pool = (page as usize / 8) + 8;
    assert!(pool <= MAX_POOL_SIZE);
    let h = match Handle::open(pool as u32, w) {
        Ok(h) => h,
        Err(e) => panic!("open: {e}"),
    };
    let now = unsafe { libc::fcntl(r, libc::F_GETPIPE_SZ) };
    assert!(
        now as usize >= pool * 8,
        "pipe holds {now} bytes, pool of {pool} needs {}",
        pool * 8
    );
    h.close();
    unsafe { libc::close(r) };
}

/// Fills the (non-blocking) write end until it refuses; returns bytes written.
fn fill_pipe(w: i32) -> usize {
    let junk = [0xEEu8; 4096];
    let mut filled = 0usize;
    loop {
        let n = unsafe { libc::write(w, junk.as_ptr() as *const libc::c_void, junk.len()) };
        if n <= 0 {
            return filled;
        }
        filled += n as usize;
    }
}

/// Reads exactly `n` bytes, polling up to `deadline`.
fn read_exact_within(fd: i32, n: usize, deadline: Duration) -> Vec<u8> {
    let start = Instant::now();
    let mut out = Vec::with_capacity(n);
    while out.len() < n && start.elapsed() < deadline {
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        if unsafe { libc::poll(&mut pfd, 1, 100) } <= 0 {
            continue;
        }
        let mut buf = vec![0u8; n - out.len()];
        let got = unsafe { libc::read(fd, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
        if got > 0 {
            out.extend_from_slice(&buf[..got as usize]);
        }
    }
    out
}

/// Two defects on the completion path, pinned together because both need a
/// stalled reader:
///
/// 1. The unit left `in_flight()` before its completion was written, so
///    `gusset_shutdown` could report a clean drain while a worker still sat in
///    the up-to-10 s write backoff.
/// 2. A completion whose write timed out was logged and dropped. Its Go waiter
///    and pool permit were stranded forever; after `pool_size` of them every
///    Submit blocked. It is now kept and delivered once the reader recovers.
#[test]
fn a_stalled_completion_counts_as_in_flight_and_is_delivered_after_recovery() {
    let (r, w) = make_pipe();
    let h = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open: {e}"),
    };
    let filled = fill_pipe(w);
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let first = match h.submit(header, &[0, 1], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit: {e}"),
    };
    std::thread::sleep(Duration::from_millis(300));
    assert_eq!(
        h.in_flight(),
        1,
        "a unit whose completion is not yet written is still in flight"
    );

    // Outlast the write timeout: the completion is now undeliverable for good
    // under the old code.
    std::thread::sleep(Duration::from_millis(10_700));

    // The reader recovers.
    let junk = read_exact_within(r, filled, Duration::from_secs(5));
    assert_eq!(junk.len(), filled, "could not drain the filler");

    let second = match h.submit(header, &[0, 2], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit after recovery: {e}"),
    };
    let bytes = read_exact_within(r, 16, Duration::from_secs(5));
    assert_eq!(
        bytes.len(),
        16,
        "expected both completions, got {} bytes",
        bytes.len()
    );
    let mut got: Vec<u64> = bytes
        .as_chunks::<8>()
        .0
        .iter()
        .map(|c| u64::from_ne_bytes(*c))
        .collect();
    got.sort_unstable();
    assert_eq!(
        got,
        vec![first, second],
        "the stalled completion was dropped"
    );
    assert!(
        h.take(first).is_ok(),
        "the stalled result must still be collectable"
    );
    assert_eq!(h.in_flight(), 0);
    h.close();
    unsafe { libc::close(r) };
}

/// Each worker's alternate signal stack is at least the documented floor and
/// sits above a guard page.
#[test]
fn workers_get_a_guard_paged_sigaltstack_of_at_least_the_floor() {
    use gusset::pool::sys::{install_sigaltstack, sigaltstack_size, SIGALTSTACK_MIN};
    assert!(sigaltstack_size() >= SIGALTSTACK_MIN);
    let res = std::thread::spawn(|| {
        let guard = install_sigaltstack();
        assert!(guard.is_some(), "install_sigaltstack failed");
        let mut cur: libc::stack_t = unsafe { std::mem::zeroed() };
        let rc = unsafe { libc::sigaltstack(std::ptr::null(), &mut cur) };
        assert_eq!(rc, 0);
        assert!(cur.ss_size >= SIGALTSTACK_MIN, "ss_size {}", cur.ss_size);
        // The page just below the stack must be mapped PROT_NONE.
        #[cfg(target_os = "linux")]
        {
            let below = cur.ss_sp as usize - 1;
            let maps = std::fs::read_to_string("/proc/self/maps").unwrap_or_default();
            let perms = maps.lines().find_map(|l| {
                let mut it = l.split_whitespace();
                let range = it.next()?;
                let perms = it.next()?;
                let (lo, hi) = range.split_once('-')?;
                let lo = usize::from_str_radix(lo, 16).ok()?;
                let hi = usize::from_str_radix(hi, 16).ok()?;
                (lo <= below && below < hi).then(|| perms.to_string())
            });
            assert_eq!(
                perms.as_deref(),
                Some("---p"),
                "no guard page below the stack"
            );
        }
        drop(guard);
    })
    .join();
    assert!(res.is_ok());
}
