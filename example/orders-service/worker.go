package orders

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/DjaPy/gokit-services/pkg/workerpool"
)

// OrderProcessor asynchronously "processes" newly created orders — in a
// real system this would reserve inventory, charge payment, and send a
// confirmation email. Processing runs on a bounded workerpool.Pool so a
// burst of orders can't spawn unbounded goroutines; Enqueue's backpressure
// (it blocks until the queue has room) is what protects the process.
type OrderProcessor struct {
	store        Store
	pool         *workerpool.Pool
	processDelay time.Duration
}

// NewOrderProcessor creates a processor backed by poolSize worker
// goroutines. processDelay simulates the latency of the downstream calls a
// real implementation would make.
func NewOrderProcessor(store Store, poolSize int, processDelay time.Duration) *OrderProcessor {
	return &OrderProcessor{
		store:        store,
		pool:         workerpool.New(poolSize, workerpool.WithDrainOnStop()),
		processDelay: processDelay,
	}
}

// Pool exposes the underlying workerpool.Pool so it can be registered as an
// entrypoint service (it implements service.Service and service.Shutdown).
func (p *OrderProcessor) Pool() *workerpool.Pool { return p.pool }

// Enqueue submits orderID for async processing. It blocks until the pool's
// queue has room or ctx is canceled — callers (the HTTP/gRPC handlers) see
// that as their own backpressure signal.
func (p *OrderProcessor) Enqueue(ctx context.Context, orderID string) error {
	if err := p.pool.Submit(ctx, func(taskCtx context.Context) {
		p.process(taskCtx, orderID)
	}); err != nil {
		return fmt.Errorf("enqueue order %s: %w", orderID, err)
	}
	return nil
}

func (p *OrderProcessor) process(ctx context.Context, orderID string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(p.processDelay):
	}

	confirmed, err := p.store.ConfirmPending(ctx, orderID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Error("order processing failed", "order_id", orderID, "error", err)
		return
	}
	if !confirmed {
		slog.Info("order no longer pending, skipped", "order_id", orderID)
		return
	}
	slog.Info("order confirmed", "order_id", orderID)
}
