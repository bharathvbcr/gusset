//! Constants duplicated across Rust, `gusset.h` and Go must agree.
//!
//! The ABI check in Go's `init()` covers struct layout and `header_match` covers
//! function signatures. Neither sees plain numbers: status codes, flag bits,
//! the pool and buffer ceilings, the inline-input limit and the take-owned bit
//! each live in two or three places. A drift compiles on every side and shows up
//! only in production — Rust lowering `MAX_POOL_SIZE` makes `Open(1024)` fail at
//! runtime; Rust raising the inline limit leaves Go refusing inputs Rust takes.
//!
//! Deliberately textual: it reads `#define NAME value` and `const Name = value`
//! lines and fails loudly on any name it cannot find.

mod common;
use common::read_repo_file;

/// Evaluates the literal forms used here: `4096u`, `1073741824ull`,
/// `(1ull << 63)`, `1 << 30`.
fn eval(expr: &str) -> u64 {
    let e = expr
        .trim()
        .trim_start_matches('(')
        .trim_end_matches(')')
        .trim();
    if let Some((l, r)) = e.split_once("<<") {
        return eval(l) << eval(r);
    }
    let digits = e.trim_end_matches(|c: char| c.is_ascii_alphabetic());
    match digits.parse::<u64>() {
        Ok(v) => v,
        Err(_) => panic!("cannot evaluate constant expression {expr:?}"),
    }
}

fn c_define(header: &str, name: &str) -> u64 {
    for line in header.lines() {
        let mut it = line.trim().splitn(3, char::is_whitespace);
        if it.next() == Some("#define") && it.next() == Some(name) {
            if let Some(v) = it.next() {
                return eval(v);
            }
        }
    }
    panic!("gusset.h has no #define {name}")
}

fn go_const(src: &str, name: &str) -> u64 {
    for line in src.lines() {
        let l = line.trim();
        let l = l.strip_prefix("const ").unwrap_or(l);
        if let Some(rest) = l.strip_prefix(name) {
            let rest = rest.trim_start();
            if let Some(v) = rest.strip_prefix('=') {
                return eval(v.split("//").next().unwrap_or(v));
            }
        }
    }
    panic!("no Go const {name}")
}

#[test]
fn constants_agree_across_rust_c_and_go() {
    use gusset::ffi::status::{FFI_BAD_ARG, FFI_ERR, FFI_OK, FFI_PANIC, FFI_POISONED};
    use gusset::header::{GUSSET_FLAG_DIAGNOSTIC_ENGINE, GUSSET_FLAG_INLINE_COMPLETION};
    use gusset::pool::{
        ring, INLINE_RECORD_FLAG, INLINE_RECORD_MAX, INLINE_RESULT_MAX, MAX_BUFFER_BYTES,
        MAX_INLINE_INPUT, MAX_POOL_SIZE, TAKE_OWNED_FLAG,
    };

    let h = read_repo_file("internal/ffi/gusset.h");
    let rust_c: &[(&str, u64)] = &[
        ("FFI_OK", FFI_OK as u64),
        ("FFI_ERR", FFI_ERR as u64),
        ("FFI_PANIC", FFI_PANIC as u64),
        ("FFI_POISONED", FFI_POISONED as u64),
        ("FFI_BAD_ARG", FFI_BAD_ARG as u64),
        (
            "GUSSET_FLAG_DIAGNOSTIC_ENGINE",
            GUSSET_FLAG_DIAGNOSTIC_ENGINE as u64,
        ),
        ("GUSSET_MAX_POOL_SIZE", MAX_POOL_SIZE as u64),
        ("GUSSET_MAX_INLINE_INPUT", MAX_INLINE_INPUT as u64),
        ("GUSSET_MAX_BUFFER_BYTES", MAX_BUFFER_BYTES as u64),
        ("GUSSET_TAKE_OWNED_FLAG", TAKE_OWNED_FLAG),
        (
            "GUSSET_FLAG_INLINE_COMPLETION",
            GUSSET_FLAG_INLINE_COMPLETION as u64,
        ),
        ("GUSSET_INLINE_RECORD_FLAG", INLINE_RECORD_FLAG),
        ("GUSSET_INLINE_RESULT_MAX", INLINE_RESULT_MAX as u64),
        ("GUSSET_INLINE_RECORD_MAX", INLINE_RECORD_MAX as u64),
        ("GUSSET_RING_OFF_CAPACITY", ring::RING_OFF_CAPACITY as u64),
        (
            "GUSSET_RING_OFF_SLOT_BYTES",
            ring::RING_OFF_SLOT_BYTES as u64,
        ),
        ("GUSSET_RING_OFF_WAITING", ring::RING_OFF_WAITING as u64),
        ("GUSSET_RING_OFF_OVERFLOW", ring::RING_OFF_OVERFLOW as u64),
        ("GUSSET_RING_SLOT_BYTES", ring::RING_SLOT_BYTES as u64),
        (
            "GUSSET_RING_SLOT_OFF_RECORD",
            ring::RING_SLOT_OFF_RECORD as u64,
        ),
    ];
    for &(name, want) in rust_c {
        assert_eq!(
            c_define(&h, name),
            want,
            "gusset.h {name} disagrees with Rust"
        );
    }

    let handle_go = read_repo_file("handle.go");
    let options_go = read_repo_file("options.go");
    let buffer_go = read_repo_file("buffer.go");
    assert_eq!(
        go_const(&options_go, "MaxPoolSize"),
        MAX_POOL_SIZE as u64,
        "Go MaxPoolSize"
    );
    assert_eq!(
        go_const(&handle_go, "inlineResultBytes"),
        MAX_INLINE_INPUT as u64,
        "Go inlineResultBytes (the submit limit and the egress threshold)"
    );
    assert_eq!(
        go_const(&buffer_go, "MaxBufferBytes"),
        MAX_BUFFER_BYTES as u64,
        "Go MaxBufferBytes"
    );
    assert_eq!(
        TAKE_OWNED_FLAG,
        1 << 63,
        "ids are capped below the take-owned bit"
    );
}

/// A completion record is a ticket word, a length word, then the result bytes.
/// `INLINE_RECORD_MAX - INLINE_RESULT_MAX` is therefore where an inline payload
/// starts, and the Go reader (`ticket_reader.go`) derives its parse offsets from
/// that difference rather than from a literal.
///
/// `constants_agree_across_rust_c_and_go` checks each maximum on its own, which
/// cannot see a header that grew in Rust and `gusset.h` together: both agree,
/// and a reader that kept a typed 16 would slice every payload from the wrong
/// byte. This pins the difference to the two documented words, and the Go
/// reader to the expression.
#[test]
fn inline_record_header_is_the_two_words_the_go_reader_derives() {
    use gusset::pool::{INLINE_RECORD_MAX, INLINE_RESULT_MAX};

    let header = 2 * std::mem::size_of::<u64>();
    assert_eq!(
        INLINE_RECORD_MAX - INLINE_RESULT_MAX,
        header,
        "Rust record header is no longer the ticket word plus the length word"
    );
    let h = read_repo_file("internal/ffi/gusset.h");
    assert_eq!(
        c_define(&h, "GUSSET_INLINE_RECORD_MAX") - c_define(&h, "GUSSET_INLINE_RESULT_MAX"),
        header as u64,
        "gusset.h record header is no longer the ticket word plus the length word"
    );
    assert_eq!(
        INLINE_RESULT_MAX % 8,
        0,
        "payloads are padded to whole words, so the maximum must be one"
    );

    let reader = read_repo_file("ticket_reader.go");
    let decl = reader
        .lines()
        .find_map(|l| l.trim().strip_prefix("const inlineHeaderBytes ="))
        .expect("ticket_reader.go no longer declares `const inlineHeaderBytes`");
    assert_eq!(
        decl.split("//").next().unwrap_or(decl).trim(),
        "ffi.InlineRecordMax - ffi.InlineResultMax",
        "inlineHeaderBytes must be the record maximum less the result maximum"
    );
    let literal = header.to_string();
    let typed: Vec<String> = reader
        .lines()
        .enumerate()
        .filter(|(_, l)| {
            l.split("//")
                .next()
                .unwrap_or(l)
                .split(|c: char| !c.is_ascii_alphanumeric() && c != '_')
                .any(|t| t == literal)
        })
        .map(|(i, l)| format!("ticket_reader.go:{}: {}", i + 1, l.trim()))
        .collect();
    assert!(
        typed.is_empty(),
        "the record header size is typed as a literal again; use inlineHeaderBytes:\n{}",
        typed.join("\n")
    );
}
