//! `gusset_drain_logs` must hand back whole lines, never half a character.
//!
//! `append_line` truncates on a char boundary "so the ring never hands Go a
//! partial UTF-8 sequence", but the drain copied `min(len, buffered)` bytes
//! and cut wherever that landed: mid-line whenever the caller's buffer was
//! smaller than the backlog, and mid-character whenever the cut fell inside a
//! multi-byte sequence. A host that decodes each drained chunk as text (the
//! natural reading of `DrainLogs(buf) int`) logged U+FFFD garbage and split
//! diagnostics across two records.
//!
//! Separate binary: the log ring is process-global.

#![allow(unsafe_code)]

use gusset::ffi::{gusset_drain_logs, log_event};

fn drain(cap: usize) -> Vec<u8> {
    let mut buf = vec![0u8; cap];
    let mut written = usize::MAX;
    unsafe { gusset_drain_logs(buf.as_mut_ptr(), buf.len(), &mut written) };
    assert!(
        written <= cap,
        "drain reported {written} bytes into a {cap}-byte buffer"
    );
    buf.truncate(written);
    buf
}

fn drain_all() {
    while !drain(1 << 16).is_empty() {}
}

#[test]
fn drain_returns_whole_lines_and_whole_characters() {
    drain_all();

    // 3-byte characters: a cut at an arbitrary byte count lands mid-character
    // two times out of three.
    let first = "€".repeat(10); // 30 bytes + '\n'
    let second = "€".repeat(10);
    log_event(&first);
    log_event(&second);

    // Room for the first line and part of the second, ending mid-character.
    let chunk = drain(31 + 5);
    assert!(
        std::str::from_utf8(&chunk).is_ok(),
        "a drained chunk split a UTF-8 sequence: {chunk:?}"
    );
    assert_eq!(
        chunk,
        format!("{first}\n").into_bytes(),
        "a drain with room for one whole line must return exactly that line"
    );
    let rest = drain(1 << 16);
    assert_eq!(
        rest,
        format!("{second}\n").into_bytes(),
        "nothing may be lost"
    );

    // A line longer than the caller's buffer still makes progress, and each
    // piece is valid UTF-8 on its own.
    let long = "€".repeat(100);
    log_event(&long);
    let mut got = Vec::new();
    for _ in 0..1000 {
        let piece = drain(32);
        if piece.is_empty() {
            break;
        }
        assert!(
            std::str::from_utf8(&piece).is_ok(),
            "a partial-line drain split a UTF-8 sequence: {piece:?}"
        );
        got.extend_from_slice(&piece);
    }
    assert_eq!(got, format!("{long}\n").into_bytes());

    // A buffer too small for even one character must not wedge the drain:
    // returning 0 forever would read as "empty" and strand the ring.
    log_event("€");
    let mut got = Vec::new();
    for _ in 0..16 {
        let piece = drain(1);
        if piece.is_empty() {
            break;
        }
        got.extend_from_slice(&piece);
    }
    assert_eq!(
        got,
        "€\n".as_bytes(),
        "a 1-byte buffer must still drain the ring"
    );
}
