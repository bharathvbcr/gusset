//! Named field offsets of every `#[repr(C)]` type that crosses the boundary.
//!
//! `gusset_abi_layout` reports the size and alignment of each struct. Two fields
//! of equal width can trade places (`span_id` with `timeout_ns`, `flags` with
//! `reserved`, any permutation of `AllocStats`) and both numbers stay the same,
//! so a size check accepts a header that reads every field from the wrong slot.
//!
//! This table is the contract `gusset_abi_fields` reports. Go compares it with
//! `unsafe.Offsetof` / `unsafe.Sizeof` on the structs cgo compiled from
//! `internal/ffi/gusset.h`. The two compilers are the two sources of truth;
//! a hand-written offset constant would be a third copy of the same mistake.
//!
//! The table is appended as its own export, rather than grown onto `AbiLayout`,
//! because a caller built against ABI version 2 allocates 36 bytes for that
//! struct. Writing a larger struct into it smashes the caller's buffer before
//! the version word can be checked.

use crate::header::CallHeader;
use std::mem::{offset_of, size_of};

use super::alloc::AllocStats;
use super::status::FfiStatus;
use super::{AbiLayout, ABI_TYPE_COUNT};

/// Number of named fields across the four crossing structs, in struct order
/// `CallHeader`, `FfiStatus`, `AbiLayout`, `AllocStats`.
pub const ABI_FIELD_COUNT: usize = 17;

/// `Type.field` in the same order as [`layout`].
///
/// Referenced from the length check below so a non-test build still owns the
/// list the source scanner compares against.
pub const ABI_FIELD_NAMES: [&str; ABI_FIELD_COUNT] = [
    "CallHeader.trace_id",
    "CallHeader.span_id",
    "CallHeader.timeout_ns",
    "CallHeader.flags",
    "CallHeader.reserved",
    "FfiStatus.code",
    "FfiStatus.msg",
    "FfiStatus.msg_len",
    "FfiStatus.file",
    "FfiStatus.file_len",
    "FfiStatus.line",
    "AbiLayout.version",
    "AbiLayout.sizes",
    "AbiLayout.aligns",
    "AllocStats.live_bytes",
    "AllocStats.peak_bytes",
    "AllocStats.alloc_count",
];

const _: [(); ABI_FIELD_COUNT] = [(); ABI_FIELD_NAMES.len()];

/// One field: byte offset from the start of its struct, and the field's size.
///
/// The closure pins the field's type. A `u32` rewritten as a `u64` under the
/// same name fails to compile here, instead of shipping a width change that
/// padding happens to hide from the struct-size check.
macro_rules! slot {
    ($ty:ty, $field:ident, $fty:ty) => {{
        const _: for<'a> fn(&'a $ty) -> &'a $fty = |v| &v.$field;
        (offset_of!($ty, $field) as u32, size_of::<$fty>() as u32)
    }};
}

/// Offsets and sizes, indexed like [`ABI_FIELD_NAMES`].
pub fn layout() -> ([u32; ABI_FIELD_COUNT], [u32; ABI_FIELD_COUNT]) {
    let slots: [(u32, u32); ABI_FIELD_COUNT] = [
        slot!(CallHeader, trace_id, [u8; 16]),
        slot!(CallHeader, span_id, [u8; 8]),
        slot!(CallHeader, timeout_ns, u64),
        slot!(CallHeader, flags, u32),
        slot!(CallHeader, reserved, u32),
        slot!(FfiStatus, code, i32),
        slot!(FfiStatus, msg, *mut u8),
        slot!(FfiStatus, msg_len, usize),
        slot!(FfiStatus, file, *const u8),
        slot!(FfiStatus, file_len, usize),
        slot!(FfiStatus, line, u32),
        slot!(AbiLayout, version, u32),
        slot!(AbiLayout, sizes, [u32; ABI_TYPE_COUNT]),
        slot!(AbiLayout, aligns, [u32; ABI_TYPE_COUNT]),
        slot!(AllocStats, live_bytes, usize),
        slot!(AllocStats, peak_bytes, usize),
        slot!(AllocStats, alloc_count, usize),
    ];
    let mut offsets = [0u32; ABI_FIELD_COUNT];
    let mut sizes = [0u32; ABI_FIELD_COUNT];
    for (i, (off, sz)) in slots.iter().enumerate() {
        offsets[i] = *off;
        sizes[i] = *sz;
    }
    (offsets, sizes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::path::Path;

    #[test]
    fn equal_width_swap_keeps_the_offset_multiset_and_moves_the_names() {
        let (off, sz) = layout();
        let mut pairs = 0usize;
        for i in 0..ABI_FIELD_COUNT {
            for j in (i + 1)..ABI_FIELD_COUNT {
                if sz[i] != sz[j] || off[i] == off[j] {
                    continue;
                }
                pairs += 1;
                let mut swapped = off;
                swapped.swap(i, j);
                if swapped == off {
                    panic!(
                        "{} and {} swapped to the same offsets",
                        ABI_FIELD_NAMES[i], ABI_FIELD_NAMES[j]
                    );
                }
                let mut sorted_a = off;
                let mut sorted_b = swapped;
                sorted_a.sort_unstable();
                sorted_b.sort_unstable();
                if sorted_a != sorted_b {
                    panic!(
                        "equal-width swap of {} and {} changed the offset multiset; \
                         a size check is blind only when the multiset stays put",
                        ABI_FIELD_NAMES[i], ABI_FIELD_NAMES[j]
                    );
                }
            }
        }
        if pairs == 0 {
            panic!("no equal-width field pair; the size check's blind spot is untested");
        }
    }

    #[test]
    fn field_table_matches_the_structs_and_the_header() {
        let src = crate_src();
        let mut by_name = std::collections::BTreeMap::<String, Vec<String>>::new();
        for (name, fields) in repr_c_structs(&src) {
            if by_name.insert(name.clone(), fields).is_some() {
                panic!("duplicate repr(C) struct {name}");
            }
        }
        let names: Vec<&str> = by_name.keys().map(String::as_str).collect();
        let expected = ["AbiLayout", "AllocStats", "CallHeader", "FfiStatus"];
        if names != expected {
            panic!("repr(C) set is {names:?}, the ABI manifest is {expected:?}");
        }

        let order = ["CallHeader", "FfiStatus", "AbiLayout", "AllocStats"];
        let mut qualified = Vec::new();
        for ty in order {
            match by_name.get(ty) {
                Some(fields) => {
                    for f in fields {
                        qualified.push(format!("{ty}.{f}"));
                    }
                }
                None => panic!("{ty} missing from repr(C) scan"),
            }
        }
        let table: Vec<String> = ABI_FIELD_NAMES.iter().map(|s| (*s).to_string()).collect();
        if qualified != table {
            panic!("field table {table:?} does not match source fields {qualified:?}");
        }

        let header_path = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../internal/ffi/gusset.h");
        let header_src = match fs::read_to_string(&header_path) {
            Ok(s) => s,
            Err(e) => panic!("cannot read {}: {e}", header_path.display()),
        };
        let header = c_structs(&header_src);
        for ty in order {
            let rust_fields = match by_name.get(ty) {
                Some(f) => f,
                None => panic!("{ty} missing from rust scan"),
            };
            let c_fields = match header.get(ty) {
                Some(f) => f,
                None => panic!("{ty} missing from gusset.h"),
            };
            if rust_fields != c_fields {
                panic!("{ty}: rust fields {rust_fields:?}, header fields {c_fields:?}");
            }
        }

        let (off, sz) = layout();
        let struct_sizes = [
            size_of::<CallHeader>() as u32,
            size_of::<FfiStatus>() as u32,
            size_of::<AbiLayout>() as u32,
            size_of::<AllocStats>() as u32,
        ];
        let mut cursor = 0usize;
        for (idx, ty) in order.iter().enumerate() {
            let n = match by_name.get(*ty) {
                Some(f) => f.len(),
                None => panic!("{ty} missing"),
            };
            if cursor + n > ABI_FIELD_COUNT {
                panic!("{ty} fields run past the table");
            }
            for i in 0..n {
                let at = cursor + i;
                if sz[at] == 0 {
                    panic!("{ty} field {i} has size 0; the check would pass vacuously");
                }
                if i > 0 {
                    let prev = cursor + i - 1;
                    if off[at] <= off[prev] {
                        panic!("{ty} field {i} offset did not advance");
                    }
                    if off[prev] + sz[prev] > off[at] {
                        panic!("{ty} field {i} overlaps the previous field");
                    }
                }
            }
            let last = cursor + n - 1;
            if off[last] + sz[last] > struct_sizes[idx] {
                panic!(
                    "{ty} last field ends at {}, struct is {}",
                    off[last] + sz[last],
                    struct_sizes[idx]
                );
            }
            cursor += n;
        }
        if cursor != ABI_FIELD_COUNT {
            panic!("table covers {cursor} fields, ABI_FIELD_COUNT is {ABI_FIELD_COUNT}");
        }
    }

    fn crate_src() -> Vec<(String, String)> {
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("src");
        let mut files = Vec::new();
        collect_rs(&root, &mut files);
        let mut out = Vec::new();
        for path in files {
            match fs::read_to_string(&path) {
                Ok(s) => out.push((path.display().to_string(), s)),
                Err(e) => panic!("cannot read {}: {e}", path.display()),
            }
        }
        if out.is_empty() {
            panic!(
                "repr(C) scan found no rust sources under {}",
                root.display()
            );
        }
        out
    }

    fn collect_rs(dir: &Path, out: &mut Vec<std::path::PathBuf>) {
        let entries = match fs::read_dir(dir) {
            Ok(e) => e,
            Err(e) => panic!("cannot read {}: {e}", dir.display()),
        };
        for entry in entries {
            let entry = match entry {
                Ok(e) => e,
                Err(e) => panic!("cannot read an entry under {}: {e}", dir.display()),
            };
            let path = entry.path();
            if path.is_dir() {
                collect_rs(&path, out);
            } else if path.extension().and_then(|e| e.to_str()) == Some("rs") {
                out.push(path);
            }
        }
    }

    /// `#[repr(C)]` structs in this crate. A line has to *be* the attribute;
    /// a doc comment that mentions the token is not a struct.
    fn repr_c_structs(files: &[(String, String)]) -> Vec<(String, Vec<String>)> {
        let mut found = Vec::new();
        for (_path, src) in files {
            let lines: Vec<&str> = src.lines().collect();
            let mut i = 0usize;
            while i < lines.len() {
                let trimmed = lines[i].trim();
                let is_repr = trimmed.starts_with("#[repr(C)]") || trimmed.starts_with("#[repr(C,");
                if !is_repr {
                    i += 1;
                    continue;
                }
                let mut j = i + 1;
                while j < lines.len() {
                    let u = lines[j].trim();
                    if u.is_empty()
                        || u.starts_with("//")
                        || u.starts_with("#[")
                        || u.starts_with("///")
                    {
                        j += 1;
                        continue;
                    }
                    let rest = u
                        .strip_prefix("pub struct ")
                        .or_else(|| u.strip_prefix("struct "));
                    if let Some(rest) = rest {
                        let name: String = rest
                            .chars()
                            .take_while(|c| c.is_ascii_alphanumeric() || *c == '_')
                            .collect();
                        if name.is_empty() {
                            panic!("repr(C) attribute not followed by a struct name");
                        }
                        found.push((name, rust_fields(src, rest)));
                    }
                    break;
                }
                i += 1;
            }
        }
        found
    }

    fn rust_fields(src: &str, after_struct_kw: &str) -> Vec<String> {
        // `after_struct_kw` is the tail of the `struct Name` line, starting at the name.
        let name_end = after_struct_kw
            .find(|c: char| !(c.is_ascii_alphanumeric() || c == '_'))
            .unwrap_or(after_struct_kw.len());
        let name = &after_struct_kw[..name_end];
        let marker = format!("struct {name}");
        let start = match src.find(&marker) {
            Some(i) => i,
            None => panic!("cannot find {marker} after its repr(C) attribute"),
        };
        let brace = match src[start..].find('{') {
            Some(i) => start + i,
            None => panic!("{name} has no body"),
        };
        let mut depth = 0usize;
        let mut end = None;
        for (i, ch) in src[brace..].char_indices() {
            match ch {
                '{' => depth += 1,
                '}' => {
                    depth -= 1;
                    if depth == 0 {
                        end = Some(brace + i);
                        break;
                    }
                }
                _ => {}
            }
        }
        let end = match end {
            Some(e) => e,
            None => panic!("{name} body is unterminated"),
        };
        let mut fields = Vec::new();
        for line in src[brace + 1..end].lines() {
            let line = line.trim();
            if line.is_empty() || line.starts_with("//") || line.starts_with('#') {
                continue;
            }
            let Some(rest) = line.strip_prefix("pub ") else {
                continue;
            };
            let Some((fname, _)) = rest.split_once(':') else {
                continue;
            };
            let fname = fname.trim();
            if fname.is_empty() {
                panic!("{name} has an empty field name");
            }
            fields.push(fname.to_string());
        }
        if fields.is_empty() {
            panic!("{name} parsed as having no fields; the scanner would approve an empty table");
        }
        fields
    }

    fn c_structs(src: &str) -> std::collections::BTreeMap<String, Vec<String>> {
        let mut map = std::collections::BTreeMap::new();
        let mut rest = src;
        let needle = "typedef struct {";
        while let Some(idx) = rest.find(needle) {
            let after = &rest[idx + needle.len()..];
            let close = match after.find('}') {
                Some(i) => i,
                None => panic!("unterminated typedef struct in gusset.h"),
            };
            let body = &after[..close];
            let tail = after[close + 1..].trim_start();
            let name: String = tail
                .chars()
                .take_while(|c| c.is_ascii_alphanumeric() || *c == '_')
                .collect();
            if name.is_empty() {
                panic!("typedef struct without a name");
            }
            let mut fields = Vec::new();
            for line in body.lines() {
                let line = line.trim();
                if line.is_empty()
                    || line.starts_with("//")
                    || line.starts_with("/*")
                    || line.starts_with('*')
                {
                    continue;
                }
                let line = line.trim_end_matches(';').trim();
                let line = line.split('[').next().unwrap_or(line).trim();
                let fname: String = line
                    .rsplit(|c: char| !(c.is_ascii_alphanumeric() || c == '_'))
                    .next()
                    .unwrap_or("")
                    .to_string();
                if fname.is_empty() {
                    panic!("header field with no name in {name}: {line}");
                }
                fields.push(fname);
            }
            if fields.is_empty() {
                panic!("{name} in gusset.h parsed as having no fields");
            }
            if map.insert(name.clone(), fields).is_some() {
                panic!("duplicate C struct {name}");
            }
            rest = &after[close..];
        }
        if map.is_empty() {
            panic!("parsed no structs from gusset.h; the scanner would approve anything");
        }
        map
    }
}
