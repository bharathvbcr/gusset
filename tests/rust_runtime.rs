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
