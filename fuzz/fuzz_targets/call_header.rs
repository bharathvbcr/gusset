//! Any 40 header bytes: JobContext is sound (I3: a non-zero timeout is never
//! "no deadline"), and a real Handle::submit either refuses unknown flags or
//! completes exactly once with the exact record inline_record builds, through
//! the pipe or the completion ring.
#![no_main]
#![allow(unsafe_code)]

#[path = "../../crates/gusset/tests/props/mod.rs"]
mod props;

libfuzzer_sys::fuzz_target!(|data: &[u8]| {
    if data.len() < 42 {
        return;
    }
    props::check_job_context(props::header_from(&data[..40]));
    let with_ring = data[41] & 1 == 1;
    props::check_submit(&data[..41], &data[42..], with_ring);
});
