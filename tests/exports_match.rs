//! R1: the C ABI surface is exactly the 14 names in `internal/ffi/exports.txt`.
//!
//! Two defects this test used to have, both of which made it report success without
//! checking the artifact that ships:
//!
//! 1. It preferred `target/debug/libgusset.a` and stopped there. `cargo test` builds
//!    debug, so it almost always ran against an archive nothing links — cgo links
//!    `target/release` (see the LDFLAGS in `internal/ffi/ffi.go`) and `make install`
//!    ships it.
//! 2. It ran Apple's `nm` and ignored both its exit status and its stderr. The
//!    release profile sets `lto = "fat"`, so that archive holds LLVM bitcode, and a
//!    system `nm` built against an older LLVM cannot read it:
//!
//!        error: ... Unknown attribute kind (105)
//!              (Producer: 'LLVM22.1.8-rust-1.98.0-stable' Reader: 'LLVM APPLE_...')
//!
//!    `nm` then printed nothing to stdout, the parse found an empty symbol set, and
//!    a check that could not run would have been indistinguishable from a check that
//!    ran and passed.
//!
//! It now reads every archive present, prefers the `llvm-nm` shipped with the Rust
//! toolchain (which understands the bitcode), and treats an unreadable archive as a
//! failure rather than as an empty export list.

use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

/// Locates the `llvm-nm` from the active Rust toolchain's `llvm-tools` component.
fn llvm_nm_path() -> Option<PathBuf> {
    let out = Command::new("rustc").arg("--print").arg("sysroot").output();
    let out = match out {
        Ok(o) if o.status.success() => o,
        _ => return None,
    };
    let sysroot = PathBuf::from(String::from_utf8_lossy(&out.stdout).trim().to_string());

    let rustlib = sysroot.join("lib/rustlib");
    let entries = match fs::read_dir(&rustlib) {
        Ok(e) => e,
        Err(_) => return None,
    };
    for entry in entries.flatten() {
        let candidate = entry.path().join("bin/llvm-nm");
        if candidate.exists() {
            return Some(candidate);
        }
    }
    None
}

/// Reads the defined text symbols of an archive, or explains why it could not.
fn defined_symbols(nm: &Path, lib: &Path) -> Result<BTreeSet<String>, String> {
    let output = match Command::new(nm).arg("--defined-only").arg(lib).output() {
        Ok(o) => o,
        Err(e) => return Err(format!("failed to execute {:?}: {}", nm, e)),
    };

    let stderr = String::from_utf8_lossy(&output.stderr);
    if !output.status.success() {
        return Err(format!(
            "{:?} exited with {}: {}",
            nm, output.status, stderr
        ));
    }
    // A reader that could not parse some members prints errors and still exits 0.
    // Those members' symbols are simply missing, which would silently look like a
    // clean export list.
    if stderr.contains("error:") {
        return Err(format!(
            "{:?} could not read every member of {:?}; its symbol list is incomplete \
             and must not be treated as an export list:\n{}",
            nm, lib, stderr
        ));
    }

    let stdout = String::from_utf8_lossy(&output.stdout);
    let mut found = BTreeSet::new();
    for line in stdout.lines() {
        if line.contains(" T _gusset_") || line.contains(" T gusset_") {
            let parts: Vec<&str> = line.split_whitespace().collect();
            if let Some(sym) = parts.last() {
                let name = sym.trim_start_matches('_');
                if name.starts_with("gusset_") {
                    found.insert(name.to_string());
                }
            }
        }
    }
    Ok(found)
}

#[test]
fn test_exports_match_list() {
    let manifest_dir = Path::new(env!("CARGO_MANIFEST_DIR"));
    let root_dir = manifest_dir.join("../..");

    let exports_path = root_dir.join("internal/ffi/exports.txt");
    let exports_txt = match fs::read_to_string(&exports_path) {
        Ok(s) => s,
        Err(e) => panic!("failed to read {:?}: {}", exports_path, e),
    };

    let expected_exports: BTreeSet<String> = exports_txt
        .lines()
        .map(|l| l.trim().to_string())
        .filter(|l| !l.is_empty() && !l.starts_with('#'))
        .collect();

    assert_eq!(
        expected_exports.len(),
        14,
        "expected exactly 14 ABI functions"
    );

    let candidates: Vec<PathBuf> = ["target/debug/libgusset.a", "target/release/libgusset.a"]
        .iter()
        .map(|p| root_dir.join(p))
        .filter(|p| p.exists())
        .collect();

    assert!(
        !candidates.is_empty(),
        "neither target/debug/libgusset.a nor target/release/libgusset.a exists; \
         run cargo build first"
    );

    // Prefer the toolchain's llvm-nm: the release archive is fat-LTO bitcode that a
    // system nm from an older LLVM cannot read.
    let readers: Vec<PathBuf> = match llvm_nm_path() {
        Some(p) => vec![p, PathBuf::from("nm")],
        None => vec![PathBuf::from("nm")],
    };

    for lib_path in &candidates {
        let mut last_err = String::new();
        let mut checked = false;

        for nm in &readers {
            match defined_symbols(nm, lib_path) {
                Ok(found) => {
                    assert_eq!(
                        found, expected_exports,
                        "exported C ABI symbols in {:?} (read with {:?}) do not match \
                         internal/ffi/exports.txt",
                        lib_path, nm
                    );
                    checked = true;
                    break;
                }
                Err(e) => last_err = e,
            }
        }

        assert!(
            checked,
            "could not read the symbol table of {:?}, so R1 went unverified for the \
             archive that actually ships. Install the toolchain's llvm-tools \
             (`rustup component add llvm-tools`) so fat-LTO archives can be read.\n\
             Last reader error: {}",
            lib_path, last_err
        );
    }
}
