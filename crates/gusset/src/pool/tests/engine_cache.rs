//! The per-worker engine cache (R7): what it may serve, and when it must stop.
//!
//! These drive `EngineCache::dispatch` directly on the test thread, so they need
//! no pipe and no worker, and Miri can run them. They stay on nonzero opcodes on
//! purpose: a global (opcode 0) handler installed here would be run by every
//! parallel unit test that dispatches through the diagnostic engine. The opcode 0
//! half, and everything that needs real workers (a clear with jobs in flight, a
//! concurrent register/clear stress), is in `tests/engine_cache.rs`, which is its
//! own process.

use super::{Must, REGISTRY_TEST_LOCK};
use crate::header::{CallHeader, JobContext, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use crate::pool::engine::{registry_generation, EngineCache};
use crate::pool::{
    clear_engine_handlers, default_dispatch, lock_recover, register_engine, JobOutput,
};
use std::sync::atomic::AtomicBool;
use std::sync::Arc;

/// Opcodes this file owns, clear of the 93xx range `engine.rs` uses.
const OP_A: u32 = 9411;
const OP_B: u32 = 9412;
const OP_C: u32 = 9413;

fn job(opcode: u32, flags: u32) -> JobContext {
    let header = CallHeader {
        reserved: opcode,
        flags,
        ..CallHeader::default()
    };
    JobContext::new(header, Arc::new(AtomicBool::new(false)))
}

/// The bytes a dispatch returned, or its refusal text.
fn bytes_of(result: Result<JobOutput, String>) -> Result<Vec<u8>, String> {
    match result {
        Ok(JobOutput::Bytes(bytes)) => Ok(bytes),
        Ok(other) => panic!("expected bytes, got {other:?}"),
        Err(msg) => Err(msg),
    }
}

fn run(cache: &mut EngineCache, opcode: u32, input: &[u8]) -> Result<Vec<u8>, String> {
    bytes_of(cache.dispatch(&job(opcode, 0), input))
}

/// A handler that answers with one fixed byte, so a result names its handler.
fn answer(
    byte: u8,
) -> impl Fn(&JobContext, &[u8]) -> Result<Vec<u8>, String> + Send + Sync + 'static {
    move |_ctx, _input| Ok(vec![byte])
}

fn assert_refused(result: &Result<Vec<u8>, String>, opcode: u32) {
    match result {
        Err(msg) => assert!(
            msg.contains(&format!("no engine handler registered for opcode {opcode}")),
            "opcode {opcode} refused with the wrong message: {msg}"
        ),
        Ok(bytes) => panic!("opcode {opcode} ran a handler after it was cleared: {bytes:?}"),
    }
}

/// The case the cache exists to get wrong: a worker that has run a handler keeps
/// it, so a clear it never hears about would keep serving it.
#[test]
fn a_cached_handler_stops_running_when_cleared_and_the_replacement_runs() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    clear_engine_handlers();
    register_engine(OP_A, answer(1)).must("register");

    let mut cache = EngineCache::default();
    assert_eq!(run(&mut cache, OP_A, b"x"), Ok(vec![1]));
    // The second call is the cache hit.
    assert_eq!(run(&mut cache, OP_A, b"x"), Ok(vec![1]));
    assert_eq!(cache.cached_opcodes(), 1, "the handler was not cached");

    clear_engine_handlers();
    assert_refused(&run(&mut cache, OP_A, b"x"), OP_A);

    register_engine(OP_A, answer(2)).must("re-register after clear");
    assert_eq!(
        run(&mut cache, OP_A, b"x"),
        Ok(vec![2]),
        "the re-registered handler must run, not the cleared one"
    );
    clear_engine_handlers();
}

/// Several opcodes cached on one worker go together: a clear is not per opcode,
/// and a reinstall of one must not revive the others.
#[test]
fn one_clear_drops_every_opcode_a_worker_has_cached() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    clear_engine_handlers();
    register_engine(OP_A, answer(1)).must("register A");
    register_engine(OP_B, answer(2)).must("register B");
    register_engine(OP_C, answer(3)).must("register C");

    let mut cache = EngineCache::default();
    for _ in 0..2 {
        assert_eq!(run(&mut cache, OP_A, b"x"), Ok(vec![1]));
        assert_eq!(run(&mut cache, OP_B, b"x"), Ok(vec![2]));
        assert_eq!(run(&mut cache, OP_C, b"x"), Ok(vec![3]));
    }
    assert_eq!(cache.cached_opcodes(), 3);

    clear_engine_handlers();
    for opcode in [OP_A, OP_B, OP_C] {
        assert_refused(&run(&mut cache, opcode, b"x"), opcode);
    }

    register_engine(OP_B, answer(20)).must("reinstall B");
    assert_refused(&run(&mut cache, OP_A, b"x"), OP_A);
    assert_eq!(run(&mut cache, OP_B, b"x"), Ok(vec![20]));
    assert_refused(&run(&mut cache, OP_C, b"x"), OP_C);
    clear_engine_handlers();
}

/// A registration made after the worker last looked is seen at once, and a
/// refusal is never cached: a caller choosing opcodes cannot grow the cache or
/// pin an opcode to "refused".
#[test]
fn a_refusal_is_not_remembered_and_the_cache_stays_bounded() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    clear_engine_handlers();

    let mut cache = EngineCache::default();
    for opcode in 20_000..20_064 {
        assert_refused(&run(&mut cache, opcode, b"x"), opcode);
    }
    assert_eq!(
        cache.cached_opcodes(),
        0,
        "refused opcodes must not be cached"
    );

    register_engine(20_010, answer(7)).must("register");
    assert_eq!(
        run(&mut cache, 20_010, b"x"),
        Ok(vec![7]),
        "a refusal was remembered past a registration"
    );
    assert_eq!(cache.cached_opcodes(), 1);
    clear_engine_handlers();
}

/// Resolution order, with the cache warm: a nonzero opcode never reaches the
/// diagnostic engine or a global handler, opcode 0 never consults the registry,
/// and the diagnostic engine runs only on the caller's flag.
#[test]
fn opcode_zero_and_nonzero_stay_isolated_through_the_cache() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    clear_engine_handlers();
    register_engine(OP_A, answer(1)).must("register");

    let mut cache = EngineCache::default();
    assert_eq!(run(&mut cache, OP_A, b"x"), Ok(vec![1]));

    // Opcode 0 does not see the registry entry: with no global handler and no
    // flag it fails closed, with the opcode 0 message, not OP_A's handler.
    match run(&mut cache, 0, b"x") {
        Err(msg) => assert!(
            msg.contains("no engine handler registered; call"),
            "opcode 0 fail-closed message: {msg}"
        ),
        Ok(bytes) => panic!("opcode 0 ran the registry's handler: {bytes:?}"),
    }
    // Twice, so the second is the cached absence.
    assert!(run(&mut cache, 0, b"x").is_err());

    // The diagnostic engine runs for opcode 0 on the caller's flag, and the
    // registry being loaded does not change that.
    let echoed = bytes_of(cache.dispatch(&job(0, GUSSET_FLAG_DIAGNOSTIC_ENGINE), &[0]));
    assert_eq!(echoed, Ok(vec![0]));

    // A nonzero opcode with no entry is refused even with the flag set.
    let other = bytes_of(cache.dispatch(&job(OP_B, GUSSET_FLAG_DIAGNOSTIC_ENGINE), &[0]));
    assert_refused(&other, OP_B);

    // And the cached opcode keeps running while all of that happens.
    assert_eq!(run(&mut cache, OP_A, b"x"), Ok(vec![1]));
    clear_engine_handlers();
}

/// The invariant the cache's soundness rests on: every write that changes what
/// dispatch would return bumps the generation, and a refused one, which changes
/// nothing, does not.
#[test]
fn only_a_registry_write_publishes_a_new_generation() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    clear_engine_handlers();

    let before = registry_generation();
    assert!(
        register_engine(0, answer(1)).is_err(),
        "opcode 0 registered"
    );
    assert_eq!(
        registry_generation(),
        before,
        "a refused opcode 0 bumped it"
    );

    register_engine(OP_A, answer(1)).must("register");
    let registered = registry_generation();
    assert!(registered > before, "a registration did not bump it");

    assert!(register_engine(OP_A, answer(2)).is_err(), "duplicate");
    assert_eq!(
        registry_generation(),
        registered,
        "a refused duplicate bumped it"
    );

    clear_engine_handlers();
    assert!(
        registry_generation() > registered,
        "a clear did not bump it"
    );
}

/// Pins the documented cost of the cache: it holds the handlers its worker ran,
/// so a clear does not free them until that worker dispatches again or exits.
#[test]
fn a_worker_releases_a_cleared_handler_at_its_next_dispatch_or_exit() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    clear_engine_handlers();
    let witness = Arc::new(());

    let held = Arc::clone(&witness);
    register_engine(OP_A, move |_ctx: &JobContext, _input: &[u8]| {
        let _ = &held;
        Ok(vec![1u8])
    })
    .must("register");
    assert_eq!(Arc::strong_count(&witness), 2, "the registry holds it");

    let mut cache = EngineCache::default();
    assert_eq!(run(&mut cache, OP_A, b"x"), Ok(vec![1]));
    clear_engine_handlers();
    assert_eq!(
        Arc::strong_count(&witness),
        2,
        "the worker's cache should still hold the cleared handler"
    );

    assert_refused(&run(&mut cache, OP_A, b"x"), OP_A);
    assert_eq!(
        Arc::strong_count(&witness),
        1,
        "the next dispatch must release the cleared handler"
    );

    // And at exit: the cache dropping releases what it holds.
    let held = Arc::clone(&witness);
    register_engine(OP_A, move |_ctx: &JobContext, _input: &[u8]| {
        let _ = &held;
        Ok(vec![1u8])
    })
    .must("register again");
    assert_eq!(run(&mut cache, OP_A, b"x"), Ok(vec![1]));
    clear_engine_handlers();
    assert_eq!(Arc::strong_count(&witness), 2);
    drop(cache);
    assert_eq!(
        Arc::strong_count(&witness),
        1,
        "a worker exiting must release its cache"
    );
}

/// No lock is held across the handler, so a handler may clear the registry it
/// was resolved from. The call in flight finishes; the next one is refused.
#[test]
fn a_handler_may_clear_the_registry_it_is_running_from() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    clear_engine_handlers();
    register_engine(OP_A, |_ctx: &JobContext, _input: &[u8]| {
        clear_engine_handlers();
        Ok(vec![9u8])
    })
    .must("register");

    let mut cache = EngineCache::default();
    assert_eq!(run(&mut cache, OP_A, b"x"), Ok(vec![9]));
    assert_refused(&run(&mut cache, OP_A, b"x"), OP_A);
}

/// `default_dispatch`, the uncached entry point, gives the same answers as the
/// cache for the same registry.
#[test]
fn default_dispatch_agrees_with_the_cache() {
    let _registry = lock_recover(&REGISTRY_TEST_LOCK);
    clear_engine_handlers();
    register_engine(OP_A, answer(5)).must("register");

    let direct = bytes_of(default_dispatch(&job(OP_A, 0), b"x"));
    assert_eq!(direct, Ok(vec![5]));
    clear_engine_handlers();
    assert_refused(&bytes_of(default_dispatch(&job(OP_A, 0), b"x")), OP_A);
}
