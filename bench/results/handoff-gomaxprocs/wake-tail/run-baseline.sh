#!/bin/bash
# run-baseline.sh OUT ROUNDS: per round, the no-Calls sleeper, then fixed and prefix beside Calls.
out=/out/$1; n=$2; : > "$out"
echo "nproc $(nproc)" >> "$out"
for ((r=1;r<=n;r++)); do
  echo "round $r loadavg $(cut -d' ' -f1-3 /proc/loadavg)" >> "$out"
  for v in baseline:TestProbe_SleeperWithoutCalls fixed:TestSerialCall_LeavesAPForOtherGoroutines prefix:TestSerialCall_LeavesAPForOtherGoroutines; do
    b=${v%%:*}; tn=${v#*:}
    res=$(/out/$b.test -test.run "$tn\$" -test.count=1 -test.v 2>&1)
    st=$(echo "$res" | grep -oE -- '--- (PASS|FAIL)' | head -1)
    echo "$b $st | $(echo "$res" | grep -oE 'n=.*' | head -1)" >> "$out"
  done
done
