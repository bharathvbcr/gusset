//! pool::ring::Ring publish/consume sequences against a FIFO model, reading
//! slots through raw pointers exactly as the Go reader does (gusset.h).
#![no_main]
#![allow(unsafe_code)]

#[path = "../../crates/gusset/tests/props/mod.rs"]
mod props;

libfuzzer_sys::fuzz_target!(|data: &[u8]| {
    if let Some((&slots, ops)) = data.split_first() {
        props::check_ring(slots, ops);
    }
});
