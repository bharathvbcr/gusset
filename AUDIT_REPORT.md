# Gusset deep audit — 2026-10-23 (main @ 320c56c == origin/main)

No source, config or dependency was modified. The only writes were GitPulse board writes and this file. The previous audit (2026-10-13, main @ 73fbf32) is in git at `320c56c:AUDIT_REPORT.md`.

## How it ran

| Phase | Who | What |
|---|---|---|
| 1. Map | Opus (orchestrator) | `gitpulse_insights` was **deferred under load on all three attempts** (every git facet `ok:false`, git spawn rate limit; only codeintel answered). Fallback evidence: `git worktree list` = 1 worktree, `git status` clean, and the board's event log showed no gusset writes by other sessions on 2026-10-23. DevMap `status`: generation 958, fresh, but `walk_incomplete` (2,713 unresolved sites; Go 593‰, Rust 627‰ net), `internal/ffi/gusset.h` partially parsed, so every empty DevMap answer was read as "unknown". `devmap_dead_symbols`: 10 candidates, all at 0.4. `devmap_clones`: test-shaped and allocator-trait groups only. Board: 30 cards, 25 open. Compact baseline read with `sqlite3 -readonly`. Criteria in that dump are truncated to 220 characters, and it has no descriptions or logs. Every card that was verified, folded or marked was read in full with `gitpulse_get_task`. **Four open cards were read only through the compact baseline**: ft-3d60ee16, ft-8a827cf5, ft-d3d3888d and ft-83151a08. None of the 35 findings touches their scope (invariant enforcement, poisoned-handle recovery, libc upstream, integrations survey). CI state was read with `gh`. |
| 2. Audit | 4 Sonnet auditors (4 at once) | A: CI-red triage + CI/gate delta · B: runtime delta (`internal/isolate`, `pool/sys.rs`, platform gate) · C: tooling delta (`bench/record.sh`, benches, gussetvet, walkthrough, install) · D: core-runtime regression sweep + docs/DECISIONS drift. Weighted to the 46-file / +5,236-line delta since 73fbf32 and to CI evidence. |
| 3. Verify | 4 Opus passes | One per slice (A, B, C+D), each re-reading every cited line, its callers and guards, and the CI logs; plus one pass checking the "done" claims on 11 cards criterion by criterion against Matrix run 39270656716 and the four nightlies. |
| 4. Prioritize | Fable | Grouping, fold vs new, priority/severity, waves, owner decisions. Followed as given. |
| 5. File | 3 Opus filers + orchestrator | Board re-pulled right before filing; each fold re-read with `gitpulse_get_task` immediately before writing and checked afterwards with read-only SQL against `work_revisions` (old description is a byte prefix of the new one; logs, links and every old criterion survive). |

**Headline facts** [verified with `gh`]:
- **main was pushed directly** on 2026-10-19 (Matrix run 39270656716, `event=push`, sha 320c56c), not through a PR as owner decision 1 on ft-97faf4f4 required. Local and remote main are equal.
- **Matrix is red in 8 of 21 jobs**, and **Nightly Sanity has been red 4/4** since 2026-10-20 (39399728258, 39575563846, 39754401724, 39930332461). GitHub issue #4 is open.
- Three causes explain all of the Matrix red. Four steps never executed on 320c56c because an earlier step failed: the simd suite, GOGC=1, allocator interop race + fallback, and the bench allocation gate.
- Upstream: `eclipse-iceoryx/iceoryx2-go` was **created 2026-10-23** (otterdog `.eclipsefdn#28`, merged 2026-10-22). Your PR #1 there is open.

## Severity × dimension (surviving findings, final severities after Opus)

| Dimension | critical | high | medium | low | total |
|---|---|---|---|---|---|
| Modular architecture | | | | 1 (A-9) | 1 |
| Strict typing | | | | | 0 |
| Defensive validation at trust boundaries | | | 1 (A-4) | | 1 |
| Async / error boundaries | | | 1 (B-2) | 4 (B-3, B-4, C-4, C-5) | 5 |
| Deterministic testability and CI gates | | 1 (A-1) | 3 (A-5, A-6, C-2) | 7 (A-7, A-8, B-5, C-3, C-8, C-10, C-11) | 11 |
| Performance and benchmarks | | | 3 (A-3, C-1, C-6) | 1 (C-7) | 4 |
| Correctness across platforms (Linux/macOS; Windows is a compile_error) | | | 1 (B-1) | 1 (C-9) | 2 |
| Security / supply chain (process) | | | 1 (direct push, no protection) | | 1 |
| Docs-vs-code drift | | | 1 (D-1) | 9 (B-6, B-7, D-2..D-6, D-N5, lib.rs text) | 10 |
| **Total** | 0 | 1 | 11 | 23 | **35** |

41 items were raised:
- 40 by the agents: A-1..A-9, B-1..B-8, C-1..C-11, D-1..D-6 and D-N5, plus D1..D5 from the review-card pass;
- 1 by the orchestrator: the direct push.

Opus then:
- merged 5 duplicates: A-2 = B-1, D1 = A-1, D2 = A-6, D3 = A-8, D4 = A-3;
- rejected B-8, plus sub-claims of C-3 and C-6;
- downgraded B-3, B-7, C-4, C-8 and A-3.

That leaves 35. D5 survives as the "lib.rs text" row.

"Strict typing" is 0 because auditor B's cast review (frame-length `uint32`/`int`, base64 limit math) came back clean.

**Runtime result.** As on 2026-10-13, no critical defect was found in the in-process runtime, which covers the Rust pool, the C ABI and the root Go package. Auditor D re-read the hot paths, Opus did not overturn that, and the only runtime change since 73fbf32 was `pool/sys.rs`.

**Where the defects are.** Every confirmed medium-or-higher defect is in one of these places:
- the CI gates and tests (two make main red by construction);
- the nightly harnesses;
- `internal/isolate` (a spike);
- the bench recorder;
- the docs.

## Top 10 risks (Fable's ranking)

1. **Main has been red since the direct push on 2026-10-19, and four steps have never run on 320c56c.** The unrun steps are the simd suite, GOGC=1, allocator interop race + fallback, and the allocation gate, so there may be further reds behind the known ones. → **ft-97faf4f4** (M1) and **ft-1f4a1228** (A-8).
2. **Branch protection is off, and the owner's PR-only decision was bypassed by a direct push.** → **ft-415ddf1f**, now ordered first (W0).
3. **v0.0.3 is blocked behind red main.** It carries the WaitBuffer write-after-free fix, and the installer still defaults to v0.0.2. → **ft-0dde896c** (W1b).
4. **Nightly has been red 4/4, so ASan-hammer, Miri and cargo-fuzz coverage is effectively off**, and a real memory bug would be invisible. → **gs-nightly-harness-defects-2026-10-23** (new) and **gs-boundary-defence-in-depth**.
5. **A-1 is a class of gate interaction.** The probe switch (ft-1f4a1228 c13) made a runtime property fatal: the Go Linux netpoller's 1 ms idle floor. All 9 `hostprobe.Skip` sites share that exposure. → **ft-c689767b** plus the contract criterion on **ft-1f4a1228**.
6. **A-7: the Go fuzz minimizer hides the first failure**, and the crash input was never committed. Issue #4's real cause is unknown. → new nightly card.
7. **B-2: `internal/isolate` drops results that already reached the host when the worker dies**, against DECISIONS.md:49's principle. This spike shapes gusset-ipc's design. → **gs-isolate-corrupt-frame-hang**.
8. **C-1: `bench/record.sh` can publish zero-row arms and overwrite a good file with a header-only one.** Every R15 figure rests on it. → **ft-568a30e2**.
9. **A-4 (product question):** raw `gusset_buf_alloc` commits up to 1 GiB zero-filled per call, and `WithBufferBudget` defaults to unlimited. → **owner decision**.
10. **The instruction file every agent reads is wrong.**
    - AGENTS.md:323 claims a green full matrix.
    - 8 of 10 docs-drift criteria are unmet.
    - The DECISIONS table stops rendering at :43.
    - The iceoryx2 docs state facts that are now false.

    → **ft-86b4ae63**.

## Main-red root causes (Matrix 39270656716)

| Cause | Jobs | Owning card | Confidence |
|---|---|---|---|
| **A-1.** `TestSerialCall_LeavesAPForOtherGoroutines`. The Go Linux netpoller rounds sub-1 ms timer waits up to `epoll_wait(1)` (`runtime/netpoll_epoll.go:122-123`), so the no-Calls baseline added in 7ea38fe reads 860–889 µs on every Linux host. `GUSSET_REQUIRE_PROBES=1` then turns its `hostprobe.Skip` into a failure. | All 8 Linux Go lanes | ft-c689767b | verified (Go 1.27.3 source + 8-lane uniformity) |
| **B-1.** `TestProc_ProtocolFDIsCloseOnExec`. The probe only checks that `Dup(3)` succeeds, so it cannot tell which file holds fd 3. On Linux the Go ≥1.25 runtime keeps the cgroup `cpu.max` fd open (`containermaxprocs`, `internal/runtime/cgroup/cgroup_linux.go:96`), and that fd lands at 3 after exec. The product's CLOEXEC (`proc.go:80`) is correct. | All Linux lanes; macOS passes | gs-isolate-corrupt-frame-hang c10 | inferred, ~90%. Settle with `GODEBUG=containermaxprocs=0 go test -run '^TestProc_ProtocolFDIsCloseOnExec$' -count=1 -v ./internal/isolate/` on Linux |
| **A-3.** benchgate fails new-only rows (`DeadlineExpiry-4`, `Discard-4`, `Stats-4`, added in 94fa191). The workflow has no allow path, and the allocation gate is then skipped. | Benchmarks (R15) | ft-1f4a1228 | verified |

Nightly causes:
- **A-4:** `ffi_exports` fuzz OOM, 4/4 → gs-boundary.
- **A-5:** Miri cancelled at the 6 h default → new card.
- **A-6:** hammer `opAbandon` oracle false positive on an empty body (2/4 nightlies, heap-check legs) → new card.

## New card

| item_id | Title (short) | Kind | P | Sev | Status | Findings |
|---|---|---|---|---|---|---|
| **ft-987c804c9da86397131d602d390cb582** (task_id gs-nightly-harness-defects-2026-10-23) | Nightly harness defects: Miri unbounded (6 h cancel), hammer opAbandon oracle false positive, Go fuzz shared handle masks the first failure, no cargo-fuzz crash artifact | bug | 1 | medium | ready | A-5, A-6, A-7, cargo-fuzz artifact upload |

## Folded into existing cards

Each overwrite carried the full existing description byte for byte, plus an "Audit 2026-10-23" section. Criteria were only prefixed with MET/PARTIAL/NOT MET/SUPERSEDED marks and appended to. Logs and links were preserved, and priority and severity were only raised.

| item_id | Status change | P / sev change | Added |
|---|---|---|---|
| ft-97faf4f4d956b3d48396e3e730e85347 | ready (rewritten as the tracker) | P1 → **P0**, high | Direct-push fact. Red causes mapped to their cards. M1: one PR carrying the W1 fixes, with the masked steps shown to execute. M2: three green nightlies. Close issue #4. Old criteria marked superseded. |
| ft-c689767b57a8ed6768589485f751f3f0 | review → **ready** | P2 → **P0**, medium → **high** | A-1 root cause. Fix direction: remove the idle baseline and return to the fixed 100 µs bound (passed 20/20 on GitHub). Mutant must fail ≥10/10 on 4-vCPU Linux. Not platform-gated, not deleted. |
| gs-isolate-corrupt-frame-hang | review → **ready** | P3 → **P1**, medium | B-1: criterion 10 rewritten (settling command first, identity probe). B-2: delivered results survive worker death. B-3: DECISIONS row, behaviour kept. B-4: progress-based drain. B-5: 30 s test. B-6: code comments. Criterion 11 amended. 1–9 and 12 marked MET. |
| gs-boundary-defence-in-depth | review → **ready** | P2 → **P1**, medium | A-4: criterion 1 restated as a green gate; fuzz allocation invariant (≤1 MiB per allocation, ≤64 MiB live). B-7: doc. gusset.h buf_alloc guidance, no cap until the owner decides. 2–8 marked MET. |
| ft-1f4a1228bb088b27773011ac1a211980 | review → **ready** | P1, high | A-3: base-only fails, new-only is a notice. A-8: `if: success() \|\| failure()` on independent steps. hostprobe.Skip contract across all 9 sites. A-9: workflow-level env replaces the ~670-line shell model, as a fenced follow-up after M1. c1 and c14 PARTIAL; 2–13 MET. |
| ft-568a30e26178bf6030c6e1d25c68c3dd | review → **ready** | P3 → **P2**, low → **medium** | C-1 to C-7 and C-11. Still the sole benchmark-baseline card. 2 and 3 MET, 1 and 4 PARTIAL, 5 NOT MET. |
| ft-86b4ae632252d1ff986ce87c9719e6aa | ready (stays) | P2, medium | D-2 (DECISIONS table split), D-3, D-4, D-5, D-6 (iceoryx2 facts only), D-N5, B-6 docs, the lib.rs refusal text. 3 and 4 MET; the other 8 NOT MET. |
| ft-0f6a723dfd8c2b3cbf7cec23c07df8bb | review → **ready** | P2, medium | C-8: the step-5 grep matches gusset's own build output. C-9: gussetvet ignores build tags. C-10: libgusset.a check has no before-snapshot. All 9 old criteria MET (c6 via run 39270656722). |
| ft-415ddf1f6ef83644e1789f98ee3b99d3 | ready (stays) | P2 → **P1**, medium | Branch protection moved to W0, before the fix PR. Job-rename hazard noted. 1–4 MET; 5 PARTIAL. |
| ft-0dde896cd5cd2c9929103414e9b43884 | ready (stays) | P1, high | Criterion 5 progress: the Makefile uses `cargo pkgid`; the card's install.sh:117 item is stale. The CHANGELOG-vs-Cargo check is still missing. Prerequisite: M1. |

## Verified-stale and verified-done cards

These stay in review with "[MET — verified in source]" marks and a verification section. **Nothing was moved to done**, because done needs Manvi verification.

| Card | Verdict |
|---|---|
| gs-gussetseal-darwin-ar-padding | **All MET**: R14 macOS green on main, job 118121740734. Ready for Manvi. |
| gs-issue-3-asan-quadratic-engine | **All MET**: ASan green on 4/4 nightlies; issue #3 closed. Ready for Manvi. |
| ft-9977d2b210e0e190aaf277a581510647 | **All MET**: the platform-gate job asserts the named errors. Ready for Manvi. |
| ft-8a7cd8ef6e74a81b8db1a0f709d7a9f2 | **All MET**. Ready for Manvi. |
| gs-ci-lint-staticcheck-go127 | 1, 3, 4 MET (staticcheck 0.8.2 PASS on main). **2 PARTIAL**: no CI run has had staticcheck failing. |
| gs-allocator-api-hygiene-leak | 1–3 MET. **4 NOT MET**: main is red for unrelated causes. Not re-scoped; it waits on M1. |
| gs-flaky-isolate-sigabrt-cgocheck2 | 1 MET. **2 PARTIAL** (darwin only). **3 PENDING**: SIGABRT passes on Linux, but the simd step was skipped. |
| ft-94a2f74b1f9d5a4d6c51bc1c8813d2c8 | 1, 2, 4, 5, 6 MET (hammer long green 4/4). **3 PARTIAL**: the link to the crash is still inferred. Heap legs are red from the A-6 oracle bug, not from a SIGSEGV. |

The "done" claims these four cards made are contradicted by CI:
- **ft-c689767b:** all Linux lanes are red.
- **gs-isolate-corrupt-frame-hang c10:** red on Linux; it was only ever run on darwin.
- **gs-boundary-defence-in-depth c1:** the new fuzz target runs out of memory every night.
- **ft-97faf4f4:** all 5 criteria are unmet as written, because no PR was ever opened.

Not touched:
- ft-6867f957 (your card; see owner decisions)
- ft-1a7e8a99, ft-8a8279f5, ft-3d60ee16, ft-8a827cf5, ft-d3d3888d, ft-83151a08 (no new evidence)

No card was deleted or merged.

## Build order (Fable; at most 4 per wave, no shared planned files within a wave)

| Wave | Cards | Rationale |
|---|---|---|
| **W0** (serial, owner "go") | ft-415ddf1f: enable branch protection + DECISIONS row | This is the control that would have stopped the direct push. It cannot block W1's PR, because required checks run on the PR head. |
| **W1** (red main; **one PR**) | ft-c689767b (handoff test) · gs-isolate-corrupt-frame-hang (isolate) · ft-1f4a1228 (matrix.yml, nightly.yml env only, benchgate, tests/ci; A-9 after green) · ft-97faf4f4 (tracker M1) | The three Matrix red causes plus the step-masking fix, so M1 can prove the masked steps ran. |
| **W1b** (serial, owner "go") | ft-0dde896c: v0.0.3 tag, installer default, advisory | Owner decision 4 from 2026-10-13. Serial, so Makefile, lint.sh and SECURITY.md cannot collide. |
| **W2** (red nightly) | gs-nightly-harness-defects-2026-10-23 (nightly.yml, ffi/mod.rs test, stress test, fuzz_test.go) · gs-boundary-defence-in-depth (ffi_exports.rs, pool/mod.rs, gusset.h) · ft-0f6a723d (walkthrough.sh, gussetvet) · ft-1a7e8a99 (Makefile, gen.go, archive_hash.go, ffi.go, .gitignore, AGENTS.md:127) | Nightly's three causes plus two low cards whose files don't overlap. |
| **W3** | ft-568a30e2 (bench pipeline, .gitignore, one matrix.yml step) · ft-86b4ae63 (docs, DECISIONS append, SECURITY.md, lib.rs text) · ft-3d60ee16 (FFI_BAD_ARG, after v0.0.3 per decision 5) | .gitignore and AGENTS.md are freed by W2; SECURITY.md is freed by W1b. |
| **W4** | ft-415ddf1f residual (container digests) · ft-8a8279f5 (perf; consumes the W3 baseline) · ft-8a827cf5 (engine registry docs + recovery decision) | Each needs a file a W3 card holds. |

Files that recur across waves, so each wave must rebase on the previous one:
- matrix.yml: W1, W3, W4
- DECISIONS.md: W0, W1, W3, W4 (appends only)
- nightly.yml: W1 env only, then W2

## Rejected findings

| ID | Claim | Why rejected |
|---|---|---|
| B-8 | `Wait` during a blocked send does not assert permit/poison | The deaf worker never answers, so the permit is correctly held. The Submit clause is asserted at `isolate_internal_test.go:904-916` and `:1003-1011`. |
| C-3 (part) | The lock race test is serial | `start_at` backgrounds each recorder (`record_test.sh:62`) and the loop never waits, so the 8 run concurrently. |
| C-3 (part) | The suite would pass on 73fbf32's record.sh | The static guard at `record_test.sh:12` matches the old `kill -TERM 0`, and `signal_case` would see 143, not 130. |
| C-6 (part) | `opcode0_again` is unused | It is reported as `dispatch/opcode0` (`dispatch.rs:190`). |
| C-6 (part) | Handler bodies differ between arms | Diagnostic mode 0 is `input.to_vec()` (`diagnostic.rs:70`), the same as `echo`. |
| D-4 (PLAN.md:135) | Promises a blocking fallback | It is a "kill or narrow if" contingency, not a claim. Only `:148` is filed. |
| A-2 | Close-on-exec red | Duplicate of B-1. |
| Dead-code candidates | DevMap's 0.4 rows | All are live: `adopt_output` is behind a cfg (`worker.rs:84`), `Handle.complete` is called at `worker.rs:178`, `nameSet.Set` is a `flag.Value`, and `ArchiveHash` is load-bearing. The exception is the Go const `InlineRecordMax`, which is already on ft-8a8279f5. |

Downgraded rather than rejected: B-3, B-7, C-4, C-8, A-3 (P1 → P2).

## Decisions made without asking

1. **Did not rebuild DevMap and did not write `gaps.jsonl`**, because writes were restricted. Every DevMap answer is a lower bound, and each auditor named its fallback (`rg -uu` or reading source).
2. **Saved the subagents' findings myself.** Subagents cannot write files here, so they returned text and I saved summaries to the session scratchpad (`findings_*.md`, `verify_*.md`, `verified_findings.md`, `fable_plan.md`, `filing_spec.md`).
3. **Scope.** "This repo" is gusset. The note that you maintain `iceoryx2-go` was taken as context, not a second audit target. Your local checkout holds only a README, and the MANVI gate refuses `cd` and `--git-dir` into a sibling repo. The upstream iceoryx2 facts were read with `gh` and filed only as gusset docs drift. Auditing iceoryx2-go itself needs a session rooted in that repo.
4. **Did not write ft-6867f957, your iceoryx2 card.** `gitpulse_add_task` has no checklist parameter, and that card carries a 7-item checklist that an overwrite could drop irreversibly. The update is here and as a fact-only criterion on ft-86b4ae63.
5. **Delegated board writes to three Opus filers** working on disjoint cards. Each fold was re-read immediately before its write and checked afterwards against the previous revision with read-only SQL. Results are in the filing log below.
6. **Followed Fable on every grouping.** That included: no single "make main green" card (ft-97faf4f4 is rewritten as the tracker); B-5 placed on gs-isolate rather than ft-8a7cd8ef; C-10 on ft-0f6a723d; the lib.rs text on ft-86b4ae63, so three all-MET cards stay all-MET.
7. **A-1 direction was decided, not escalated.** Fixed 100 µs bound; no platform gate; no deletion. It is test-only with 20/20 GitHub evidence. The 2026-10-11 card log said a bystander *redesign* is your call, and this is not a redesign. You can override it.
8. **A-9 approved as a fenced follow-up after M1.** This reverses the 2026-10-13 approval of the shell model.
9. **B-3 resolved by an appended DECISIONS row**, keeping the fail-closed behaviour.
10. **gs-allocator criterion 4 was not re-scoped**, because that would lower its bar.
11. **Branch protection reordered to W0**, ahead of the fix PR.
12. **Did not read four out-of-scope cards in full** (ft-3d60ee16, ft-8a827cf5, ft-d3d3888d, ft-83151a08), to save context. They were deduped against the compact baseline only, and no finding maps to them.
13. **Priority and severity were only raised**: ft-97faf4f4, ft-c689767b, gs-isolate, gs-boundary, ft-568a30e2, ft-415ddf1f. Six cards moved review → ready, because criteria they claimed were contradicted or new criteria were added: ft-c689767b, gs-isolate, gs-boundary, ft-1f4a1228, ft-568a30e2 and ft-0f6a723d.

## Owner decisions needed before building

1. **Branch protection "go"** (W0, before the fix PR). The rule set is the one decided on 2026-10-13:
   - PR required, 0 approvals
   - required checks = Matrix + adopter, strict
   - force-push and deletion blocked
   - `enforce_admins`

   A Matrix job renamed in W1 must be updated in the rule in the same change.
2. **A-4, buffer caps.** Either keep "no default budget, no allocator-side ceiling" (DECISIONS.md:63, :54) and only document C-host guidance in `gusset.h`, or cap raw `gusset_buf_alloc` / default `WithBufferBudget`. The second is a security-default and behaviour change and needs a DECISIONS row and a CHANGELOG entry.
3. **iceoryx2-go PR #1 is 1,291 additions across 22 files.** It combines scaffold with NodeBuilder/Node, while DECISIONS.md:86 recorded step 1 as **two** PRs of **≤1,000 lines of content** each, which is its condition for needing no IP Team review. Either split it or accept an IP review. Check how Eclipse counts "lines of content" (licence, NOTICE, go.sum) first. Your card's 2026-10-26 check-in stands.
4. **Execution "go"s**, decided on 2026-10-13 and not yet executed:
   - the W1 PR and merge
   - v0.0.3 tag and advisory
   - closing issue #4
   - dispatching nightly on a branch
5. **(Optional override) A-1 direction**: see decision 7 above.

## Tool gaps

- **GitPulse Insights** was deferred under load through Phases 1–4 (orchestrator ×3 and every auditor and verifier). It answered in full only during Phase 5: all three filers got every facet `ok: true` right before writing, showing one worktree on main at 320c56c, a clean tree, no collisions, and one live Claude session (this one). Its ledger facet reported `stale: true`. So the pre-filing collision check did run; the Phase 1 map relied on the labelled fallback.
- **DevMap:**
  - The index is fresh, but `walk_incomplete` is set and `gusset.h` is partially parsed.
  - It does not index shell, YAML or Makefile files, so every workflow, record.sh and walkthrough finding rests on reading source.
  - `devmap_neighbors` on `proc.go` was too broad to use, at 68k.
  - Untyped receivers such as `h.complete` are unbound, which is why live symbols looked dead.
- **Nothing was built or run.** No Linux reproduction was possible: the podman machine is down, and the gate refuses writes outside the repo. B-1's occupant and every "would pass at HEAD" claim are inferred until the settling command runs. The A-1 mechanism is verified from Go runtime source plus CI logs.
- **Missing artifacts:** the cargo-fuzz job uploads none, and the Go crash input `582528ddfad69eb5` is not in the tree, so A-4's exact input and A-7's first failure cannot be recovered.
- **Shell gate:** blocked `cd`, `git -C`/`--git-dir`, `$(…)`, redirects outside the repo, and scripts in the scratchpad. One verifier's read-only `gh … --job "$(…)"` went through despite the rule; it wrote nothing.

## Filing log

The board was re-pulled right before filing. The event log showed no gusset writes by other sessions on 2026-10-23, and every card was at the revision expected from the Phase 1 baseline. Each filer ran `gitpulse_get_task` immediately before each write.

**Post-write check.** The orchestrator independently ran a read-only SQL check (`work_items` against `work_revisions` at revision−1). It passed for all 18 folds:
- the old description is a byte prefix of the new one;
- logs are unchanged;
- the checklist is unchanged;
- 0 old criteria are missing (each survives verbatim or behind a MET/PARTIAL/NOT MET/SUPERSEDED/PENDING prefix);
- the `audit-2026-10-23` label is present.

Links were checked by the filers.

| item_id | rev | status | P | sev |
|---|---|---|---|---|
| ft-97faf4f4d956b3d48396e3e730e85347 (tracker, retitled) | 2→3 | ready | 0 | high |
| ft-c689767b57a8ed6768589485f751f3f0 | 4→5 | ready | 0 | high |
| gs-isolate-corrupt-frame-hang | 8→9 | ready | 1 | medium |
| gs-boundary-defence-in-depth | 7→8 | ready | 1 | medium |
| ft-1f4a1228bb088b27773011ac1a211980 | 8→9 | ready | 1 | high |
| ft-568a30e26178bf6030c6e1d25c68c3dd | 3→4 | ready | 2 | medium |
| ft-86b4ae632252d1ff986ce87c9719e6aa | 5→6 | ready | 2 | medium |
| ft-0f6a723dfd8c2b3cbf7cec23c07df8bb | 6→7 | ready | 2 | medium |
| ft-415ddf1f6ef83644e1789f98ee3b99d3 | 4→5 | ready | 1 | medium |
| ft-0dde896cd5cd2c9929103414e9b43884 | 8→9 | ready | 1 | high |
| ft-8a7cd8ef6e74a81b8db1a0f709d7a9f2 (verification only) | 3→4 | review | 3 | low |
| ft-9977d2b210e0e190aaf277a581510647 (verification only) | 4→5 | review | 3 | low |
| gs-gussetseal-darwin-ar-padding (verification only) | 7→8 | review | 0 | critical |
| gs-issue-3-asan-quadratic-engine (verification only) | 7→8 | review | 1 | high |
| gs-ci-lint-staticcheck-go127 (verification only) | 8→9 | review | 0 | critical |
| gs-allocator-api-hygiene-leak (verification only) | 5→6 | review | 1 | medium |
| gs-flaky-isolate-sigabrt-cgocheck2 (verification only) | 4→5 | review | 1 | medium |
| ft-94a2f74b1f9d5a4d6c51bc1c8813d2c8 (verification only) | 6→7 | review | 2 | high |
| **ft-987c804c9da86397131d602d390cb582** (new) | 1 | ready | 1 | medium |

**The new card** was first refused with `related_tasks_exist`, which listed 8 open cards. The filer read each one, found none that covers Miri, the hammer oracle, the Go fuzz handle or the artifact upload, and retried with `reviewed_related` naming all 8. The retry was accepted.

**ft-6867f957** (your card) is still at revision 5 and was not written.
