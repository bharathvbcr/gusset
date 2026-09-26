//! R1/I6: `internal/ffi/gusset.h` must agree with the Rust exports, signature by
//! signature.
//!
//! `build.rs` does not run cbindgen, so the header is written by hand. Three things
//! already guard part of that surface, and none of them covers this:
//!
//! - `tests/exports_match.rs` compares `exports.txt` against the archive's symbol
//!   table, so it sees *names* but never types.
//! - The ABI check in Go's `init()` compares struct sizes and alignments, so it sees
//!   `#[repr(C)]` layout drift but not function signatures.
//! - cgo compiles the header, so a declaration that is syntactically wrong fails the
//!   build — but a declaration that is merely *untrue* compiles fine and miscalls
//!   Rust at runtime.
//!
//! The gap that leaves is the dangerous one: a parameter added to a Rust export and
//! not to the header, a `u32` that became a `u64`, or a `*const` that became a
//! `*mut`. Each compiles, links, and then reads arguments off the wrong stack slots.
//!
//! This test is deliberately not a general C parser. It understands exactly the
//! shapes this ABI uses, and it fails loudly on anything it does not recognise
//! rather than skipping it — a declaration this test cannot read is a declaration it
//! must not silently approve.

use std::collections::BTreeMap;
use std::fs;
use std::path::PathBuf;

/// One `extern "C"` function, in a form both sides can be reduced to.
#[derive(Debug, PartialEq, Eq)]
struct Signature {
    ret: String,
    params: Vec<String>,
}

fn repo_root() -> PathBuf {
    // This file lives in the workspace-root `tests/` directory but is registered as
    // a `[[test]]` of `crates/gusset`, so CARGO_MANIFEST_DIR points at the crate.
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..")
}

fn read(path: &str) -> String {
    let full = repo_root().join(path);
    match fs::read_to_string(&full) {
        Ok(s) => s,
        Err(e) => panic!("cannot read {}: {}", full.display(), e),
    }
}

/// Maps a Rust FFI type onto the C spelling the header must use.
///
/// Unknown base types are returned unchanged, so a genuinely new type shows up as a
/// mismatch against the header rather than being quietly normalised into agreement.
fn rust_type_to_c(t: &str) -> String {
    let mut rest = t.trim();
    let mut stars = 0usize;
    let mut is_const = false;

    loop {
        if let Some(r) = rest.strip_prefix("*mut") {
            stars += 1;
            rest = r.trim_start();
        } else if let Some(r) = rest.strip_prefix("*const") {
            stars += 1;
            is_const = true;
            rest = r.trim_start();
        } else {
            break;
        }
    }

    let base = match rest {
        "i32" => "int32_t",
        "u32" => "uint32_t",
        "u64" => "uint64_t",
        "u8" => "uint8_t",
        "usize" => "size_t",
        "()" | "" => "void",
        // The handle is opaque across the boundary and is spelled differently on
        // each side; this correspondence is exactly the kind of hand-maintained
        // mapping that needs pinning down.
        "Handle" => "GussetHandle",
        // The completion ring is opaque the same way; C reads it through the
        // pointers and GUSSET_RING_* offsets, never through this type.
        "Ring" => "GussetRing",
        other => other,
    };

    let mut out = String::new();
    if is_const {
        out.push_str("const");
    }
    out.push_str(base);
    for _ in 0..stars {
        out.push('*');
    }
    out
}

/// Canonicalises a C type by removing whitespace, so `const CallHeader *` and
/// `const CallHeader*` compare equal.
fn normalise_c_type(t: &str) -> String {
    t.chars().filter(|c| !c.is_whitespace()).collect()
}

/// Splits a declarator such as `GussetHandle** out_handle` into its type.
///
/// The parameter *name* is discarded on purpose: a renamed parameter is not an ABI
/// change, and treating it as one would make this test fire on cosmetic edits and
/// train people to bypass it.
fn c_param_type(decl: &str) -> String {
    let decl = decl.trim();
    // The trailing identifier is the parameter name; everything before it is the
    // type. A declarator with no name (legal C) leaves the whole string as the type.
    match decl.rfind(|c: char| !(c.is_alphanumeric() || c == '_')) {
        Some(split) => {
            let (ty, name) = decl.split_at(split + 1);
            if name.is_empty() || ty.trim().is_empty() {
                normalise_c_type(decl)
            } else {
                normalise_c_type(ty)
            }
        }
        None => normalise_c_type(decl),
    }
}

/// Extracts every `gusset_*` function declaration from the C header.
fn parse_header(src: &str) -> BTreeMap<String, Signature> {
    let mut out = BTreeMap::new();

    for line in src.lines() {
        let line = line.trim();
        if !line.ends_with(");") || !line.contains("gusset_") || line.starts_with("//") {
            continue;
        }

        let open = match line.find('(') {
            Some(i) => i,
            None => continue,
        };
        let close = match line.rfind(')') {
            Some(i) => i,
            None => continue,
        };

        let head = &line[..open];
        let name_start = match head.rfind(|c: char| !(c.is_alphanumeric() || c == '_')) {
            Some(i) => i + 1,
            None => 0,
        };
        let name = head[name_start..].trim().to_string();
        let ret = normalise_c_type(&head[..name_start]);

        let inner = &line[open + 1..close];
        let params: Vec<String> = if inner.trim() == "void" || inner.trim().is_empty() {
            Vec::new()
        } else {
            inner.split(',').map(c_param_type).collect()
        };

        out.insert(name, Signature { ret, params });
    }

    out
}

/// Extracts every `pub [unsafe] extern "C" fn` from the Rust FFI module, mapped
/// into C spelling.
fn parse_rust(src: &str) -> BTreeMap<String, Signature> {
    let mut out = BTreeMap::new();

    for marker in ["pub unsafe extern \"C\" fn ", "pub extern \"C\" fn "] {
        let mut cursor = 0usize;
        while let Some(found) = src[cursor..].find(marker) {
            let start = cursor + found + marker.len();
            let open = match src[start..].find('(') {
                Some(i) => start + i,
                None => break,
            };
            let name = src[start..open].trim().to_string();

            // Balanced scan: parameter types here contain no nested parens today,
            // but a scan that assumed so would silently truncate if one appeared.
            let mut depth = 0usize;
            let mut close = None;
            for (i, ch) in src[open..].char_indices() {
                match ch {
                    '(' => depth += 1,
                    ')' => {
                        depth -= 1;
                        if depth == 0 {
                            close = Some(open + i);
                            break;
                        }
                    }
                    _ => {}
                }
            }
            let close = match close {
                Some(c) => c,
                None => panic!("unterminated parameter list for {}", name),
            };

            let params: Vec<String> = src[open + 1..close]
                .split(',')
                .map(str::trim)
                .filter(|p| !p.is_empty())
                .map(|p| match p.split_once(':') {
                    Some((_, ty)) => rust_type_to_c(ty),
                    None => panic!("parameter `{}` of {} has no type", p, name),
                })
                .collect();

            // Return type runs from `->` to the opening brace of the body.
            let tail = &src[close + 1..];
            let body = match tail.find('{') {
                Some(i) => i,
                None => panic!("no body found for {}", name),
            };
            let ret = match tail[..body].trim().strip_prefix("->") {
                Some(r) => rust_type_to_c(r),
                None => "void".to_string(),
            };

            out.insert(name, Signature { ret, params });
            cursor = close;
        }
    }

    out
}

#[test]
fn header_declarations_match_the_rust_exports() {
    let rust = parse_rust(&read("crates/gusset/src/ffi/mod.rs"));
    let header = parse_header(&read("internal/ffi/gusset.h"));

    assert!(
        !rust.is_empty(),
        "parsed zero exports out of crates/gusset/src/ffi/mod.rs: the parser has \
         drifted from the source and would approve anything"
    );

    let rust_names: Vec<&String> = rust.keys().collect();
    let header_names: Vec<&String> = header.keys().collect();
    assert_eq!(
        rust_names, header_names,
        "gusset.h declares a different set of functions than Rust exports"
    );

    for (name, rust_sig) in &rust {
        let header_sig = match header.get(name) {
            Some(s) => s,
            None => panic!(
                "{} is exported from Rust but not declared in gusset.h",
                name
            ),
        };
        assert_eq!(
            rust_sig.ret, header_sig.ret,
            "{}: Rust returns `{}`, gusset.h declares `{}`",
            name, rust_sig.ret, header_sig.ret
        );
        assert_eq!(
            rust_sig.params.len(),
            header_sig.params.len(),
            "{}: Rust takes {} parameters, gusset.h declares {}",
            name,
            rust_sig.params.len(),
            header_sig.params.len()
        );
        for (i, (r, h)) in rust_sig
            .params
            .iter()
            .zip(header_sig.params.iter())
            .enumerate()
        {
            assert_eq!(
                r, h,
                "{} parameter {}: Rust has `{}`, gusset.h declares `{}` — cgo will \
                 pass this argument in the wrong shape",
                name, i, r, h
            );
        }
    }
}

#[test]
fn exports_list_matches_the_header_and_the_rust_source() {
    let listed: Vec<String> = read("internal/ffi/exports.txt")
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty() && !l.starts_with('#'))
        .map(str::to_string)
        .collect();

    let mut sorted = listed.clone();
    sorted.sort();
    assert_eq!(
        listed, sorted,
        "internal/ffi/exports.txt must stay sorted so diffs stay reviewable"
    );

    let rust: Vec<String> = parse_rust(&read("crates/gusset/src/ffi/mod.rs"))
        .into_keys()
        .collect();
    let header: Vec<String> = parse_header(&read("internal/ffi/gusset.h"))
        .into_keys()
        .collect();

    assert_eq!(
        listed, rust,
        "exports.txt disagrees with the `extern \"C\"` functions in the Rust source"
    );
    assert_eq!(
        listed, header,
        "exports.txt disagrees with the declarations in gusset.h"
    );
}
