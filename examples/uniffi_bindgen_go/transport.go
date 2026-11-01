package uniffi_bindgen_go

import (
	"context"
	"errors"
	"fmt"

	"github.com/bharathvbcr/gusset"
)

var (
	// ErrEnginePanic is returned when a Rust panic was caught by Gusset's panic firewall.
	ErrEnginePanic = errors.New("uniffi: engine panicked (contained by Gusset panic firewall)")
	// ErrHandlePoisoned is returned when attempting an operation on a poisoned Gusset handle.
	ErrHandlePoisoned = errors.New("uniffi: handle is poisoned from prior engine panic")
	// ErrTransportClosed is returned when the underlying Gusset handle has been closed.
	ErrTransportClosed = errors.New("uniffi: transport handle is closed")
)

// Transport defines the execution layer between UniFFI serialization and the native engine.
// Gusset implements this interface to provide bounded concurrency, panic containment,
// deadlines, and poisoned handle semantics.
type Transport interface {
	// Call executes a synchronous call with context deadline/cancellation.
	Call(ctx context.Context, payload []byte) ([]byte, error)
	// Submit submits an asynchronous job returning a ticket.
	Submit(ctx context.Context, payload []byte) (uint64, error)
	// Wait waits for completion of a submitted ticket.
	Wait(ctx context.Context, ticket uint64) ([]byte, error)
	// Close terminates the transport and joins worker threads.
	Close() error
}

// GussetTransport wraps a *gusset.Handle to serve as the UniFFI execution transport.
type GussetTransport struct {
	handle *gusset.Handle
}

// NewGussetTransport creates a new GussetTransport wrapping an active Gusset handle.
func NewGussetTransport(h *gusset.Handle) *GussetTransport {
	return &GussetTransport{handle: h}
}

// Handle returns the underlying *gusset.Handle.
func (g *GussetTransport) Handle() *gusset.Handle {
	return g.handle
}

// Call executes payload on the Gusset worker pool.
func (g *GussetTransport) Call(ctx context.Context, payload []byte) ([]byte, error) {
	if g.handle == nil {
		return nil, ErrTransportClosed
	}

	res, err := g.handle.Call(ctx, payload)
	if err != nil {
		return nil, mapGussetError(err)
	}
	return res, nil
}

// Submit enqueues payload on Gusset's bounded queue.
func (g *GussetTransport) Submit(ctx context.Context, payload []byte) (uint64, error) {
	if g.handle == nil {
		return 0, ErrTransportClosed
	}

	ticket, err := g.handle.Submit(ctx, payload)
	if err != nil {
		return 0, mapGussetError(err)
	}
	return ticket, nil
}

// Wait waits for completion of a ticket.
func (g *GussetTransport) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
	if g.handle == nil {
		return nil, ErrTransportClosed
	}

	res, err := g.handle.Wait(ctx, ticket)
	if err != nil {
		return nil, mapGussetError(err)
	}
	return res, nil
}

// Close closes the underlying Gusset handle.
func (g *GussetTransport) Close() error {
	if g.handle == nil {
		return nil
	}
	return g.handle.Close()
}

// mapGussetError translates Gusset runtime errors into typed UniFFI transport errors.
func mapGussetError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gusset.ErrPanic) {
		return fmt.Errorf("%w: %v", ErrEnginePanic, err)
	}
	if errors.Is(err, gusset.ErrPoisoned) {
		return fmt.Errorf("%w: %v", ErrHandlePoisoned, err)
	}
	if errors.Is(err, gusset.ErrClosed) {
		return fmt.Errorf("%w: %v", ErrTransportClosed, err)
	}
	return err
}
