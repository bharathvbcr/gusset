#!/bin/sh
# Exercises bench/record.sh: argument refusals, signal cleanup, lock publication
# and takeover, variant order, the cargo arm, and the provenance header. CI runs
# it on every Matrix `unit` leg (ubuntu, ubuntu-arm, macos).
#
# Recorders that must be interruptible are started in their own session (or, for
# the shared-group cases, in a group of their own that this shell is not in), so
# a regression that signals process group 0 cannot reach this shell.
#
# Everything lives under bench/.probe/run.XXXXXX, removed on every exit path,
# failure included. RECORD_TEST_FAULT=1 makes a run fail straight after setup,
# which is how the suite checks that claim about itself. RECORD_TEST_ONLY names
# the cases to run (see run_case at the bottom).
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
SCRIPT=$ROOT/bench/record.sh
# The recorder the cases run. signal_case is also run against a copy with its
# group kill mutated, to show the case can tell a group kill from a pid kill.
SUT=$SCRIPT
PROBE=$ROOT/bench/.probe
HOST=$(uname -s)
mkdir -p "$PROBE"
WORK=$(mktemp -d "$PROBE/run.XXXXXX")

if grep -n -E 'kill[[:space:]]+-(TERM|INT|KILL|HUP)[[:space:]]+0([^0-9]|$)' "$SCRIPT"; then
	echo "FAIL: record.sh signals process group 0" >&2
	exit 1
fi
if grep -n -E '(^|[^[:alnum:]_])(pkill|killall)([^[:alnum:]_]|$)' "$SCRIPT"; then
	echo "FAIL: record.sh uses pkill or killall" >&2
	exit 1
fi

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

CANARY=
PIDS=
EXTRA=
cleanup() {
	status=$?
	for p in $EXTRA $CANARY; do
		kill "$p" 2>/dev/null || true
	done
	for p in $PIDS; do
		kill -TERM "$p" 2>/dev/null || true
	done
	for p in $PIDS $CANARY; do
		wait "$p" 2>/dev/null || true
	done
	# The stub go's grandchildren, for a case that failed before it reaped its own.
	for f in "$WORK"/*/gc.pid; do
		if [ -s "$f" ]; then
			kill "$(cat "$f")" 2>/dev/null || true
		fi
	done
	rm -rf "$WORK"
	rmdir "$PROBE" 2>/dev/null || true
	exit $status
}
trap cleanup EXIT
# A signal ends this shell through the EXIT trap, so the scratch tree goes too.
trap 'exit 130' INT TERM HUP

note_pid() {
	PIDS="$PIDS $1"
}

note_extra() {
	EXTRA="$EXTRA $1"
}

# start_at DIR MODE OUT -- argv
# MODE 1: new session, with SIGINT/SIGTERM at their default disposition (this
# agent session ignores SIGINT, and an ignored signal survives exec and cannot be
# trapped by the shell). MODE 0: the caller's own session and dispositions.
# Sets STARTED to the child pid. Not called from a command substitution: the
# child must stay in this shell, or the substitution's subshell would SIGHUP it.
start_at() {
	dir=$1
	sess=$2
	out=$3
	shift 3
	python3 -c 'import os,signal,sys
os.chdir(sys.argv[1])
if sys.argv[2] == "1":
    os.setsid()
    for s in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT):
        signal.signal(s, signal.SIG_DFL)
os.execvp(sys.argv[3], sys.argv[3:])' "$dir" "$sess" "$@" >"$out" 2>&1 &
	STARTED=$!
}

# start_family DIR FAMFILE OUT -- argv
# A recorder that shares a process group with a canary and with the process that
# launched it, as it does when an agent harness or a CI step shell starts it, and
# whose group this shell is not in. FAMFILE gets "CANARY RECORDER" pids once both
# run, and FAMFILE.status the recorder's exit status once it ends. The recorder
# starts with default signal dispositions, like start_at MODE 1.
start_family() {
	dir=$1
	fam=$2
	out=$3
	shift 3
	python3 -c 'import os,signal,sys
os.chdir(sys.argv[1])
fam = sys.argv[2]
argv = sys.argv[3:]
os.setsid()
canary = os.fork()
if canary == 0:
    os.execvp("sleep", ["sleep", "300"])
rec = os.fork()
if rec == 0:
    for s in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT):
        signal.signal(s, signal.SIG_DFL)
    os.execvp(argv[0], argv)
with open(fam + ".tmp", "w") as f:
    f.write("%d %d\n" % (canary, rec))
os.rename(fam + ".tmp", fam)
_, st = os.waitpid(rec, 0)
code = os.WEXITSTATUS(st) if os.WIFEXITED(st) else 128 + os.WTERMSIG(st)
with open(fam + ".status.tmp", "w") as f:
    f.write("%d\n" % code)
os.rename(fam + ".status.tmp", fam + ".status")
os.waitpid(canary, 0)' "$dir" "$fam" "$@" >"$out" 2>&1 &
	STARTED=$!
}

# start_zombie DIR: a zombie that this shell does not own. A python3 parent in a
# session of its own forks a child that exits at once and then sleeps without
# ever calling wait, so the child stays a zombie until the parent is killed.
# DIR/zombie gets "PARENT ZOMBIE" pids once the child has exited. Sets STARTED to
# the parent's pid.
start_zombie() {
	python3 -c 'import os,sys,time
os.setsid()
child = os.fork()
if child == 0:
    os._exit(0)
time.sleep(0.3)
with open(sys.argv[1] + ".tmp", "w") as f:
    f.write("%d %d\n" % (os.getpid(), child))
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
time.sleep(300)' "$1/zombie" >"$1/zombie.out" 2>&1 &
	STARTED=$!
}

# wait_grep FILE PATTERN: up to 20s, generous because the host may be busy.
wait_grep() {
	file=$1
	pat=$2
	i=0
	while [ "$i" -lt 400 ]; do
		if grep -q "$pat" "$file" 2>/dev/null; then
			return 0
		fi
		sleep 0.05
		i=$((i + 1))
	done
	return 1
}

wait_file() {
	file=$1
	i=0
	while [ "$i" -lt 400 ]; do
		if [ -s "$file" ]; then
			return 0
		fi
		sleep 0.05
		i=$((i + 1))
	done
	return 1
}

# alive PID: running. A zombie is not: kill -0 succeeds for one, and under a
# PID 1 that does not reap orphans a group-killed process stays one, which read
# as a survivor. ps -o stat= puts Z in the state for a zombie on both procps and
# BSD ps (modifier letters never include Z); any other state is alive, so the
# check stays strict for a live process.
alive() {
	kill -0 "$1" 2>/dev/null || return 1
	state=$(ps -o stat= -p "$1" 2>/dev/null || true)
	case $state in
	*Z*) return 1 ;;
	esac
	return 0
}

# wait_dead PID: up to 5s for a process that was signalled to be gone.
wait_dead() {
	i=0
	while [ "$i" -lt 100 ]; do
		if ! alive "$1"; then
			return 0
		fi
		sleep 0.05
		i=$((i + 1))
	done
	return 1
}

# start_statfeed FILE: a background writer that keeps a /proc/stat-shaped file
# moving at 90% idle, so the Linux branch of cpu_idle (two samples a second apart)
# reads a machine this test controls. Atomic replace, so a sample never sees half
# a line. Leaves the pid in STATFEED_PID; the caller notes it for cleanup.
start_statfeed() {
	feed=$1
	if [ -n "${STATFEED_PID:-}" ]; then
		kill "$STATFEED_PID" 2>/dev/null || true
	fi
	owner=$$
	(
		u=100
		i=900
		# Ends with the test shell, so a case run in a subshell that dies cannot
		# strand it.
		while kill -0 "$owner" 2>/dev/null; do
			printf 'cpu  %s 0 0 %s 0 0 0 0\n' "$u" "$i" >"$feed.new"
			mv "$feed.new" "$feed"
			u=$((u + 10))
			i=$((i + 90))
			sleep 0.1
		done
	) >/dev/null 2>&1 &
	STATFEED_PID=$!
	note_extra "$STATFEED_PID"
	wait_file "$feed" || fail "stat feed did not start"
}

# install_stubs DEST KIND [UNAME]
# KIND selects what `go` does:
#   fast    prints one benchmark row and logs the -C directory to $ORDER_LOG
#   slow    starts a grandchild, records its pid in $GO_GC_PIDFILE and its own in
#           $GO_PIDFILE (last, so a non-empty go.pid means both exist), and waits
#   norows  exits 0 having printed no benchmark row
# `cargo` logs its argv to $CARGO_LOG and, unless it is a --no-run build, prints
# rows on stdout and noise on stderr. `ps -Ao` prints $FAKE_PS_LINES (default
# nothing), hiding the machine's real benchmark processes; `ps -o` and `ps -p`
# are the real ps. `top` answers 90% idle.
# UNAME, when given (Darwin or Linux), makes `uname` answer for that platform, to
# steer cpu_idle onto a branch the host would not take. Without it `uname` is the
# host's own.
install_stubs() {
	dest=$1
	kind=$2
	fake_uname=${3:-}
	mkdir -p "$dest"
	if [ -n "$fake_uname" ]; then
		printf '%s\n' '#!/bin/sh' \
			'case "$1" in' \
			"-s) echo $fake_uname ;;" \
			'-n) echo record-test ;;' \
			"-srm) echo $fake_uname test test ;;" \
			"*) echo $fake_uname ;;" \
			'esac' >"$dest/uname"
		chmod +x "$dest/uname"
	fi
	printf '%s\n' '#!/bin/sh' \
		'echo "CPU usage: 1.23% user, 2.34% sys, 90.00% idle"' >"$dest/top"
	printf '%s\n' '#!/bin/sh' \
		'case "$1" in -o|-p) exec /bin/ps "$@" ;; esac' \
		'if [ -n "${FAKE_PS_LINES:-}" ]; then printf "%s\n" "$FAKE_PS_LINES"; fi' \
		'exit 0' >"$dest/ps"
	case $kind in
	slow)
		printf '%s\n' '#!/bin/sh' \
			'sleep 120 &' \
			'echo $! > "$GO_GC_PIDFILE"' \
			'echo $$ > "$GO_PIDFILE"' \
			'wait' >"$dest/go"
		;;
	norows)
		printf '%s\n' '#!/bin/sh' 'echo PASS' 'exit 0' >"$dest/go"
		;;
	*)
		printf '%s\n' '#!/bin/sh' \
			'prev=' \
			'dir=' \
			'for a in "$@"; do' \
			'	if [ "$prev" = "-C" ]; then' \
			'		dir=$a' \
			'	fi' \
			'	prev=$a' \
			'done' \
			'echo "$dir" >> "$ORDER_LOG"' \
			'echo "BenchmarkDispatch 1 100 ns/op"' \
			'exit 0' >"$dest/go"
		;;
	esac
	printf '%s\n' '#!/bin/sh' \
		'echo "$*" >> "$CARGO_LOG"' \
		'echo "   Compiling noise-that-must-not-be-recorded" >&2' \
		'for a in "$@"; do' \
		'	if [ "$a" = "--no-run" ]; then exit 0; fi' \
		'done' \
		'echo "BenchmarkDispatch/stub_arm	10	100.0 ns/op"' \
		'echo "BenchmarkDispatch/stub_arm	10	101.0 ns/op"' \
		'exit 0' >"$dest/cargo"
	chmod +x "$dest/top" "$dest/ps" "$dest/go" "$dest/cargo"
}

# new_case NAME KIND [UNAME]: a scratch directory with stubs, a run directory and
# a live stat feed. Sets D.
new_case() {
	D=$(mktemp -d "$WORK/$1.XXXXXX")
	install_stubs "$D/bin" "$2" "${3:-}"
	mkdir -p "$D/run/bench/results"
	: >"$D/go.pid"
	: >"$D/order.log"
	: >"$D/cargo.log"
	start_statfeed "$D/stat"
}

# The environment every recorder under test gets.
STUB_ENV='export PATH="$1:$PATH"
export GO_PIDFILE="$2/go.pid"
export GO_GC_PIDFILE="$2/gc.pid"
export ORDER_LOG="$2/order.log"
export CARGO_LOG="$2/cargo.log"
export RECORD_PROC_STAT="$2/stat"
'

# rec DIR [ENV=VAL ...] -- ARG...: runs the recorder to completion from DIR/run.
# Sets REC_RC; stdout and stderr land in DIR/stdout and DIR/stderr.
rec() {
	d=$1
	shift
	REC_RC=0
	(
		cd "$d/run"
		while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
			export "$1"
			shift
		done
		shift
		export PATH="$d/bin:$PATH"
		export GO_PIDFILE="$d/go.pid" GO_GC_PIDFILE="$d/gc.pid"
		export ORDER_LOG="$d/order.log" CARGO_LOG="$d/cargo.log"
		export RECORD_PROC_STAT="$d/stat"
		/bin/sh "$SUT" "$@"
	) >"$d/stdout" 2>"$d/stderr" || REC_RC=$?
}

sentinel() {
	printf 'sentinel: the previous results\n' >"$1"
}

assert_sentinel() {
	if [ "$(cat "$1")" != "sentinel: the previous results" ]; then
		echo "--- results file now ---" >&2
		cat "$1" >&2
		fail "$2: the previous results file was touched"
	fi
}

assert_no_lock() {
	if [ -d "$1/run/bench/results/.record.lock" ]; then
		fail "$2: the lock directory was left behind"
	fi
	for left in "$1"/run/bench/results/.record.lock.stage.* "$1"/run/bench/results/.record.lock.claim; do
		if [ -e "$left" ]; then
			fail "$2: left $left behind"
		fi
	done
}

# A process in this shell's own group that outlives the whole run (a fixed sleep
# did not: a loaded host once took longer than it) and ends with it.
/bin/sh -c 'while kill -0 "$1" 2>/dev/null; do sleep 1; done' _ "$$" &
CANARY=$!

if [ "${RECORD_TEST_FAULT:-}" = "1" ]; then
	new_case fault fast
	: >"$D/run/state"
	fail "injected failure after setup"
fi

# --- COUNT must be a positive integer, in both modes, before results are touched
count_case() {
	new_case count fast
	mkdir -p "$D/va" "$D/vb"
	for bad in 0 abc -1 1.5 ''; do
		sentinel "$D/out.txt"
		rec "$D" -- "$D/out.txt" 1x "$bad" "Dispatch\$"
		[ "$REC_RC" = 2 ] || fail "COUNT '$bad': exit $REC_RC, want 2"
		grep -q "positive integer" "$D/stderr" || fail "COUNT '$bad': no explanation"
		assert_sentinel "$D/out.txt" "COUNT '$bad'"
		assert_no_lock "$D" "COUNT '$bad'"
		sentinel "$D/out.txt"
		rec "$D" "RECORD_VARIANTS=alpha=../va beta=../vb" -- "$D/out.txt" 1x "$bad" "Dispatch\$"
		[ "$REC_RC" = 2 ] || fail "variant COUNT '$bad': exit $REC_RC, want 2"
		assert_sentinel "$D/out.txt" "variant COUNT '$bad'"
	done
	echo "ok COUNT refusals"
}

# --- duplicate variant names are refused, not doubled ---
dup_variant_case() {
	new_case dup fast
	mkdir -p "$D/va" "$D/vb"
	sentinel "$D/out.txt"
	rec "$D" "RECORD_VARIANTS=alpha=../va alpha=../vb" -- "$D/out.txt" 1x 2 "Dispatch\$"
	[ "$REC_RC" = 2 ] || fail "duplicate variant: exit $REC_RC, want 2"
	grep -q "twice" "$D/stderr" || fail "duplicate variant: no explanation"
	assert_sentinel "$D/out.txt" "duplicate variant"
	[ ! -s "$D/order.log" ] || fail "duplicate variant: a benchmark ran before the refusal"
	echo "ok duplicate variant names"
}

# --- an arm with no Benchmark row publishes nothing ---
norows_case() {
	new_case norows norows
	mkdir -p "$D/va" "$D/vb"
	sentinel "$D/out.txt"
	rec "$D" -- "$D/out.txt" 1x 1 "NoSuchBench"
	[ "$REC_RC" = 1 ] || fail "zero-row arm: exit $REC_RC, want 1"
	grep -q "no Benchmark rows" "$D/stderr" || fail "zero-row arm: no explanation"
	assert_sentinel "$D/out.txt" "zero-row arm"
	assert_no_lock "$D" "zero-row arm"
	sentinel "$D/out.txt"
	rec "$D" "RECORD_VARIANTS=alpha=../va beta=../vb" -- "$D/out.txt" 1x 2 "NoSuchBench"
	[ "$REC_RC" = 1 ] || fail "zero-row variant arm: exit $REC_RC, want 1"
	assert_sentinel "$D/out.txt" "zero-row variant arm"
	echo "ok zero-row arm"
}

# --- variant order: round 1 A then B, round 2 B then A ---
order_case() {
	new_case order fast
	mkdir -p "$D/va" "$D/vb"
	rec "$D" "RECORD_VARIANTS=alpha=../va beta=../vb" -- "$D/out.txt" 1x 2 "Dispatch\$"
	if [ "$REC_RC" != 0 ]; then
		echo "--- variant stderr ---" >&2
		cat "$D/stderr" >&2
		fail "variant recording exited $REC_RC"
	fi
	grep -q "gusset-bench-round-1: alpha beta" "$D/out.txt" || {
		cat "$D/out.txt" >&2
		fail "round 1 order was not alpha then beta"
	}
	grep -q "gusset-bench-round-2: beta alpha" "$D/out.txt" || {
		cat "$D/out.txt" >&2
		fail "round 2 order was not beta then alpha"
	}
	got=$(cat "$D/order.log")
	want="../va
../va
../vb
../vb
../va"
	if [ "$got" != "$want" ]; then
		echo "order log:" >&2
		printf '%s\n' "$got" >&2
		fail "variant run order was not warmup, A B, B A"
	fi
	assert_no_lock "$D" "variant order"
	echo "ok variant order"
}

# --- a lock with no pgid is not stolen immediately ---
grace_case() {
	new_case grace slow
	mkdir -p "$D/run/bench/results/.record.lock"
	start_at "$D/run" 1 "$D/stdio" /bin/sh -c "$STUB_ENV"'exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' \
		_ "$D/bin" "$D" "$SUT" "$D/out.txt"
	pid=$STARTED
	note_pid "$pid"
	if ! wait_grep "$D/stdio" "no pgid"; then
		echo "--- grace stdio ---" >&2
		cat "$D/stdio" >&2
		fail "empty lock was not held for a grace period"
	fi
	if grep -q "clearing a lock" "$D/stdio"; then
		cat "$D/stdio" >&2
		fail "empty lock was stolen inside the grace period"
	fi
	if [ ! -d "$D/run/bench/results/.record.lock" ]; then
		fail "empty lock directory disappeared during the grace period"
	fi
	if [ -s "$D/run/bench/results/.record.lock/pgid" ]; then
		fail "empty lock gained a pgid without being published by a holder"
	fi
	kill -TERM "$pid" 2>/dev/null || true
	status=0
	wait "$pid" || status=$?
	PIDS=
	if [ "$status" -ne 130 ]; then
		echo "--- grace stdio ---" >&2
		cat "$D/stdio" >&2
		fail "grace-period interrupt status $status, want 130"
	fi
	if grep -q "clearing a lock" "$D/stdio"; then
		fail "interrupt during the grace period cleared the lock"
	fi
	echo "ok lock grace"
}

# --- a lock with no pgid is taken over once the grace period has passed ---
grace_expiry_case() {
	new_case graceend fast
	mkdir -p "$D/run/bench/results/.record.lock"
	rec "$D" -- "$D/out.txt" 1x 1 "Dispatch\$"
	[ "$REC_RC" = 0 ] || {
		cat "$D/stderr" >&2
		fail "recording over an empty lock exited $REC_RC"
	}
	grep -q "no pgid" "$D/stderr" || fail "empty lock: the grace period was not announced"
	grep -q "clearing a lock" "$D/stderr" || fail "empty lock was not cleared after the grace period"
	grep -q '^BenchmarkDispatch' "$D/out.txt" || fail "no results written after taking over an empty lock"
	assert_no_lock "$D" "empty lock takeover"
	echo "ok lock grace expiry"
}

# dead_lock DIR: a lock whose owner and process group are both gone.
dead_lock() {
	/bin/sh -c 'exit 0' &
	gone=$!
	wait "$gone" || true
	lockdir=$1/run/bench/results/.record.lock
	mkdir -p "$lockdir"
	echo "$gone" >"$lockdir/owner"
	echo "$gone" >"$lockdir/pgid"
}

# --- a lock whose owner and group are dead is taken over ---
stale_case() {
	new_case stale fast
	dead_lock "$D"
	rec "$D" -- "$D/out.txt" 1x 1 "Dispatch\$"
	[ "$REC_RC" = 0 ] || {
		cat "$D/stderr" >&2
		fail "recording over a dead lock exited $REC_RC"
	}
	grep -q "clearing a lock" "$D/stderr" || fail "dead lock was not reported as cleared"
	grep -q "in progress" "$D/stderr" && fail "dead lock was reported as a live recording"
	grep -q '^BenchmarkDispatch' "$D/out.txt" || fail "no results written after taking over a dead lock"
	assert_no_lock "$D" "dead lock takeover"
	echo "ok stale lock takeover"
}

# --- a zombie holder is gone ---
# A recorder SIGKILLed while its parent has not yet reaped it is a zombie: `ps -p`
# still lists it, so a liveness check built on that alone read it as running, and
# its lock (or takeover claim) was never cleared. MODE lock: the lock's owner is
# the zombie and its recorded group is the zombie's real one, so the pid-reuse key
# agrees and only the state can say it is dead. MODE claim: the lock is dead and
# the claim blocking its takeover belongs to the zombie.
zombie_case() {
	mode=$1
	new_case zombie fast
	start_zombie "$D"
	note_pid "$STARTED"
	wait_file "$D/zombie" || fail "zombie/$mode: the zombie was never made"
	zpid=$(cut -d' ' -f2 "$D/zombie")
	state=$(ps -o stat= -p "$zpid" 2>/dev/null || true)
	case $state in
	*Z*) ;;
	*) fail "zombie/$mode: pid $zpid is not a zombie (state '$state'), so the case proves nothing" ;;
	esac
	zgroup=$(ps -o pgid= -p "$zpid" 2>/dev/null | tr -d ' ' || true)
	lockdir=$D/run/bench/results/.record.lock
	if [ "$mode" = "lock" ]; then
		mkdir -p "$lockdir"
		echo "$zpid" >"$lockdir/owner"
		echo "${zgroup:-$(cut -d' ' -f1 "$D/zombie")}" >"$lockdir/pgid"
	else
		dead_lock "$D"
		mkdir -p "$lockdir.claim"
		echo "$zpid" >"$lockdir.claim/owner"
	fi
	# Started in the background and waited for with a bound: a recorder that reads
	# the zombie's claim as live retries forever instead of exiting.
	start_at "$D/run" 0 "$D/stdio" /bin/sh -c "$STUB_ENV"'exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' \
		_ "$D/bin" "$D" "$SUT" "$D/out.txt"
	recorder=$STARTED
	note_pid "$recorder"
	i=0
	while [ "$i" -lt 300 ] && alive "$recorder"; do
		sleep 0.05
		i=$((i + 1))
	done
	if alive "$recorder"; then
		kill -TERM "$recorder" 2>/dev/null || true
		wait "$recorder" 2>/dev/null || true
		cat "$D/stdio" >&2
		fail "zombie/$mode: the recorder was still waiting on the zombie's $mode after 15 s"
	fi
	status=0
	wait "$recorder" || status=$?
	if [ "$status" != 0 ]; then
		cat "$D/stdio" >&2
		fail "zombie/$mode: the recording exited $status"
	fi
	grep -q "another recording is in progress" "$D/stdio" && fail "zombie/$mode: a zombie was treated as a live recorder"
	grep -q "clearing a lock" "$D/stdio" || fail "zombie/$mode: the zombie's lock was not cleared"
	if [ "$mode" = "claim" ]; then
		grep -q "clearing a takeover claim left by pid $zpid" "$D/stdio" || fail "zombie/$mode: the zombie's claim was not cleared"
	fi
	grep -q '^BenchmarkDispatch' "$D/out.txt" || fail "zombie/$mode: no results written"
	assert_no_lock "$D" "zombie $mode"
	echo "ok a zombie holder of a $mode is gone"
}

# --- N concurrent starts: exactly one winner ---
# race_case N [stale]: with "stale", the lock they all start against is a dead
# one, so they race to take it over as well as to publish.
race_case() {
	n=$1
	label=${2:-}
	new_case race slow
	if [ "$label" = "stale" ]; then
		dead_lock "$D"
	fi
	: >"$D/pids"
	i=0
	while [ "$i" -lt "$n" ]; do
		i=$((i + 1))
		start_at "$D/run" 0 "$D/stdio.$i" /bin/sh -c "$STUB_ENV"'exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' \
			_ "$D/bin" "$D" "$SUT" "$D/out.txt"
		echo "$STARTED" >>"$D/pids"
		note_pid "$STARTED"
	done
	i=0
	another=0
	alive_n=0
	winner=
	# Losers refuse at the lock, before the idle sample. The winner may still
	# be inside that sample; do not require its preflight line to have landed.
	while [ "$i" -lt 400 ]; do
		another=$(grep -l "another recording is in progress" "$D"/stdio.* 2>/dev/null | wc -l | tr -d '[:space:]')
		alive_n=0
		winner=
		while read -r p; do
			if [ -n "$p" ] && alive "$p"; then
				alive_n=$((alive_n + 1))
				winner=$p
			fi
		done <"$D/pids"
		if [ "$another" = "$((n - 1))" ] && [ "$alive_n" = "1" ]; then
			break
		fi
		sleep 0.05
		i=$((i + 1))
	done
	clearing=$(grep -l "clearing a lock" "$D"/stdio.* 2>/dev/null | wc -l | tr -d '[:space:]' || true)
	want_clearing=0
	if [ "$label" = "stale" ]; then
		want_clearing=1
	fi
	if [ "$another" != "$((n - 1))" ] || [ "${clearing:-0}" != "$want_clearing" ] || [ "$alive_n" != "1" ]; then
		echo "another=$another clearing=${clearing:-0} (want $want_clearing) alive=$alive_n" >&2
		cat "$D"/stdio.* >&2
		fail "concurrent starts ${label:+against a $label lock }did not yield exactly one winner"
	fi
	kill -TERM "$winner" 2>/dev/null || true
	status=0
	wait "$winner" || status=$?
	if [ "$status" -ne 130 ]; then
		fail "winner interrupt status $status, want 130"
	fi
	assert_no_lock "$D" "race winner"
	# Reap the losers (they already exited).
	while read -r p; do
		wait "$p" 2>/dev/null || true
	done <"$D/pids"
	PIDS=
	if [ -s "$D/gc.pid" ]; then
		kill "$(cat "$D/gc.pid")" 2>/dev/null || true
	fi
	echo "ok one winner of $n${label:+ against a $label lock}"
}

# kill_gc DIR: the stub go's grandchild, whatever happened to the case.
kill_gc() {
	if [ -s "$1/gc.pid" ]; then
		kill "$(cat "$1/gc.pid")" 2>/dev/null || true
	fi
}

# sig_fail DIR MESSAGE: fail, but not before the case's grandchild is gone.
sig_fail() {
	kill_gc "$1"
	fail "$2"
}

# --- INT/TERM/HUP/QUIT: the group started by the recorder dies, nothing else does,
# the lock is released, and the next run starts cleanly ---
# signal_case SIG MODE: MODE sep starts the recorder in its own session, MODE
# family in a process group it shares with a canary and its launcher.
#
# What would make this pass against a broken recorder, and the guard against
# each:
#   - a pid-only kill leaves the stub go's grandchild running; the stub starts
#     one and the case checks it died (the shell's own $$ was what it used to
#     check, which a pid kill also ends);
#   - a signal sent before the recorder has a child proves little; it is sent
#     once go.pid exists, which the stub writes after the grandchild;
#   - a recorder that signalled its own group would take a canary with it, but
#     only a canary in that group can show it: in `sep` the recorder is alone in
#     its session and the canary beside it proves nothing, so `family` puts the
#     canary in the recorder's group and signals the recorder alone.
signal_case() {
	sig=$1
	mode=$2
	new_case "sig$sig" slow
	canary_sep=
	if [ "$mode" = "family" ]; then
		start_family "$D/run" "$D/fam" "$D/stdio" /bin/sh -c "$STUB_ENV"'exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' \
			_ "$D/bin" "$D" "$SUT" "$D/out.txt"
		note_pid "$STARTED"
		if ! wait_file "$D/fam"; then
			sig_fail "$D" "$sig/$mode: the family did not start"
		fi
		canary_group=$(cut -d' ' -f1 "$D/fam")
		pid=$(cut -d' ' -f2 "$D/fam")
		note_extra "$canary_group"
	else
		start_at "$D/run" 1 "$D/stdio" /bin/sh -c "$STUB_ENV"'exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' \
			_ "$D/bin" "$D" "$SUT" "$D/out.txt"
		pid=$STARTED
		note_pid "$pid"
		# A canary in a session of its own: a group kill aimed at the wrong group
		# is the one thing that could reach it.
		# It ends with this shell, so a run of this case in a subshell that dies
		# cannot strand it.
		start_at "$D" 1 "$D/canary.out" /bin/sh -c 'while kill -0 "$1" 2>/dev/null; do sleep 0.5; done' _ "$$"
		canary_sep=$STARTED
		note_pid "$canary_sep"
	fi
	if ! wait_file "$D/go.pid"; then
		echo "--- $sig stdio ---" >&2
		cat "$D/stdio" >&2
		sig_fail "$D" "$sig/$mode: the stub go never started"
	fi
	gc=$(cat "$D/gc.pid")
	alive "$gc" || sig_fail "$D" "$sig/$mode: the stub go's grandchild was not running"
	kill "-$sig" "$pid" 2>/dev/null || true
	status=0
	if [ "$mode" = "family" ]; then
		if ! wait_file "$D/fam.status"; then
			echo "--- $sig stdio ---" >&2
			cat "$D/stdio" >&2
			sig_fail "$D" "$sig/$mode: the recorder did not exit"
		fi
		status=$(cat "$D/fam.status")
	else
		wait "$pid" || status=$?
		PIDS=$canary_sep
	fi
	if [ "$status" -ne 130 ]; then
		echo "--- $sig stdio ---" >&2
		cat "$D/stdio" >&2
		sig_fail "$D" "$sig/$mode: status $status, want 130"
	fi
	if [ -d "$D/run/bench/results/.record.lock" ]; then
		sig_fail "$D" "$sig/$mode: the lock directory was left in place"
	fi
	assert_no_lock "$D" "$sig/$mode"
	if ! wait_dead "$gc"; then
		sig_fail "$D" "$sig/$mode: the stub go's grandchild survived: the child's group was not killed, only its pid"
	fi
	if [ "$mode" = "family" ]; then
		alive "$canary_group" || fail "$sig/$mode: the recorder's own process group was signalled (its canary died)"
	else
		alive "$canary_sep" || fail "$sig/$mode: a canary in another session died"
	fi
	start_at "$D/run" 1 "$D/stdio2" /bin/sh -c "$STUB_ENV"'exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' \
		_ "$D/bin" "$D" "$SUT" "$D/out.txt"
	pid2=$STARTED
	note_pid "$pid2"
	if ! wait_grep "$D/stdio2" "preflight ok"; then
		echo "--- $sig next stdio ---" >&2
		cat "$D/stdio2" >&2
		sig_fail "$D" "$sig/$mode: the next run did not start cleanly"
	fi
	if grep -q "another recording is in progress" "$D/stdio2"; then
		sig_fail "$D" "$sig/$mode: the next run printed \"another recording is in progress\""
	fi
	kill -TERM "$pid2" 2>/dev/null || true
	wait "$pid2" 2>/dev/null || true
	kill_gc "$D"
	echo "ok signal $sig ($mode)"
}

# --- a recorder SIGKILLed inside a group that outlives it ---
# Its lock names a pid that is gone and a group that is not (the canary, and the
# launcher, are still in it). Keying liveness on the group made every later run
# refuse for as long as the launcher lived.
kill9_case() {
	new_case kill9 slow
	start_family "$D/run" "$D/fam" "$D/stdio" /bin/sh -c "$STUB_ENV"'exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' \
		_ "$D/bin" "$D" "$SUT" "$D/out.txt"
	note_pid "$STARTED"
	wait_file "$D/fam" || sig_fail "$D" "kill -9: the family did not start"
	canary_group=$(cut -d' ' -f1 "$D/fam")
	pid=$(cut -d' ' -f2 "$D/fam")
	note_extra "$canary_group"
	wait_file "$D/go.pid" || sig_fail "$D" "kill -9: the stub go never started"
	kill -KILL "$pid"
	wait_file "$D/fam.status" || sig_fail "$D" "kill -9: the recorder did not die"
	[ -d "$D/run/bench/results/.record.lock" ] || sig_fail "$D" "kill -9: no lock was left to recover from"
	alive "$canary_group" || sig_fail "$D" "kill -9: the shared group did not outlive the recorder"
	kill_gc "$D"
	# The next run's go is a fast one, so it finishes by itself.
	install_stubs "$D/bin" fast
	rec "$D" -- "$D/out.txt" 1x 1 "Dispatch\$"
	if [ "$REC_RC" != 0 ]; then
		cat "$D/stderr" >&2
		fail "kill -9: the next run exited $REC_RC"
	fi
	grep -q "another recording is in progress" "$D/stderr" && fail "kill -9: a dead recorder's lock was treated as live because its group lives"
	grep -q "clearing a lock" "$D/stderr" || fail "kill -9: the dead recorder's lock was not cleared"
	assert_no_lock "$D" "kill -9"
	echo "ok SIGKILL in a shared group recovers"
}

# --- the refusal never advises signalling a process group ---
refusal_case() {
	new_case refusal slow
	start_at "$D/run" 1 "$D/stdio" /bin/sh -c "$STUB_ENV"'exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' \
		_ "$D/bin" "$D" "$SUT" "$D/out.txt"
	holder=$STARTED
	note_pid "$holder"
	wait_file "$D/go.pid" || sig_fail "$D" "refusal: the holder never started"
	rec "$D" -- "$D/out2.txt" 1x 1 "Dispatch\$"
	[ "$REC_RC" = 1 ] || fail "refusal: exit $REC_RC, want 1"
	grep -q "another recording is in progress (pid $holder)" "$D/stderr" || {
		cat "$D/stderr" >&2
		fail "refusal: does not name the holder's pid"
	}
	if grep -E 'kill[[:space:]]+(-[A-Z]+[[:space:]]+)?-[0-9]' "$D/stderr"; then
		fail "refusal: advises signalling a process group"
	fi
	kill -TERM "$holder" 2>/dev/null || true
	wait "$holder" 2>/dev/null || true
	PIDS=
	kill_gc "$D"
	echo "ok refusal names the pid, not a group"
}

# --- a signal inside the lock-staging window leaves nothing behind ---
stage_case() {
	new_case stage fast
	# A mkdir that, for the staging directory only, signals the recorder as soon
	# as the directory exists: the recorder is then between creating it and
	# renaming it onto the lock.
	printf '%s\n' '#!/bin/sh' \
		'/bin/mkdir "$@" || exit 1' \
		'case "$*" in' \
		'*.record.lock.stage.*)' \
		'	i=0' \
		'	while [ ! -s "$STAGE_PIDFILE" ] && [ "$i" -lt 200 ]; do sleep 0.05; i=$((i + 1)); done' \
		'	kill -TERM "$(cat "$STAGE_PIDFILE")"' \
		'	;;' \
		'esac' >"$D/bin/mkdir"
	chmod +x "$D/bin/mkdir"
	start_at "$D/run" 1 "$D/stdio" /bin/sh -c "$STUB_ENV"'export STAGE_PIDFILE="$2/stage.pid"
exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"' _ "$D/bin" "$D" "$SUT" "$D/out.txt"
	pid=$STARTED
	note_pid "$pid"
	echo "$pid" >"$D/stage.pid"
	status=0
	wait "$pid" || status=$?
	PIDS=
	if [ "$status" -ne 130 ]; then
		cat "$D/stdio" >&2
		fail "staging signal: status $status, want 130"
	fi
	assert_no_lock "$D" "staging signal"
	echo "ok a signal while staging the lock leaves no stage directory"
}

# --- the cargo arm ---
cargo_case() {
	new_case cargo fast
	rec "$D" RECORD_DIR=. -- "$D/out.txt" 1x 12 cargo:gusset/dispatch
	if [ "$REC_RC" != 0 ]; then
		cat "$D/stderr" >&2
		fail "cargo arm exited $REC_RC"
	fi
	grep -q 'bench --locked --manifest-path ./Cargo.toml -p gusset --bench dispatch --no-run' "$D/cargo.log" ||
		{ cat "$D/cargo.log" >&2; fail "cargo arm did not build before running"; }
	grep -q 'bench --locked --manifest-path ./Cargo.toml -p gusset --bench dispatch -- --rounds 12$' "$D/cargo.log" ||
		{ cat "$D/cargo.log" >&2; fail "cargo arm did not pass COUNT as --rounds"; }
	rows=$(grep -c '^BenchmarkDispatch/stub_arm' "$D/out.txt" || true)
	# Warm-up output is discarded; the measured run printed two rows.
	[ "$rows" = 2 ] || { cat "$D/out.txt" >&2; fail "cargo arm recorded $rows rows, want 2"; }
	grep -q 'noise-that-must-not-be-recorded' "$D/out.txt" && fail "cargo's stderr leaked into the results file"
	grep -q '^# gusset-bench-arms: cargo:gusset/dispatch' "$D/out.txt" || fail "the header does not name the cargo arm"
	# The warm-up asked for the bench's own floor of rounds, not -count=1.
	grep -q -- '-- --rounds 10$' "$D/cargo.log" || { cat "$D/cargo.log" >&2; fail "the cargo warm-up did not ask for 10 rounds"; }

	# Mixed with a go arm, each in its own process, both recorded.
	: >"$D/cargo.log"
	rec "$D" RECORD_DIR=. -- "$D/mixed.txt" 1x 12 "Dispatch\$" cargo:gusset/dispatch
	[ "$REC_RC" = 0 ] || { cat "$D/stderr" >&2; fail "mixed arms exited $REC_RC"; }
	grep -q '^BenchmarkDispatch 1 100 ns/op' "$D/mixed.txt" || fail "mixed arms: the go row is missing"
	grep -q '^BenchmarkDispatch/stub_arm' "$D/mixed.txt" || fail "mixed arms: the cargo rows are missing"

	# A malformed spec is refused before anything runs.
	sentinel "$D/out.txt"
	rec "$D" RECORD_DIR=. -- "$D/out.txt" 1x 12 cargo:nonsense
	[ "$REC_RC" = 2 ] || fail "malformed cargo arm: exit $REC_RC, want 2"
	assert_sentinel "$D/out.txt" "malformed cargo arm"

	# Variant mode runs one round per arm, below the bench's floor of rounds: a
	# cargo arm there is refused up front, not after the lock and the warm-up.
	mkdir -p "$D/va" "$D/vb"
	sentinel "$D/out.txt"
	: >"$D/cargo.log"
	rec "$D" "RECORD_VARIANTS=alpha=../va beta=../vb" -- "$D/out.txt" 1x 12 cargo:gusset/dispatch
	[ "$REC_RC" = 2 ] || fail "cargo arm under RECORD_VARIANTS: exit $REC_RC, want 2"
	grep -q "cannot be a RECORD_VARIANTS arm" "$D/stderr" || fail "cargo arm under RECORD_VARIANTS: no explanation"
	assert_sentinel "$D/out.txt" "cargo arm under RECORD_VARIANTS"
	[ ! -s "$D/cargo.log" ] || fail "cargo arm under RECORD_VARIANTS: cargo ran before the refusal"

	# A cargo arm that prints nothing is a zero-row arm like any other.
	printf '%s\n' '#!/bin/sh' 'exit 0' >"$D/bin/cargo"
	sentinel "$D/out.txt"
	rec "$D" RECORD_DIR=. -- "$D/out.txt" 1x 12 cargo:gusset/dispatch
	[ "$REC_RC" = 1 ] || fail "silent cargo arm: exit $REC_RC, want 1"
	assert_sentinel "$D/out.txt" "silent cargo arm"
	echo "ok cargo arm"
}

# --- an arm that fails reports why, and writes nothing ---
# A cargo arm whose build fails never opens its results file, and the report of
# the failure read that file: under `set -e` the script exited on the missing file
# before it printed the compiler's own words, so the one thing that explains the
# failure was the one thing lost.
arm_failure_case() {
	new_case armfail fast
	marker=ARMFAIL-MARKER-7f3a
	# A cargo whose build step fails, and one whose run step fails.
	printf '%s\n' '#!/bin/sh' \
		'for a in "$@"; do' \
		'	if [ "$a" = "--no-run" ]; then echo "error: $ARMFAIL_MARKER could not compile" >&2; exit 1; fi' \
		'done' \
		'exit 0' >"$D/bin/cargo"
	printf '%s\n' '#!/bin/sh' \
		'for a in "$@"; do' \
		'	if [ "$a" = "--no-run" ]; then exit 0; fi' \
		'done' \
		'echo "error: $ARMFAIL_MARKER panicked in the bench" >&2' \
		'exit 1' >"$D/bin/cargo-run-fails"
	printf '%s\n' '#!/bin/sh' \
		'echo "FAIL $ARMFAIL_MARKER build failed"' \
		'echo "$ARMFAIL_MARKER on stderr" >&2' \
		'exit 1' >"$D/bin/go-fails"
	chmod +x "$D/bin/cargo" "$D/bin/cargo-run-fails" "$D/bin/go-fails"

	arm_fails() {
		desc=$1
		shift
		sentinel "$D/out.txt"
		rec "$D" RECORD_DIR=. "ARMFAIL_MARKER=$marker" -- "$D/out.txt" "$@"
		[ "$REC_RC" != 0 ] || fail "$desc: a failing arm exited 0"
		if ! cat "$D/stdout" "$D/stderr" | grep -q "$marker"; then
			echo "--- stdout ---" >&2
			cat "$D/stdout" >&2
			echo "--- stderr ---" >&2
			cat "$D/stderr" >&2
			fail "$desc: the arm's own diagnostic ($marker) was not printed"
		fi
		grep -q "failed in" "$D/stderr" || fail "$desc: no 'failed in' report"
		assert_sentinel "$D/out.txt" "$desc"
		assert_no_lock "$D" "$desc"
	}

	arm_fails "cargo build fails (first arm)" 1x 12 cargo:gusset/dispatch
	arm_fails "cargo build fails (after a go arm)" 1x 12 "Dispatch\$" cargo:gusset/dispatch

	cp "$D/bin/cargo-run-fails" "$D/bin/cargo"
	arm_fails "cargo run fails" 1x 12 cargo:gusset/dispatch

	cp "$D/bin/go-fails" "$D/bin/go"
	arm_fails "go arm fails" 1x 1 "Dispatch\$"
	echo "ok a failing arm prints its diagnostic and writes nothing"
}


# --- a benchmark process the recorder did not start refuses the recording ---
foreign_case() {
	new_case foreign fast
	refuses() {
		desc=$1
		line=$2
		sentinel "$D/out.txt"
		rec "$D" "FAKE_PS_LINES=$line" -- "$D/out.txt" 1x 1 "Dispatch\$"
		[ "$REC_RC" = 1 ] || {
			cat "$D/stderr" >&2
			fail "foreign benchmark ($desc): exit $REC_RC, want a refusal"
		}
		grep -q "another .*benchmark is running" "$D/stderr" || fail "foreign benchmark ($desc): no refusal message"
		assert_sentinel "$D/out.txt" "foreign benchmark ($desc)"
	}
	allows() {
		desc=$1
		line=$2
		rec "$D" "FAKE_PS_LINES=$line" -- "$D/out.txt" 1x 1 "Dispatch\$"
		[ "$REC_RC" = 0 ] || {
			cat "$D/stderr" >&2
			fail "unrelated process ($desc): exit $REC_RC, want it ignored"
		}
	}
	refuses "go test binary" '4242 /tmp/go-build/b001/bench.test -test.run=^$ -test.bench=. -test.count=10'
	refuses "cargo bench binary" '4242 /w/target/release/deps/dispatch-0c4b1d947bd60981 --bench --rounds 10'
	refuses "cargo bench binary with a toolchain path" '4242 /w/repo/target/aarch64-apple-darwin/release/deps/dispatch-0c4b1d947bd60981 --bench'
	refuses "cargo bench itself" '4243 cargo bench -p gusset --bench dispatch'
	refuses "cargo bench with a toolchain" '4243 /Users/x/.cargo/bin/cargo +nightly bench --locked'
	allows "cargo build" '4244 cargo build --locked --release'
	allows "cargo clippy over benches" '4244 cargo clippy --all-targets --benches'
	allows "a bench-named editor buffer" '4245 vim /w/deps/dispatch-0c4b1d947bd60981.rs'
	allows "this script's own cargo arm" '4246 /bin/sh bench/record.sh out.txt 1s 10 cargo:gusset/dispatch'
	echo "ok foreign benchmarks, Go and cargo"
}

# --- the provenance header names the commit that was benchmarked ---
# The version control tool is a stub here, answering from FAKE_VCS_HEAD and
# FAKE_VCS_MODS: what is checked is that the header carries what was asked of it,
# in full, for the directory that was benchmarked, and says `unknown` rather than
# an empty or borrowed value when it cannot answer.
commit_case() {
	new_case commit fast
	printf '%s\n' '#!/bin/sh' \
		'echo "$*" >> "$VCS_LOG"' \
		'if [ "$1" = "-C" ]; then shift 2; fi' \
		'case "$1 $2" in' \
		'"rev-parse HEAD")' \
		'	if [ -z "${FAKE_VCS_HEAD:-}" ]; then echo "fatal: not a repository" >&2; exit 128; fi' \
		'	echo "$FAKE_VCS_HEAD"' \
		'	;;' \
		'"status --porcelain")' \
		'	n=${FAKE_VCS_MODS:-0}; i=0' \
		'	while [ "$i" -lt "$n" ]; do echo " M tracked$i.txt"; i=$((i + 1)); done' \
		'	;;' \
		'*) exit 99 ;;' \
		'esac' >"$D/bin/git"
	chmod +x "$D/bin/git"
	sha=0123456789abcdef0123456789abcdef01234567
	: >"$D/vcs.log"
	rec "$D" "RECORD_DIR=tree" "VCS_LOG=$D/vcs.log" "FAKE_VCS_HEAD=$sha" "FAKE_VCS_MODS=0" -- "$D/out.txt" 1x 1 "Dispatch\$"
	[ "$REC_RC" = 0 ] || { cat "$D/stderr" >&2; fail "commit header: exit $REC_RC"; }
	grep -qx "# gusset-bench-commit: $sha" "$D/out.txt" || { head -12 "$D/out.txt" >&2; fail "header lacks the full commit $sha"; }
	grep -qx '# gusset-bench-commit-modified: 0' "$D/out.txt" || fail "a clean tree was not reported as 0 modified paths"
	grep -q '^-C tree rev-parse HEAD$' "$D/vcs.log" || { cat "$D/vcs.log" >&2; fail "the commit was not asked of the directory that was benchmarked"; }
	rec "$D" "RECORD_DIR=tree" "VCS_LOG=$D/vcs.log" "FAKE_VCS_HEAD=$sha" "FAKE_VCS_MODS=3" -- "$D/out2.txt" 1x 1 "Dispatch\$"
	grep -qx "# gusset-bench-commit: $sha" "$D/out2.txt" || fail "a modified tree changed the commit field"
	grep -qx '# gusset-bench-commit-modified: 3' "$D/out2.txt" || fail "modified tracked paths were not counted"
	rec "$D" "RECORD_DIR=tree" "VCS_LOG=$D/vcs.log" "FAKE_VCS_HEAD=" -- "$D/out3.txt" 1x 1 "Dispatch\$"
	[ "$REC_RC" = 0 ] || fail "a tree the tool cannot describe stopped the recording: exit $REC_RC"
	grep -qx '# gusset-bench-commit: unknown' "$D/out3.txt" || fail "a tree the tool cannot describe was not reported as unknown"
	# Variant mode names the first variant's commit, and still lists each variant.
	mkdir -p "$D/va" "$D/vb"
	rec "$D" "RECORD_VARIANTS=alpha=../va beta=../vb" "VCS_LOG=$D/vcs.log" "FAKE_VCS_HEAD=$sha" -- "$D/out5.txt" 1x 2 "Dispatch\$"
	[ "$REC_RC" = 0 ] || fail "variant recording through the stub: exit $REC_RC"
	grep -qx "# gusset-bench-commit: $sha" "$D/out5.txt" || fail "variant mode lost the commit field"
	grep -q '^-C ../va rev-parse HEAD$' "$D/vcs.log" || { cat "$D/vcs.log" >&2; fail "variant mode did not ask the first variant for its commit"; }
	echo "ok provenance commit"
}


# --- cpu_idle: the branch the host takes reads a real measurement ---
# Not stubbed: on Linux this reads the host's /proc/stat, on macOS it runs top.
# Whether the host is quiet enough to record is not the question (a busy machine
# refuses, or waits for it to settle, and says what it measured); that a number
# was measured is, against "could not measure".
real_idle_case() {
	d=$(mktemp -d "$WORK/realidle.XXXXXX")
	install_stubs "$d/bin" fast
	rm -f "$d/bin/top"
	mkdir -p "$d/run/bench/results"
	start_at "$d/run" 1 "$d/stdio" /bin/sh -c '
export PATH="$1:$PATH"
export ORDER_LOG="$2/order.log"
exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"
' _ "$d/bin" "$d" "$SUT" "$d/out.txt"
	pid=$STARTED
	note_pid "$pid"
	if ! wait_grep "$d/stdio" '[0-9.][0-9.]*% idle'; then
		cat "$d/stdio" >&2
		fail "the $HOST idle measurement produced no reading"
	fi
	if grep -q "could not measure" "$d/stdio"; then
		cat "$d/stdio" >&2
		fail "the $HOST idle measurement reported that it could not measure"
	fi
	kill -TERM "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
	PIDS=
	echo "ok idle measured on $HOST: $(grep -o '[0-9.][0-9.]*% idle' "$d/stdio" | head -1)"
}

# --- cpu_idle: the Linux branch, on any host, against a counter this test moves ---
linux_branch_case() {
	new_case linuxidle fast Linux
	rec "$D" -- "$D/out.txt" 1x 1 "Dispatch\$"
	[ "$REC_RC" = 0 ] || { cat "$D/stderr" >&2; fail "the Linux idle branch: exit $REC_RC"; }
	grep -qE 'preflight ok — (8[5-9]|9[0-9])[.0-9]*% idle' "$D/stdout" || {
		cat "$D/stdout" >&2
		fail "the Linux branch did not read the 90% idle the fed counters describe"
	}
	grep -qE '^# gusset-bench-cpu-idle-after: (8[5-9]|9[0-9])' "$D/out.txt" || fail "the postflight Linux reading is not in the header"
	echo "ok Linux idle branch"
}

# --- recorder_stage_mutation: the suite's own checks have to be able to fail ---
mutant_case() {
	mutant=$WORK/record-mutant.sh
	sed -e 's|kill -TERM "-\$pgid"|kill -TERM "$pid"|' -e 's|kill -KILL "-\$pgid"|kill -KILL "$pid"|' "$SCRIPT" >"$mutant"
	if cmp -s "$SCRIPT" "$mutant"; then
		fail "mutation did not apply: record.sh no longer kills the child's group with a negative pgid"
	fi
	status=0
	(
		SUT=$mutant
		signal_case TERM sep
	) >/dev/null 2>&1 || status=$?
	if [ "$status" = 0 ]; then
		fail "signal_case passed against a recorder that kills only the child's pid"
	fi
	echo "ok signal_case fails against a pid-only kill"
}

# --- the scratch tree is removed on a failing run ---
cleanup_case() {
	before=$(ls "$PROBE" | wc -l | tr -d ' ')
	status=0
	RECORD_TEST_FAULT=1 /bin/sh "$0" >/dev/null 2>&1 || status=$?
	[ "$status" != 0 ] || fail "the injected failure did not fail the run"
	after=$(ls "$PROBE" | wc -l | tr -d ' ')
	if [ "$before" != "$after" ]; then
		ls -a "$PROBE" >&2
		fail "a failing run left $((after - before)) directory(ies) under bench/.probe"
	fi
	echo "ok a failing run removes its scratch tree"
}

# RECORD_TEST_ONLY="count_case order_case" runs just those cases (a case name is
# the function name; the arguments of a case that takes some are not part of it).
run_case() {
	if [ -n "${RECORD_TEST_ONLY:-}" ]; then
		case " $RECORD_TEST_ONLY " in
		*" $1 "*) ;;
		*) return 0 ;;
		esac
	fi
	"$@"
}

run_case count_case
run_case dup_variant_case
run_case norows_case
run_case order_case
run_case grace_case
run_case grace_expiry_case
run_case stale_case
run_case zombie_case lock
run_case zombie_case claim
run_case race_case 8
run_case race_case 8 stale
run_case signal_case INT sep
run_case signal_case TERM sep
run_case signal_case HUP sep
run_case signal_case QUIT sep
run_case signal_case INT family
run_case signal_case TERM family
run_case kill9_case
run_case refusal_case
run_case stage_case
run_case cargo_case
run_case arm_failure_case
run_case foreign_case
run_case commit_case
run_case real_idle_case
run_case linux_branch_case
run_case mutant_case
run_case cleanup_case

kill -0 "$CANARY" 2>/dev/null || fail "parent process group did not survive"
echo "ok record.sh"
