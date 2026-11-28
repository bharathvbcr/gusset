# Quickstart: 002 - Out-of-Process Crash Isolation

## Verification Commands

Validate this specification:

```bash
devcouncil spec validate 002-out-of-process-isolation --strict --json --project-root /Users/bharath/Code/devtools/gusset
```

Run the isolate suite (links the Rust archive, so build it first with `make build`; not run during authoring):

```bash
go -C /Users/bharath/Code/devtools/gusset test ./internal/isolate/
```

Read the design: `docs/ipc.md` (architecture, fault lifecycle, prototype notes) and `DECISIONS.md` entries dated 2026-10-08, 2026-10-23, 2026-10-24, 2026-11-04, 2026-11-06 to 2026-11-09.
