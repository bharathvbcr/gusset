#!/bin/bash
# Builds baseline.test: the worktree plus the no-Calls sleeper probe.
set -euo pipefail
mkdir -p /work
tar -C /src --exclude=./target --exclude=./.git -cf - . | tar -C /work -xf -
cd /work
cp /out/noload_probe_internal_test.go .
cargo build --release -p gusset 2>&1 | tail -1
go test -c -o /out/baseline.test .
