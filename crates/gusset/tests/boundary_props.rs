//! Seeded randomized property tests over the Rust side of the byte-level
//! boundary: panic payload truncation (I2's message path), the completion
//! ring, `CallHeader` decoding and the completion records `inline_record`
//! builds (I4: every completion reaches its reader exactly once).
//!
//! Fixed seeds, so a failure reproduces; `GUSSET_PROP_ITERS` raises the
//! iteration count for a longer soak. The same checkers back the libFuzzer
//! targets in `fuzz/` (see props/mod.rs).

#![allow(unsafe_code)]

mod props;

use props::{check_job_context, check_ring, check_submit, check_truncate, header_from, iters, Rng};

#[test]
fn truncate_payload_is_a_bounded_char_boundary_prefix() {
    let mut rng = Rng(0x9E37_79B9_7F4A_7C15);
    for _ in 0..iters(300) {
        let flen = 1 + (rng.next() % 16) as usize;
        let fill = rng.bytes(flen);
        // Half the lengths land within a few bytes of the 32 KiB cap.
        let hint = if rng.next() & 1 == 0 {
            (32 * 1024 - 8 + (rng.next() % 16)) as u32
        } else {
            rng.next() as u32
        };
        check_truncate(&fill, hint);
    }
}

#[test]
fn ring_round_trips_records_in_order_and_refuses_when_full() {
    let mut rng = Rng(0xD1B5_4A32_D192_ED03);
    for _ in 0..iters(2000) {
        let n = (rng.next() % 64) as usize;
        let ops = rng.bytes(n);
        check_ring(rng.next() as u8, &ops);
    }
}

#[test]
fn any_header_bytes_make_a_sound_job_context() {
    let mut rng = Rng(0xA076_1D64_78BD_642F);
    for _ in 0..iters(5000) {
        let mut b = rng.bytes(40);
        // Bias timeouts toward the edges: 0, tiny, huge, u64::MAX.
        match rng.next() % 4 {
            0 => b[24..32].copy_from_slice(&0u64.to_ne_bytes()),
            1 => b[24..32].copy_from_slice(&(rng.next() % 1000 + 1).to_ne_bytes()),
            2 => b[24..32].copy_from_slice(&u64::MAX.to_ne_bytes()),
            _ => {}
        }
        check_job_context(header_from(&b));
    }
}

#[test]
fn submitted_headers_complete_once_with_exact_records_pipe() {
    let mut rng = Rng(0xE703_7ED1_A0B4_28DB);
    for _ in 0..iters(400) {
        let hb = rng.bytes(41);
        let plen = (rng.next() % 80) as usize;
        let payload = rng.bytes(plen);
        check_submit(&hb, &payload, false);
    }
}

#[test]
fn submitted_headers_complete_once_with_exact_records_ring() {
    let mut rng = Rng(0x8EBC_6AF0_9C88_C6E3);
    for _ in 0..iters(400) {
        let hb = rng.bytes(41);
        let plen = (rng.next() % 80) as usize;
        let payload = rng.bytes(plen);
        check_submit(&hb, &payload, true);
    }
}
