//! I6: every field of the four crossing structs has the same C type in
//! `gusset.h` as in Rust.
//!
//! Go's `init()` compares Rust's size, alignment and named-field offsets with
//! what cgo compiled from the header. `header_match` compares function
//! signatures. The source scan in `ffi/fields.rs` compares field *names*.
//! Nothing compared field *types*, so any change that keeps widths passes all
//! of them: `uint32_t line` declared `int32_t`, `size_t live_bytes` declared
//! `int64_t`, `int32_t code` declared `uint32_t`, or `const uint8_t* file`
//! losing its `const`. cgo then gives Go the header's type, and Go converts
//! with the header's signedness — a line number or byte count past 2^31 turns
//! negative on the Go side, a `code` compared against a negative sentinel
//! never matches.
//!
//! Deliberately textual, like `header_match`: it understands exactly the field
//! shapes this ABI uses and fails on anything else rather than skipping it.

use std::collections::BTreeMap;

#[path = "../../../tests/common/mod.rs"]
mod common;
use common::read_repo_file;

/// Rust field type, in C spelling. Arrays become `base[len]`.
fn rust_to_c(t: &str) -> String {
    let t = t.trim();
    if let Some(inner) = t.strip_prefix('[').and_then(|s| s.strip_suffix(']')) {
        let (elem, len) = match inner.split_once(';') {
            Some(p) => p,
            None => panic!("array type without a length: {t}"),
        };
        let len = match len.trim() {
            "ABI_TYPE_COUNT" => gusset::ffi::ABI_TYPE_COUNT.to_string(),
            n => match n.parse::<usize>() {
                Ok(v) => v.to_string(),
                Err(_) => panic!("unknown array length `{n}` in {t}"),
            },
        };
        return format!("{}[{}]", rust_to_c(elem), len);
    }
    let (is_const, stars, base) = if let Some(b) = t.strip_prefix("*const ") {
        (true, 1, b.trim())
    } else if let Some(b) = t.strip_prefix("*mut ") {
        (false, 1, b.trim())
    } else {
        (false, 0, t)
    };
    let c = match base {
        "u8" => "uint8_t",
        "i32" => "int32_t",
        "u32" => "uint32_t",
        "u64" => "uint64_t",
        "usize" => "size_t",
        other => panic!("field type `{other}` has no C mapping here; extend the table"),
    };
    let mut out = String::new();
    if is_const {
        out.push_str("const ");
    }
    out.push_str(c);
    out.push_str(&"*".repeat(stars));
    out
}

fn rust_struct(src: &str, name: &str) -> Vec<(String, String)> {
    let marker = format!("pub struct {name} {{");
    let start = match src.find(&marker) {
        Some(i) => i + marker.len(),
        None => panic!("`{marker}` not found"),
    };
    let end = match src[start..].find("\n}") {
        Some(i) => start + i,
        None => panic!("{name} body is unterminated"),
    };
    let mut fields = Vec::new();
    for line in src[start..end].lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with("//") || line.starts_with('#') {
            continue;
        }
        let rest = match line.strip_prefix("pub ") {
            Some(r) => r,
            None => panic!("{name}: non-public field line `{line}`"),
        };
        let (f, ty) = match rest.split_once(':') {
            Some(p) => p,
            None => panic!("{name}: cannot parse field `{line}`"),
        };
        fields.push((f.trim().to_string(), rust_to_c(ty.trim_end_matches(','))));
    }
    if fields.is_empty() {
        panic!("{name} parsed with no fields");
    }
    fields
}

/// `typedef struct { ... } Name;` bodies as (field, normalised C type).
fn c_structs(src: &str) -> BTreeMap<String, Vec<(String, String)>> {
    let mut out = BTreeMap::new();
    let mut rest = src;
    while let Some(i) = rest.find("typedef struct {") {
        let after = &rest[i + "typedef struct {".len()..];
        let close = match after.find('}') {
            Some(c) => c,
            None => panic!("unterminated typedef struct"),
        };
        let name: String = after[close + 1..]
            .trim_start()
            .chars()
            .take_while(|c| c.is_ascii_alphanumeric() || *c == '_')
            .collect();
        let mut fields = Vec::new();
        for decl in after[..close].split(';') {
            let decl = decl.trim();
            if decl.is_empty() {
                continue;
            }
            if decl.starts_with("/*") || decl.starts_with("//") {
                panic!("{name}: comment inside a struct body is not understood: {decl}");
            }
            let (decl, array) = match decl.split_once('[') {
                Some((d, a)) => (d.trim(), format!("[{}", a.trim())),
                None => (decl, String::new()),
            };
            let split = match decl.rfind(|c: char| !(c.is_ascii_alphanumeric() || c == '_')) {
                Some(s) => s + 1,
                None => panic!("{name}: field without a type: {decl}"),
            };
            let (ty, field) = decl.split_at(split);
            // `uint8_t* msg` and `uint8_t *msg` are the same type.
            let ty: String = ty.split_whitespace().collect::<Vec<_>>().join(" ");
            let ty = ty.replace(" *", "*");
            fields.push((field.to_string(), format!("{ty}{array}")));
        }
        out.insert(name, fields);
        rest = &after[close..];
    }
    out
}

#[test]
fn header_struct_field_types_match_rust() {
    let header = c_structs(&read_repo_file("internal/ffi/gusset.h"));
    let cases = [
        ("crates/gusset/src/header.rs", "CallHeader"),
        ("crates/gusset/src/ffi/status.rs", "FfiStatus"),
        ("crates/gusset/src/ffi/mod.rs", "AbiLayout"),
        ("crates/gusset/src/ffi/alloc.rs", "AllocStats"),
    ];
    for (file, name) in cases {
        let rust = rust_struct(&read_repo_file(file), name);
        let c = match header.get(name) {
            Some(f) => f,
            None => panic!("gusset.h has no typedef struct {name}"),
        };
        assert_eq!(
            &rust, c,
            "{name}: Rust fields (in C spelling) differ from gusset.h; Go reads the header's types"
        );
    }
}

#[test]
fn the_type_scan_sees_a_signedness_flip() {
    // The scan must be able to fail: a header differing only in the sign of
    // one field is rejected.
    let flipped =
        read_repo_file("internal/ffi/gusset.h").replace("    uint32_t line;", "    int32_t line;");
    let header = c_structs(&flipped);
    let rust = rust_struct(
        &read_repo_file("crates/gusset/src/ffi/status.rs"),
        "FfiStatus",
    );
    let c = match header.get("FfiStatus") {
        Some(f) => f,
        None => panic!("FfiStatus missing"),
    };
    assert_ne!(&rust, c, "a signedness flip went unnoticed");
}
