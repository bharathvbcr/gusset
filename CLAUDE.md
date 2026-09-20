<!-- Managed by devmap: keep this file in sync with the repo map. -->

# Agent Workspace Guide

Use `.devcouncil/repo_map.json` as the primary file index for this workspace.
Repo map: `.devcouncil/repo_map.json`
Code graph: `.devcouncil/graph/code_graph.json` (symbol-level; query with `devmap`).

Before you change anything, ask GitPulse Insights what else is live in this repository. `gitpulse_insights` names the other worktrees, the running agent sessions, uncommitted work, contended files and index health in one call. Then `gitpulse_collision_risk` before touching a file another worktree may hold, `gitpulse_active_changes` for what is in flight, `gitpulse_change_context` / `gitpulse_provenance` for what changed and why, and `gitpulse_ledger_events` for the recorded history. Its facets fail independently — check each `ok`, because a facet that could not scan is not a facet that came back clean.

Workflow for agents:
1. Run `devmap paths --json` and `devmap status --json` first. Generated state is per-worktree and is not copied by Git. Read `.devcouncil/repo_map.json` when present; if its location differs in this checkout, use the resolved `repo_map` path. If the store or map is missing, run `devmap build --manifest` from this worktree's root, then check status again. Never copy a sibling worktree's database.
2. Use the `files` list to resolve module ownership and nearby siblings.
3. Use `subsystems` for subsystem-level navigation.
4. In `subsystems`, use `entry_points` + `critical_files` for entry points and starting context.
5. Use `role_files` in `subsystems` for subsystem role buckets (tests, entry, api, models, services, config, docs, other). Each bucket is a capped **sample** for orientation, not an inventory — `role_file_counts` carries the real per-role total, and `files` is the complete list.
6. Use `neighbors` and `handoff_paths` in `subsystems` to follow cross-subsystem flow. `handoff_paths` names the ordered file pairs a subsystem reaches other subsystems through; both are capped, and `liveness_meta.subsystems` reports what was cut.
7. For dead code, run `devmap dead --json` and read each row's own `confidence` before acting — a high-confidence row is a parsed fact (no inbound evidence), a low one is unconfirmed (`only_ambiguous_callers`, unresolved namesake, or coverage-capped), and the command returns both rather than pre-filtering. `NoNamesake` sites are explained gaps in the unresolved ledger, not dead findings. Every answer also carries `walk_incomplete` when unattributed calls mean the list is a lower bound. Prefer `unwired_candidates` / `dead_symbol_candidates` in the map over `unreachable_files` (static BFS is noisy for routers, dynamic imports and JSX). If `entry_roots` is empty or `liveness_unreachable_unreliable` is set, ignore `unreachable_files` entirely. Check `unwired_candidates` before creating a new module — wire what you create into a real caller. Neither list can ever name prose, data, a lockfile, an environment file, a Terraform file, a package marker, a fixture tree, a shebang script or a tool config; how many were left out, and why, is in `liveness_meta.unwired.excluded_*`, so an empty list means nothing is unwired rather than that the filter swallowed the repository.
8. Prefer DevMap MCP tools (`devmap_explore`, `devmap_search`, `devmap_impact`, `devmap_trace`, `devmap_neighbors`, `devmap_dead_symbols`, `devmap_affected_tests`) or the matching `devmap` CLI commands: `devmap explore <name>`, `devmap search`, `devmap impact`, `devmap trace <a> <b>`, `devmap dead`, `devmap affected <target>`. Always pass `repo_path` (the absolute repository path) on every `devmap_*` call, and check `repository.root` in the envelope before trusting the answer — Cursor shares one MCP process across workspace tabs. Read `truncated` and `total` on every envelope before treating a list as complete. When DevMap cannot answer, record a gap in `.devcouncil/codeintel/sessions/gaps.jsonl` — do not switch indexes.
9. The store (`.devcouncil/codeintel/devmap.sqlite`) is canonical — prefer `devmap` commands when `.devcouncil/graph/code_graph.json` is missing or a size-capped stub. `devmap preview` asks what an unsaved edit would break before it is written.
10. Run `devmap build --manifest` after large refactors to refresh the database and exported maps, or `devmap serve` to keep the index warm; `devmap status` reports generation, counts and freshness.

This binds every agent working here, not only the one reading this file. When you delegate — a subagent, a worktree lane, a handoff plan — name DevMap and GitPulse Insights in the prompt with the same precedence, rather than leaving that agent to grep its way in. When either tool cannot answer (truncated, `walk_incomplete`, empty on a built index, or a capability it does not have), record the gap and say so; do not silently fall back to grep and report the result as if the graph had confirmed it.

Important surfaces:
1. `bench/seed/go/` — bench/seed/go: 4 files, mostly go, config (2 tests)
2. `bench/seed/esc/` — bench/seed/esc: 3 files, mostly go, config (1 tests)
3. `bench/seed/` — bench/seed: 10 files, mostly go, config, markdown (3 tests)
4. `bench/seed/rs/` — bench/seed/rs: 2 files, mostly config, rust
5. `bench/seed/rs/src/` — bench/seed/rs/src: 1 file, rust
6. `docs/` — docs: 1 file, markdown (1 docs)

## Repository hygiene

- Keep generated output in ignored, producer-owned directories. Use per-workspace Cargo targets and the native Go build cache; reuse valid caches instead of clearing them after every build.
- Before cleanup, measure the exact paths and check producer markers, the Git index, ignore rules, nested repositories, symlinks, active tasks, open files and retention. An unavailable or partial check is a refusal, not approval.
- Preserve source, uncommitted work, secrets, environments, dependencies, models, datasets, agent state and Git history. Never use home-wide rm sweeps or git clean -X as routine hygiene.
- Prefer Cargo automatic GC, Go automatic expiry, npm cache verify, uv cache prune and pnpm store prune for shared caches. Explain download and rebuild costs; never silently change global target/cache configuration.
- Agents must not enable or widen cleanup schedules. Only execute a user's saved scope and limits, with fresh checks immediately before mutation; stop when files, policy or activity change.
- Record what was measured, removed, skipped and failed, including incomplete scans and logical versus physical bytes. Cancellation does not restore entries already removed.

If the map and source disagree, trust the source and re-run `devmap build --manifest`.
