#!/bin/bash
# run.sh OUT ROUNDS: interleaved runs of each variant, one process per run.
out=/out/$1; n=$2; : > "$out"
echo "nproc $(nproc) GOMAXPROCS-default $(go env GOMAXPROCS 2>/dev/null) cpu.max $(cat /sys/fs/cgroup/cpu.max 2>/dev/null)" >> "$out"
for ((r=1;r<=n;r++)); do
  echo "round $r loadavg $(cut -d' ' -f1-3 /proc/loadavg)" >> "$out"
  for v in fixed prefix fixed-race prefix-race fixed:GOGC=1 prefix:GOGC=1; do
    b=${v%%:*}; env=""; [[ $v == *:* ]] && env=${v#*:}
    res=$(env $env /out/$b.test -test.run 'TestSerialCall_LeavesAPForOtherGoroutines$' -test.count=1 -test.v 2>&1)
    st=$(echo "$res" | grep -oE -- '--- (PASS|FAIL)' | head -1)
    echo "$v $st | $(echo "$res" | grep -oE 'n=.*' | head -1)" >> "$out"
  done
done
