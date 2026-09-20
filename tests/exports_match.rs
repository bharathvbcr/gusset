use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

#[test]
fn test_exports_match_list() {
    let manifest_dir = Path::new(env!("CARGO_MANIFEST_DIR"));
    let root_dir = manifest_dir.join("../..");

    let exports_path = root_dir.join("internal/ffi/exports.txt");
    let exports_txt = fs::read_to_string(&exports_path)
        .unwrap_or_else(|e| panic!("failed to read {:?}: {}", exports_path, e));

    let expected_exports: BTreeSet<String> = exports_txt
        .lines()
        .map(|l| l.trim().to_string())
        .filter(|l| !l.is_empty() && !l.starts_with('#'))
        .collect();

    assert_eq!(expected_exports.len(), 14, "expected exactly 14 ABI functions");

    let lib_debug = root_dir.join("target/debug/libgusset.a");
    let lib_release = root_dir.join("target/release/libgusset.a");

    let lib_path: PathBuf = if lib_debug.exists() {
        lib_debug
    } else if lib_release.exists() {
        lib_release
    } else {
        panic!("neither target/debug/libgusset.a nor target/release/libgusset.a exists; run cargo build first");
    };

    let output = Command::new("nm")
        .arg("-U")
        .arg(&lib_path)
        .output()
        .expect("failed to execute nm");

    let stdout = String::from_utf8_lossy(&output.stdout);
    let mut found_exports = BTreeSet::new();

    for line in stdout.lines() {
        if line.contains(" T _gusset_") || line.contains(" T gusset_") {
            let parts: Vec<&str> = line.split_whitespace().collect();
            if let Some(sym) = parts.last() {
                let name = sym.trim_start_matches('_');
                if name.starts_with("gusset_") {
                    found_exports.insert(name.to_string());
                }
            }
        }
    }

    assert_eq!(
        found_exports, expected_exports,
        "exported C ABI symbols in {:?} do not match internal/ffi/exports.txt",
        lib_path
    );
}
