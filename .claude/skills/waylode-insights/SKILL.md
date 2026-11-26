---
name: waylode-insights
description: Read Waylode repository insights over MCP — worktrees, agent sessions, uncommitted changes, overlapping dirty files, ledger verdicts, and code-graph availability. Use when an agent needs a honest snapshot of a checkout before editing, when multiple worktrees or coding agents are in flight, or when the user asks what is happening in a Waylode-managed repo.
license: MIT
compatibility: Requires the waylode-mcp binary on PATH and an absolute git repository path.
metadata:
  mcp-protocol: "2026-07-28"
  agent-plugins: "1.0.0"
---

# Waylode insights

Waylode's MCP server is **read-only**. It never checks out a branch, writes a file, or takes a task lease. Ask it what is true, then mutate through the agent that already holds the writer lease.

It surfaces Waylode-owned facts and selected **DevCouncil** modules (in-process DevMap). Policy and agent hosting go through **Manvi**, not this server.

## Before you edit

1. Call `waylode_insights` with the absolute `repo_path`.
2. Read every facet's `ok` / `error` / `available` field. An empty list with `ok: false` means the check did not run — not that the repository is clean.
3. If `collisions.overlapping_files > 0`, or `collisions.unscanned_worktrees > 0`, or `collisions.shared_worktree_files > 0`, call `waylode_collision_risk` and `waylode_change_context` for the worktree you are about to touch.
4. Prefer `waylode_change_context` over guessing the branch, dirty files, or parked merge/rebase of a worktree.

## Tool map

| Tool | When |
| --- | --- |
| `waylode_insights` | First look at a repo |
| `waylode_change_context` | About to edit one worktree |
| `waylode_collision_risk` | Parallel agents or several worktrees |
| `waylode_active_changes` | Need the file list, not just counts |
| `waylode_status` | Narrower status (ledger + worktrees + codeintel) |
| `waylode_ledger_events` | Policy verdicts and recorded mutations |
| `waylode_task_view` | DevCouncil tasks and leases, when a store exists |
| `waylode_codeintel_*` | Symbol search, impact, dependencies, trace, dead symbols |
| `waylode_provenance` | Verification freshness for a commit |

## Honesty rules this server already enforces

- Unscanned worktrees are counted, never implied clean.
- A missing code graph is `available: false` with a reason, not an empty hit list.
- Agent worktrees are recognised from `/.<agent>/worktrees/` layout, never from a branch name.
- `agents.ok: false` is "the worktree listing failed", not "no agents"; `agents.truncated: true` means the counts came from a capped sample, so `agents.sessions` is a floor and a kind whose worktrees all fall past the cap is absent from `agents.kinds` rather than undercounted.
- An agent worktree is a *checkout*, not a running process: this server reports the sessions a repository has on disk, and cannot tell one still running from one abandoned mid-change.

Pass `repo_path` as an absolute filesystem path on every call. The protocol is MCP `2026-07-28`: include `_meta.io.modelcontextprotocol/protocolVersion` and `clientCapabilities` on each request. Legacy `initialize` still works for older clients.
