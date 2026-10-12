#!/bin/bash
# Runs inside localhost/gusset-stress: copies /src (read-only worktree) to /work,
# builds libgusset for Linux, and writes fixed/prefix test binaries (plain and -race) to /out.
set -euo pipefail
mkdir -p /work
tar -C /src --exclude=./target --exclude=./.git -cf - . | tar -C /work -xf -
cd /work
cargo build --release -p gusset 2>&1 | tail -1
go test -c -o /out/fixed.test . 
go test -race -c -o /out/fixed-race.test .
sed -i 's/func quietAt(procs int) bool { return procs >= quietMinProcs }/func quietAt(procs int) bool { return procs > 1 }/' drain.go
grep -n 'func quietAt' drain.go
go test -c -o /out/prefix.test .
go test -race -c -o /out/prefix-race.test .
ls -la /out
