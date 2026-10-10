//! Two threads logging onto a full ring must not lose lines.
//!
//! `log_event` used to try the ring lock 16 times, a `spin_loop` hint apart,
//! and then drop the line. Any eviction from a full ring (one memmove of up
//! to 64 KiB) outlasts nanoseconds of spinning, and the ring stays full in
//! normal use because Go drains it only on `DrainLogs`. With one thread
//! writing short lines and another writing the lines that matter most
//! ("completion write failed", "drain budget expired", "WITHOUT
//! sigaltstack"), thousands of lines were dropped per run, the critical ones
//! among them. Only a `log_event` re-entered on the thread already holding
//! the ring may drop its line now.
//!
//! Separate binary: the log ring is process-global.

#![allow(unsafe_code)]

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Barrier};
use std::thread;

use gusset::ffi::{log_dropped_total, log_event};

mod props;

use props::{drain_everything, drain_real};

/// Each the production wording, numbered so the last one is recognisable.
fn critical_lines(i: usize) -> [String; 3] {
    [
        format!("gusset: completion write failed for ticket {i}: Broken pipe (os error 32)"),
        format!(
            "gusset: shutdown drain budget of {i} ms expired with 1 work unit(s) still in flight"
        ),
        format!(
            "gusset: worker gusset-w{i} started WITHOUT sigaltstack; a Rust stack overflow on \
             this thread will abort the process (I5/R8)"
        ),
    ]
}

#[test]
fn critical_lines_survive_two_thread_contention_on_a_full_ring() {
    const ROUNDS: usize = 5_000;

    drain_everything();
    // Full: 8000 eight-byte lines is 64000 bytes of the 65536-byte budget,
    // so every later line evicts.
    for _ in 0..8000 {
        log_event("filler7");
    }
    let dropped_before = log_dropped_total();

    let start = Arc::new(Barrier::new(2));
    let done = Arc::new(AtomicBool::new(false));
    let spammer = {
        let (start, done) = (Arc::clone(&start), Arc::clone(&done));
        thread::spawn(move || {
            start.wait();
            // Bounded as well: the ring lock is not fair, and on a starved
            // runner this loop could keep winning it until the test hangs.
            let mut n = 0u64;
            while n < 200_000 && !done.load(Ordering::Acquire) {
                log_event("short7");
                n += 1;
            }
            n
        })
    };
    start.wait();
    for i in 0..ROUNDS {
        for line in critical_lines(i) {
            log_event(&line);
        }
    }
    done.store(true, Ordering::Release);
    let spam = spammer
        .join()
        .unwrap_or_else(|_| panic!("spammer panicked"));

    let dropped = log_dropped_total() - dropped_before;
    assert_eq!(
        dropped,
        0,
        "{dropped} of {} lines dropped under contention ({spam} short lines alongside)",
        3 * ROUNDS as u64 + spam
    );

    // Nothing counted as dropped is not the same as the lines landing: the
    // last critical line must be in the ring (the spammer stops within a few
    // short lines of it, far less than the 64 KiB needed to evict it).
    let mut rest = Vec::new();
    loop {
        let chunk = drain_real(1 << 17);
        if chunk.is_empty() {
            break;
        }
        rest.extend(chunk);
    }
    assert!(rest.len() <= 65536, "ring grew to {} bytes", rest.len());
    let text = String::from_utf8(rest).unwrap_or_else(|e| panic!("ring not UTF-8: {e}"));
    let [_, _, last] = critical_lines(ROUNDS - 1);
    assert!(
        text.lines().any(|l| l == last),
        "the last critical line is missing from the ring"
    );
    assert!(
        !text.contains("dropped while the ring was busy"),
        "the ring reports dropped lines"
    );
}
