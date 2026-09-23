## Summary of Changes

A clear and concise description of the changes made and the problem solved.

## Invariant Protection

Which Invariant (I1–I6) or Rule (R1–R16) does this change protect?
- [ ] Invariant: 
- [ ] Rule: 

## PR Checklist

- [ ] `make test`: All 20 Rust tests and Go test suites pass.
- [ ] `make lint`: `cargo clippy`, `cargo fmt`, `go vet`, and `gussetvet` pass cleanly.
- [ ] `make docs-check`: Documentation matches committed `benchstat` results (Rule R15).
- [ ] If fixing a bug, a failing test was added in `tests/panic_zoo/` or `tests/pitfalls/` proving the bug before the fix.
- [ ] The public surface remains strictly at 15 Rust exports and <= 12 Go entry points (or an entry has been added to `DECISIONS.md`).
- [ ] No `//go:linkname`, `asmcgocall`, or internal Go runtime hacks.
