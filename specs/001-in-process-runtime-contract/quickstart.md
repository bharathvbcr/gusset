# Quickstart: 001 - In-Process Go-Rust Runtime Contract

## Verification Commands

Validate this specification:

```bash
devcouncil spec validate 001-in-process-runtime-contract --strict --json --project-root /Users/bharath/Code/devtools/gusset
```

Run the CI-wiring guards (pure Go, no Rust archive needed; passed during authoring):

```bash
go -C /Users/bharath/Code/devtools/gusset test ./tests/ci/ -count=1
```

Run the full local gate (builds the Rust archive and runs every suite; not run during authoring):

```bash
make -C /Users/bharath/Code/devtools/gusset ci-local
```

Adopt Gusset in another project: follow `docs/adoption.md`, which `tests/adopter/walkthrough.sh` executes end to end. Decide first whether Gusset fits at all with `docs/choosing.md`.
