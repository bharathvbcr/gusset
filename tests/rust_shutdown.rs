//! `gusset_shutdown` drain semantics.
//!
//! Its own test binary: shutdown sets a process-global flag that refuses every
//! submission, so a parallel test in the same binary would see its work rejected.
//!
//! Before this change `gusset_shutdown` was `{ FFI_OK }` — it returned success
//! without draining anything, cancelling anything, or refusing anything, while the
//! README advertised "graceful shutdown and worker drain".

use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::{self, Handle};

fn make_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
    assert_eq!(rc, 0, "pipe() failed");
    (fds[0], fds[1])
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

    handle.close();
    let _ = drainer.join();
    unsafe {
        libc::close(r);
    }
}
