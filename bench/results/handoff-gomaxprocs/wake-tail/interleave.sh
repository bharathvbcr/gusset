#!/bin/bash
# interleave.sh OUT N binA binB ... : N rounds, each binary once per round, load logged per round.
out=$1; n=$2; shift 2
for ((r=1;r<=n;r++)); do
  echo "round $r load $(sysctl -n vm.loadavg)" >> "$out"
  for b in "$@"; do
    res=$("$b" -test.run 'TestSerialCall_LeavesAPForOtherGoroutines$' -test.count=1 -test.v 2>&1)
    st=$(echo "$res" | rg -o -- '--- (PASS|FAIL)' | head -1)
    echo "$(basename $b) $st | $(echo "$res" | rg -o 'n=.*' | head -1)" >> "$out"
  done
done
echo "end load $(sysctl -n vm.loadavg)" >> "$out"
