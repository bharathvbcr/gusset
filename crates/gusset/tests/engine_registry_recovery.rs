//! The process-global engine registry after a poisoned handle, and the routing
//! rules `docs/adoption.md` and the `WithOpcode` godoc state.
//!
//! Separate binary: these tests install a global (opcode 0) handler, which the
//! diagnostic-engine tests in the library's unit tests would run instead of the
//! diagnostic engine if they shared a process. The tests here share the registry
//! with each other, so `REGISTRY` serialises them.

#![allow(unsafe_code)]

use gusset::header::{CallHeader, JobContext, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::{
    clear_engine_handlers, has_engine_handler, register_engine, set_engine_handler, Handle,
    JobResult,
};
use std::sync::{Arc, Mutex};

#[path = "../../../tests/common/mod.rs"]
mod common;
use common::{make_pipe, read_ticket, Must};

/// Serialises the tests in this binary: the registry is one map for the process.
static REGISTRY: Mutex<()> = Mutex::new(());

fn registry_lock() -> std::sync::MutexGuard<'static, ()> {
    REGISTRY.lock().unwrap_or_else(|e| e.into_inner())
}

/// One handle with its own completion pipe, for tests that open several.
struct Session {
    handle: Arc<Handle>,
    read_fd: i32,
}

impl Session {
    fn open() -> Self {
        let (read_fd, write_fd) = make_pipe();
        Session {
            handle: Handle::open(1, write_fd).must("open"),
            read_fd,
        }
    }

    /// Submits `input` under `header` and waits for its result.
    fn run_with(&self, header: CallHeader, input: &[u8]) -> JobResult {
        let ticket = self.handle.submit(header, input, 0).must("submit");
        assert_eq!(read_ticket(self.read_fd), ticket);
        self.handle.take(ticket).must("take")
    }

    fn run(&self, opcode: u32, input: &[u8]) -> JobResult {
        let header = CallHeader {
            reserved: opcode,
            ..Default::default()
        };
        self.run_with(header, input)
    }

    fn close(self) {
        self.handle.close().must("close");
        // SAFETY: `read_fd` is the read end `make_pipe` returned for this
        // session, and nothing else closes it.
        unsafe {
            libc::close(self.read_fd);
        }
    }
}

/// An engine whose state lives behind a `Mutex`: the shape of an adopter that
/// keeps a cache, a connection or a parser table in its handler's closure.
///
/// The input `tear` panics while holding the guard, which poisons the mutex.
/// Every later call, on any handle, then panics at `lock` with the poison
/// error. That is the "torn state" a panic leaves in the process-global hook.
fn tearable(
    state: &Arc<Mutex<u32>>,
) -> impl Fn(&JobContext, &[u8]) -> Result<Vec<u8>, String> + Send + Sync + 'static {
    let state = Arc::clone(state);
    move |_ctx: &JobContext, input: &[u8]| {
        let mut tears = match state.lock() {
            Ok(guard) => guard,
            Err(poisoned) => panic!("engine state is torn: {poisoned}"),
        };
        if input == b"tear" {
            *tears += 1;
            panic!("engine state torn mid-update");
        }
        Ok(input.to_vec())
    }
}

fn is_panic(r: &JobResult) -> bool {
    matches!(r, JobResult::Panic { .. })
}

/// What a recovered handle runs after a poison, and what it takes to make it
/// run something sound. This is the recipe `docs/adoption.md` gives:
/// `Close` the poisoned handle, `clear_engine_handlers`, reinstall every
/// engine, `Open`.
///
/// Closing and reopening alone is not enough. The registry is process-global
/// and the panic leaves it installed, so the new handle runs the very closure
/// whose state the panic tore, and poisons on its first call. Reinstalling
/// without clearing is no better for an opcode entry: `register_engine`
/// refuses the duplicate and the torn entry stays.
#[test]
fn a_recovered_handle_runs_the_torn_engine_until_the_adopter_clears_and_reinstalls() {
    let _registry = registry_lock();
    const OPCODE: u32 = 9304;
    clear_engine_handlers();

    // One engine state behind both the global handler (opcode 0) and an
    // opcode entry, as an umbrella sharing one engine between them would.
    let torn_state = Arc::new(Mutex::new(0u32));
    set_engine_handler(tearable(&torn_state));
    register_engine(OPCODE, tearable(&torn_state)).must("register");

    // A healthy engine first: nothing here is torn yet.
    let first = Session::open();
    match first.run(OPCODE, b"fine") {
        JobResult::Ok(bytes) => assert_eq!(bytes, b"fine"),
        other => panic!("the engine failed before any panic: {other:?}"),
    }

    // The panic poisons the handle and the engine's own mutex.
    assert!(is_panic(&first.run(OPCODE, b"tear")), "tear must panic");
    assert!(first.handle.is_poisoned());
    assert!(
        torn_state.is_poisoned(),
        "the premise: the panic tore the engine's state"
    );
    first.close();

    // Close + Open alone: the recovered handle runs the torn closure and is
    // poisoned by its first job, on the opcode entry and on the global slot.
    let recovered = Session::open();
    assert!(
        is_panic(&recovered.run(OPCODE, b"fine")),
        "a handle opened after the poison must still run the torn opcode engine"
    );
    assert!(recovered.handle.is_poisoned());
    recovered.close();
    let recovered_global = Session::open();
    assert!(
        is_panic(&recovered_global.run(0, b"fine")),
        "a handle opened after the poison must still run the torn global engine"
    );
    assert!(recovered_global.handle.is_poisoned());
    recovered_global.close();

    // Reinstalling an opcode without clearing is refused and changes nothing.
    match register_engine(OPCODE, tearable(&Arc::new(Mutex::new(0u32)))) {
        Ok(()) => panic!("a reinstall without clear_engine_handlers replaced the torn entry"),
        Err(e) => assert!(e.contains("already registered"), "got: {e}"),
    }
    let still_torn = Session::open();
    assert!(
        is_panic(&still_torn.run(OPCODE, b"fine")),
        "the refused reinstall must leave the torn entry running"
    );
    still_torn.close();

    // The recipe: clear, then nothing runs until the adopter installs again.
    clear_engine_handlers();
    assert!(!has_engine_handler(), "clear must drop every engine");
    let cleared = Session::open();
    for opcode in [0, OPCODE] {
        match cleared.run(opcode, b"fine") {
            JobResult::Err(msg) => assert!(
                msg.contains("no engine handler registered"),
                "opcode {opcode} after clear, got: {msg}"
            ),
            other => panic!("opcode {opcode} ran after clear: {other:?}"),
        }
    }

    // Reinstall with fresh state. The handle opened before the reinstall runs
    // it too: dispatch resolves the engine per job, not at Open.
    let fresh_state = Arc::new(Mutex::new(0u32));
    set_engine_handler(tearable(&fresh_state));
    register_engine(OPCODE, tearable(&fresh_state)).must("reinstall");
    for opcode in [0, OPCODE] {
        match cleared.run(opcode, b"healthy") {
            JobResult::Ok(bytes) => assert_eq!(bytes, b"healthy", "opcode {opcode}"),
            other => panic!("opcode {opcode} after clear and reinstall: {other:?}"),
        }
    }
    assert!(!cleared.handle.is_poisoned());
    cleared.close();
    clear_engine_handlers();
}

/// `set_engine_handler` replaces the global slot: the last install wins, with
/// no error. `register_engine` is the call that refuses a duplicate, and the
/// two docs say so.
#[test]
fn set_engine_handler_replaces_the_global_slot_and_the_last_install_wins() {
    let _registry = registry_lock();
    clear_engine_handlers();
    set_engine_handler(|_ctx: &JobContext, _input: &[u8]| Ok(b"first".to_vec()));
    set_engine_handler(|_ctx: &JobContext, _input: &[u8]| Ok(b"second".to_vec()));

    let session = Session::open();
    match session.run(0, b"x") {
        JobResult::Ok(bytes) => assert_eq!(bytes, b"second", "the second install must win"),
        other => panic!("opcode 0 did not run an installed handler: {other:?}"),
    }
    session.close();
    clear_engine_handlers();
}

/// The global handler serves opcode 0 only. A nonzero opcode with no registry
/// entry is refused, even with a global handler installed and even when the
/// caller sets the diagnostic-engine flag: forgetting to register opcode 100
/// must not run the engine installed for opcode 0.
#[test]
fn a_nonzero_opcode_with_only_a_global_handler_is_refused() {
    let _registry = registry_lock();
    const OPCODE: u32 = 9305;
    clear_engine_handlers();
    set_engine_handler(|_ctx: &JobContext, _input: &[u8]| Ok(b"global".to_vec()));

    let session = Session::open();
    // Control: the handler is installed and serves opcode 0.
    match session.run(0, b"x") {
        JobResult::Ok(bytes) => assert_eq!(bytes, b"global"),
        other => panic!("opcode 0 did not run the global handler: {other:?}"),
    }

    let expected = format!("no engine handler registered for opcode {OPCODE}");
    match session.run(OPCODE, b"x") {
        JobResult::Err(msg) => assert!(msg.contains(&expected), "got: {msg}"),
        JobResult::Ok(bytes) => panic!(
            "a nonzero opcode ran the global handler: {:?}",
            String::from_utf8_lossy(&bytes)
        ),
        other => panic!("unexpected result: {other:?}"),
    }

    let diagnostic = CallHeader {
        reserved: OPCODE,
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    match session.run_with(diagnostic, &[0, 7]) {
        JobResult::Err(msg) => assert!(msg.contains(&expected), "got: {msg}"),
        other => panic!("the diagnostic flag reached the engine for a nonzero opcode: {other:?}"),
    }
    assert!(!session.handle.is_poisoned());
    session.close();
    clear_engine_handlers();
}
