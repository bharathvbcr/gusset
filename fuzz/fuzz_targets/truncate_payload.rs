//! truncate_payload through extract_panic_payload: valid UTF-8, the longest
//! char-boundary prefix within the cap plus the marker, a prefix of the input.
#![no_main]
#![allow(unsafe_code)]

#[path = "../../crates/gusset/tests/props/mod.rs"]
mod props;

libfuzzer_sys::fuzz_target!(|data: &[u8]| {
    if data.len() < 4 {
        return;
    }
    let hint = u32::from_le_bytes([data[0], data[1], data[2], data[3]]);
    props::check_truncate(&data[4..], hint);
    // Also arbitrary (lossily decoded) text straight in, at its own length.
    props::check_truncate_str(String::from_utf8_lossy(&data[4..]).into_owned());
});
