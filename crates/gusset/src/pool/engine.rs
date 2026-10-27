//! The engine registry and dispatch order (R9).

use super::diagnostic::{diagnostic_allocated, diagnostic_dispatch, DIAG_MODE_ALLOCATED};
use super::{IdMap, JobOutput};
use crate::header::{JobContext, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock};

/// Engine handler function type.
pub type EngineFn =
    Arc<dyn Fn(&JobContext, &[u8]) -> Result<JobOutput, String> + Send + Sync + 'static>;

static GLOBAL_ENGINE: RwLock<Option<EngineFn>> = RwLock::new(None);
static ENGINE_REGISTRY: RwLock<Option<IdMap<u32, EngineFn>>> = RwLock::new(None);

/// Counts every mutation of [`GLOBAL_ENGINE`] and [`ENGINE_REGISTRY`] (R7).
///
/// A worker keeps the engines it resolved in an [`EngineCache`] and trusts them
/// for as long as this still reads the value they were resolved under, so that a
/// job costs one load of this counter instead of a process-wide read lock and an
/// `Arc` clone that every worker bounces between cores. The rules that keep that
/// sound, each held by the code below:
///
/// - Every write to either registry bumps it, by [`publish_mutation`], while the
///   write guard is still held and after the write itself. A reader that sees
///   the new value therefore finds the new contents: the bump is a `Release`
///   store the reader's `Acquire` load pairs with, and the writer holds the lock
///   for both.
/// - A reader tags what it resolves with the value read *under the registry's
///   read lock*, never a value read before or after it, so an entry is never
///   labelled with a newer generation than the map it was copied from.
/// - It only ever grows, so a cache that reads a value equal to its own has seen
///   no mutation since it resolved.
static REGISTRY_GENERATION: AtomicU64 = AtomicU64::new(0);

/// Publishes a registry write. Called with the write guard still held, as the
/// last step of the mutation (see [`REGISTRY_GENERATION`]).
fn publish_mutation() {
    REGISTRY_GENERATION.fetch_add(1, Ordering::Release);
}

/// The current registry generation, for the tests that pin which calls bump it.
#[cfg(test)]
pub(super) fn registry_generation() -> u64 {
    REGISTRY_GENERATION.load(Ordering::Acquire)
}

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
/// A worker caches the handler it last ran, so the replaced one is released
/// when its last holder drops it: at once when no worker has run it, otherwise
/// when each worker that did next dispatches a job or exits. A job that
/// reaches a worker after this call returns runs the new handler.
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
    publish_mutation();
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
    publish_mutation();
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
///
/// Once this returns, a job that reaches a worker runs no cleared handler. A
/// job a worker is already running is not interrupted. The handlers' memory is
/// not necessarily released by the time this returns: a worker keeps the
/// handlers it has run in a cache and drops them when it next dispatches a job
/// (the cache notices the clear then) or exits, so a worker that stays idle
/// holds its last handler until then. `Close` joins the workers, so the
/// recovery recipe above (`Close`, clear, reinstall, `Open`) releases them.
///
/// The same delay moves where a panic in a handler's `Drop` surfaces. It no
/// longer unwinds into the caller of this function (or of `set_engine_handler`)
/// when a worker holds the last reference: it is a panic in the job that makes
/// the worker release the handler, which poisons that job's handle, or a
/// logged fault when the worker releases it at exit.
pub fn clear_engine_handlers() {
    let mut g = GLOBAL_ENGINE.write().unwrap_or_else(|e| e.into_inner());
    *g = None;
    let mut reg = ENGINE_REGISTRY.write().unwrap_or_else(|e| e.into_inner());
    *reg = None;
    // One bump with both guards held: a reader of either slot is blocked on its
    // lock until this returns, so none can pair a cleared slot with the old
    // generation.
    publish_mutation();
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

/// What a worker has resolved of the opcode 0 slot.
#[derive(Default)]
enum GlobalSlot {
    /// Not looked up since the cache was last reset.
    #[default]
    Unresolved,
    /// Looked up and empty: caching it keeps the fail-closed and diagnostic
    /// paths off the lock too.
    Absent,
    Handler(EngineFn),
}

/// The engines one worker has resolved, trusted while [`REGISTRY_GENERATION`]
/// still reads the value they were resolved under (R7).
///
/// Owned by the worker's thread and passed down by `&mut`, not kept in a
/// `thread_local!`: `clippy.toml` bans that macro for this crate, and a plain
/// local also dies with the worker, so a respawned worker starts empty and
/// nothing outlives the thread that built it.
///
/// Bounded by the registry: it holds at most the global handler and one entry
/// per registered opcode, and a miss for an opcode that is not registered is
/// not remembered, so a caller cycling through arbitrary opcodes cannot grow it.
#[derive(Default)]
pub(super) struct EngineCache {
    generation: u64,
    global: GlobalSlot,
    by_opcode: IdMap<u32, EngineFn>,
}

impl EngineCache {
    /// How many nonzero opcodes are cached, for the test that pins the bound.
    #[cfg(test)]
    pub(super) fn cached_opcodes(&self) -> usize {
        self.by_opcode.len()
    }

    /// Forgets everything resolved under another generation.
    ///
    /// The generation is assigned last. Dropping a cleared handler can run its
    /// `Drop`, and that can panic: unwinding out of here before the entries are
    /// gone must leave the old tag in place, so the next call refreshes again
    /// instead of trusting entries the registry no longer holds.
    fn adopt(&mut self, generation: u64) {
        if self.generation != generation {
            self.global = GlobalSlot::Unresolved;
            self.by_opcode.clear();
            self.generation = generation;
        }
    }

    fn is_current(&self) -> bool {
        self.generation == REGISTRY_GENERATION.load(Ordering::Acquire)
    }

    /// The handler registered for nonzero `opcode`: from the cache while no
    /// registry write has happened since it was resolved, else from the registry.
    fn registered(&mut self, opcode: u32) -> Option<&EngineFn> {
        if !(self.is_current() && self.by_opcode.contains_key(&opcode)) {
            let (found, generation) = {
                let reg = ENGINE_REGISTRY.read().unwrap_or_else(|e| e.into_inner());
                let generation = REGISTRY_GENERATION.load(Ordering::Acquire);
                (
                    reg.as_ref().and_then(|map| map.get(&opcode).cloned()),
                    generation,
                )
            };
            self.adopt(generation);
            if let Some(engine) = found {
                self.by_opcode.insert(opcode, engine);
            }
        }
        self.by_opcode.get(&opcode)
    }

    /// The opcode 0 handler, with the same cache rule as [`Self::registered`].
    fn global(&mut self) -> Option<&EngineFn> {
        if !(self.is_current() && !matches!(self.global, GlobalSlot::Unresolved)) {
            let (found, generation) = {
                let slot = GLOBAL_ENGINE.read().unwrap_or_else(|e| e.into_inner());
                (slot.clone(), REGISTRY_GENERATION.load(Ordering::Acquire))
            };
            self.adopt(generation);
            self.global = match found {
                Some(engine) => GlobalSlot::Handler(engine),
                None => GlobalSlot::Absent,
            };
        }
        match &self.global {
            GlobalSlot::Handler(engine) => Some(engine),
            GlobalSlot::Unresolved | GlobalSlot::Absent => None,
        }
    }

    /// [`default_dispatch`] through this cache: the same resolution order and
    /// the same refusals, with the registry consulted only when it has changed.
    pub(super) fn dispatch(&mut self, ctx: &JobContext, input: &[u8]) -> Result<JobOutput, String> {
        let opcode = ctx.opcode();
        if opcode != 0 {
            return match self.registered(opcode) {
                Some(engine) => engine(ctx, input),
                None => Err(format!(
                    "gusset: no engine handler registered for opcode {} (submission refused)",
                    opcode
                )),
            };
        }

        // 2. Opcode 0: Check global registration
        if let Some(engine) = self.global() {
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
}

/// Dispatches one work unit (R9), looking the engine up in the registries afresh.
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
///
/// Workers do not call this: each dispatches through its own `EngineCache`, which
/// returns what this returns and reads the registries only after a registration
/// or a clear. This is that lookup without a cache to carry between calls.
pub fn default_dispatch(ctx: &JobContext, input: &[u8]) -> Result<JobOutput, String> {
    EngineCache::default().dispatch(ctx, input)
}
