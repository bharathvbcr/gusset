use std::env;
use std::path::Path;

include!("allocator_probe.rs");

fn is_kani_verifier() -> bool {
    if env::var_os("CARGO_CFG_KANI").is_some() {
        return true;
    }
    for key in ["RUSTC_WRAPPER", "RUSTC"] {
        if let Some(val) = env::var_os(key) {
            if Path::new(&val)
                .file_stem()
                .and_then(|s| s.to_str())
                .is_some_and(|stem| stem == "kani-compiler")
            {
                return true;
            }
        }
    }
    false
}

fn main() {
    println!("cargo::rustc-check-cfg=cfg(kani)");
    println!("cargo::rerun-if-env-changed=RUSTC_WRAPPER");

    // R2: Ensure panic strategy is "unwind". Abort silently disables the panic firewall.
    // Kani's `kani-compiler` forces `-C panic=abort` so CBMC lowers panics directly to
    // solver assertion checks rather than DWARF stack unwinding; every non-Kani build
    // strictly enforces `panic = "unwind"`.
    if !is_kani_verifier() {
        if let Ok(panic_strategy) = env::var("CARGO_CFG_PANIC") {
            if panic_strategy == "abort" {
                panic!(
                    "gusset requires panic = 'unwind'. Found panic = 'abort', which disables the panic firewall (R2)."
                );
            }
        }
    }

    probe_allocator_api();
}
