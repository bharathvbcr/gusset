//! log_event / gusset_drain_logs against the documented model (append_line,
//! drain_cut): exact bytes, bounded, progress, no split characters.
#![no_main]
#![allow(unsafe_code)]

#[path = "../../crates/gusset/tests/props/mod.rs"]
mod props;

libfuzzer_sys::fuzz_target!(|ops: &[u8]| {
    props::check_log_ring(ops);
});
