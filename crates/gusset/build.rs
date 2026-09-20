use std::env;

fn main() {
    // R2: Ensure panic strategy is "unwind". Abort silently disables the panic firewall.
    if let Ok(panic_strategy) = env::var("CARGO_CFG_PANIC") {
        if panic_strategy == "abort" {
            panic!(
                "gusset requires panic = 'unwind'. Found panic = 'abort', which disables the panic firewall (R2)."
            );
        }
    }
}
