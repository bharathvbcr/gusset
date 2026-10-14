#!/bin/sh
# Exercises bench/record.sh: signal cleanup, lock publication, variant order.
# Recorders that must be interruptible are started with setsid so a regression
# that signals process group 0 cannot reach this shell.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
SCRIPT=$ROOT/bench/record.sh
WORK=$ROOT/bench/.probe
mkdir -p "$WORK"

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
cleanup() {
	if [ -n "$CANARY" ]; then
		kill "$CANARY" 2>/dev/null || true
		wait "$CANARY" 2>/dev/null || true
	fi
	for p in $PIDS; do
		kill -TERM "$p" 2>/dev/null || true
	done
	for p in $PIDS; do
		wait "$p" 2>/dev/null || true
	done
}
trap cleanup EXIT

note_pid() {
	PIDS="$PIDS $1"
}

# start_at DIR SETSID OUT -- argv
# SETSID=1 puts the child in a new session with SIGINT/SIGTERM at their
# default disposition (this agent session ignores SIGINT).
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
    signal.signal(signal.SIGINT, signal.SIG_DFL)
    signal.signal(signal.SIGTERM, signal.SIG_DFL)
os.execvp(sys.argv[3], sys.argv[3:])' "$dir" "$sess" "$@" >"$out" 2>&1 &
	STARTED=$!
}

wait_grep() {
	file=$1
	pat=$2
	i=0
	while [ "$i" -lt 80 ]; do
		if grep -q "$pat" "$file" 2>/dev/null; then
			return 0
		fi
		sleep 0.05
		i=$((i + 1))
	done
	return 1
}

install_stubs() {
	dest=$1
	kind=$2
	mkdir -p "$dest"
	cat >"$dest/uname" <<'EOF'
#!/bin/sh
case "$1" in
-s) echo Darwin ;;
-n) echo record-test ;;
-srm) echo Darwin test test ;;
*) echo Darwin ;;
esac
EOF
	cat >"$dest/top" <<'EOF'
#!/bin/sh
echo "CPU usage: 1.23% user, 2.34% sys, 90.00% idle"
EOF
	cat >"$dest/ps" <<'EOF'
#!/bin/sh
if [ "$1" = "-o" ]; then
	exec /bin/ps "$@"
fi
exit 0
EOF
	if [ "$kind" = "slow" ]; then
		cat >"$dest/go" <<'EOF'
#!/bin/sh
echo $$ > "$GO_PIDFILE"
sleep 120
EOF
	else
		cat >"$dest/go" <<'EOF'
#!/bin/sh
prev=
dir=
for a in "$@"; do
	if [ "$prev" = "-C" ]; then
		dir=$a
	fi
	prev=$a
done
echo "$dir" >> "$ORDER_LOG"
echo "BenchmarkDispatch 1 100 ns/op"
exit 0
EOF
	fi
	chmod +x "$dest/uname" "$dest/top" "$dest/ps" "$dest/go"
}

sleep 300 &
CANARY=$!

# --- variant order: round 1 A then B, round 2 B then A ---
order_case() {
	d=$(mktemp -d "$WORK/order.XXXXXX")
	install_stubs "$d/bin" fast
	mkdir -p "$d/va" "$d/vb" "$d/run"
	: >"$d/order.log"
	python3 -c 'import os,sys
os.chdir(sys.argv[1])
os.execvp(sys.argv[2], sys.argv[2:])' "$d/run" /bin/sh -c '
export PATH="$1:$PATH"
export ORDER_LOG="$2"
export RECORD_VARIANTS="$3"
exec /bin/sh "$4" "$5" 1x 2 "Dispatch\$"
' _ "$d/bin" "$d/order.log" "alpha=../va beta=../vb" "$SCRIPT" "$d/out.txt" \
		>"$d/stdout" 2>"$d/stderr" || {
		echo "--- variant stderr ---" >&2
		cat "$d/stderr" >&2
		fail "variant recording exited non-zero"
	}
	grep -q "gusset-bench-round-1: alpha beta" "$d/out.txt" || {
		cat "$d/out.txt" >&2
		fail "round 1 order was not alpha then beta"
	}
	grep -q "gusset-bench-round-2: beta alpha" "$d/out.txt" || {
		cat "$d/out.txt" >&2
		fail "round 2 order was not beta then alpha"
	}
	got=$(cat "$d/order.log")
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
	rm -rf "$d"
	echo "ok variant order"
}

# --- a lock with no pgid is not stolen immediately ---
grace_case() {
	d=$(mktemp -d "$WORK/grace.XXXXXX")
	install_stubs "$d/bin" slow
	mkdir -p "$d/run/bench/results/.record.lock"
	: >"$d/go.pid"
	start_at "$d/run" 0 "$d/stdio" /bin/sh -c '
export PATH="$1:$PATH"
export GO_PIDFILE="$2"
exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"
' _ "$d/bin" "$d/go.pid" "$SCRIPT" "$d/out.txt"
	pid=$STARTED
	note_pid "$pid"
	if ! wait_grep "$d/stdio" "no pgid"; then
		echo "--- grace stdio ---" >&2
		cat "$d/stdio" >&2
		fail "empty lock was not held for a grace period"
	fi
	if grep -q "clearing a lock" "$d/stdio"; then
		cat "$d/stdio" >&2
		fail "empty lock was stolen inside the grace period"
	fi
	if [ ! -d "$d/run/bench/results/.record.lock" ]; then
		fail "empty lock directory disappeared during the grace period"
	fi
	if [ -s "$d/run/bench/results/.record.lock/pgid" ]; then
		fail "empty lock gained a pgid without being published by a holder"
	fi
	kill -TERM "$pid" 2>/dev/null || true
	status=0
	wait "$pid" || status=$?
	PIDS=
	if [ "$status" -ne 130 ]; then
		echo "--- grace stdio ---" >&2
		cat "$d/stdio" >&2
		fail "grace-period interrupt status $status, want 130"
	fi
	if grep -q "clearing a lock" "$d/stdio"; then
		fail "interrupt during the grace period cleared the lock"
	fi
	rm -rf "$d"
	echo "ok lock grace"
}

# --- N concurrent starts: exactly one winner ---
race_case() {
	d=$(mktemp -d "$WORK/race.XXXXXX")
	install_stubs "$d/bin" slow
	mkdir -p "$d/run"
	: >"$d/go.pid"
	n=8
	: >"$d/pids"
	i=0
	while [ "$i" -lt "$n" ]; do
		i=$((i + 1))
		start_at "$d/run" 0 "$d/stdio.$i" /bin/sh -c '
export PATH="$1:$PATH"
export GO_PIDFILE="$2"
exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"
' _ "$d/bin" "$d/go.pid" "$SCRIPT" "$d/out.txt"
		echo "$STARTED" >>"$d/pids"
		note_pid "$STARTED"
	done
	i=0
	another=0
	alive=0
	winner=
	# Losers refuse at the lock, before the idle sample. The winner may still
	# be inside that sample; do not require its preflight line to have landed.
	while [ "$i" -lt 200 ]; do
		another=$(grep -l "another recording is in progress" "$d"/stdio.* 2>/dev/null | wc -l | tr -d '[:space:]')
		alive=0
		winner=
		while read -r p; do
			if [ -n "$p" ] && kill -0 "$p" 2>/dev/null; then
				alive=$((alive + 1))
				winner=$p
			fi
		done <"$d/pids"
		if [ "$another" = "$((n - 1))" ] && [ "$alive" = "1" ]; then
			break
		fi
		sleep 0.05
		i=$((i + 1))
	done
	clearing=$(grep -l "clearing a lock" "$d"/stdio.* 2>/dev/null | wc -l | tr -d '[:space:]' || true)
	if [ "$another" != "$((n - 1))" ] || [ "${clearing:-0}" != "0" ] || [ "$alive" != "1" ]; then
		echo "another=$another clearing=${clearing:-0} alive=$alive" >&2
		cat "$d"/stdio.* >&2
		fail "concurrent starts did not yield exactly one winner"
	fi
	kill -TERM "$winner" 2>/dev/null || true
	status=0
	wait "$winner" || status=$?
	if [ "$status" -ne 130 ]; then
		fail "winner interrupt status $status, want 130"
	fi
	if [ -d "$d/run/bench/results/.record.lock" ]; then
		fail "winner left the lock behind"
	fi
	# Reap the losers (they already exited).
	while read -r p; do
		wait "$p" 2>/dev/null || true
	done <"$d/pids"
	PIDS=
	rm -rf "$d"
	echo "ok one winner of $n"
}

# --- INT/TERM under setsid: parent group survives, lock is released ---
signal_case() {
	sig=$1
	d=$(mktemp -d "$WORK/sig.XXXXXX")
	install_stubs "$d/bin" slow
	mkdir -p "$d/run"
	: >"$d/go.pid"
	start_at "$d/run" 1 "$d/stdio" /bin/sh -c '
export PATH="$1:$PATH"
export GO_PIDFILE="$2"
exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"
' _ "$d/bin" "$d/go.pid" "$SCRIPT" "$d/out.txt"
	pid=$STARTED
	note_pid "$pid"
	if ! wait_grep "$d/stdio" "warm-up"; then
		echo "--- $sig stdio ---" >&2
		cat "$d/stdio" >&2
		fail "$sig: recorder did not reach warm-up"
	fi
	kill "-$sig" "$pid" 2>/dev/null || true
	status=0
	wait "$pid" || status=$?
	PIDS=
	if [ "$status" -ne 130 ]; then
		echo "--- $sig stdio ---" >&2
		cat "$d/stdio" >&2
		fail "$sig status $status, want 130"
	fi
	if [ -d "$d/run/bench/results/.record.lock" ]; then
		fail "$sig left the lock directory in place"
	fi
	if [ -s "$d/go.pid" ] && kill -0 "$(cat "$d/go.pid")" 2>/dev/null; then
		fail "$sig left the stub go process running"
	fi
	kill -0 "$CANARY" 2>/dev/null || fail "$sig killed the parent process group"
	start_at "$d/run" 1 "$d/stdio2" /bin/sh -c '
export PATH="$1:$PATH"
export GO_PIDFILE="$2"
exec /bin/sh "$3" "$4" 1x 1 "Dispatch\$"
' _ "$d/bin" "$d/go.pid" "$SCRIPT" "$d/out.txt"
	pid2=$STARTED
	note_pid "$pid2"
	if ! wait_grep "$d/stdio2" "preflight ok"; then
		echo "--- $sig next stdio ---" >&2
		cat "$d/stdio2" >&2
		fail "$sig: next run did not start cleanly"
	fi
	if grep -q "another recording is in progress" "$d/stdio2"; then
		fail "$sig: next run printed \"another recording is in progress\""
	fi
	kill -TERM "$pid2" 2>/dev/null || true
	wait "$pid2" 2>/dev/null || true
	PIDS=
	rm -rf "$d"
	echo "ok signal $sig"
}

order_case
grace_case
race_case
signal_case INT
signal_case TERM
kill -0 "$CANARY" 2>/dev/null || fail "parent process group did not survive"
echo "ok record.sh"
