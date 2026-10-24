//! The engine registry and dispatch order (R9).

use super::diagnostic::{diagnostic_allocated, diagnostic_dispatch, DIAG_MODE_ALLOCATED};
use super::{IdMap, JobOutput};
use crate::header::{JobContext, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use std::sync::{Arc, RwLock};

/// Engine handler function type.
pub type EngineFn =
    Arc<dyn Fn(&JobContext, &[u8]) -> Result<JobOutput, String> + Send + Sync + 'static>;

static GLOBAL_ENGINE: RwLock<Option<EngineFn>> = RwLock::new(None);
static ENGINE_REGISTRY: RwLock<Option<IdMap<u32, EngineFn>>> = RwLock::new(None);

/// Sets the global engine execution handler: the one engine opcode 0 runs.
///
/// **This replaces the slot.** The global handler has one occupant, so a second
/// call drops the first handler and the last install wins, with no error and no
/// signal to the caller (`DECISIONS.md`, 2026-09-30, "`register_engine` refuses
/// opcode 0 and a collision"). Two crates linked into one umbrella that each
/// call `set_engine_handler` from their own init cannot both be opcode 0: only
/// whichever init ran last serves it. Give each engine its own nonzero opcode
/// with [`register_engine`] instead; that call refuses a duplicate.
///
/// The handler serves opcode 0 only. A job whose `ctx.opcode()` is nonzero is
/// looked up in the opcode registry and never reaches this handler; with no
/// entry there it is refused (see [`default_dispatch`]).
///
/// The handler is process-global, shared by every handle. A panic in it poisons
/// the handle that ran it and leaves it installed, so a handle opened after
/// recovery runs the same closure with whatever state the panic tore. Recovery
/// is `Close`, [`clear_engine_handlers`], reinstall, `Open`; `docs/adoption.md`
/// has the recipe.
///
/// Lock poisoning is recovered from rather than swallowed: a previous panic while
/// the registry was held must not leave the process permanently unable to register
/// an engine, and it must never be reported to the caller as a successful install.
pub fn set_engine_handler<F, R>(f: F)
where
    F: Fn(&JobContext, &[u8]) -> Result<R, String> + Send + Sync + 'static,
    R: Into<JobOutput> + 'static,
{
    let mut w = GLOBAL_ENGINE.write().unwrap_or_else(|e| e.into_inner());
    *w = Some(Arc::new(move |ctx, input| f(ctx, input).map(Into::into)));
}

/// Registers an engine execution handler for a specific opcode (R9).
///
/// Dispatches calls matching `ctx.opcode() == opcode` directly to this handler.
/// A handle's opcode comes from `WithOpcode`, or from `ContextWithOpcode` for one
/// call. The global handler never serves a nonzero opcode, so every opcode a
/// handle uses needs its own registration.
///
/// Opcode 0 is refused. Dispatch never consults the registry for it: that
/// opcode is the global handler installed by [`set_engine_handler`], then the
/// diagnostic engine. Inserting opcode 0 here would report a registration
/// that no submission can reach.
///
/// A second registration of the same opcode is refused and the first handler
/// stays. Call [`clear_engine_handlers`] before replacing one. The registry
/// is process-global, shared by every handle, and a panic in a handler leaves
/// its entry in place: reinstalling after a poison without clearing first is
/// refused as a duplicate and the torn handler keeps running.
pub fn register_engine<F, R>(opcode: u32, f: F) -> Result<(), String>
where
    F: Fn(&JobContext, &[u8]) -> Result<R, String> + Send + Sync + 'static,
    R: Into<JobOutput> + 'static,
{
    if opcode == 0 {
        return Err(
            "opcode 0 is the global engine; call set_engine_handler, not register_engine \
             (a registry entry for opcode 0 is never dispatched)"
                .to_string(),
        );
    }
    let mut w = ENGINE_REGISTRY.write().unwrap_or_else(|e| e.into_inner());
    let map = w.get_or_insert_with(IdMap::default);
    if map.contains_key(&opcode) {
        return Err(format!(
            "opcode {opcode} is already registered; call clear_engine_handlers before \
             installing a replacement (the existing handler was left in place)"
        ));
    }
    map.insert(
        opcode,
        Arc::new(move |ctx, input| f(ctx, input).map(Into::into)),
    );
    Ok(())
}

/// Clears all registered engine handlers: the global one and every opcode entry.
///
/// This is the first step of recovering from a poisoned handle whose engine
/// state was torn by the panic. It is not atomic with the reinstall that must
/// follow: a job another handle submits in between is refused with `no engine
/// handler registered` (an error result, not a crash). It clears every opcode,
/// not the one that panicked, so the adopter reinstalls all of them. Tests use
/// it too.
pub fn clear_engine_handlers() {
    let mut g = GLOBAL_ENGINE.write().unwrap_or_else(|e| e.into_inner());
    *g = None;
    let mut reg = ENGINE_REGISTRY.write().unwrap_or_else(|e| e.into_inner());
    *reg = None;
}

/// Reports whether an adopter engine handler is currently registered.
pub fn has_engine_handler() -> bool {
    let global_has = GLOBAL_ENGINE
        .read()
        .unwrap_or_else(|e| e.into_inner())
        .is_some();
    if global_has {
        return true;
    }
    let reg = ENGINE_REGISTRY.read().unwrap_or_else(|e| e.into_inner());
    match *reg {
        Some(ref m) => !m.is_empty(),
        None => false,
    }
}

/// Dispatches one work unit (R9).
///
/// Resolution order, and why it is this order. It depends on `ctx.opcode()`.
///
/// A nonzero opcode:
///
/// 1. The opcode registry entry for exactly that opcode runs.
/// 2. With no entry the job is refused with `no engine handler registered for
///    opcode N`. It never falls through to the global handler or to the
///    diagnostic engine, even when `GUSSET_FLAG_DIAGNOSTIC_ENGINE` is set: a
///    handle configured for opcode 100 must not run an engine installed for
///    opcode 0 because the registration of 100 was forgotten.
///
/// Opcode 0 never consults the registry:
///
/// 1. A global handler installed with [`set_engine_handler`] runs.
///    `GUSSET_FLAG_DIAGNOSTIC_ENGINE` cannot displace it, so a stray flag can
///    never silently swap a production engine for the diagnostic one.
/// 2. Otherwise, if the *caller* set `GUSSET_FLAG_DIAGNOSTIC_ENGINE`, the
///    built-in diagnostic engine runs. Only Gusset's own tests set that bit.
/// 3. Otherwise fail closed. Running an implicit echo-or-panic engine because
///    registration was forgotten, or lost a startup race, is how a payload's
///    first byte comes to select `panic!` in a production process.
pub fn default_dispatch(ctx: &JobContext, input: &[u8]) -> Result<JobOutput, String> {
    let opcode = ctx.opcode();
    if opcode != 0 {
        let engine = {
            let reg = ENGINE_REGISTRY.read().unwrap_or_else(|e| e.into_inner());
            reg.as_ref().and_then(|map| map.get(&opcode).cloned())
        };
        if let Some(engine) = engine {
            return engine(ctx, input);
        }
        return Err(format!(
            "gusset: no engine handler registered for opcode {} (submission refused)",
            opcode
        ));
    }

    // 2. Opcode 0: Check global registration
    let global_engine = {
        let r = GLOBAL_ENGINE.read().unwrap_or_else(|e| e.into_inner());
        r.clone()
    };
    if let Some(engine) = global_engine {
        return engine(ctx, input);
    }

    // 3. Diagnostic engine fallback (only for opcode 0)
    if ctx.header().flags & GUSSET_FLAG_DIAGNOSTIC_ENGINE != 0 {
        if input.first() == Some(&DIAG_MODE_ALLOCATED) {
            return diagnostic_allocated(ctx, input);
        }
        return diagnostic_dispatch(ctx, input).map(JobOutput::Bytes);
    }

    Err(
        "gusset: no engine handler registered; call gusset::set_engine_handler() before \
         submitting work (submission refused rather than run against a built-in engine)"
            .to_string(),
    )
}
