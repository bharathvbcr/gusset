//! The resource-hygiene baseline waits for the thread count recorded before
//! any handle existed, not for its own first reading to repeat.
//!
//! `/proc/self/task` keeps listing a thread for a moment after `join` returns.
//! The hygiene tests used to take the reading after their warm-up as the
//! baseline, so a worker or `gusset-close` joiner still on its way out was
//! counted in it, and the later reading, one thread lower, failed as a leak
//! (CI run 37944107330: threads 3 then 2, descriptors and mappings exact).
//!
//! The canary here is such a thread, slowed down: detached, it is still
//! running when the baseline is taken and exits 1.5 s later. A baseline that
//! counts it reports a leak once it is gone; `procfs::baseline` must wait it
//! out. A thread that never leaves must stop the baseline loudly instead.
//!
//! Linux only (it reads `/proc/self`). One test in its own binary, so no
//! other test's threads move the count while it measures.

#![cfg(target_os = "linux")]

#[path = "../../../tests/common/mod.rs"]
mod common;
use common::procfs::{baseline, settled, usage};
use std::time::Duration;

#[test]
fn baseline_waits_out_an_exiting_thread_and_refuses_a_lingering_one() {
    let idle_threads = usage().threads;

    std::thread::spawn(|| std::thread::sleep(Duration::from_millis(1500)));
    assert_eq!(
        usage().threads,
        idle_threads + 1,
        "the canary is not listed, so this would not test the wait"
    );
    let base = baseline(idle_threads);
    let after = settled(base);
    assert_eq!(
        base.threads, idle_threads,
        "the baseline counted a thread that was on its way out"
    );
    assert!(
        !after.leaked_since(base),
        "a thread that exited before the comparison read as a leak: baseline {base:?}, \
         after {after:?}"
    );

    // A thread that outlives the wait: the baseline panics rather than
    // measuring against a process that is not idle.
    let (release, held) = std::sync::mpsc::channel::<()>();
    let lingering = std::thread::spawn(move || {
        let _ = held.recv();
    });
    let refused = std::panic::catch_unwind(|| baseline(idle_threads));
    drop(release);
    if lingering.join().is_err() {
        panic!("the lingering thread panicked");
    }
    assert!(
        refused.is_err(),
        "a baseline was taken with a thread still running that was not there at idle"
    );
}
