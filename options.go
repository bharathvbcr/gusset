package gusset

import (
	"fmt"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// Option configures Handle settings.
type Option func(*handleConfig)

type handleConfig struct {
	poolSize uint32
	// poolSizeErr is why the last WithPoolSize value was refused, nil when it
	// was valid. Open returns it.
	poolSizeErr error
	callFlags   uint32
	// pipeOnly skips the shared-memory completion ring, so every completion
	// crosses the pipe. Unexported: it exists for tests and A/B benchmarks.
	pipeOnly      bool
	defaultOpcode uint32
	bufferBudget  int64
}

// MaxPoolSize mirrors gusset::pool::MAX_POOL_SIZE.
//
// Each worker is an OS thread with an 8 MiB stack, so the pool size is a bounded
// resource request, not a free dial. Rust refuses anything larger; this constant
// lets callers check before they ask.
const MaxPoolSize = 1024

// WithPoolSize sets the worker thread pool size for this handle.
//
// Values above MaxPoolSize are not silently clamped: Open returns an error, because
// a caller who asked for 10,000 workers and quietly received 1024 would keep the
// wrong capacity model. Values below 1 are refused too, with an error that says
// so: 0 is not "use the default".
func WithPoolSize(n int) Option {
	return func(c *handleConfig) {
		switch {
		case n < 1:
			c.poolSizeErr = fmt.Errorf("gusset: pool_size must be at least 1 (got %d)", n)
		case n > MaxPoolSize:
			c.poolSizeErr = fmt.Errorf("gusset: pool_size %d exceeds maximum %d (each worker is an OS thread with an 8 MiB stack)", n, MaxPoolSize)
		default:
			c.poolSizeErr = nil
			c.poolSize = uint32(n)
		}
	}
}

// withPipeOnly keeps every completion on the pipe (no completion ring).
func withPipeOnly() Option {
	return func(c *handleConfig) { c.pipeOnly = true }
}

// WithDiagnosticEngine routes this handle's calls to Gusset's built-in diagnostic
// engine when no adopter engine is registered in Rust.
//
// The diagnostic engine selects its behaviour from the first input byte, including
// several deliberate panics, so it must never see untrusted data. Gusset's own panic
// zoo and pitfall suite use it; production callers must not. Without this option a
// handle with no registered engine refuses every submission instead of falling back
// to an implicit echo-or-panic engine.
func WithDiagnosticEngine() Option {
	return func(c *handleConfig) {
		c.callFlags |= ffi.FlagDiagnosticEngine
	}
}

// WithBufferBudget caps the bytes of live buffers allocated with NewBuffer on
// this handle; NewBuffer beyond it returns ErrBufferBudget. 0 (the default)
// means unlimited. A negative budget is refused by Open rather than read as
// unlimited: a caller who asked for a cap and got the sign wrong would
// otherwise get no cap at all, silently.
//
// Rust memory is invisible to Go's GC pacer and each *Buffer is a tiny Go
// object, so a caller that loops on NewBuffer without Free can reach an OOM
// long before any GC runs the AddCleanup backstop. The budget turns that into
// an error at the allocation that crossed it. Result buffers (WaitBuffer,
// CallBuffer) are not charged: refusing them would lose finished work.
func WithBufferBudget(bytes int64) Option {
	return func(c *handleConfig) {
		c.bufferBudget = bytes
	}
}

// WithOpcode sets the default engine dispatch opcode for this handle (R9).
// Dispatches to an engine registered with that opcode in Rust without payload byte mangling.
func WithOpcode(opcode uint32) Option {
	return func(c *handleConfig) {
		c.defaultOpcode = opcode
	}
}
