//! Seeded randomized property test of the log ring against a model built from
//! its documentation: interleaved `log_event` and `gusset_drain_logs` with line
//! lengths up to and past the 64 KiB budget and drain buffers from one byte
//! (smaller than a character) upward. Bytes out equal the model's bytes out,
//! the ring stays bounded, a drain always makes progress, and a drain into a
//! buffer that can hold a character never splits one.
//!
//! Separate binary: the log ring is process-global.

#![allow(unsafe_code)]

mod props;

use props::{check_log_ring, iters, Rng};

#[test]
fn log_ring_matches_its_model() {
    let mut rng = Rng(0x2545_F491_4F6C_DD1D);
    for _ in 0..iters(300) {
        let n = 3 * (rng.next() % 40) as usize;
        let ops = rng.bytes(n);
        check_log_ring(&ops);
    }
}
