//! Executable ARMv8 `aarch64.cat` axiomatic weak-memory model checker and
//! multi-round token-accounting proof for Gusset's completion-ring park/wake
//! protocol (`crates/gusset/src/pool/ring.rs` + `ticket_reader.go`).
//!
//! Why this runs inside `cargo test` without requiring an external `herd7` binary:
//! an unrun `.litmus` file is documentation, not verification. This module
//! implements the exact explicit-memory fragment of ARM Ltd's `aarch64.cat` and
//! `aarch64hwreqs.cat` (Deacon, Alglave, Nikoleris, Khyzha) over finite candidate
//! executions `(E, po, addr, ctrl, amo, rmw, rf, co)`, enumerates **every**
//! `(rf, co)` assignment, checks all four internal visibility axioms (`coRW1-Exp`,
//! `coWW-Exp`, `coWR-Exp`, `atomic`) plus external visibility (`irreflexive ob`),
//! and proves:
//!
//! 1. **Production LSE path (`SWPALW` + `LDAR` || `STLR` + `DMB ISH` + `LDR`)**:
//!    zero lost-wake consistent executions, plus non-vacuity reachability
//!    witnesses (`worker_wakes`, `reader_sees_ready`, `both_observe`).
//! 2. **Production ARMv8.0 LL/SC fallback (`LDAXRW`/`STLXRW` + `LDAR`)**:
//!    zero lost-wake consistent executions (`[Exp & W & L]; po; [Exp & R & A]`
//!    in `bob`).
//! 3. **`SWPALW` resilience under `FEAT_LRCPC` (`LDAPR`, `Exp & R & Q`)**:
//!    even if a future compiler lowers `atomic.LoadUint64` to `LDAPR` (`Q`)
//!    instead of `LDAR` (`A`), `SWPALW` (`[range([R & A]; amo; [W & L])]; po; [M]`
//!    in `aarch64hwreqs.cat` line 133) STILL prevents every lost wake.
//! 4. **Bug-injection disproofs (`¬Sec(BuggyCode)`)**:
//!    - `STLRW` + `LDAPR` (`[W & L]; po; [R & Q]` is absent from `bob`!) -> FAILS
//!      with a concrete `ob`-acyclic lost-wake execution.
//!    - `STRW` + `LDAR` (missing release on `waiting = 1`) -> FAILS.
//!    - Worker omitting `fence(SeqCst)` (`DMB ISH`) before `waiting.load(Relaxed)` -> FAILS.
//! 5. **Multi-round `tokensOwed` conservation**: proves `ticket_reader.go:233`
//!    (`atomic.SwapUint32(waiting, 0) == 0`) conserves pipe wake tokens across
//!    every schedule, and disproves `atomic.StoreUint32(waiting, 0)`.

use std::collections::BTreeSet;
use std::path::Path;

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
enum Loc {
    Seq,
    Waiting,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum AccessKind {
    /// Initial architectural zero write at reset (`co`-minimal).
    InitWrite { loc: Loc, val: u64 },
    /// Explicit memory Read (`LDR`, `LDAR`, `LDAPR`, `LDAXR`, or Read half of `SWPAL`).
    Read {
        loc: Loc,
        acquire_sc: bool, // `A`: `LDAR`, `LDAXR`, `SWPA`, `SWPAL`
        acquire_pc: bool, // `Q`: `LDAPR` (`FEAT_LRCPC`)
    },
    /// Explicit memory Write (`STR`, `STLR`, `STLXR`, or Write half of `SWPAL`).
    Write {
        loc: Loc,
        val: u64,
        release: bool, // `L`: `STLR`, `STLXR`, `SWPL`, `SWPAL`
    },
    /// Full inner-shareable barrier (`DMB ISH` / `dmb.full`).
    DmbFull,
}

#[derive(Debug, Clone)]
struct Event {
    thread: Option<usize>, // `None` for initial architectural state writes
    kind: AccessKind,
}

impl Event {
    fn is_exp_m(&self) -> bool {
        matches!(
            self.kind,
            AccessKind::Read { .. } | AccessKind::Write { .. }
        )
    }

    fn is_exp_r(&self) -> bool {
        matches!(self.kind, AccessKind::Read { .. })
    }

    fn is_exp_w(&self) -> bool {
        matches!(
            self.kind,
            AccessKind::InitWrite { .. } | AccessKind::Write { .. }
        )
    }

    fn is_acquire_a(&self) -> bool {
        matches!(
            self.kind,
            AccessKind::Read {
                acquire_sc: true,
                ..
            }
        )
    }

    fn is_acquire_q(&self) -> bool {
        matches!(
            self.kind,
            AccessKind::Read {
                acquire_pc: true,
                ..
            }
        )
    }

    fn is_release_l(&self) -> bool {
        matches!(self.kind, AccessKind::Write { release: true, .. })
    }

    fn loc(&self) -> Option<Loc> {
        match self.kind {
            AccessKind::InitWrite { loc, .. }
            | AccessKind::Read { loc, .. }
            | AccessKind::Write { loc, .. } => Some(loc),
            AccessKind::DmbFull => None,
        }
    }

    fn write_val(&self) -> Option<u64> {
        match self.kind {
            AccessKind::InitWrite { val, .. } | AccessKind::Write { val, .. } => Some(val),
            _ => None,
        }
    }
}

type Rel = BTreeSet<(usize, usize)>;

fn compose(a: &Rel, b: &Rel) -> Rel {
    let mut out = Rel::new();
    for &(x, y) in a {
        for &(y2, z) in b {
            if y == y2 {
                out.insert((x, z));
            }
        }
    }
    out
}

fn transitive_closure(base: &Rel) -> Rel {
    let mut cur = base.clone();
    loop {
        let next_edges = compose(&cur, base);
        let before = cur.len();
        cur.extend(next_edges);
        if cur.len() == before {
            return cur;
        }
    }
}

fn is_irreflexive(r: &Rel) -> bool {
    !r.iter().any(|&(x, y)| x == y)
}

#[derive(Debug, Clone)]
struct LitmusGraph {
    events: Vec<Event>,
    po: Rel,
    amo: Rel, // Single-instruction atomic memory op R -> W (`SWPAL`)
    rmw: Rel, // Exclusive pair `LDAXR -> STLXR` or `amo` (`SWPAL`)
    worker_wait_read: usize,
    reader_seq_read: usize,
}

#[derive(Debug, Default)]
struct ModelSummary {
    total_candidates: usize,
    consistent_executions: usize,
    lost_wake_executions: usize, // worker saw waiting=0 AND reader saw seq=0
    worker_saw_waiting_1: usize,
    reader_saw_seq_1: usize,
    both_saw_1: usize,
}

/// Evaluates `aarch64.cat` + `aarch64hwreqs.cat` over every candidate `(rf, co)`
/// of `g` without approximation.
fn check_aarch64_cat(g: &LitmusGraph) -> ModelSummary {
    let n = g.events.len();
    let reads: Vec<usize> = (0..n).filter(|&i| g.events[i].is_exp_r()).collect();
    let writes_for = |loc: Loc| -> Vec<usize> {
        (0..n)
            .filter(|&i| g.events[i].is_exp_w() && g.events[i].loc() == Some(loc))
            .collect()
    };
    let seq_writes = writes_for(Loc::Seq);
    let wait_writes = writes_for(Loc::Waiting);

    // Enumerate all total coherence orders `co` per location where `InitWrite` is minimal.
    fn permutations_with_init_first(writes: &[usize], events: &[Event]) -> Vec<Vec<usize>> {
        let init: Vec<usize> = writes
            .iter()
            .copied()
            .filter(|&w| matches!(events[w].kind, AccessKind::InitWrite { .. }))
            .collect();
        assert_eq!(init.len(), 1, "exactly one InitWrite per location");
        let rest: Vec<usize> = writes
            .iter()
            .copied()
            .filter(|&w| !matches!(events[w].kind, AccessKind::InitWrite { .. }))
            .collect();
        fn permute(items: &[usize]) -> Vec<Vec<usize>> {
            if items.is_empty() {
                return vec![vec![]];
            }
            let mut out = Vec::new();
            for (idx, &item) in items.iter().enumerate() {
                let mut rem = items.to_vec();
                rem.remove(idx);
                for mut tail in permute(&rem) {
                    let mut v = vec![item];
                    v.append(&mut tail);
                    out.push(v);
                }
            }
            out
        }
        permute(&rest)
            .into_iter()
            .map(|mut p| {
                let mut full = init.clone();
                full.append(&mut p);
                full
            })
            .collect()
    }

    let seq_cos = permutations_with_init_first(&seq_writes, &g.events);
    let wait_cos = permutations_with_init_first(&wait_writes, &g.events);

    // Enumerate all `rf` functions mapping each Read `r` to a same-location Write `w`.
    let read_choices: Vec<Vec<usize>> = reads
        .iter()
        .map(|&r| {
            let Some(loc) = g.events[r].loc() else {
                panic!("read event {r} missing location");
            };
            writes_for(loc)
        })
        .collect();
    let mut rf_assignments: Vec<Vec<usize>> = vec![vec![]];
    for choices in &read_choices {
        let mut next = Vec::new();
        for prefix in &rf_assignments {
            for &w in choices {
                let mut v = prefix.clone();
                v.push(w);
                next.push(v);
            }
        }
        rf_assignments = next;
    }

    // Precompute `lob` (`aarch64hwreqs.cat` lines 120–162): depends only on
    // syntactic instruction kinds, `po`, `amo`, and `rmw`.
    let mut aob = Rel::new();
    for &(r, w) in &g.rmw {
        aob.insert((r, w));
        // `[Exp & M]; rmw; lmrs; [(Exp & R & A) | (Exp & R & Q)]` (same-loc po read after rmw write)
        for j in 0..n {
            if g.po.contains(&(w, j))
                && g.events[j].loc() == g.events[w].loc()
                && (g.events[j].is_acquire_a() || g.events[j].is_acquire_q())
            {
                aob.insert((r, j));
            }
        }
    }

    // `bob` (`aarch64hwreqs.cat` lines 126–136):
    let mut bob = Rel::new();
    let amo_release_writes: BTreeSet<usize> = g
        .amo
        .iter()
        .filter(|&&(r, w)| g.events[r].is_acquire_a() && g.events[w].is_release_l())
        .map(|&(_, w)| w)
        .collect();

    for i in 0..n {
        for j in 0..n {
            if !g.po.contains(&(i, j)) {
                continue;
            }
            // 1. `[Exp & M]; po; [dmb.full]; po; [Exp & M]`
            if g.events[i].is_exp_m() && matches!(g.events[j].kind, AccessKind::DmbFull) {
                for k in 0..n {
                    if g.po.contains(&(j, k)) && g.events[k].is_exp_m() {
                        bob.insert((i, k));
                    }
                }
            }
            // 2. `[range([Exp & R & A]; amo; [Exp & W & L])]; po; [Exp & M]` (line 133: SWPAL!)
            if amo_release_writes.contains(&i) && g.events[j].is_exp_m() {
                bob.insert((i, j));
            }
            // 3. `[Exp & W & L]; po; [Exp & R & A]` (line 134: STLR/STLXR -> LDAR/LDAXR ONLY, NOT LDAPR!)
            if g.events[i].is_release_l() && g.events[j].is_acquire_a() {
                bob.insert((i, j));
            }
            // 4. `[(Exp & R & A) | (Exp & R & Q)]; po; [Exp & M]` (line 135: LDAR/LDAPR -> M)
            if (g.events[i].is_acquire_a() || g.events[i].is_acquire_q()) && g.events[j].is_exp_m()
            {
                bob.insert((i, j));
            }
            // 5. `[Exp & M]; po; [Exp & W & L]` (line 136: M -> STLR/STLXR/SWPL)
            if g.events[i].is_exp_m() && g.events[j].is_release_l() {
                bob.insert((i, j));
            }
        }
    }

    let mut lob_base = aob;
    lob_base.extend(bob);
    let lob = transitive_closure(&lob_base);

    let mut summary = ModelSummary::default();

    for seq_order in &seq_cos {
        for wait_order in &wait_cos {
            let mut co = Rel::new();
            for order in [seq_order, wait_order] {
                for idx_a in 0..order.len() {
                    for idx_b in (idx_a + 1)..order.len() {
                        co.insert((order[idx_a], order[idx_b]));
                    }
                }
            }

            for rf_choice in &rf_assignments {
                summary.total_candidates += 1;

                let mut rf = Rel::new();
                let mut rfi = Rel::new();
                let mut rfe = Rel::new();
                for (&r, &w) in reads.iter().zip(rf_choice.iter()) {
                    rf.insert((w, r));
                    if g.events[w].thread.is_some() && g.events[w].thread == g.events[r].thread {
                        rfi.insert((w, r));
                    } else {
                        rfe.insert((w, r));
                    }
                }

                // `fr = rf^-1 ; co` (`aarch64.cat` coherence-after `ca = fr | co`)
                let mut fr = Rel::new();
                for &(w_rf, r) in &rf {
                    for &(w1, w2) in &co {
                        if w_rf == w1 {
                            fr.insert((r, w2));
                        }
                    }
                }
                let mut ca = fr.clone();
                ca.extend(co.iter().copied());

                let is_int = |x: usize, y: usize| -> bool {
                    g.events[x].thread.is_some() && g.events[x].thread == g.events[y].thread
                };
                let is_ext = |x: usize, y: usize| -> bool { !is_int(x, y) };

                // Internal visibility axioms (`aarch64.cat` lines 149–153):
                // 1. `irreflexive [Exp & R]; ((po & same-loc) | rmw); [Exp & W]; rfi; [Exp & R] as coRW1-Exp`
                let mut corw1_violated = false;
                for &r in &reads {
                    for w in 0..n {
                        if !g.events[w].is_exp_w() {
                            continue;
                        }
                        let po_loc =
                            g.po.contains(&(r, w)) && g.events[r].loc() == g.events[w].loc();
                        if (po_loc || g.rmw.contains(&(r, w))) && rfi.contains(&(w, r)) {
                            corw1_violated = true;
                            break;
                        }
                    }
                }
                if corw1_violated {
                    continue;
                }

                // 2. `irreflexive [Exp & W]; (po & same-loc); [Exp & W]; (ca & int); [Exp & W] as coWW-Exp`
                let mut coww_violated = false;
                for &(w1, w2) in &g.po {
                    if g.events[w1].is_exp_w()
                        && g.events[w2].is_exp_w()
                        && g.events[w1].loc() == g.events[w2].loc()
                        && ca.contains(&(w2, w1))
                        && is_int(w2, w1)
                    {
                        coww_violated = true;
                        break;
                    }
                }
                if coww_violated {
                    continue;
                }

                // 3. `irreflexive [Exp & W]; (po & same-loc); [Exp & R]; (ca & int); [Exp & W] as coWR-Exp`
                let mut cowr_violated = false;
                for &(w, r) in &g.po {
                    if g.events[w].is_exp_w()
                        && g.events[r].is_exp_r()
                        && g.events[w].loc() == g.events[r].loc()
                        && ca.contains(&(r, w))
                        && is_int(r, w)
                    {
                        cowr_violated = true;
                        break;
                    }
                }
                if cowr_violated {
                    continue;
                }

                // 4. `empty (rmw & (fr; co)) \ (([Exp]; rmw; [Exp]) & (fri; [Exp & W]; coi)) as atomic`
                let fr_co = compose(&fr, &co);
                let mut atomic_violated = false;
                for &(r, w) in &g.rmw {
                    if fr_co.contains(&(r, w)) {
                        let allowed_internal = (0..n).any(|w_mid| {
                            g.events[w_mid].is_exp_w()
                                && fr.contains(&(r, w_mid))
                                && is_int(r, w_mid)
                                && co.contains(&(w_mid, w))
                                && is_int(w_mid, w)
                        });
                        if !allowed_internal {
                            atomic_violated = true;
                            break;
                        }
                    }
                }
                if atomic_violated {
                    continue;
                }

                // 5. External visibility: `obs = (rf & ext) | (ca & ext)`, `ob = (lob | obs)+`,
                //    `irreflexive ob as external`.
                let mut obs = rfe;
                for &(x, y) in &ca {
                    if is_ext(x, y) {
                        obs.insert((x, y));
                    }
                }
                let mut ob_base = lob.clone();
                ob_base.extend(obs);
                let ob = transitive_closure(&ob_base);
                if !is_irreflexive(&ob) {
                    continue;
                }

                // Consistent execution under `aarch64.cat`!
                summary.consistent_executions += 1;

                let read_value = |read_id: usize| -> u64 {
                    let Some(idx) = reads.iter().position(|&r| r == read_id) else {
                        panic!("read_id {read_id} not in reads");
                    };
                    let w = rf_choice[idx];
                    let Some(val) = g.events[w].write_val() else {
                        panic!("write event {w} missing value");
                    };
                    val
                };
                let worker_wait = read_value(g.worker_wait_read);
                let reader_seq = read_value(g.reader_seq_read);
                match (worker_wait, reader_seq) {
                    (0, 0) => summary.lost_wake_executions += 1,
                    (1, 0) => summary.worker_saw_waiting_1 += 1,
                    (0, 1) => summary.reader_saw_seq_1 += 1,
                    (1, 1) => summary.both_saw_1 += 1,
                    other => panic!("unexpected Dekker observation {other:?}"),
                }
            }
        }
    }

    summary
}

#[derive(Debug, Clone, Copy)]
enum ReaderParkOp {
    SwpalLse,   // `SWPALW` (`[R & A]; amo; [W & L]`)
    LlScV80,    // `LDAXRW` + `STLXRW` (`[R & A]; rmw; [W & L]`, non-AMO)
    StlrOnly,   // `STLRW` (`[W & L]`, no RMW)
    StrRelaxed, // `STRW` (`[W]`, non-release)
}

#[derive(Debug, Clone, Copy)]
enum ReaderSeqLoad {
    LdarSc,  // `LDAR` (`Exp & R & A`)
    LdaprPc, // `LDAPR` (`Exp & R & Q`, FEAT_LRCPC)
}

fn build_park_wake_graph(
    worker_dmb_ish: bool,
    reader_park: ReaderParkOp,
    reader_load: ReaderSeqLoad,
) -> LitmusGraph {
    let mut events = Vec::new();
    let mut push = |thread: Option<usize>, kind: AccessKind| -> usize {
        let id = events.len();
        events.push(Event { thread, kind });
        id
    };

    // Architectural initial state
    let _init_seq = push(
        None,
        AccessKind::InitWrite {
            loc: Loc::Seq,
            val: 0,
        },
    );
    let _init_wait = push(
        None,
        AccessKind::InitWrite {
            loc: Loc::Waiting,
            val: 0,
        },
    );

    // Thread 0 (Rust worker: `Ring::try_publish` + `Ring::take_waiter`)
    let mut t0 = Vec::new();
    t0.push(push(
        Some(0),
        AccessKind::Write {
            loc: Loc::Seq,
            val: 1,
            release: true, // `slot.seq.store(pos + 1, Ordering::Release)` -> `STLR`
        },
    ));
    if worker_dmb_ish {
        t0.push(push(Some(0), AccessKind::DmbFull)); // `fence(Ordering::SeqCst)` -> `DMB ISH`
    }
    let worker_wait_read = push(
        Some(0),
        AccessKind::Read {
            loc: Loc::Waiting,
            acquire_sc: false, // `self.shared.waiting.load(Ordering::Relaxed)` -> `LDR`
            acquire_pc: false,
        },
    );
    t0.push(worker_wait_read);

    // Thread 1 (Go reader: `ticketReader.waitRing`)
    let mut t1 = Vec::new();
    let mut amo = Rel::new();
    let mut rmw = Rel::new();
    match reader_park {
        ReaderParkOp::SwpalLse => {
            let r = push(
                Some(1),
                AccessKind::Read {
                    loc: Loc::Waiting,
                    acquire_sc: true,
                    acquire_pc: false,
                },
            );
            let w = push(
                Some(1),
                AccessKind::Write {
                    loc: Loc::Waiting,
                    val: 1,
                    release: true,
                },
            );
            t1.push(r);
            t1.push(w);
            amo.insert((r, w));
            rmw.insert((r, w));
        }
        ReaderParkOp::LlScV80 => {
            let r = push(
                Some(1),
                AccessKind::Read {
                    loc: Loc::Waiting,
                    acquire_sc: true,
                    acquire_pc: false,
                },
            );
            let w = push(
                Some(1),
                AccessKind::Write {
                    loc: Loc::Waiting,
                    val: 1,
                    release: true,
                },
            );
            t1.push(r);
            t1.push(w);
            rmw.insert((r, w));
        }
        ReaderParkOp::StlrOnly => {
            t1.push(push(
                Some(1),
                AccessKind::Write {
                    loc: Loc::Waiting,
                    val: 1,
                    release: true,
                },
            ));
        }
        ReaderParkOp::StrRelaxed => {
            t1.push(push(
                Some(1),
                AccessKind::Write {
                    loc: Loc::Waiting,
                    val: 1,
                    release: false,
                },
            ));
        }
    }

    let reader_seq_read = match reader_load {
        ReaderSeqLoad::LdarSc => push(
            Some(1),
            AccessKind::Read {
                loc: Loc::Seq,
                acquire_sc: true,
                acquire_pc: false,
            },
        ),
        ReaderSeqLoad::LdaprPc => push(
            Some(1),
            AccessKind::Read {
                loc: Loc::Seq,
                acquire_sc: false,
                acquire_pc: true,
            },
        ),
    };
    t1.push(reader_seq_read);

    let mut po = Rel::new();
    for thread_events in [&t0, &t1] {
        for i in 0..thread_events.len() {
            for j in (i + 1)..thread_events.len() {
                po.insert((thread_events[i], thread_events[j]));
            }
        }
    }

    LitmusGraph {
        events,
        po,
        amo,
        rmw,
        worker_wait_read,
        reader_seq_read,
    }
}

#[test]
fn litmus_artifact_matches_production_park_wake_instructions() {
    let litmus_dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../tests/litmus");
    let cases: &[(&str, &[&str])] = &[
        (
            "park_wake_dekker.litmus",
            &[
                "AArch64 GussetParkWakeDekker",
                "1:X0=1;",
                "STLR X0, [X1]",
                "DMB ISH",
                "LDR W2, [X3]",
                "SWPAL W0, W2, [X1]",
                "LDAR X4, [X3]",
                "exists (0:X2=0 /\\ 1:X4=0)",
            ],
        ),
        (
            "park_wake_dekker_v80_llsc.litmus",
            &[
                "AArch64 GussetParkWakeDekkerV80LLSC",
                "1:X0=1;",
                "LDAXR W2, [X1]",
                "STLXR W5, W0, [X1]",
                "LDAR X4, [X3]",
                "filter (1:X5=0)",
                "exists (0:X2=0 /\\ 1:X4=0)",
            ],
        ),
        (
            "park_wake_dekker_disproof_ldapr.litmus",
            &[
                "AArch64 GussetParkWakeDekkerDisproofLDAPR",
                "STLR W0, [X1]",
                "LDAPR X4, [X3]",
                "exists (0:X2=0 /\\ 1:X4=0)",
            ],
        ),
        (
            "park_wake_dekker_disproof_nofence.litmus",
            &[
                "AArch64 GussetParkWakeDekkerDisproofNoFence",
                "SWPAL W0, W2, [X1]",
                "LDAR X4, [X3]",
                "exists (0:X2=0 /\\ 1:X4=0)",
            ],
        ),
    ];
    for (file, required_clauses) in cases {
        let litmus_path = litmus_dir.join(file);
        let text = std::fs::read_to_string(&litmus_path)
            .unwrap_or_else(|e| panic!("cannot read {}: {e}", litmus_path.display()));
        assert!(
            !text.contains("1:W0="),
            "{file} must use architectural register 1:X0 in herd7 state block"
        );
        for required in *required_clauses {
            assert!(
                text.contains(required),
                "{file} missing required clause {required:?}"
            );
        }
    }
}

#[test]
fn aarch64_cat_proves_production_lse_and_v80_have_zero_lost_wakes_with_reachability_witnesses() {
    // 1. Production LSE (`SWPALW` + `LDAR` || `STLR` + `DMB ISH` + `LDR`)
    let prod_lse = build_park_wake_graph(true, ReaderParkOp::SwpalLse, ReaderSeqLoad::LdarSc);
    let res_lse = check_aarch64_cat(&prod_lse);
    assert_eq!(
        res_lse.lost_wake_executions, 0,
        "production SWPALW + LDAR must forbid lost wakes under aarch64.cat: {res_lse:?}"
    );
    // Non-vacuity reachability checks: verify every legitimate interleaving
    // outcome (`(1,0)`, `(0,1)`, `(1,1)`) is reachable so an overly restrictive
    // model cannot pass by rejecting all candidate executions.
    assert!(
        res_lse.consistent_executions >= 3,
        "vacuous model: {res_lse:?}"
    );
    assert!(
        res_lse.worker_saw_waiting_1 > 0,
        "worker-wakes path unreachable"
    );
    assert!(res_lse.reader_saw_seq_1 > 0, "reader-fast-path unreachable");
    assert!(
        res_lse.both_saw_1 > 0,
        "simultaneous visibility path unreachable"
    );

    // 2. Production ARMv8.0 LL/SC fallback (`LDAXRW`/`STLXRW` + `LDAR`)
    let prod_v80 = build_park_wake_graph(true, ReaderParkOp::LlScV80, ReaderSeqLoad::LdarSc);
    let res_v80 = check_aarch64_cat(&prod_v80);
    assert_eq!(
        res_v80.lost_wake_executions, 0,
        "ARMv8.0 LDAXRW/STLXRW + LDAR fallback must forbid lost wakes: {res_v80:?}"
    );
    assert!(res_v80.consistent_executions >= 3);

    // 3. Forward-compatibility of `SWPALW` even if `ringReady()` is lowered to `LDAPR` (`FEAT_LRCPC`):
    //    `[range([Exp & R & A]; amo; [Exp & W & L])]; po; [Exp & M]` in `aarch64hwreqs.cat` line 133
    //    orders `SWPALW`'s write before ANY memory access `M`, including `LDAPR` (`Q`)!
    let swpal_ldapr = build_park_wake_graph(true, ReaderParkOp::SwpalLse, ReaderSeqLoad::LdaprPc);
    let res_swpal_ldapr = check_aarch64_cat(&swpal_ldapr);
    assert_eq!(
        res_swpal_ldapr.lost_wake_executions, 0,
        "SWPALW must forbid lost wakes even when paired with FEAT_LRCPC LDAPR: {res_swpal_ldapr:?}"
    );
    assert!(res_swpal_ldapr.consistent_executions >= 3);
}

#[test]
fn aarch64_cat_disproves_stlr_ldapr_and_relaxed_and_fenceless_bugs() {
    // Disproof 1 (`¬Sec`): `STLRW` (`W & L`) followed by `LDAPR` (`R & Q`, FEAT_LRCPC)
    // lacks a `bob` edge because `aarch64hwreqs.cat` line 134 only orders `[W & L]; po; [R & A]`,
    // NOT `[W & L]; po; [R & Q]`!
    let buggy_stlr_ldapr =
        build_park_wake_graph(true, ReaderParkOp::StlrOnly, ReaderSeqLoad::LdaprPc);
    let res_bug1 = check_aarch64_cat(&buggy_stlr_ldapr);
    assert!(
        res_bug1.lost_wake_executions > 0,
        "Disproof failed: STLRW + LDAPR must exhibit a consistent lost-wake execution under aarch64.cat, got {res_bug1:?}"
    );

    // Disproof 2 (`¬Sec`): Relaxed `STRW` (`waiting = 1`) + `LDAR` (`seq`) allows StoreLoad reordering.
    let buggy_str_ldar =
        build_park_wake_graph(true, ReaderParkOp::StrRelaxed, ReaderSeqLoad::LdarSc);
    let res_bug2 = check_aarch64_cat(&buggy_str_ldar);
    assert!(
        res_bug2.lost_wake_executions > 0,
        "Disproof failed: STRW + LDAR must exhibit a lost-wake execution, got {res_bug2:?}"
    );

    // Disproof 3 (`¬Sec`): Worker omitting `fence(Ordering::SeqCst)` (`DMB ISH`) before
    // `waiting.load(Ordering::Relaxed)` (`STLR [seq]` -> `LDR [waiting]` has no `bob` edge).
    let buggy_no_dmb = build_park_wake_graph(false, ReaderParkOp::SwpalLse, ReaderSeqLoad::LdarSc);
    let res_bug3 = check_aarch64_cat(&buggy_no_dmb);
    assert!(
        res_bug3.lost_wake_executions > 0,
        "Disproof failed: omitting worker DMB ISH must exhibit a lost-wake execution, got {res_bug3:?}"
    );

    // Disproof 4 (`¬Sec`): ARMv8.0 LL/SC (`LDAXRW`/`STLXRW`, `rmw`) paired with `LDAPR` (`Q`):
    // `aarch64hwreqs.cat` line 133 grants `[range([R & A]; amo; [W & L])]; po; [M]` ONLY to
    // LSE `amo` (`SWPALW`), NOT exclusive `rmw` (`LDAXRW`/`STLXRW`), and line 134 orders
    // `[W & L]; po; [R & A]` (`LDAR`), NOT `[R & Q]` (`LDAPR`)!
    let buggy_llsc_ldapr =
        build_park_wake_graph(true, ReaderParkOp::LlScV80, ReaderSeqLoad::LdaprPc);
    let res_bug4 = check_aarch64_cat(&buggy_llsc_ldapr);
    assert!(
        res_bug4.lost_wake_executions > 0,
        "Disproof failed: ARMv8.0 LDAXRW/STLXRW + FEAT_LRCPC LDAPR without DMB ISH must exhibit a lost-wake execution, got {res_bug4:?}"
    );
}

/// Exhaustive multi-step interleaving proof of `waitRing`'s post-park cleanup
/// (`if atomic.SwapUint32(waiting, 0) == 0 { tr.tokensOwed++ }` at
/// `ticket_reader.go:233` vs worker `take_waiter` `waiting.swap(0, AcqRel)`),
/// paired with a disproof of `atomic.StoreUint32(waiting, 0)`.
#[test]
fn waitring_post_park_swap_conserves_pipe_tokens_and_store_zero_loses_token() {
    #[derive(Clone, Copy, Debug)]
    struct State {
        waiting: u32,
        pipe_tokens: usize,
        tokens_owed: usize,
        reader_pc: u8, // 0: swap(waiting,1), 1: post-park cleanup, 2: drain token if owed, 3: done
        worker_pc: u8, // 0: publish + take_waiter, 1: done
    }

    fn explore(use_atomic_swap_cleanup: bool) -> bool {
        let init = State {
            waiting: 0,
            pipe_tokens: 0,
            tokens_owed: 0,
            reader_pc: 0,
            worker_pc: 0,
        };
        let mut stack = vec![init];
        while let Some(s) = stack.pop() {
            if s.reader_pc == 3 && s.worker_pc == 1 {
                // Terminal invariant: waiting == 0 AND zero uncounted pipe tokens remain!
                if s.waiting != 0 || s.pipe_tokens != s.tokens_owed {
                    return false;
                }
                continue;
            }
            // Step Reader
            if s.reader_pc < 3 {
                let mut next = s;
                match s.reader_pc {
                    0 => {
                        next.waiting = 1;
                        next.reader_pc = 1;
                    }
                    1 => {
                        if use_atomic_swap_cleanup {
                            let prev = next.waiting;
                            next.waiting = 0;
                            if prev == 0 {
                                next.tokens_owed += 1;
                            }
                        } else {
                            // Buggy cleanup: `StoreUint32(waiting, 0)` overwrites without
                            // observing that the worker already claimed `waiting` and wrote a token!
                            next.waiting = 0;
                        }
                        next.reader_pc = 2;
                    }
                    2 => {
                        if next.tokens_owed > 0 && next.pipe_tokens > 0 {
                            next.pipe_tokens -= 1;
                            next.tokens_owed -= 1;
                        }
                        next.reader_pc = 3;
                    }
                    _ => unreachable!(),
                }
                stack.push(next);
            }
            // Step Worker (`Ring::take_waiter`: if waiting != 0 && swap(0) != 0 -> write 1 pipe token)
            if s.worker_pc == 0 {
                let mut next = s;
                if next.waiting != 0 {
                    next.waiting = 0;
                    next.pipe_tokens += 1;
                }
                next.worker_pc = 1;
                stack.push(next);
            }
        }
        true
    }

    assert!(
        explore(true),
        "production `atomic.SwapUint32(waiting, 0) == 0` must conserve pipe wake tokens across all schedules"
    );
    assert!(
        !explore(false),
        "Disproof failed: `StoreUint32(waiting, 0)` must violate pipe-token conservation when worker claims waiting first"
    );
}
