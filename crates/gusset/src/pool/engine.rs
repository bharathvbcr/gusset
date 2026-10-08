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

/// Sets the global engine execution handler.
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
///
/// Opcode 0 is refused. Dispatch never consults the registry for it: that
/// opcode is the global handler installed by [`set_engine_handler`], then the
/// diagnostic engine. Inserting opcode 0 here would report a registration
/// that no submission can reach.
///
/// A second registration of the same opcode is refused and the first handler
/// stays. Call [`clear_engine_handlers`] before replacing one. The registry
/// is process-global, shared by every handle.
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

/// Clears all registered engine handlers (global and opcode-specific). Diagnostic/test use.
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
/// Resolution order, and why it is this order:
///
/// 1. An opcode-specific registered engine handler matching `ctx.opcode()` wins.
/// 2. A registered adopter global engine wins next. `GUSSET_FLAG_DIAGNOSTIC_ENGINE`
///    cannot displace it, so a stray flag can never silently swap a production
///    engine for the diagnostic one.
/// 3. Otherwise, if the *caller* set `GUSSET_FLAG_DIAGNOSTIC_ENGINE`, run the
///    built-in diagnostic engine. Only Gusset's own tests set that bit.
/// 4. Otherwise fail closed. Running an implicit echo-or-panic engine because
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
