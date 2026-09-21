#!/bin/sh
# record.sh — record benchmark arms into one results file, or record nothing.
#
# This is a script rather than a Makefile recipe because the thing it has to get
# right cannot be expressed in one: a lock held across several commands, a trap
# that survives an interrupt, and an output file that is only assembled once
# every arm has passed its checks. Make runs each recipe line in its own shell,
# so a trap set on one line is gone by the next.
#
# What went wrong, and why each guard below exists:
#
# Two recordings ran at once and both appended to the same file through
# `tee -a`. (They came from two different working sessions on the same
# checkout, which is the ordinary way this happens and needs no mishap to
# explain it.) Because `go test` prints a benchmark's name, then runs it, then
# prints its numbers onto that same line, the second writer spliced itself
# between the two.
# 38 of the 163 rows in the resulting file carried a metric the named benchmark
# never reports; one claimed a no-op cgo call took 91,916 ns. Meanwhile the two
# runs competed for the same 18 cores, so even the structurally intact rows were
# wrong: a serial arm that reads 7.2 us alone read 46 us beside the other run.
# Nothing downstream could tell. The file went on to draw charts.
#
# The guards, in the order they run:
#
#   1. A lock, so a second recording refuses instead of interleaving. The lock
#      records its process group, so an abandoned run is reported with the exact
#      pgid to kill rather than silently blocking forever.
#   2. A preflight and a between-arms check for any other Go benchmark process
#      on the machine — ours or somebody else's.
#   3. CPU idle sampled before and after. Load average is useless here: macOS
#      reported 62.9 on an 18-core machine that was 80% idle.
#   4. Per-arm temporary files, assembled at the end. Nothing appends to the
#      published path, so interleaving is structurally impossible rather than
#      merely checked for.
#   5. A provenance header naming what was checked. tools/benchplot refuses a
#      file without one, which is the part that matters: a check that could not
#      run must not look like a check that ran and passed.
#
# The signal trap is defence in depth rather than a response to a diagnosed
# fault: whether an interrupted run here ever orphaned its arm loop was never
# established, but a recording that leaves a benchmark running behind it is a
# recording that poisons the next one, and the trap costs nothing.
#
# A run that fails any guard writes no output at all. The previous file is left
# untouched, because a stale honest measurement beats a fresh contaminated one.
#
# Usage:
#   bench/record.sh OUT BENCHTIME COUNT ARM...
#
#   OUT        published results path, written only on success
#   BENCHTIME  passed to -benchtime (e.g. 1s, 5x)
#   COUNT      passed to -count
#   ARM...     one -bench regex per arm; each runs in its own process

set -eu

if [ $# -lt 4 ]; then
	echo "usage: $0 OUT BENCHTIME COUNT ARM..." >&2
	exit 2
fi

OUT=$1
BENCHTIME=$2
COUNT=$3
shift 3

# Which module and package the arms live in. The crossover and scaling arms are
# in bench/seed/go, which is a separate module because Go forbids cgo in
# _test.go files; `make bench` records the main module's own benchmarks instead.
RECORD_DIR=${RECORD_DIR:-bench/seed/go}
RECORD_PKG=${RECORD_PKG:-.}

LOCK=bench/results/.record.lock
TMP=

# MIN_IDLE is the CPU idle percentage below which a recording is refused.
#
# Not a round number for its own sake: the clean reference runs measured 74.8%,
# 73.4% and 80.5% idle on a desktop with an IDE and a browser open, which is the
# realistic floor for "quiet" on a machine somebody actually uses. The
# contaminated run that started this was at 0.9%.
MIN_IDLE=60

# ---------------------------------------------------------------------------

cleanup_exit() {
	status=$?
	# An `if`, not `[ -n "$TMP" ] && rm ...`: with `set -e` the && form returns
	# 1 when TMP is unset, which aborts the handler and replaces a clean exit
	# status with a failure.
	if [ -n "$TMP" ]; then
		rm -rf "$TMP"
	fi
	# Only release the lock if this process is the one holding it. A run that
	# refused because somebody else held the lock must not delete their lock.
	if [ -f "$LOCK/pgid" ] && [ -f "$LOCK/owner" ]; then
		if [ "$(cat "$LOCK/owner")" = "$$" ]; then
			rm -rf "$LOCK"
		fi
	fi
	exit $status
}

cleanup_signal() {
	echo "" >&2
	echo "record: interrupted — killing this recording's process group so no arm" >&2
	echo "        is left running to contaminate the next run" >&2
	trap - INT TERM
	# The whole process group: this script, the make that invoked it, and any
	# live `go test`. An orphaned arm loop is exactly the failure this file
	# exists to prevent, and it cannot be prevented by killing only the child.
	kill -TERM 0 2>/dev/null || true
	exit 130
}

trap cleanup_exit EXIT
trap cleanup_signal INT TERM

# cpu_idle prints the CPU idle percentage, or nothing at all if it cannot
# measure it. Printing nothing is deliberate: an unmeasured machine must not be
# recorded as a quiet one.
cpu_idle() {
	case "$(uname -s)" in
	Darwin)
		# Two samples: the first is since boot and says nothing about now.
		top -l 2 -n 0 -s 1 2>/dev/null |
			awk '/^CPU usage/ { gsub("%",""); v=$(NF-1) } END { if (v != "") print v }'
		;;
	Linux)
		awk '/^cpu /{i1=$5; t1=0; for(j=2;j<=NF;j++) t1+=$j}
		     END{print i1"\t"t1}' /proc/stat > "$TMP/stat1"
		sleep 1
		awk '/^cpu /{i2=$5; t2=0; for(j=2;j<=NF;j++) t2+=$j}
		     END{print i2"\t"t2}' /proc/stat > "$TMP/stat2"
		awk 'NR==1{i1=$1;t1=$2} NR==2{i2=$1;t2=$2}
		     END{ if (t2>t1) printf "%.2f\n", 100*(i2-i1)/(t2-t1) }' \
			"$TMP/stat1" "$TMP/stat2"
		;;
	esac
}

# foreign_benchmarks lists Go benchmark processes running on this machine.
#
# Matches any compiled test binary invoked with -test.bench, which is what a
# `go test -bench` run looks like no matter which module started it. Somebody
# else's benchmark competes for the same cores as effectively as our own.
foreign_benchmarks() {
	ps -Ao pid,command |
		grep -E '\.test( |$).*-test\.bench' |
		grep -v grep || true
}

require_exclusive() {
	when=$1
	foreign_benchmarks > "$TMP/foreign"
	if [ -s "$TMP/foreign" ]; then
		echo "record: refusing to record — another Go benchmark is running ($when):" >&2
		cut -c1-140 < "$TMP/foreign" >&2
		echo "record: wait for it, or kill its process group, then re-run." >&2
		exit 1
	fi
}

# settle_idle waits, briefly and boundedly, for the machine to go quiet before
# giving up on it.
#
# The preflight runs immediately after `make` has finished two cargo builds, so
# the first sample catches their tail: the very first run of this script refused
# at 0.0% idle on a machine that was 90% idle four seconds later. Refusing there
# is a correct measurement of the wrong moment, and it throws away a seven-minute
# target for a transient.
#
# Bounded, because waiting forever would turn "the machine is busy" into "the
# recording hangs", and a machine that is still loaded after a minute is loaded,
# not settling. Prints the last reading either way, so the caller still refuses
# on a machine that never quietens.
settle_idle() {
	tries=0
	value=$(cpu_idle)
	while [ "$tries" -lt 10 ]; do
		if [ -z "$value" ]; then
			break
		fi
		if [ "$(awk -v v="$value" -v m="$MIN_IDLE" 'BEGIN{print (v<m)?0:1}')" = "1" ]; then
			break
		fi
		if [ "$tries" = "0" ]; then
			echo "record: ${value}% idle — waiting for the machine to settle" >&2
		fi
		sleep 5
		tries=$((tries + 1))
		value=$(cpu_idle)
	done
	echo "$value"
}

require_idle() {
	when=$1
	value=$2
	if [ -z "$value" ]; then
		echo "record: could not measure CPU idle ($when) on $(uname -s)." >&2
		echo "record: refusing rather than recording a machine whose load is unknown." >&2
		exit 1
	fi
	if [ "$(awk -v v="$value" -v m="$MIN_IDLE" 'BEGIN{print (v<m)?1:0}')" = "1" ]; then
		echo "record: refusing to record — CPU was ${value}% idle ($when), below ${MIN_IDLE}%." >&2
		echo "record: the numbers would describe the load, not the code." >&2
		exit 1
	fi
}

# ---------------------------------------------------------------------------
# 1. Lock.

mkdir -p bench/results
if ! mkdir "$LOCK" 2>/dev/null; then
	held_pgid=""
	[ -f "$LOCK/pgid" ] && held_pgid=$(cat "$LOCK/pgid")
	if [ -n "$held_pgid" ] && kill -0 "-$held_pgid" 2>/dev/null; then
		echo "record: another recording is in progress (process group $held_pgid)." >&2
		echo "record: wait for it, or stop it with:  kill -TERM -$held_pgid" >&2
		exit 1
	fi
	echo "record: clearing a lock left by an abandoned run (process group ${held_pgid:-unknown})" >&2
	rm -rf "$LOCK"
	mkdir "$LOCK"
fi
echo "$$" > "$LOCK/owner"
# ps is the portable way to ask for our own process group; $$ is the pid, and
# the two differ exactly when it matters (make invoked us).
ps -o pgid= -p $$ | tr -d ' ' > "$LOCK/pgid"

TMP=$(mktemp -d)

# ---------------------------------------------------------------------------
# 2-3. Preflight.

require_exclusive "before starting"
IDLE_BEFORE=$(settle_idle)
require_idle "before starting" "$IDLE_BEFORE"
echo "record: preflight ok — ${IDLE_BEFORE}% idle, no other benchmark running"

# ---------------------------------------------------------------------------
# 3b. A discarded warm-up, so the first measured arm is not the cold one.
#
# An idle, exclusive machine is not yet a machine running at a steady clock.
# Recorded here on an idle M5 Pro (87.9% -> 90.0% idle, exclusive), each arm
# calibrating the *same* deterministic loop in its own process, in the order the
# arms were recorded:
#
#   1. Serial/RawCgo    1000ns   11us   113us   1048us
#   2. Serial/Gusset     917ns    7us    73us    751us
#   3. Parallel/RawCgo   833ns    6us    69us    737us
#   4. Parallel/Gusset   709ns    6us    69us    696us
#
# Monotonic in recording order at every one of the four work sizes. That is a
# frequency ramp, not noise — both Apple silicon and modern x86 clock up under
# sustained load, and the first process pays the cold start. It is not small:
# the serial blocking-cgo baseline read 1.5x slow, which inverts the comparison
# the whole recording exists to make, and is the shape behind a committed claim
# that Gusset was "24% faster" than the cgo call it wraps.
#
# Idle and exclusivity checks cannot see this; both passed on that recording.
# The warm-up runs the first arm and throws the numbers away: only its effect on
# the clock is wanted. It is deliberately not a `-benchtime=1x` token run, which
# would finish before the ramp it exists to trigger.
echo "record: warm-up (discarded) — $1"
go test -C "$RECORD_DIR" -run '^$' -bench "$1" \
	-benchtime=1s -count=1 "$RECORD_PKG" > "$TMP/warmup" 2>&1 || true

# ---------------------------------------------------------------------------
# 4. One process per arm, into per-arm temporary files.

n=0
for arm in "$@"; do
	n=$((n + 1))
	if [ "$n" -gt 1 ]; then
		require_exclusive "between arms, before $arm"
	fi
	echo "record: arm $n/$# — $arm"
	if ! go test -C "$RECORD_DIR" -run '^$' -bench "$arm" \
		-benchtime="$BENCHTIME" -count="$COUNT" "$RECORD_PKG" > "$TMP/arm.$n" 2>&1; then
		echo "record: arm $arm failed; no results written" >&2
		cut -c1-200 < "$TMP/arm.$n" >&2
		exit 1
	fi
	grep -E '^Benchmark' "$TMP/arm.$n" | cut -c1-120 || true
done

# ---------------------------------------------------------------------------
# Postflight.

# No settle here, deliberately. The point of the closing reading is to catch
# load that arrived *during* the run, and waiting for it to disappear is exactly
# how that evidence would be lost. cpu_idle's second sample is taken a second
# after the last arm exits, which is enough for our own benchmark's tail to
# drain without hiding somebody else's work.
require_exclusive "after the last arm"
IDLE_AFTER=$(cpu_idle)
require_idle "after the last arm" "$IDLE_AFTER"

# ---------------------------------------------------------------------------
# 5. Assemble, header first. Written to a temporary file and moved into place,
# so an interrupt during assembly cannot leave a half-written results file
# looking like a complete one.

{
	echo "# gusset-bench: v1"
	echo "# gusset-bench-recorded: $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	echo "# gusset-bench-host: $(uname -n)"
	echo "# gusset-bench-uname: $(uname -srm)"
	echo "# gusset-bench-package: $RECORD_DIR $RECORD_PKG"
	echo "# gusset-bench-arms: $*"
	echo "# gusset-bench-benchtime: $BENCHTIME"
	echo "# gusset-bench-count: $COUNT"
	echo "# gusset-bench-cpu-idle-before: $IDLE_BEFORE"
	echo "# gusset-bench-cpu-idle-after: $IDLE_AFTER"
	echo "# gusset-bench-min-idle: $MIN_IDLE"
	echo "# gusset-bench-exclusive: yes"
	i=0
	while [ "$i" -lt "$n" ]; do
		i=$((i + 1))
		cat "$TMP/arm.$i"
	done
} > "$TMP/out"

mkdir -p "$(dirname "$OUT")"
mv "$TMP/out" "$OUT"
echo "record: wrote $OUT (${IDLE_BEFORE}% -> ${IDLE_AFTER}% idle, $n arm(s), exclusive)"
