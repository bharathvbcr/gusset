//! Opening and closing handles must not leak descriptors, threads or mappings
//! (I4, I5).
//!
//! Per handle, Rust creates `pool_size` worker threads (each an 8 MiB stack
//! plus a guard-paged `sigaltstack` mapping), takes ownership of the
//! completion pipe's write end, and — once a reader attaches one — a
//! completion ring. `close` must give every one of them back, and so must each
//! way `open` can fail. This drives thousands of handles through the success
//! and failure paths and checks that `/proc/self/fd`, `/proc/self/task` and
//! `/proc/self/maps` return to the baseline taken after a warm-up.
//!
//! Linux only (it reads `/proc/self`). One test in its own binary, so no other
//! test's threads or descriptors move the counts while it measures.

#![cfg(target_os = "linux")]
#![allow(unsafe_code)]

use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::{Handle, MAX_POOL_SIZE};
use std::time::Duration;

fn make_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
    assert_eq!(rc, 0, "pipe() failed");
    (fds[0], fds[1])
}

fn close(fd: i32) {
    unsafe {
        libc::close(fd);
    }
}

fn count_dir(path: &str) -> usize {
    match std::fs::read_dir(path) {
        Ok(d) => d.count(),
        Err(e) => panic!("read {path}: {e}"),
    }
}

fn count_lines(path: &str) -> usize {
    match std::fs::read_to_string(path) {
        Ok(s) => s.lines().count(),
        Err(e) => panic!("read {path}: {e}"),
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Usage {
    fds: usize,
    threads: usize,
    maps: usize,
}

fn usage() -> Usage {
    Usage {
        fds: count_dir("/proc/self/fd"),
        threads: count_dir("/proc/self/task"),
        maps: count_lines("/proc/self/maps"),
    }
}

/// Thread exit is asynchronous to `join` returning only in the kernel's
/// bookkeeping of `/proc/self/task`; give it a moment before comparing.
fn settled(base: Usage) -> Usage {
    let mut now = usage();
    for _ in 0..200 {
        if now.threads <= base.threads && now.fds <= base.fds && now.maps <= base.maps {
            break;
        }
        std::thread::sleep(Duration::from_millis(5));
        now = usage();
    }
    now
}

fn read_ticket(fd: i32) -> u64 {
    let mut buf = [0u8; 8];
    let mut got = 0usize;
    while got < 8 {
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        if unsafe { libc::poll(&mut pfd, 1, 5000) } <= 0 {
            panic!("no completion within 5s");
        }
        let n = unsafe {
            libc::read(
                fd,
                buf.as_mut_ptr().add(got) as *mut libc::c_void,
                buf.len() - got,
            )
        };
        assert!(n > 0, "completion read failed");
        got += n as usize;
    }
    u64::from_ne_bytes(buf)
}

/// One full lifetime: open, run jobs on every worker, attach a ring (and be
/// refused a second), take the results, close. Returns the usage measured
/// just before the close, while everything the handle owns is live.
fn one_lifetime(pool: u32, ring: bool) -> Usage {
    let (r, w) = make_pipe();
    let h = match Handle::open(pool, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {e}"),
    };
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    let attached = if ring {
        let a = match h.attach_ring() {
            Ok(a) => a,
            Err(e) => panic!("attach_ring failed: {e}"),
        };
        assert!(h.attach_ring().is_err(), "a second ring must be refused");
        Some(a)
    } else {
        None
    };
    // Pipe path only: with a ring attached the completion lands in the ring,
    // which this test does not read, so submit only without one.
    if !ring {
        for _ in 0..pool {
            let t = match h.submit(header, &[0, 1, 2], 0) {
                Ok(t) => t,
                Err(e) => panic!("submit failed: {e}"),
            };
            assert_eq!(read_ticket(r), t);
            assert!(h.take(t).is_ok(), "take failed");
        }
    }
    let open_usage = usage();
    h.close();
    drop(attached);
    drop(h);
    close(r);
    open_usage
}

#[test]
fn handles_release_descriptors_threads_and_mappings() {
    let quick = std::env::var_os("GUSSET_HYGIENE_ROUNDS")
        .and_then(|v| v.into_string().ok())
        .and_then(|v| v.parse::<usize>().ok());
    let rounds = quick.unwrap_or(2000);

    // Warm-up: the allocator's per-thread arenas and glibc's cache of freed
    // thread stacks are process-lifetime and bounded; let them reach their
    // steady size before taking the baseline.
    for _ in 0..50 {
        one_lifetime(4, false);
        one_lifetime(4, true);
    }
    let base = settled(usage());

    // What one open handle holds, so the report shows what was released.
    let mut peak = base;
    for i in 0..rounds {
        let u = one_lifetime(4, i % 2 == 1);
        peak.fds = peak.fds.max(u.fds);
        peak.threads = peak.threads.max(u.threads);
        peak.maps = peak.maps.max(u.maps);
    }
    let after_success = settled(base);

    // Failure paths of open. Each must leave the caller's descriptor open
    // and create nothing that outlives the call.
    for _ in 0..rounds {
        // Pool above the ceiling: refused before any thread exists.
        let (r, w) = make_pipe();
        assert!(Handle::open(MAX_POOL_SIZE as u32 + 1, w).is_err());
        close(w);
        close(r);

        // Invalid descriptors: refused before any allocation.
        assert!(Handle::open(2, -1).is_err());
        let (r, w) = make_pipe();
        close(w);
        close(r);
        assert!(Handle::open(2, w).is_err(), "a closed fd must be refused");
    }
    let after_failures = settled(base);

    eprintln!(
        "resource hygiene over {rounds} lifetimes (pool 4, half with a ring) and \
         {rounds}x3 failed opens: baseline {base:?}, peak with one handle open {peak:?}, after \
         lifetimes {after_success:?}, after failed opens {after_failures:?}"
    );
    assert_eq!(after_success, base, "handle lifetimes leaked resources");
    assert_eq!(after_failures, base, "failed opens leaked resources");
}
