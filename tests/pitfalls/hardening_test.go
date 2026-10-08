package pitfalls_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestHardening_UntrustedInputCannotSelectPanic is the headline regression.
//
// Gusset's built-in diagnostic engine picks its behaviour from input[0]: byte 1 is
// panic!("plain panic"), byte 2 panics with an embedded NUL, byte 3 is
// panic_any(42). That engine used to be the *default* whenever no adopter engine was
// registered in Rust — and no C export registers one, so any Go service linking
// libgusset.a before its Rust engine registered itself ran it.
//
// The consequence: the first byte of an untrusted payload selected a Rust panic,
// which poisoned the handle and failed every subsequent call. One attacker-chosen
// byte, and the handle is dead until the process restarts.
//
// A handle opened without WithDiagnosticEngine must now refuse every byte instead.
func TestHardening_UntrustedInputCannotSelectPanic(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Every possible first byte, including the panic selectors.
	for b := 0; b < 256; b++ {
		_, err := h.Call(ctx, []byte{byte(b), 0xAA, 0xBB})
		if err == nil {
			t.Fatalf("byte %d was executed by an implicit engine; submission must be refused", b)
		}
		if errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("byte %d drove the engine into a panic (I2 breach via untrusted input)", b)
		}
		if !strings.Contains(err.Error(), "no engine handler registered") {
			t.Fatalf("byte %d: expected a refusal naming the missing engine, got: %v", b, err)
		}
	}

	// 256 refusals, and the handle is still healthy: a refusal is not a panic, so
	// it must not poison.
	if _, err := h.Call(ctx, []byte{1}); !strings.Contains(err.Error(), "no engine handler registered") {
		t.Fatalf("handle was poisoned by refusals; expected a plain refusal, got: %v", err)
	}
}

// TestHardening_DiagnosticEngineOptionExercisesBothSettings satisfies the working
// rule that every config option has a test covering both settings: the same input
// byte is refused without the option and executed with it.
func TestHardening_DiagnosticEngineOptionExercisesBothSettings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	off, err := gusset.Open(gusset.WithPoolSize(2))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer off.Close()

	if _, err := off.Call(ctx, []byte{0, 9, 9}); err == nil {
		t.Fatal("without WithDiagnosticEngine, the diagnostic echo must not run")
	}

	on, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer on.Close()

	out, err := on.Call(ctx, []byte{0, 9, 9})
	if err != nil {
		t.Fatalf("with WithDiagnosticEngine, mode 0 echo must run: %v", err)
	}
	if len(out) != 3 || out[1] != 9 || out[2] != 9 {
		t.Fatalf("diagnostic echo returned %v", out)
	}
}

// TestHardening_PoolSizeIsBounded pins the ceiling on worker threads.
//
// WithPoolSize fed straight through to thread::Builder::spawn with no bound, so
// WithPoolSize(1<<20) asked for a million OS threads with 8 MiB stacks each.
func TestHardening_PoolSizeIsBounded(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(gusset.MaxPoolSize + 1))
	if err == nil {
		_ = h.Close()
		t.Fatalf("pool size %d must be refused, not spawned", gusset.MaxPoolSize+1)
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("expected the error to name the ceiling, got: %v", err)
	}

	// An ordinary size still works: the bound must not break the normal path.
	ok, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("ordinary pool size rejected: %v", err)
	}
	defer ok.Close()
}

// Unknown CallHeader flag bits are rejected rather than silently dropped. Covered
// in tests/rust_runtime.rs (submit_rejects_unknown_header_flags): the public Go API
// deliberately exposes no way to set arbitrary flag bits, and adding a test-only
// accessor would put production surface in the package to serve a test.

// TestHardening_BufferBytesNotServedAfterRelease closes the window where Bytes()
// handed out a Go slice over Rust memory that had already been released.
//
// The Go garbage collector does not trace Rust memory, so nothing about holding the
// slice keeps the buffer alive or marks it invalid.
func TestHardening_BufferBytesNotServedAfterRelease(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	buf, err := h.NewBuffer(8192)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	if got := buf.Bytes(); len(got) != 8192 {
		t.Fatalf("live buffer must expose its bytes, got %d", len(got))
	}

	if err := buf.Free(); err != nil {
		t.Fatalf("Free failed: %v", err)
	}
	if got := buf.Bytes(); got != nil {
		t.Fatalf("Bytes() must not serve released memory, got %d bytes", len(got))
	}

	// A buffer outlived by its handle is released when the handle closes, so the
	// same rule applies there.
	buf2, err := h.NewBuffer(4096)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if got := buf2.Bytes(); got != nil {
		t.Fatalf("Bytes() must not serve memory released by Handle.Close, got %d bytes", len(got))
	}
}

// TestHardening_TraceContextPropagates covers R9's trace correlation.
//
// The header was read from ctx.Value("spanContext") — a bare string key, which go
// vet flags and which no real tracing library writes to, so trace_id and span_id
// were always zero and the code was unreachable in practice.
func TestHardening_TraceContextPropagates(t *testing.T) {
	ctx := context.WithValue(context.Background(), gusset.SpanContextKey, testSpan{})

	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Diagnostic mode 7 returns the header's trace_id followed by its span_id, so
	// this reads back what actually crossed the boundary.
	out, err := h.Call(ctx, []byte{7})
	if err != nil {
		t.Fatalf("call with a trace carrier failed: %v", err)
	}
	if len(out) != 24 {
		t.Fatalf("expected 16-byte trace id + 8-byte span id, got %d bytes", len(out))
	}
	if out[0] != 0xAB {
		t.Fatalf("trace id did not reach Rust: got %#v", out[:16])
	}
	if out[16] != 0xCD {
		t.Fatalf("span id did not reach Rust: got %#v", out[16:])
	}

	// A context with no carrier must leave the ids zeroed, not carry stale values.
	bare, cancelBare := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelBare()
	out2, err := h.Call(bare, []byte{7})
	if err != nil {
		t.Fatalf("call without a trace carrier failed: %v", err)
	}
	for i, b := range out2 {
		if b != 0 {
			t.Fatalf("expected a zeroed header without a carrier, byte %d = %d", i, b)
		}
	}
}

type testSpan struct{}

func (testSpan) TraceID() [16]byte {
	var b [16]byte
	b[0] = 0xAB
	return b
}

func (testSpan) SpanID() [8]byte {
	var b [8]byte
	b[0] = 0xCD
	return b
}

// TestHardening_WaitBufferZeroCopyEgress exercises the zero-copy return path (R16).
// Ensures WaitBuffer returns a Rust-owned *Buffer, Bytes() returns the content,
// and Free releases it cleanly.
func TestHardening_WaitBufferZeroCopyEgress(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Diagnostic engine: byte 0 echoes the tail
	payload := []byte{0, 1, 2, 3, 4, 5}
	ticket, err := h.Submit(ctx, payload)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	buf, err := h.WaitBuffer(ctx, ticket)
	if err != nil {
		t.Fatalf("WaitBuffer failed: %v", err)
	}
	if buf == nil {
		t.Fatal("expected non-nil Buffer from WaitBuffer")
	}

	bytes := buf.Bytes()
	if len(bytes) != 6 || bytes[0] != 0 || bytes[5] != 5 {
		t.Fatalf("unexpected buffer bytes: %v", bytes)
	}

	if err := buf.Free(); err != nil {
		t.Fatalf("buf.Free failed: %v", err)
	}
	if buf.Bytes() != nil {
		t.Fatalf("expected nil bytes after Free, got %v", buf.Bytes())
	}

	// Idempotent free
	if err := buf.Free(); err != nil {
		t.Fatalf("double Free returned error: %v", err)
	}
}

// TestHardening_OpcodeOptionsAndContext verifies WithOpcode and ContextWithOpcode options (R9).
func TestHardening_OpcodeOptionsAndContext(t *testing.T) {
	// Handle with default opcode
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithOpcode(42))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()
	ctxOp := gusset.ContextWithOpcode(ctx, 99)

	if op, ok := ctxOp.Value(gusset.OpcodeContextKey).(uint32); !ok || op != 99 {
		t.Fatalf("expected opcode 99 attached to context, got %v", op)
	}
}

// TestPitfall_PoolSizeOverflowIsRefusedNotTruncated pins the uint32 conversion.
//
// WithPoolSize stored uint32(n) and Open only compared the truncated value to
// MaxPoolSize. WithPoolSize(1<<40) became 0 (a 0-capacity semaphore that hangs
// every Call) and WithPoolSize(1<<32+4) silently became a 4-worker pool. The
// contract is refuse, not clamp, including values that do not fit in uint32.
func TestPitfall_PoolSizeOverflowIsRefusedNotTruncated(t *testing.T) {
	for _, n := range []int{1 << 40, 1<<32 + 4, 0, -1} {
		h, err := gusset.Open(gusset.WithPoolSize(n), gusset.WithDiagnosticEngine())
		if err == nil {
			_ = h.Close()
			t.Fatalf("WithPoolSize(%d) must be refused, not truncated into a live handle", n)
		}
		if !strings.Contains(err.Error(), "pool_size") {
			t.Fatalf("WithPoolSize(%d): expected the error to name pool_size, got: %v", n, err)
		}
	}
}

// TestPitfall_PoolSizeRefusalNamesTheActualProblem pins one message per
// invalid case.
//
// WithPoolSize(0) and WithPoolSize(-1) were refused with "pool_size exceeds
// maximum 1024", which sent a caller who passed zero looking for a number that
// was too large. Each refusal now says what was wrong with the value given.
func TestPitfall_PoolSizeRefusalNamesTheActualProblem(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "pool_size must be at least 1 (got 0)"},
		{-1, "pool_size must be at least 1 (got -1)"},
		{gusset.MaxPoolSize + 1, "pool_size 1025 exceeds maximum 1024"},
		{1 << 40, "pool_size 1099511627776 exceeds maximum 1024"},
	}
	for _, c := range cases {
		h, err := gusset.Open(gusset.WithPoolSize(c.n), gusset.WithDiagnosticEngine())
		if err == nil {
			_ = h.Close()
			t.Fatalf("WithPoolSize(%d) must be refused", c.n)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("WithPoolSize(%d): got %q, want it to say %q", c.n, err, c.want)
		}
		if c.n < 1 && strings.Contains(err.Error(), "exceeds") {
			t.Errorf("WithPoolSize(%d): %q blames a ceiling the value is below", c.n, err)
		}
	}

	// The last WithPoolSize wins, as before: a valid size after an invalid one
	// opens.
	h, err := gusset.Open(gusset.WithPoolSize(0), gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("a valid WithPoolSize after an invalid one was refused: %v", err)
	}
	_ = h.Close()
}

// TestPitfall_NegativeBufferBudgetIsRefused pins refuse-not-clamp for the
// NewBuffer budget (DECISIONS 2026-09-20).
//
// WithBufferBudget(-1) used to become 0, which means unlimited: a caller who
// computed a budget, got the sign wrong, and asked for a cap received no cap at
// all, silently. Open now refuses it, and both settings of the option still
// behave: a positive budget refuses the allocation that crosses it, and 0
// leaves NewBuffer unlimited.
func TestPitfall_NegativeBufferBudgetIsRefused(t *testing.T) {
	for _, n := range []int64{-1, -(64 << 10), math.MinInt64} {
		h, err := gusset.Open(gusset.WithBufferBudget(n), gusset.WithDiagnosticEngine())
		if err == nil {
			_ = h.Close()
			t.Fatalf("WithBufferBudget(%d) opened a handle; a negative budget must be refused", n)
		}
		want := fmt.Sprintf("buffer budget must not be negative (got %d)", n)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("WithBufferBudget(%d): got %q, want it to say %q", n, err, want)
		}
	}

	capped, err := gusset.Open(gusset.WithBufferBudget(64<<10), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("a positive budget was refused: %v", err)
	}
	defer capped.Close()
	b, err := capped.NewBuffer(64 << 10)
	if err != nil {
		t.Fatalf("an allocation within the budget was refused: %v", err)
	}
	defer b.Free()
	if _, err := capped.NewBuffer(1); !errors.Is(err, gusset.ErrBufferBudget) {
		t.Fatalf("an allocation over the budget: got %v, want ErrBufferBudget", err)
	}

	unlimited, err := gusset.Open(gusset.WithBufferBudget(0), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("WithBufferBudget(0) was refused: %v", err)
	}
	defer unlimited.Close()
	for i := 0; i < 2; i++ {
		u, err := unlimited.NewBuffer(64 << 10)
		if err != nil {
			t.Fatalf("WithBufferBudget(0) is unlimited, but NewBuffer %d failed: %v", i, err)
		}
		defer u.Free()
	}
}

// TestPitfall_InlineSliceOver4KiBIsRefused is R16 at the Go boundary.
//
// Handle::submit copied every []byte into TaskPayload::Inline, including a 1 MiB
// payload, so the cgo call itself did the copy and pinned an M for the duration.
// Inputs above 4 KiB must arrive as a Rust-owned Buffer; a raw slice is refused.
func TestPitfall_InlineSliceOver4KiBIsRefused(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	over := make([]byte, 4097)
	over[0] = 0
	if _, err := h.Call(ctx, over); err == nil {
		t.Fatal("Call with 4097-byte []byte must be refused (R16); use NewBuffer")
	}
	if _, err := h.Submit(ctx, over); err == nil {
		t.Fatal("Submit with 4097-byte []byte must be refused (R16); use NewBuffer")
	}

	// 4 KiB is the documented copy ceiling and must still work.
	ok := make([]byte, 4096)
	ok[0] = 0
	if _, err := h.Call(ctx, ok); err != nil {
		t.Fatalf("Call with 4096-byte []byte must still be copied, got: %v", err)
	}

	buf, err := h.NewBuffer(len(over))
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	defer buf.Free()
	copy(buf.Bytes(), over)
	ticket, err := h.Submit(ctx, buf)
	if err != nil {
		t.Fatalf("Submit of a Buffer above 4 KiB must succeed: %v", err)
	}
	got, err := h.Wait(ctx, ticket)
	if err != nil {
		t.Fatalf("Wait of a Buffer above 4 KiB failed: %v", err)
	}
	if len(got) != len(over) {
		t.Fatalf("expected %d echoed bytes, got %d", len(over), len(got))
	}
}

// TestPitfall_NewBufferRefusesPoisonedHandle is R10 for the alloc path.
//
// After a caught panic, Submit was refused but NewBuffer still entered Rust and
// allocated. A poisoned handle must fail closed on every later call except Close
// and the drain of tickets that were already in flight.
func TestPitfall_NewBufferRefusesPoisonedHandle(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.Call(ctx, []byte{1}); !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected a caught panic, got %v", err)
	}
	if _, err := h.NewBuffer(64); !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("NewBuffer on a poisoned handle must return ErrPoisoned, got %v", err)
	}
}

// TestPitfall_ThreadsMetricIsLive is the R11 soak's assertion source.
//
// Go 1.26 release notes named `/sched/threads:threads`. The 1.27 runtime/metrics
// catalogue only lists `/sched/threads/total:threads`. Threads() must read a live
// metric, not return -1 because the documented name drifted.
func TestPitfall_ThreadsMetricIsLive(t *testing.T) {
	if n := gusset.Threads(); n < 1 {
		t.Fatalf("Threads() returned %d; the soak cannot assert I4 against a missing metric", n)
	}

	samples := []metrics.Sample{
		{Name: "/sched/threads/total:threads"},
		{Name: "/sched/threads:threads"},
	}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindUint64 {
		t.Fatalf("Go %s does not publish /sched/threads/total:threads (kind %v); update Threads()",
			runtime.Version(), samples[0].Value.Kind())
	}
}

// waitLiveBytes polls Stats().LiveBytes until ok accepts it or 30s pass, and
// returns the last reading with whether ok accepted it.
func waitLiveBytes(ok func(uint64) bool) (uint64, bool) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		live := gusset.Stats().LiveBytes
		if ok(live) {
			return live, true
		}
		if time.Now().After(deadline) {
			return live, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPitfall_DiscardReleasesUncollectedResults pins the bound on results
// nobody Waits for.
//
// Since a finished Submit returns its permit before Wait (DECISIONS
// 2026-09-30), nothing back-pressures a fire-and-forget caller: every result
// it never collects stays on the handle until Wait or Close, and a result over
// 4 KiB is a live Rust take buffer that the Go GC pacer cannot see. There was
// no way to let one go short of closing the handle. Discard is that way.
//
// The test first shows the retention (Rust live bytes grow by the results
// nobody collected), then that Discard gives them back, for results already
// stored and for one discarded while still running.
func TestPitfall_DiscardReleasesUncollectedResults(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 64 KiB echoes: over the 4 KiB inline ceiling, so each result is a Rust
	// take buffer held on the handle, not a Go copy.
	const payload = 64 * 1024
	const rounds = 16
	submit := func(prefix []byte) uint64 {
		t.Helper()
		b, err := h.NewBuffer(payload)
		if err != nil {
			t.Fatalf("NewBuffer failed: %v", err)
		}
		s := b.Bytes()
		copy(s, prefix)
		for i := len(prefix); i < len(s); i++ {
			s[i] = byte(i)
		}
		ticket, err := h.Submit(ctx, b)
		if err != nil {
			_ = b.Free()
			t.Fatalf("Submit failed: %v", err)
		}
		if err := b.Free(); err != nil {
			t.Fatalf("Free failed: %v", err)
		}
		return ticket
	}

	// Warm-up so one-time allocations are in the baseline.
	if _, err := h.Wait(ctx, submit([]byte{0})); err != nil {
		t.Fatalf("warm-up Wait failed: %v", err)
	}
	baseline := gusset.Stats().LiveBytes

	tickets := make([]uint64, 0, rounds)
	for i := 0; i < rounds; i++ {
		tickets = append(tickets, submit([]byte{0}))
	}
	retained := baseline + rounds*payload
	if live, ok := waitLiveBytes(func(v uint64) bool { return v >= retained }); !ok {
		t.Fatalf("%d uncollected %d-byte results should be held until Wait, Discard or Close: "+
			"live %d, baseline %d", rounds, payload, live, baseline)
	}

	for i, ticket := range tickets {
		if err := h.Discard(ticket); err != nil {
			t.Fatalf("Discard of stored result %d: %v", i, err)
		}
	}
	if live, ok := waitLiveBytes(func(v uint64) bool { return v < baseline+payload }); !ok {
		t.Fatalf("Discard did not release the take buffers: live %d, baseline %d, %d results of %d bytes",
			live, baseline, rounds, payload)
	}

	// A discarded ticket is spent, like an abandoned one.
	if _, err := h.Wait(ctx, tickets[0]); !errors.Is(err, gusset.ErrUnknownTicket) {
		t.Fatalf("Wait after Discard: got %v, want ErrUnknownTicket", err)
	}
	if err := h.Discard(tickets[0]); !errors.Is(err, gusset.ErrUnknownTicket) {
		t.Fatalf("second Discard: got %v, want ErrUnknownTicket", err)
	}
	if err := h.Discard(1 << 62); !errors.Is(err, gusset.ErrUnknownTicket) {
		t.Fatalf("Discard of a ticket never issued: got %v, want ErrUnknownTicket", err)
	}

	// Discarded while running: the engine finishes 200 ms later, and the
	// result it produces then is freed on arrival instead of stored.
	running := submit(nonCoopThen(200*time.Millisecond, 0))
	if err := h.Discard(running); err != nil {
		t.Fatalf("Discard of a running ticket: %v", err)
	}
	if live, ok := waitLiveBytes(func(v uint64) bool { return v < baseline+payload }); !ok {
		t.Fatalf("a result discarded while running was kept when it landed: live %d, baseline %d", live, baseline)
	}

	// The handle still works, and Discard did not cancel or poison anything.
	if _, err := h.Call(ctx, []byte{0, 1}); err != nil {
		t.Fatalf("Call after Discard: %v", err)
	}
}
