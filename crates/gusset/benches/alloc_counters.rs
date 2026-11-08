//! Parallel cost of the allocator accounting counters.
//!
//! Every Gusset buffer is counted by [`count_buffer_alloc`] and uncounted by
//! [`count_buffer_dealloc`], and so is every byte a `Counting` allocator sees. All of
//! that lands on three process-wide atomics (live bytes, peak bytes, allocation count in
//! `src/ffi/alloc.rs`), so threads that share nothing else still contend on them. This
//! bench drives exactly those two functions, with no allocator behind them, from 1, 2, 4
//! and 8 threads, so what it times is the counters and nothing else: no malloc, no cgo,
//! no Go.
//!
//! That isolation is also its limit. The counter update is a small part of a real
//! buffer allocation, so a difference here is an upper bound on what an adopter sees.
//! `BenchmarkBufferAllocParallel` (`bench/alloc_counters_test.go`) measures the same
//! counters behind a cgo call and a 64-byte aligned malloc, and it can be recorded as
//! `RECORD_VARIANTS`, which a cargo arm cannot (`bench/record.sh` refuses the
//! combination: variant mode runs one round per arm, below this bench's floor). To
//! compare two builds of this bench, record each in turn with `RECORD_DIR` set to its
//! checkout and compare the files with `benchstat`; that is two runs, not interleaved.
//!
//! # Interleaving
//!
//! Same scheme as `dispatch.rs`: the arms run in rounds, every round runs each arm once,
//! odd rounds in the listed order and even rounds reversed (ABBA), and a discarded
//! round 0 comes first for the frequency ramp. `common::MIN_ROUNDS` is a floor, not a default:
//! benchstat will not quote a confidence interval below 6 samples.
//!
//! # Output
//!
//! One `BenchmarkAllocCounters/<arm>  <iterations>  <value> ns/op` line per round per
//! arm, grouped by arm (benchfile rejects an arm that resumes after another), with the
//! execution order in `# gusset-bench-rust-round-N:` comment lines. One op is one
//! alloc-and-free pair on one thread; the wall time of a sample is divided by the pairs
//! each thread ran, so a figure that grows with the thread count is contention.
//!
//! ```text
//! cargo bench -p gusset --bench alloc_counters                  # 10 rounds
//! cargo bench -p gusset --bench alloc_counters -- --rounds 20
//! ```
//!
//! Record it through `bench/record.sh` (`make bench-alloc-counters`), not by hand: the
//! recorder refuses a loaded or shared machine and stamps the file.

mod common;

use common::parse_args;
use gusset::alloc::{count_buffer_alloc, count_buffer_dealloc};
use std::hint::black_box;
use std::sync::Barrier;
use std::thread;
use std::time::{Duration, Instant};

/// Alloc-and-free pairs each thread runs per sample, unless `--iters` overrides it.
const DEFAULT_ITERS: u32 = 200_000;

/// Bytes each pair counts: one cache line, the size of a small Gusset buffer.
const BYTES: usize = 64;

/// Thread counts, one arm each.
const ARMS: [usize; 4] = [1, 2, 4, 8];

fn fail(msg: &str) -> ! {
    common::fail("alloc_counters", msg)
}

/// Wall time for `threads` threads to each run `iters` alloc-and-free pairs, from the
/// moment every one of them is ready until the last one finishes.
fn time_pairs(threads: usize, iters: u32) -> Duration {
    // The workers and this thread: nobody starts counting until everyone exists, so
    // thread creation is not part of the sample.
    let start = Barrier::new(threads + 1);
    thread::scope(|scope| {
        let workers: Vec<_> = (0..threads)
            .map(|_| {
                scope.spawn(|| {
                    start.wait();
                    for _ in 0..iters {
                        count_buffer_alloc(black_box(BYTES));
                        count_buffer_dealloc(black_box(BYTES));
                    }
                })
            })
            .collect();
        start.wait();
        let started = Instant::now();
        for worker in workers {
            if worker.join().is_err() {
                fail("a counting thread panicked");
            }
        }
        started.elapsed()
    })
}

fn arm_name(threads: usize) -> String {
    format!("threads_{threads}")
}

/// The arms in the order round `round` (1-based) runs them: as listed on odd rounds,
/// reversed on even ones.
fn round_order(round: u32) -> Vec<usize> {
    let mut order: Vec<usize> = (0..ARMS.len()).collect();
    if round.is_multiple_of(2) {
        order.reverse();
    }
    order
}

fn main() {
    let cfg = parse_args("alloc_counters", DEFAULT_ITERS);

    // Round 0: every arm once, discarded. An idle machine is not yet at a steady clock,
    // and the first arm to run would otherwise pay for the ramp.
    for threads in ARMS {
        let _ = time_pairs(threads, cfg.iters);
    }

    let mut samples: Vec<Vec<Duration>> = vec![Vec::new(); ARMS.len()];
    let mut orders: Vec<String> = Vec::new();
    for round in 1..=cfg.rounds {
        let order = round_order(round);
        let names: Vec<String> = order.iter().map(|&arm| arm_name(ARMS[arm])).collect();
        orders.push(format!(
            "# gusset-bench-rust-round-{round}: {}",
            names.join(" ")
        ));
        for arm in order {
            samples[arm].push(time_pairs(ARMS[arm], cfg.iters));
        }
    }

    println!(
        "# gusset-bench-rust-order: {} rounds after a discarded round 0; each round ran every \
         arm once; order alternates (odd rounds as listed, even rounds reversed)",
        cfg.rounds
    );
    for line in &orders {
        println!("{line}");
    }
    for (threads, runs) in ARMS.iter().zip(&samples) {
        for elapsed in runs {
            let ns = elapsed.as_secs_f64() * 1e9 / f64::from(cfg.iters);
            println!(
                "BenchmarkAllocCounters/{}\t{}\t{ns:.1} ns/op",
                arm_name(*threads),
                cfg.iters
            );
        }
    }
}
