//! Rust-only round-trip latency, for profiling the pool without Go in the way.
//!
//!     cargo run --release --example rt_latency
//!     N=20000 POOL=4 SPIN=1 cargo run --release --example rt_latency
//!
//! `SPIN=1` (the default) polls a non-blocking pipe the way Go's drain reader
//! does, so the numbers isolate the Rust side: submit, queue handoff, worker,
//! completion write. `SPIN=0` blocks in `read`, which adds a thread wake-up to
//! every call. Results travel as inline completion records.
use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE, GUSSET_FLAG_INLINE_COMPLETION};
use gusset::pool::{Handle, INLINE_RECORD_FLAG, INLINE_RECORD_MAX};
use std::time::{Duration, Instant};

fn env<T: std::str::FromStr>(name: &str, default: T) -> T {
    std::env::var(name)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default)
}

fn pct(v: &mut [Duration], p: usize) -> Duration {
    v.sort_unstable();
    v[(v.len() * p / 100).min(v.len() - 1)]
}

fn main() {
    let n: usize = env("N", 20000);
    let pool: u32 = env("POOL", 4);
    let spin = env("SPIN", 1u8) != 0;

    let mut fds = [0i32; 2];
    // SAFETY: pipe(2) fills the two-element array.
    if unsafe { libc::pipe(fds.as_mut_ptr()) } != 0 {
        panic!("pipe failed");
    }
    // SAFETY: fcntl on descriptors this process just created.
    unsafe {
        libc::fcntl(fds[1], libc::F_SETFL, libc::O_NONBLOCK);
        if spin {
            libc::fcntl(fds[0], libc::F_SETFL, libc::O_NONBLOCK);
        }
    }
    let h = Handle::open(pool, fds[1]).unwrap_or_else(|e| panic!("{e}"));
    let hdr = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE | GUSSET_FLAG_INLINE_COMPLETION,
        ..Default::default()
    };

    let mut submit = Vec::with_capacity(n);
    let mut total = Vec::with_capacity(n);
    let mut rec = [0u8; INLINE_RECORD_MAX];
    let t0 = Instant::now();
    for _ in 0..n {
        let s = Instant::now();
        let t = h.submit(hdr, &[0], 0).unwrap_or_else(|e| panic!("{e}"));
        submit.push(s.elapsed());
        // Mode 0 echoes one byte: a 24-byte inline record, one atomic write.
        loop {
            // SAFETY: reading into a valid stack buffer.
            let m = unsafe { libc::read(fds[0], rec.as_mut_ptr() as *mut _, 24) };
            if m == 24 {
                break;
            }
            assert!(m < 0, "short record read");
            std::hint::spin_loop();
        }
        let mut w = [0u8; 8];
        w.copy_from_slice(&rec[..8]);
        assert_eq!(u64::from_ne_bytes(w), t | INLINE_RECORD_FLAG);
        total.push(s.elapsed());
    }
    let mean = t0.elapsed() / n as u32;
    println!(
        "rust-only round trip (pool {pool}, {}): mean {mean:?}  p50 {:?}  p90 {:?}  p99 {:?}",
        if spin {
            "spinning reader"
        } else {
            "blocking reader"
        },
        pct(&mut total, 50),
        pct(&mut total, 90),
        pct(&mut total, 99)
    );
    println!(
        "  submit: p50 {:?}  p90 {:?}  p99 {:?}",
        pct(&mut submit, 50),
        pct(&mut submit, 90),
        pct(&mut submit, 99)
    );
}
