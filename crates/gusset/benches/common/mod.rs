//! What the interleaved bench binaries (`dispatch`, `alloc_counters`) share:
//! the round floor, the failure path and the command line.
//!
//! This is `benches/common/mod.rs`, not `benches/common.rs`, and each bench
//! declares it with `mod common;`. Cargo discovers a bench target from
//! `benches/*.rs` and `benches/*/main.rs`; a directory holding only `mod.rs` is
//! neither, so it is compiled into the benches that name it and is not a target
//! of its own. `crates/gusset/Cargo.toml` also lists its benches explicitly.
//!
//! [`MIN_ROUNDS`] is the one copy of the floor. `bench/record.sh` keeps a
//! `WARM_COUNT` that must equal it, because the warm-up asks the cargo bench for
//! exactly that many rounds.

/// The fewest rounds a run may ask for; also the default. benchstat will not
/// quote a confidence interval below 6 samples, and fewer rounds than this
/// cannot tell an order effect from a difference between arms.
pub const MIN_ROUNDS: u32 = 10;

/// What a bench was asked to do.
pub struct Config {
    pub rounds: u32,
    pub iters: u32,
}

/// Prints `<bench> bench: <msg>` and exits non-zero.
pub fn fail(bench: &str, msg: &str) -> ! {
    eprintln!("{bench} bench: {msg}");
    std::process::exit(1);
}

fn positive(bench: &str, flag: &str, value: Option<String>) -> u32 {
    match value.as_deref().map(str::parse::<u32>) {
        Some(Ok(n)) if n > 0 => n,
        _ => fail(bench, &format!("{flag} needs a positive integer")),
    }
}

/// Reads `--rounds N` and `--iters N` for the bench called `bench`, defaulting
/// to [`MIN_ROUNDS`] and `default_iters`. cargo appends `--bench` when it runs a
/// bench target, so that one is accepted and ignored; anything else is refused
/// rather than skipped, because a typo'd flag would otherwise run the defaults
/// and look like the run that was asked for. A round count below [`MIN_ROUNDS`]
/// is refused too.
pub fn parse_args(bench: &str, default_iters: u32) -> Config {
    let mut cfg = Config {
        rounds: MIN_ROUNDS,
        iters: default_iters,
    };
    let mut args = std::env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--bench" => {}
            "--rounds" => cfg.rounds = positive(bench, "--rounds", args.next()),
            "--iters" => cfg.iters = positive(bench, "--iters", args.next()),
            other => fail(
                bench,
                &format!("unknown argument {other}; usage: {bench} [--rounds N] [--iters N]"),
            ),
        }
    }
    if cfg.rounds < MIN_ROUNDS {
        fail(
            bench,
            &format!(
                "--rounds {} is below the floor of {MIN_ROUNDS}: fewer rounds cannot separate an \
                 order effect from a difference between arms",
                cfg.rounds
            ),
        );
    }
    cfg
}
