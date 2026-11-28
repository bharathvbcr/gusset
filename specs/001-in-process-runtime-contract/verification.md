# Verification Plan: 001 - In-Process Go-Rust Runtime Contract

## Deterministic Verification Matrix

| Check | Command | Expected Outcome | Run during authoring |
| :--- | :--- | :--- | :--- |
| Spec schema and strictness | `devcouncil spec validate 001-in-process-runtime-contract --strict --json --project-root /Users/bharath/Code/devtools/gusset` | `valid: true`, no blocking findings | yes |
| CI wiring guards | `go -C /Users/bharath/Code/devtools/gusset test ./tests/ci/ -count=1` | `ok` | yes, passed |
| Export list | `wc -l /Users/bharath/Code/devtools/gusset/internal/ffi/exports.txt` | 17 | yes |
| Panic zoo | `go -C /Users/bharath/Code/devtools/gusset test ./tests/panic_zoo/` | all seven tests pass | no (needs the Rust archive) |
| ABI, header and exports match | `cargo test --locked --workspace` in the repository root | passes | no |
| Full local gate | `make ci-local` | exit 0 | no |
| Spec gate | `make check-specs` | exit 0 | see `specs/README.md` |

## Not Verified Here

Linux, musl, race, `cgocheck2` and AddressSanitizer legs run only in `.github/workflows/matrix.yml` and `nightly.yml`. Their current state was not checked.
