# Gusset

The runtime contract for running a Rust engine inside a Go service: panic firewall, bounded concurrency, timeouts, poisoned handles, ABI verification, allocator accounting, and the CI matrix that proves them. Not a binding generator, not a cgo-free calling path, not an IPC transport.

Status: Phase 0 (seed). Start with `AGENTS.md`, then `docs/PLAN.md` and `DECISIONS.md`. The Phase 0 benchmark and firewall repro are in `bench/seed`.

License: MIT OR Apache-2.0.
