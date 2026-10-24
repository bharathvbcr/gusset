//! The memory bound of the `ffi_exports` fuzz target, and the tests that pin
//! it. `fuzz/fuzz_targets/ffi_exports.rs` includes this file by path; Cargo
//! also builds it as an integration test of this crate, so the bound is
//! checked by `cargo test --workspace` on every push. The fuzz crate is its
//! own workspace and has no test step.
//!
//! `gusset_buf_alloc` commits every byte it accepts. `BUFFER_ALIGN` (64) is
//! above the system allocator's minimum alignment, so a zeroed allocation is
//! `posix_memalign` followed by a write over the whole length, up to
//! `MAX_BUFFER_BYTES` (1 GiB) per call. The nightly cargo-fuzz job runs with
//! `-rss_limit_mb=2048`. While op 2 drew its length from the whole `u64`
//! range, valid lengths near 1 GiB ran that job out of memory on four
//! nightlies in a row.
//!
//! So one input never commits more than [`MAX_REAL_BUFFER`] per allocation,
//! nor more than [`MAX_LIVE_BYTES`] live in total. A hostile length is drawn
//! only from above `MAX_BUFFER_BYTES`, which the export refuses before the
//! allocator runs. Lengths between the two bounds are valid and would be
//! committed in full, so the target never picks them.

#![allow(dead_code)]

use gusset::pool::MAX_BUFFER_BYTES;

/// Calls per input. Bounds the work and keeps the completion pipe (at least
/// 64 KiB on every supported OS) from filling: each call completes at most one
/// record of 64 bytes.
pub const MAX_OPS: usize = 48;

/// Largest buffer one call really allocates.
pub const MAX_REAL_BUFFER: usize = 1 << 20;

/// The target keeps a diagnostic job's generated output (mode 16, length from
/// `input[1..5]`) below this many bytes.
pub const MAX_JOB_OUTPUT: u32 = 1 << 16;

/// Ceiling on the buffer bytes one input holds live at once.
pub const MAX_LIVE_BYTES: usize = 64 << 20;

// Each call allocates at most one buffer or runs at most one job, so this is
// the most one input can hold live. Raising MAX_OPS or MAX_REAL_BUFFER past it
// fails the build.
const _: () = assert!(MAX_OPS * (MAX_REAL_BUFFER + MAX_JOB_OUTPUT as usize) <= MAX_LIVE_BYTES);

/// The length op 2 passes to `gusset_buf_alloc`, chosen by `selector` from
/// zero, the extremes, a hostile length, or a small valid one taken from `raw`.
///
/// The hostile choice covers every length from `MAX_BUFFER_BYTES + 1` to
/// `usize::MAX`. The valid choice is at most [`MAX_REAL_BUFFER`].
pub fn buf_alloc_len(selector: u8, raw: u64) -> usize {
    match selector % 6 {
        0 => 0,
        1 => usize::MAX,
        2 => isize::MAX as usize,
        3 => MAX_BUFFER_BYTES + 1 + raw as usize % (usize::MAX - MAX_BUFFER_BYTES),
        _ => raw as usize % (MAX_REAL_BUFFER + 1),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gusset::pool::sys::check_buffer_len;

    /// Raw words at and around every bound the choice must respect.
    fn edge_raws() -> Vec<u64> {
        let mut raws = vec![0u64, 1, u64::MAX, u64::from(u32::MAX), 1 << 29];
        for b in [MAX_REAL_BUFFER as u64, MAX_BUFFER_BYTES as u64] {
            raws.extend([b - 1, b, b + 1]);
        }
        // Seeded xorshift64*, so a failure reproduces.
        let mut x = 0x9E37_79B9_7F4A_7C15u64;
        for _ in 0..4096 {
            x ^= x >> 12;
            x ^= x << 25;
            x ^= x >> 27;
            raws.push(x.wrapping_mul(0x2545_F491_4F6C_DD1D));
        }
        raws
    }

    /// Every length the export would accept is at most MAX_REAL_BUFFER. Checked
    /// against the library's own refusal, so no allocation happens here.
    #[test]
    fn no_accepted_length_exceeds_max_real_buffer() {
        for selector in 0..=u8::MAX {
            for raw in edge_raws() {
                let len = buf_alloc_len(selector, raw);
                if check_buffer_len(len).is_ok() {
                    assert!(
                        len <= MAX_REAL_BUFFER,
                        "selector {selector}, raw {raw:#x}: length {len} would be \
                         committed, above MAX_REAL_BUFFER ({MAX_REAL_BUFFER})"
                    );
                }
            }
        }
    }

    /// The hostile choices stay hostile: every one is above MAX_BUFFER_BYTES
    /// and refused, from exactly one past the ceiling up to usize::MAX.
    #[test]
    fn hostile_lengths_stay_above_the_ceiling_and_refused() {
        for raw in edge_raws() {
            for selector in [1u8, 2, 3] {
                let len = buf_alloc_len(selector, raw);
                assert!(
                    len > MAX_BUFFER_BYTES,
                    "selector {selector}, raw {raw:#x}: hostile length {len} is not above the ceiling"
                );
                assert!(check_buffer_len(len).is_err(), "length {len} was accepted");
            }
        }
        assert_eq!(buf_alloc_len(3, 0), MAX_BUFFER_BYTES + 1);
        let top = (usize::MAX - MAX_BUFFER_BYTES - 1) as u64;
        assert_eq!(buf_alloc_len(3, top), usize::MAX);
    }

    /// The valid choice reaches its bound and zero is still tried.
    #[test]
    fn valid_lengths_reach_max_real_buffer() {
        assert_eq!(buf_alloc_len(4, MAX_REAL_BUFFER as u64), MAX_REAL_BUFFER);
        assert_eq!(buf_alloc_len(5, 0), 0);
        assert_eq!(buf_alloc_len(0, u64::MAX), 0);
    }
}
