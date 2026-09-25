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

use std::fs;
use std::path::PathBuf;

fn root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..")
}

fn read(rel: &str) -> String {
    match fs::read_to_string(root().join(rel)) {
        Ok(s) => s,
        Err(e) => panic!("read {rel}: {e}"),
    }
}

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
        INLINE_RECORD_FLAG, INLINE_RECORD_MAX, INLINE_RESULT_MAX, MAX_BUFFER_BYTES,
        MAX_INLINE_INPUT, MAX_POOL_SIZE, TAKE_OWNED_FLAG,
    };

    let h = read("internal/ffi/gusset.h");
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
    ];
    for &(name, want) in rust_c {
        assert_eq!(
            c_define(&h, name),
            want,
            "gusset.h {name} disagrees with Rust"
        );
    }

    let handle_go = read("handle.go");
    let buffer_go = read("buffer.go");
    assert_eq!(
        go_const(&handle_go, "MaxPoolSize"),
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
