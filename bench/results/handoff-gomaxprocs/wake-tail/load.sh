#!/bin/bash
# load.sh N SECONDS: N busy loops for SECONDS, then exit; killed on any signal.
n=$1; secs=$2; pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null; exit 0; }
trap cleanup INT TERM HUP EXIT
for ((i=0;i<n;i++)); do yes > /dev/null & pids+=($!); done
sleep "$secs" & wait $!
