# Gusset deep audit — 2026-10-13 (main @ 73fbf32)

No source, config or dependency was modified. The only writes were GitPulse board writes and this file.

## How it ran

| Phase | Who | What |
|---|---|---|
| 1. Map | Opus (orchestrator) | GitPulse Insights: 1 worktree, clean, 0 collisions, 1 live session (this one). DevMap `status`: query-ready but **stale** (only `docs/PLAN.md` differs from generation 945), `walk_incomplete` (2588 unresolved sites; Go net resolution 579‰), `internal/ffi/gusset.h` partially parsed. So every empty DevMap answer was treated as "unknown". `devmap dead`: 9 candidates, all at confidence 0.4. `devmap clones`: 30 groups. Board baseline: 25 cards, 20 open, all read with `gitpulse_get_task`. |
| 2. Audit | 6 Sonnet auditors, at most 4 at once | A Rust pool core · B C ABI boundary (Rust and Go) · C Go package + `internal/isolate` · D tooling/CI/install/release · E test determinism and enforcement · F adopter surface, example, bench, platforms |
| 3. Verify | 4 Opus passes | Re-read every cited line and its callers, looked for upstream guards and DECISIONS entries, and gave each finding a verdict. Checked each "claimed done" review card criterion by criterion, plus `gh run view` on the CI evidence. |
| 4. Prioritize | Fable | Grouping, fold vs new, priority/severity, build waves, owner decisions. Followed as given. |
| 5. File | Opus (orchestrator) | Re-pulled the board before filing and re-checked revisions before each batch of folds. |

**Headline fact** [verified: `git ls-remote origin main` = 984209b; `git branch -vv` → `ahead 77`]: local `main` is **77 commits ahead of GitHub**. Every fix that ~13 review cards claim as done (2026-10-12) has been verified only on local runs.

## Severity × dimension (surviving findings, final severities after Opus)

| Dimension | critical | high | medium | low | total |
|---|---|---|---|---|---|
| Modular architecture | | | | 3 | 3 |
| Strict typing / C contract | | | | 2 | 2 |
| Defensive validation at trust boundaries | | | | 4 | 4 |
| Async / error boundaries | | | 2 | 2 | 4 |
| Deterministic testability and CI gates | | 1 | 1 | 7 | 9 |
| Performance and benchmarks | | | 1 | 3 | 4 |
| Correctness across platforms | | | | 2 | 2 |
| Security / supply chain | | | 2 | 1 | 3 |
| Docs-vs-code drift | | | | 5 | 5 |
| **Total** | 0 | 1 | 6 | 29 | **36** |

The auditors raised 41 items: 36 Sonnet findings, 4 new items from Opus, and 1 dead-code verdict. Opus rejected 6, and 36 survived (F-2 survives only as a docs residual). Opus downgraded or narrowed 12 and upgraded 1 (D-7). No critical defect was found in runtime code. Slices A–C turned up no race, use-after-free, fd double close or lost completion in the Rust pool, the FFI boundary or the root Go package. The confirmed medium defects are all in `internal/isolate` (a spike), CI gates, and release/installer hygiene.

## Top 10 risks (Fable's ranking)

1. Thirteen review cards' "done" rests on local runs only. No 2026-10-12 fix has run on GitHub main. → **ft-97faf4f4**
2. `install.sh`'s default tag v0.0.2 ships the WaitBuffer write-after-free, ignores DESTDIR, and its gussetvet scans the user's working directory. There is no advisory. → **ft-0dde896c**
3. `benchgate` silently stops gating every later sec/op row once one benchmark is renamed or removed (D-7), so perf regressions pass. → **ft-1f4a1228**
4. `internal/isolate`: a worker that stops reading stdin, or an OOM-killed worker whose descendant holds fd 3, hangs Submit/Wait until Close (C-1/C-2). This breaks the premise of DECISIONS.md:77. → **gs-isolate-corrupt-frame-hang**
5. `make build` with `CARGO_TARGET_DIR` set links the stale archive in `./target`, so tests can run old Rust code (B-1). → **ft-1a7e8a99**
6. Nine host-load `t.Skip` sites on the Go lanes have no SKIP guard (E-2), so I4 permit retention is never proven in CI. → **ft-1f4a1228**
7. Parity between `make lint` and the CI lint job is asserted only by a substring match (D-2). → **ft-1f4a1228**
8. ft-86b4ae63 was moved to review with 6 of its 8 criteria unmet. The review step itself is the weak gate.
9. `CGO_ENABLED=0` and BSD/illumos builds fail with confusing errors instead of the documented named error (F-1/A-3). → **ft-9977d2b2**
10. The isolate SIGABRT test failed on the latest Matrix run (9c64bad) in a second way. HEAD's accept-list is unproven on CI. → **gs-flaky-isolate-sigabrt-cgocheck2**

## New cards (all labelled `audit-2026-10-13`)

| item_id | Title (short) | Kind | P | Sev | Status | Findings |
|---|---|---|---|---|---|---|
| ft-97faf4f4d956b3d48396e3e730e85347 | Land main on GitHub: push 77 commits, run Matrix + adopter + nightly, record run ids | chore | 1 | high | ready | D-1 (owner-gated) |
| ft-9977d2b210e0e190aaf277a581510647 | Unsupported targets fail with the named error on both sides | bug | 3 | low | ready | A-3, F-1 |
| ft-1a7e8a995978987a62ee66b6151f9675 | Build identity: hashed archive == linked archive | bug | 3 | low | ready | B-1, B-2, N-AB1 |
| ft-568a30e26178bf6030c6e1d25c68c3dd | Bench evidence pipeline (record.sh fixes + registered-engine baseline). This is the benchmark-baseline card | chore | 3 | low | ready | D-6, F-4, F-6 |
| ft-8a7cd8ef6e74a81b8db1a0f709d7a9f2 | Go test hygiene: `LiveBytes ==` on a global gauge; redundant wall-clock bounds | chore | 3 | low | ready | E-3, E-5 |

Each was filed after a `related_tasks_exist` refusal. I read every listed card and retried with `reviewed_related`; the reason each one is separate is written into its description.

## Folded into existing cards (each overwrite carried the full existing content; logs and links preserved)

| item_id | Status change | Priority/severity change | Added |
|---|---|---|---|
| gs-boundary-defence-in-depth | review → **ready** | — | A-1, B-4, and the fuzz "every export" wording (15/17) |
| gs-isolate-corrupt-frame-hang | review → **ready** | sev low → **medium** | C-1, C-2 (+CLOEXEC), C-3 (typed-nil only), C-5 (in-progress Close) |
| ft-3d60ee168007d53b0ffed9056d9204a6 | review → **ready** | — | E-4, B-3 (FFI_BAD_ARG part), C-4 |
| ft-1f4a1228bb088b27773011ac1a211980 | review → **ready** | — | D-2, D-3, D-7, E-2, E-6 |
| ft-0f6a723dfd8c2b3cbf7cec23c07df8bb | review → **ready** | — | D-11, N-D1, F-5, adopter.yml run id |
| ft-0dde896cd5cd2c9929103414e9b43884 | backlog → **ready** | P2 → **P1**, sev none → **high** (after decision 4) | D-4, D-5, advisory criterion |
| ft-86b4ae632252d1ff986ce87c9719e6aa | review → **ready** | P3 → **P2**, low → **medium** | NOT-MET evidence (#1, 2, 5, 6, 7, 8), N-F1, N-F2 |
| ft-415ddf1f6ef83644e1789f98ee3b99d3 | review → **ready** (after decision 2) | — | D-10; criteria #5 and #6 status; branch-protection criterion |
| ft-8a8279f5d4de6464bee390b09823f33d | review → **ready** (after decision 6) | — | InlineRecordMax; R7 waits for ft-568a30e2; docs crossover replaces the 1.5x bar |
| ft-8a827cf5d4de697dbee38db09823ee24 | stays backlog | — | F-2 docs residual, F-3 |
| gs-flaky-isolate-sigabrt-cgocheck2 | stays review | — | new CI failure mode (38219108886); CI criterion |
| gs-ci-lint-staticcheck-go127 | stays review | — | v0.8.2 never run on CI; CI criterion |
| gs-gussetseal-darwin-ar-padding | stays review | — | criterion 3 on the pushed sha; resolved the R14 log reading |

Not touched: gs-allocator-api-hygiene-leak, gs-issue-3-asan-quadratic-engine, ft-94a2f74b (spot-check MET), ft-c689767b, ft-d3d3888d, ft-83151a08, and the owner card ft-6867f957. No card was moved to done, deleted or merged.

## Verified-stale cards (claimed done, but criteria not met)

| Card | Finding |
|---|---|
| ft-86b4ae63 docs drift | **6 of 8 criteria NOT MET in source.** AGENTS.md:119 (Windows CI leg), :121 (uniffi), :127 (`go build -a`); no DECISIONS row supersedes the :25 cargo-fuzz rejection (the log's claim that a 2026-10-12 row does so is false); `bench/seed/README.md:18-26` is hand-typed and :40 still says R13; PLAN.md:99-100 still says Phase 1 SHIPPED; SECURITY.md unchanged. Moved back to ready and raised. |
| gs-isolate-corrupt-frame-hang | Criterion 6 ("Submit honours ctx") is only partly met: a Submit blocked inside `send` ignores ctx (C-1). |
| gs-boundary-defence-in-depth | Its first criterion says "every export", but the fuzz target covers 15 of 17. |
| ft-415ddf1f | #5 is partial: the default tag v0.0.2 cannot honour the new installer, and the log's "not changed" is stale. #6 is not met (owner decision). |
| gs-ci-lint-staticcheck-go127, gs-gussetseal-darwin-ar-padding, ft-1f4a1228, ft-0f6a723d, gs-flaky-isolate-sigabrt-cgocheck2 | Met in source, but **never run on GitHub main**. adopter.yml has never executed. The branch run 38219108886 used the staticcheck pseudo-version, not v0.8.2. |
| ft-94a2f74b (hammer SIGSEGV) | The 38f0296 fix is present and correct at buffer.go:132-139, wait.go:57-62 and drain.go:108-136. Its link to the 2026-09-30 crash is still only inferred, as the card itself says. |

Confirmed MET by Opus, criterion by criterion: ft-1f4a1228 #1–#9 in source; ft-0f6a723d #2–#4; ft-415ddf1f #1–#4; gussetseal #1, #2, #4, #5; isolate #1–#5 and #7; gs-boundary pipe check, `statusToError`, sigaltstack refusal, guarded post-engine `deliver`, and the double-close DECISIONS row.

## Build order (Fable; at most 4 per wave, no shared planned files within a wave)

| Wave | Cards |
|---|---|
| **W0** (owner-gated) | ft-97faf4f4: land main via PR + CI. Its outcome decides whether the "keep review" cards close or reopen. Then ft-415ddf1f enables branch protection. |
| **W0b** (serial) | ft-0dde896c: cut v0.0.3, bump the installer's default ref, publish the advisory (decision 4; moved here from W3) |
| **W1** | gs-isolate-corrupt-frame-hang · ft-8a7cd8ef (test hygiene) · ft-1a7e8a99 (build identity) · ft-1f4a1228 (CI gates) |
| **W2** | gs-boundary-defence-in-depth · ft-9977d2b2 (platform gate) · ft-0f6a723d (adopter path) · ft-568a30e2 (bench pipeline) |
| **W3** | ft-3d60ee16 (invariant enforcement, incl. FFI_BAD_ARG) · ft-415ddf1f (container digest residual) |
| **W4** | ft-86b4ae63 (docs drift) · ft-8a8279f5 (perf; no DECISIONS.md edits in this wave) |
| **W5** | ft-8a827cf5 (engine registry docs + recovery decision) |

`matrix.yml` is touched in W1, W2 and W3, so each wave rebases on the previous one.

## Rejected findings

| ID | Claim | Why rejected |
|---|---|---|
| A-2 | A failed respawn fails submit even with live workers | Deliberate: DECISIONS.md:83 items 3–4, and pinned by `pool/tests/mod.rs:399-414` and `respawn.rs:42-66` |
| E-1 | `go test ./...` package parallelism causes timing flakes; add `-p 1` | The cited bounds have at least 3x headroom. The CI failure was the SIGABRT race already on gs-flaky-isolate |
| E-7 | Soak goroutine check has zero slack | Only per-handle `go` statements exist; quiesce and GC run first; the CI count is flat at 3 |
| D-8 | SVG byte-compare across arch; unpinned local benchstat | The amd64 CI job matched the arm64 SVGs, and CI pins benchstat, so it can only fail, never false-pass |
| D-9 | Unify duplicated doc-tool helpers | Look-alikes that answer different questions; unifying them would be over-unification |
| F-2 (code) | `set_engine_handler` silently replaces | Recorded decision DECISIONS.md:70. Only the docs residual was filed |
| Parts dropped | F-5's opcode-11 panic (deliberate, documented, tested); B-3's "rename fails a Rust test" and "engine-spoofed cancel text" (strings are pinned; the engine is trusted by design); C-3's opcode kinds (DECISIONS.md:77); C-5 after Close returns (already ErrClosed) | |

## Decisions made without asking

1. **Did not rebuild the stale DevMap index**, because the only writes allowed were board writes and this report. Every DevMap answer is therefore a lower bound, and every auditor named its fallback to reading source or `rg`.
2. **Saved the subagents' findings myself.** Subagents are not allowed to write report files, so they returned text and I saved each verbatim summary to the session scratchpad (`findings_*.md`, `verify_*.md`, `fable_plan.md`).
3. **Followed Fable's grouping** over the Opus fold suggestions where they differed: F-1 joined A-3 on a new card rather than ft-0f6a723d; B-4 went to gs-boundary rather than docs drift; F-2/F-3 went to ft-8a827cf5.
4. **Moved seven cards to ready**, six from review and ft-0dde896c from backlog, because their own criteria are unmet or new code criteria were added. Five cards stayed in review because their only gap is CI evidence or an owner decision.
5. **Raised priority/severity only** on three cards: gs-isolate sev → medium, ft-0dde896c sev → medium, ft-86b4ae63 P3→P2 and low → medium. Nothing was lowered.
6. **Set all five new cards to ready**, since each sits in a build wave. ft-97faf4f4 is labelled `owner-decision`.
7. **Used `reviewed_related`** on all five new cards after reading every listed card.
8. **Fold format**: new planned files are written inline in each "Audit 2026-10-13" section, because the `planned_files` and `repositories` parameters append a duplicate section to an existing description. My first fold (ft-8a827cf5, revision 6) duplicated "Related repositories". I corrected it at revision 7, and the logs and links survived both writes (verified with `gitpulse_get_task`).
9. **Another session was active on the board.** It updated gs-issue-3 to revision 5 and wrote board sequences 3641–3642 while I was filing. I did not touch gs-issue-3, and I re-checked the revisions of every card before folding into it.

## Owner decisions — made 2026-10-13 (owner: "choose the best"), recorded on the cards

| # | Decision | Card | Why |
|---|---|---|---|
| 1 | **Land main through a PR, not a direct push.** Push local main to a branch, open a PR to main, get Matrix + adopter (both trigger on `pull_request` to main) and a nightly dispatched on the branch green, then merge. | ft-97faf4f4 | Every gate runs before `main` moves. |
| 2 | **Enable branch protection** right after that merge: PR required with 0 approvals (single maintainer), required checks = Matrix + adopter jobs with strict up-to-date, force-push and deletion blocked, **enforced for admins**. Recorded in a DECISIONS row. Card moved review → ready. | ft-415ddf1f | The threat is a compromised push credential, which is the admin's own, so an admin bypass would leave it open. |
| 3 | **Support only the latest release; publish a GitHub security advisory** for the v0.0.2 WaitBuffer write-after-free (affected ≤ 0.0.2, patched 0.0.3, fix 38f0296) once v0.0.3 is tagged. | ft-0dde896c | A released memory-safety bug warrants disclosure. Nothing backports. |
| 4 | **Cut v0.0.3 immediately after W0, as a serial step W0b**, ahead of the hardening waves. Raised P2 → P1 and medium → high. | ft-0dde896c | The installer's default ref ships the bug. A solo step cannot collide on Makefile/lint.sh with the W1 lanes. |
| 5 | **Do the FFI_ERR → FFI_BAD_ARG change**, with a DECISIONS row, a `gusset.h` list of refusals, and a CHANGELOG entry in the release *after* v0.0.3. | ft-3d60ee16 | Pre-1.0. `status.rs:14` already teaches BAD_ARG, and a C host otherwise cannot tell misuse from engine failure. |
| 6 | **Move the docs line; no lone-call fast path now.** The 1.5x criterion is marked superseded (not deleted). New criterion: `docs/choosing.md` states the generated crossover. The other candidates stay open as opportunistic A/B wins. Card moved review → ready. | ft-8a8279f5 | The serial floor is three cross-thread handoffs. Every lock-level candidate lost its A/B, and a fast path reworks I4 permit ownership. |

The outward actions (push, PR, merge, workflow dispatch, branch-protection setting, tag, advisory) are decided but **not executed**. Each runs only on your explicit "go".

## Tool gaps

- **DevMap**: stale (generation 945), `walk_incomplete`, `gusset.h` partially parsed. The `set_nonblocking` caller list was truncated (32 of 66). Shell, YAML and Makefile are not indexed. GitPulse's codeintel facet reports `analyzer_freshness:false` while `devmap status` reports `true`, so the two disagree. No absence claim rests on DevMap alone: each was cross-checked with `rg -uu` and reading source, and labelled as such. Gaps were not written to `.devcouncil/codeintel/sessions/gaps.jsonl` because writes were restricted.
- **The repo's MANVI shell gate** blocked `cd`, `git -C`, `$(…)`, variable paths and scripts outside the repo. Every pass worked around it with literal paths, and no check was skipped because of it.
- **Nothing was built or run.** Every "would pass at HEAD" claim comes from reading source, and every runtime failure scenario not backed by a CI log is marked [inferred] on its card.
- **Upstream evidence**: the benchstat CSV shape (D-7) and cgo's package-wide `noescape` merge (D-11) were read from upstream master via `gh api`, not from the pinned versions.
