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
#   1. A lock, so a second recording refuses instead of interleaving. Owner and
#      process group are written in a private directory and renamed into place,
#      so the lock is not visible until both exist. A lock with no pgid is held
#      for a grace period rather than stolen. An abandoned run is reported with
#      the exact pgid to kill.
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
# recording that poisons the next one. INT and TERM kill only the child
# process group this script started (never this script's own group), wait for
# it, then the EXIT trap removes the lock and the temporary directory and the
# script exits 130. Signalling group 0 killed the script before that cleanup
# and left the lock held, status 143.
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
#
# Two optional environment variables, both off by default:
#
#   RECORD_CPU       passed to -cpu (e.g. 2,3,4,8). Each benchmark runs once per
#                    GOMAXPROCS value and carries it as its -N name suffix.
#   RECORD_VARIANTS  space-separated NAME=DIR pairs: builds of the same
#                    benchmarks to compare, one checkout each, with DIR in place
#                    of RECORD_DIR. COUNT becomes a number of rounds, and each
#                    round runs every arm once (-count=1) in every variant.
#                    Variant order alternates by round (round 1 A then B,
#                    round 2 B then A), so a drift in the machine's clock or
#                    load cannot land only on the variant that always runs
#                    last. The provenance header records the order each round
#                    used. The output is still grouped by arm, then by variant,
#                    with each block behind a `variant: NAME` configuration
#                    line, which is what `benchstat -col variant` splits on.

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
RECORD_CPU=${RECORD_CPU:-}
RECORD_VARIANTS=${RECORD_VARIANTS:-}

# Checked before the lock is taken, so a typo costs nothing. A variant whose
# directory is missing would otherwise fail in its first round, after the other
# variants had already spent their time.
WARM_DIR=$RECORD_DIR
if [ -n "$RECORD_VARIANTS" ]; then
	WARM_DIR=
	for v in $RECORD_VARIANTS; do
		name=${v%%=*}
		dir=${v#*=}
		if [ "$name" = "$v" ] || ! printf '%s\n' "$name" | grep -Eq '^[a-z][a-z0-9_-]*$'; then
			echo "record: RECORD_VARIANTS entry '$v' is not NAME=DIR with a lower-case NAME" >&2
			exit 2
		fi
		if [ ! -d "$dir" ]; then
			echo "record: variant $name: no directory $dir" >&2
			exit 2
		fi
		if [ -z "$WARM_DIR" ]; then
			WARM_DIR=$dir
		fi
	done
fi
if [ -n "$RECORD_CPU" ] && ! printf '%s\n' "$RECORD_CPU" | grep -Eq '^[1-9][0-9]*(,[1-9][0-9]*)*$'; then
	echo "record: RECORD_CPU '$RECORD_CPU' is not a comma-separated list of positive integers" >&2
	exit 2
fi

LOCK=bench/results/.record.lock
TMP=

# A published lock has both owner and pgid. A directory with no pgid (a crash
# before the rename, or the old mkdir-then-write window) is not treated as
# abandoned until this many seconds have passed.
LOCK_GRACE_SECONDS=2

# Children of this script run in their own process group. The signal trap
# kills that group by the pgid recorded here, never this script's group.
ACTIVE_CHILD_PID=
ACTIVE_CHILD_PGID=
SELF_PGID=

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

# stop_active_child kills the child process group this script started, then
# waits for it. It never signals this script's own group.
stop_active_child() {
	pid=$ACTIVE_CHILD_PID
	pgid=$ACTIVE_CHILD_PGID
	if [ -z "$pid" ]; then
		return 0
	fi
	if [ -n "$pgid" ] && [ "$pgid" != "$SELF_PGID" ]; then
		kill -TERM "-$pgid" 2>/dev/null || true
	else
		kill -TERM "$pid" 2>/dev/null || true
	fi
	i=0
	while [ "$i" -lt 50 ]; do
		if ! kill -0 "$pid" 2>/dev/null; then
			break
		fi
		sleep 0.1
		i=$((i + 1))
	done
	if kill -0 "$pid" 2>/dev/null; then
		if [ -n "$pgid" ] && [ "$pgid" != "$SELF_PGID" ]; then
			kill -KILL "-$pgid" 2>/dev/null || true
		else
			kill -KILL "$pid" 2>/dev/null || true
		fi
	fi
	wait "$pid" 2>/dev/null || true
	ACTIVE_CHILD_PID=
	ACTIVE_CHILD_PGID=
	return 0
}

cleanup_signal() {
	# Disarm before stopping children so a second signal cannot re-enter, and
	# so the child's death cannot be mistaken for a new interrupt.
	trap - INT TERM
	echo "" >&2
	echo "record: interrupted — stopping this recording's child processes so no arm" >&2
	echo "        is left running to contaminate the next run" >&2
	stop_active_child
	# EXIT runs cleanup_exit, which removes $TMP and this run's lock.
	exit 130
}

trap cleanup_exit EXIT
trap cleanup_signal INT TERM

# start_isolated runs "$@" in a new process group and records its pid.
# setsid(1) is not on macOS; perl and python3 are the fallbacks. The child
# must not stay in this script's group: killing that group is what used to
# take the script down before the EXIT trap.
start_isolated() {
	if command -v setsid >/dev/null 2>&1; then
		setsid "$@" &
	elif command -v perl >/dev/null 2>&1; then
		perl -MPOSIX -e 'POSIX::setsid() or die "setsid: $!\n"; exec @ARGV or die "exec: $!\n"' -- "$@" &
	elif command -v python3 >/dev/null 2>&1; then
		python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' "$@" &
	else
		echo "record: cannot isolate child processes (need setsid, perl, or python3)" >&2
		exit 1
	fi
	ACTIVE_CHILD_PID=$!
	ACTIVE_CHILD_PGID=
}

# confirm_pgid records the child's pgid once it is no longer ours. A child
# that has already exited, or that never left this group, leaves the pgid
# empty so the trap kills that pid only.
confirm_pgid() {
	ACTIVE_CHILD_PGID=
	i=0
	while [ "$i" -lt 50 ]; do
		if [ -z "$ACTIVE_CHILD_PID" ] || ! kill -0 "$ACTIVE_CHILD_PID" 2>/dev/null; then
			return 0
		fi
		g=$(ps -o pgid= -p "$ACTIVE_CHILD_PID" 2>/dev/null | tr -d ' ' || true)
		if [ -n "$g" ] && [ "$g" != "$SELF_PGID" ]; then
			ACTIVE_CHILD_PGID=$g
			return 0
		fi
		i=$((i + 1))
		if [ "$i" -eq 10 ] || [ "$i" -eq 30 ]; then
			sleep 0.05
		fi
	done
	return 0
}

# spawn_group runs one child in its own process group and waits for it.
spawn_group() {
	start_isolated "$@"
	confirm_pgid
	rc=0
	wait "$ACTIVE_CHILD_PID" || rc=$?
	ACTIVE_CHILD_PID=
	ACTIVE_CHILD_PGID=
	return "$rc"
}

record_sleep() {
	spawn_group sleep "$1" || true
}

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
		record_sleep 1
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
		record_sleep 5
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
#
# publish_lock renames a directory that already contains owner and pgid onto
# $LOCK. The rename is the publication: a racer either sees no lock or sees
# both files. `mv` onto an existing directory would nest the stage inside it,
# so a publish only counts when the top-level owner is this process.

SELF_PGID=$(ps -o pgid= -p $$ | tr -d ' ' || true)
if [ -z "$SELF_PGID" ]; then
	echo "record: could not read this process's process group" >&2
	exit 1
fi

publish_lock() {
	stage=$(mktemp -d "${LOCK}.stage.XXXXXX")
	echo "$$" > "$stage/owner"
	printf '%s\n' "$SELF_PGID" > "$stage/pgid"
	if [ ! -s "$stage/owner" ] || [ ! -s "$stage/pgid" ]; then
		rm -rf "$stage"
		echo "record: could not write lock owner and process group" >&2
		exit 1
	fi
	if [ -e "$LOCK" ]; then
		rm -rf "$stage"
		return 1
	fi
	if ! mv "$stage" "$LOCK" 2>/dev/null; then
		rm -rf "$stage"
		return 1
	fi
	owner_now=
	if [ -f "$LOCK/owner" ]; then
		owner_now=$(cat "$LOCK/owner" 2>/dev/null || true)
	fi
	if [ "$owner_now" = "$$" ] && [ -s "$LOCK/pgid" ]; then
		return 0
	fi
	base=${stage##*/}
	if [ -n "$base" ] && [ -d "$LOCK/$base" ]; then
		rm -rf "$LOCK/$base"
	fi
	rm -rf "$stage" 2>/dev/null || true
	return 1
}

lock_pgid() {
	held_pgid=
	if [ -s "$LOCK/pgid" ]; then
		held_pgid=$(cat "$LOCK/pgid" 2>/dev/null || true)
	fi
	printf '%s\n' "$held_pgid"
}

refuse_live_lock() {
	echo "record: another recording is in progress (process group $1)." >&2
	echo "record: wait for it, or stop it with:  kill -TERM -$1" >&2
	exit 1
}

acquire_lock() {
	mkdir -p "$(dirname "$LOCK")"
	while true; do
		if publish_lock; then
			return 0
		fi
		held_pgid=$(lock_pgid)
		if [ -n "$held_pgid" ] && kill -0 "-$held_pgid" 2>/dev/null; then
			refuse_live_lock "$held_pgid"
		fi
		if [ -z "$held_pgid" ]; then
			echo "record: lock has no pgid — not stealing it for ${LOCK_GRACE_SECONDS}s" >&2
			waited=0
			while [ "$waited" -lt "$LOCK_GRACE_SECONDS" ]; do
				record_sleep 1
				waited=$((waited + 1))
				held_pgid=$(lock_pgid)
				if [ -n "$held_pgid" ]; then
					break
				fi
			done
			if [ -n "$held_pgid" ] && kill -0 "-$held_pgid" 2>/dev/null; then
				refuse_live_lock "$held_pgid"
			fi
		fi
		echo "record: clearing a lock left by an abandoned run (process group ${held_pgid:-unknown})" >&2
		rm -rf "$LOCK"
	done
}

acquire_lock

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
CPU_FLAG=
if [ -n "$RECORD_CPU" ]; then
	CPU_FLAG=-cpu=$RECORD_CPU
fi

echo "record: warm-up (discarded) — $1"
spawn_group go test -C "$WARM_DIR" -run '^$' -bench "$1" ${CPU_FLAG:+"$CPU_FLAG"} \
	-benchtime=1s -count=1 "$RECORD_PKG" > "$TMP/warmup" 2>&1 || true

# ---------------------------------------------------------------------------
# 4. One process per arm, into per-arm temporary files.
#
# $TMP/parts lists those files in the order they are assembled.

# run_arm DIR ARM COUNT: one benchmark process into $TMP/run, or no results.
run_arm() {
	if ! spawn_group go test -C "$1" -run '^$' -bench "$2" ${CPU_FLAG:+"$CPU_FLAG"} \
		-benchtime="$BENCHTIME" -count="$3" "$RECORD_PKG" > "$TMP/run" 2>&1; then
		echo "record: arm $2 failed in $1; no results written" >&2
		cut -c1-200 < "$TMP/run" >&2
		exit 1
	fi
	grep -E '^Benchmark' "$TMP/run" | cut -c1-120 || true
}

: > "$TMP/parts"
n=0
if [ -z "$RECORD_VARIANTS" ]; then
	for arm in "$@"; do
		n=$((n + 1))
		if [ "$n" -gt 1 ]; then
			require_exclusive "between arms, before $arm"
		fi
		echo "record: arm $n/$# — $arm"
		run_arm "$RECORD_DIR" "$arm" "$COUNT"
		mv "$TMP/run" "$TMP/arm.$n"
		echo "$TMP/arm.$n" >> "$TMP/parts"
	done
else
	n=$#
	runs=0
	round=0
	idx=0
	for v in $RECORD_VARIANTS; do
		idx=$((idx + 1))
		printf '%s\n' "$idx" > "$TMP/vindex.${v%%=*}"
	done
	: > "$TMP/arms"
	for arm in "$@"; do
		printf '%s\n' "$arm" >> "$TMP/arms"
	done
	: > "$TMP/roundorder"
	while [ "$round" -lt "$COUNT" ]; do
		round=$((round + 1))
		# Odd rounds walk variants as listed; even rounds reverse them (ABBA).
		if [ $((round % 2)) -eq 0 ]; then
			order=
			for v in $RECORD_VARIANTS; do
				order="$v${order:+ $order}"
			done
		else
			order=$RECORD_VARIANTS
		fi
		names=
		for v in $order; do
			names="${names:+$names }${v%%=*}"
		done
		echo "# gusset-bench-round-$round: $names" >> "$TMP/roundorder"
		a=0
		while read -r arm; do
			a=$((a + 1))
			for v in $order; do
				name=${v%%=*}
				j=$(cat "$TMP/vindex.$name")
				runs=$((runs + 1))
				if [ "$runs" -gt 1 ]; then
					require_exclusive "between runs, before round $round of $arm in $name"
				fi
				echo "record: round $round/$COUNT — $arm — $name"
				run_arm "${v#*=}" "$arm" 1
				part="$TMP/arm.$a.$j"
				if [ "$round" = "1" ]; then
					echo "$part" >> "$TMP/parts"
				fi
				{
					echo "variant: $name"
					cat "$TMP/run"
				} >> "$part"
			done
		done < "$TMP/arms"
	done
fi

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
	if [ -n "$RECORD_VARIANTS" ]; then
		echo "# gusset-bench-package: each variant's directory, $RECORD_PKG"
	else
		echo "# gusset-bench-package: $RECORD_DIR $RECORD_PKG"
	fi
	echo "# gusset-bench-arms: $*"
	echo "# gusset-bench-benchtime: $BENCHTIME"
	echo "# gusset-bench-count: $COUNT"
	echo "# gusset-bench-cpu: ${RECORD_CPU:-default}"
	if [ -n "$RECORD_VARIANTS" ]; then
		echo "# gusset-bench-order: $COUNT rounds; each round ran every arm once (-count=1); variant order alternates (odd rounds listed, even rounds reversed)"
		cat "$TMP/roundorder"
		for v in $RECORD_VARIANTS; do
			dir=${v#*=}
			rev=$(git -C "$dir" rev-parse --short HEAD 2>/dev/null || echo unknown)
			mods=$(git -C "$dir" status --porcelain --untracked-files=no 2>/dev/null | wc -l | tr -d ' ')
			echo "# gusset-bench-variant-${v%%=*}: $rev, $mods tracked path(s) modified, $dir"
		done
	fi
	echo "# gusset-bench-cpu-idle-before: $IDLE_BEFORE"
	echo "# gusset-bench-cpu-idle-after: $IDLE_AFTER"
	echo "# gusset-bench-min-idle: $MIN_IDLE"
	echo "# gusset-bench-exclusive: yes"
	while read -r part; do
		cat "$part"
	done < "$TMP/parts"
} > "$TMP/out"

mkdir -p "$(dirname "$OUT")"
mv "$TMP/out" "$OUT"
echo "record: wrote $OUT (${IDLE_BEFORE}% -> ${IDLE_AFTER}% idle, $n arm(s), exclusive)"
