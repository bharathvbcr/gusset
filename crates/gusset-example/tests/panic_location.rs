//! A panic's reported location must be its own, never an earlier panic's.
//!
//! The hook records locations keyed by thread id. An engine that catches a
//! panic internally leaves an entry behind; a later panic propagated with
//! `resume_unwind` (rayon, cross-thread joins) never runs the hook, so the
//! worker reported the *handled* panic's file and line as the new failure's.
//!
//! Own binary: registers engines, which are process-global.

use gusset::header::CallHeader;
use gusset::pool::{register_engine, Handle, JobResult};

mod common;
use common::{make_pipe, read_ticket};

const OP_HANDLED: u32 = 0x7A01;
const OP_RESUMED: u32 = 0x7A02;

#[test]
fn a_resumed_panic_does_not_inherit_a_handled_panics_location() {
    register_engine(OP_HANDLED, |_ctx, _input: &[u8]| {
        // Panics and recovers internally: the hook records this line.
        let _ = std::panic::catch_unwind(|| panic!("handled inside the engine"));
        Ok(Vec::new())
    });
    register_engine(
        OP_RESUMED,
        |_ctx, _input: &[u8]| -> Result<Vec<u8>, String> {
            // Propagated without running the hook.
            std::panic::resume_unwind(Box::new("real failure"))
        },
    );

    let (r, w) = make_pipe();
    let h = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open: {e}"),
    };
    for op in [OP_HANDLED, OP_RESUMED] {
        let t = match h.submit(
            CallHeader {
                reserved: op,
                ..Default::default()
            },
            &[],
            0,
        ) {
            Ok(t) => t,
            Err(e) => panic!("submit: {e}"),
        };
        assert_eq!(read_ticket(r), t);
        let res = match h.take(t) {
            Ok(res) => res,
            Err(e) => panic!("take: {e}"),
        };
        if op == OP_RESUMED {
            match res {
                JobResult::Panic { msg, file, line } => {
                    assert!(msg.contains("real failure"), "msg: {msg}");
                    assert!(
                        file.is_none(),
                        "reported {file:?}:{line}, the location of the earlier handled panic"
                    );
                }
                other => panic!("expected a panic result, got {other:?}"),
            }
        }
    }
    h.close();
    unsafe { libc::close(r) };
}
