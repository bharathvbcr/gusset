# Branch consolidation report

Date: 2026-10-13. Repository: `/Users/bharath/Code/devtools/gusset`. Nothing was pushed.

`main` is `97842d914405ee0a91aa9d49d7a616f7adf50326`, 96 commits ahead of `origin/main` (`984209b542885b10b43e3d9aaf2073fd871e9eb9`). The five local topic branches are merged and deleted. Their tips are still reachable from `main` and from `refs/backup/2026-10-13/`.

## Inventory

GitPulse Insights and collision risk, both before and after the deletes: one worktree, this checkout; `collisions.ok` true; `overlapping_files` 0; `unscanned_worktrees` 0; `sessions_ok` true. Live Claude sessions 0, live Codex sessions 0. No parked merge or rebase. `change_context` `task_id` was empty: no GitPulse task was bound to the worktree. No board task was `in_progress` (30 tasks listed, `has_more` false).

`git worktree list` showed only this checkout. Directories under `/Users/bharath/.grok/worktrees/devtools-gusset/` are separate repositories (one sampled `.git` is a directory of its own, and none of them appear in `git worktree list`). They were not modified.

Ahead/behind is `git rev-list --left-right --count main...<branch>` (left = behind `main`, right = ahead). Merged is `git branch --merged main` at inventory time, before this consolidation. Author of every tip below is BHARATH CHANDRA VADDARAM.

| Ref | Ahead | Behind | Tip date | Last commit | Merged into local main then? | Checked out | Dirty | Live session | GitPulse task | Class |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `main` | 0 | 0 | 2026-10-13 09:50 -0700 | `73fbf32` docs: otterdog request not yet opened | is `main` | no (was not; is now) | no | no | none | keep |
| `w2-boundary` | 7 | 0 | 2026-10-13 20:48 -0500 | `790967a` Pin caller descriptor-flag rollback | no | no | no | no | review cards, not bound | mergeable |
| `w2-adopter` | 9 | 0 | 2026-10-13 21:01 -0500 | `6d16320` gussetvet R5 counts compiled files | no | no | no | no | review cards, not bound | mergeable |
| `w2-bench` | 10 | 0 | 2026-10-13 21:01 -0500 | `94fa191` bench recorder lock must not fail open | no | no | no | no | review cards, not bound | mergeable |
| `w2-platform` | 12 | 0 | 2026-10-13 21:12 -0500 | `324921f` named refusal for unsupported targets | no | no | no | no | review cards, not bound | mergeable |
| `wave-w1` | 13 | 0 | 2026-10-13 21:25 -0500 | `e7400a4` record the W2 platform named-error gate | no | yes, this checkout | 2 untracked reports | no other session | review cards, not bound | mergeable |
| `origin/main` | 0 | 77 | 2026-10-10 12:15 -0700 | `984209b` rejected G6 records point at archive tags | ancestor of local `main` | no | no | no | none | keep (remote) |
| `origin/gitpulse/main-red-lint-staticcheck-go127` | 0 | 67 | 2026-10-12 00:27 -0700 | `9c64bad` lint job runs every audit on Go 1.27.2 | ancestor of local `main`; `git cherry` empty | no | no | no | `gs-ci-lint-staticcheck-go127` (review) | keep (remote) |

`git cherry -v main <branch>` marked every commit on the five topic branches `+`. None of those patches were already on `main`. `git merge-base --is-ancestor` was exit 0 for the whole chain:

`main` ⊂ `w2-boundary` ⊂ `w2-adopter` ⊂ `w2-bench` ⊂ `w2-platform` ⊂ `wave-w1`

So the four `w2-*` branches had no commit that `wave-w1` did not already contain. They were not superseded. Merging them in that order lands each commit once.

Review cards named by the untracked `BUILD_REPORT.md`, none of them `in_progress`: `ft-8a7cd8ef` (`c8115b8`), `ft-1f4a1228` (`c49844d`, `959d7e7`), `gs-isolate-corrupt-frame-hang` (`0776d42`), `gs-boundary-defence-in-depth` (`76e2bb1`, `790967a`), `ft-0f6a723d` (`1cfa0d4`, `6d16320`), `ft-568a30e2` (`94fa191`), `ft-9977d2b2` (`324921f`, `e7400a4`). Those cards were not moved.

Fable's review agreed with this classification and with the merge order. The one flag was AGENTS.md's full-matrix rule against `BUILD_REPORT.md` saying GitHub Actions had not run. This request already answered it: merge locally, prove each merge with `make ci-local`, and do not push. No branch was reclassified to keep.

DevMap impact on `crates/gusset/src/pool/sys.rs` (generation 955, `repository.root` matched) was a lower bound: `walk_incomplete` at depth 1, 30 of 146 edges shown. It did not change the order. The ancestry chain already forces it. The gap is in `.devcouncil/codeintel/sessions/gaps.jsonl`.

## Backup

Taken before checkout and before any merge or delete.

| Backup ref | Object |
| --- | --- |
| `refs/backup/2026-10-13/main` | `73fbf32c593933c04168b49b404e7745d1d19d8e` |
| `refs/backup/2026-10-13/w2-boundary` | `790967ab20c3642398252de63221b40b9d945408` |
| `refs/backup/2026-10-13/w2-adopter` | `6d16320ab758d1b449051cf00c257a2daeada8d9` |
| `refs/backup/2026-10-13/w2-bench` | `94fa191678174bc6f72093ec3994b8db8e97caa5` |
| `refs/backup/2026-10-13/w2-platform` | `324921fe2d49f1bd5d2524d30824bf358c396890` |
| `refs/backup/2026-10-13/wave-w1` | `e7400a480e97c22e9d7c5deb6498615f0c1767d0` |
| `refs/backup/2026-10-13/origin-main` | `984209b542885b10b43e3d9aaf2073fd871e9eb9` |
| `refs/backup/2026-10-13/origin-gitpulse-main-red-lint-staticcheck-go127` | `9c64bad071d4a801e1f74bb850dd4cb61c5ff2f5` |

Bundle: `target/branch-backups/2026-10-13/gusset-all-refs.bundle` (2.9 MB). `git bundle verify` said the bundle is okay, records a complete history, and lists 24 refs (the backup refs, the six local branches as they were, both remote-tracking refs, `HEAD`, and the six tags). `.gitignore` ignores `target/`.

The harness refused a bundle outside the repository (`path.outside_root`) and refused one under `.git` (`path.restricted`). The durable copy is the `refs/backup/2026-10-13/` namespace. `cargo clean` can delete the bundle. Do not run `git gc` if you still want the dangling pre-rebase objects `git fsck` reported.

## Pending work

The worktree's only dirtiness was untracked `AUDIT_REPORT.md` and `BUILD_REPORT.md`. `BUILD_REPORT.md` says the audit file stays untracked. They were not committed. They are still untracked. No other worktree of this repository was dirty. No other agent's edits were staged.

## Merges

Style is `git merge --no-ff`, which matches AGENTS.md ("every change is a merge"). CONTRIBUTING.md does not ask for linear history. Each merge was clean. No file was resolved with `--ours` or `--theirs`. There was no conflict, so there was no conflict resolution for Opus to review.

| Order | Branch | Merge commit | Parents | `make ci-local` |
| --- | --- | --- | --- | --- |
| 1 | `w2-boundary` | `c7ecc3e7483742e42393c92c3cb0ddf08df7c9f0` | `73fbf32` `790967a` | first run failed staticcheck; after `ac0f83e`, exit 0 |
| 2 | `w2-adopter` | `3de3ba2ed3263398d370f0c8ca8d507c8e68838d` | `ac0f83e` `6d16320` | exit 0 |
| 3 | `w2-bench` | `2be41ca4e3cc2ffe0aec637954fc73cd9be2f678` | `3de3ba2` `94fa191` | exit 0 |
| 4 | `w2-platform` | `01afcd7e9ca3c141ab839cd7b400233b34d71584` | `2be41ca` `324921f` | exit 0 |
| 5 | `wave-w1` | `97842d914405ee0a91aa9d49d7a616f7adf50326` | `01afcd7` `e7400a4` | exit 0 |

`make ci-local` is `build`, `test`, `lint`, `docs-check`, then a 100ms bench of `./bench` and `alloc-gate`. Exit 0 means that whole target passed. Logs, gitignored under `target/`: `ci-local-w2-boundary.log`, `ci-local-w2-boundary-2.log`, `ci-local-w2-adopter.log`, `ci-local-w2-bench.log`, `ci-local-w2-platform.log`, `ci-local-wave-w1.log`, `ci-local-final.log`.

The first boundary run reached lint and failed:

- `internal/isolate/isolate_internal_test.go:1399` ST1008
- `internal/isolate/isolate_internal_test.go:1634` ST1008

`readQueuedFrames` and `readQueuedIDs` returned `(error, []result)`. Call sites were updated to `got, dead :=` and the functions now return `([]result, error)`. Commit `ac0f83e8a1e3c35d4f2988d7cf0ade17b3c1b9d5` (`test(isolate): return the reader error last so staticcheck ST1008 stays quiet, protecting I4.`). The second `ci-local` on that commit exited 0. This is the only commit that was not already on one of the branches.

`go generate` inside `make build` rewrites `internal/ffi/archive_hash.go` to the sha256 of the local `libgusset.a`. That digest was restored after each run and was not committed. The tracked hash is still the pre-merge value.

## Conflicts

None.

## Deleted

Deleted with `git branch -d` after each tip was an ancestor of `main` and a backup ref existed. `git branch -d` succeeded for all five. `-D` was not used. No worktree was removed: the only worktree is this checkout, which is on `main`, and `git worktree remove` was not used. No remote branch was deleted.

| Branch | Was | Backup ref |
| --- | --- | --- |
| `w2-boundary` | `790967ab20c3642398252de63221b40b9d945408` | `refs/backup/2026-10-13/w2-boundary` |
| `w2-adopter` | `6d16320ab758d1b449051cf00c257a2daeada8d9` | `refs/backup/2026-10-13/w2-adopter` |
| `w2-bench` | `94fa191678174bc6f72093ec3994b8db8e97caa5` | `refs/backup/2026-10-13/w2-bench` |
| `w2-platform` | `324921fe2d49f1bd5d2524d30824bf358c396890` | `refs/backup/2026-10-13/w2-platform` |
| `wave-w1` | `e7400a480e97c22e9d7c5deb6498615f0c1767d0` | `refs/backup/2026-10-13/wave-w1` |

## Kept

| Ref | Reason |
| --- | --- |
| `main` | Integration branch. Now `97842d9`. Not pushed. 96 commits ahead of `origin/main`. |
| `origin/main` | Remote-tracking ref. Remote deletion was not requested, and `main` was not pushed. |
| `origin/gitpulse/main-red-lint-staticcheck-go127` | Already contained in local `main` (`git cherry` empty, ancestor check exit 0). Remote deletion was not requested. |
| `v0.0.1`, `v0.0.2` | Release tags. |
| `archive/g6-rejected-b-a-c`, `archive/g6-rejected-bprime-recycle`, `archive/g6-rejected-inline`, `archive/g6-step0-takeid` | Archive tags, not topic branches. |
| `refs/backup/2026-10-13/*` | Restore points for this consolidation. |
| Separate clones under `~/.grok/worktrees/devtools-gusset/` | Not worktrees of this repository. Left untouched. |

## Blocked

None. No merge was aborted.

## Verify

Final `make ci-local` on `97842d914405ee0a91aa9d49d7a616f7adf50326` exited 0 (`target/ci-local-final.log`).

`git fsck` exited 0. No `error`, `missing`, or `unreachable` lines. 17 dangling commits and other dangling trees remain; none of them is a merged tip, a merge commit from this session, or `73fbf32`. They were already unreachable from the branch tips (earlier rebases). They were not deleted. `git gc` was not run.

GitPulse Insights after the deletes: branch `main`, one worktree, not detached, no operation in progress, live Claude 0, live Codex 0, `sessions_ok` true, collisions 0, `unscanned_worktrees` 0. The two untracked reports are still the only dirty files it counted. This report was written after that call, so the worktree now also has untracked `BRANCH_REPORT.md`.

## Restore

Recreate a deleted branch from its backup ref:

```
git branch w2-boundary refs/backup/2026-10-13/w2-boundary
git branch w2-adopter refs/backup/2026-10-13/w2-adopter
git branch w2-bench refs/backup/2026-10-13/w2-bench
git branch w2-platform refs/backup/2026-10-13/w2-platform
git branch wave-w1 refs/backup/2026-10-13/wave-w1
```

If those refs are gone and the bundle is still on disk:

```
git fetch target/branch-backups/2026-10-13/gusset-all-refs.bundle refs/backup/2026-10-13/w2-boundary:refs/heads/w2-boundary
git fetch target/branch-backups/2026-10-13/gusset-all-refs.bundle refs/backup/2026-10-13/w2-adopter:refs/heads/w2-adopter
git fetch target/branch-backups/2026-10-13/gusset-all-refs.bundle refs/backup/2026-10-13/w2-bench:refs/heads/w2-bench
git fetch target/branch-backups/2026-10-13/gusset-all-refs.bundle refs/backup/2026-10-13/w2-platform:refs/heads/w2-platform
git fetch target/branch-backups/2026-10-13/gusset-all-refs.bundle refs/backup/2026-10-13/wave-w1:refs/heads/wave-w1
```

Moving `main` back to the pre-merge tip drops the merge commits and the ST1008 fix from the branch. The commits stay reachable from the backup refs. This was not done:

```
git switch main
git reset --hard refs/backup/2026-10-13/main
```
