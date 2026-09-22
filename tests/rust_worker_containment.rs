//! A worker must complete every ticket it dequeues, whatever the engine does.
//!
//! The panic firewall is `catch_unwind` around the engine call, but the payload it
//! catches is dropped afterwards, *outside* that `catch_unwind`. A payload whose
//! `Drop` panics therefore unwound the worker thread itself: the ticket was never
//! stored or written to the completion pipe, its cancel flag stayed registered, and
//! the Go caller waiting on it — with no deadline — parked forever while holding a
//! pool permit (I2, I4).
//!
//! Separate test binary because it registers a process-global engine handler.

use gusset::pool::{set_engine_handler, Handle, JobResult};
use std::time::{Duration, Instant};

fn make_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
    assert_eq!(rc, 0, "pipe() failed");
    (fds[0], fds[1])
}

/// Reads one 8-byte completion ticket, or `None` once `limit` passes without one.
fn read_ticket(fd: i32, limit: Duration) -> Option<u64> {
    let deadline = Instant::now() + limit;
    let mut buf = [0u8; 8];
    let mut got = 0usize;
    while got < buf.len() {
        let left = deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return None;
        }
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        let ms = left.as_millis().min(i32::MAX as u128) as i32;
        let rc = unsafe { libc::poll(&mut pfd, 1, ms.max(1)) };
        if rc <= 0 {
            continue;
        }
        let n = unsafe {
            libc::read(
                fd,
                buf.as_mut_ptr().add(got) as *mut libc::c_void,
                buf.len() - got,
            )
        };
        if n <= 0 {
            return None;
        }
        got += n as usize;
    }
    Some(u64::from_ne_bytes(buf))
}

/// A panic payload whose destructor panics again.
struct DropBomb {
    /// How many further bombs the destructor re-throws before giving up.
    depth: u8,
}

impl Drop for DropBomb {
    fn drop(&mut self) {
        if self.depth == 0 {
            panic!("DropBomb destructor panicked");
        }
        std::panic::panic_any(DropBomb {
            depth: self.depth - 1,
        });
    }
}

/// Line of the `panic_any` below, which the Panic result must report: disposal of
/// the payload panics again from `Drop`, and that location must not replace it.
const THROW_LINE: u32 = line!() + 2;
fn throw_bomb(depth: u8) -> ! {
    std::panic::panic_any(DropBomb { depth })
}

fn engine(_ctx: &gusset::JobContext, input: &[u8]) -> Result<Vec<u8>, String> {
    match input.first().copied() {
        Some(1) => throw_bomb(0),
        // Every drop re-throws a fresh bomb: a containment that recursed into
        // dropping the second payload would never terminate.
        Some(2) => throw_bomb(3),
        _ => Ok(input.to_vec()),
    }
}

fn assert_bomb_completes(kind: u8) {
    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    let ticket = match handle.submit(Default::default(), &[kind], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };

    let Some(done) = read_ticket(r, Duration::from_secs(5)) else {
        panic!(
            "no completion for ticket {ticket} within 5s: the worker died outside the \
             panic firewall and the Go waiter would park forever (I2, I4)"
        );
    };
    assert_eq!(done, ticket);

    match handle.take(ticket) {
        Ok(JobResult::Panic { line, .. }) => assert_eq!(
            line, THROW_LINE,
            "the engine's panic location must survive a panicking payload destructor"
        ),
        other => panic!("expected a contained Panic result, got {:?}", other),
    }
    assert_eq!(
        handle.in_flight(),
        0,
        "the ticket's cancel flag must be cleared; shutdown drains on in_flight"
    );
    assert!(
        handle.is_poisoned(),
        "a caught panic poisons the handle (I2)"
    );

    handle.close();
    unsafe {
        libc::close(r);
    }
}

#[test]
fn panicking_payload_destructor_does_not_kill_the_worker() {
    set_engine_handler(engine);
    assert_bomb_completes(1);
    assert_bomb_completes(2);

    // A healthy handle on the same process still round-trips: containment must not
    // have wedged any process-global state (hook, location list, log ring).
    let (r, w) = make_pipe();
    let handle = match Handle::open(2, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };
    let ticket = match handle.submit(Default::default(), &[7, 7], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    assert_eq!(read_ticket(r, Duration::from_secs(5)), Some(ticket));
    match handle.take(ticket) {
        Ok(JobResult::Ok(out)) => assert_eq!(out, vec![7, 7]),
        other => panic!("expected echo, got {:?}", other),
    }
    handle.close();
    unsafe {
        libc::close(r);
    }
}
