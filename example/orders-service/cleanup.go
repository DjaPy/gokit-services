package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/DjaPy/gokit-services/pkg/periodic"
)

var errCleanupNotWarmedUp = errors.New("cleanup job has not completed its first sweep yet")

// CleanupJob periodically cancels orders that have sat in PENDING longer
// than staleAfter — the real-world policy this models is "if we haven't
// confirmed an order in time, don't leave it dangling forever". It also
// tracks whether it has completed at least one pass, which the readiness
// Prober uses: the service isn't ready until stale orders have actually
// been swept once, not just until the process started.
type CleanupJob struct {
	store      Store
	staleAfter time.Duration
	ready      func(context.Context) error
	warmedUp   atomic.Bool
}

// CleanupOption configures a CleanupJob.
type CleanupOption func(*CleanupJob)

// WithReadyGate makes each sweep wait for ready(ctx) to return nil first. It
// gates the first sweep until the backend is actually usable (e.g. the Postgres
// schema exists), so the job never runs against a store that is still
// connecting. Once the gate first passes it returns immediately.
func WithReadyGate(ready func(context.Context) error) CleanupOption {
	return func(j *CleanupJob) { j.ready = ready }
}

func NewCleanupJob(store Store, staleAfter time.Duration, opts ...CleanupOption) *CleanupJob {
	j := &CleanupJob{store: store, staleAfter: staleAfter}
	for _, opt := range opts {
		opt(j)
	}
	return j
}

// Service builds the periodic.Service that runs this job. WithImmediateStart
// means the first sweep (and therefore warmedUp) happens right away instead
// of waiting a full interval.
func (j *CleanupJob) Service(interval time.Duration) *periodic.Service {
	return periodic.New(interval, j.run, periodic.WithImmediateStart())
}

func (j *CleanupJob) run(ctx context.Context) error {
	if j.ready != nil {
		if err := j.ready(ctx); err != nil {
			return nil
		}
	}
	cutoff := time.Now().Add(-j.staleAfter)
	canceled, err := j.store.CancelStalePending(ctx, cutoff)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("cancel stale pending orders: %w", err)
	}
	if len(canceled) > 0 {
		slog.Info("canceled stale pending orders", "count", len(canceled), "order_ids", canceled)
	}
	j.warmedUp.Store(true)
	return nil
}

// Probe implements service.Prober: not ready until the first sweep ran.
func (j *CleanupJob) Probe(_ context.Context) error {
	if !j.warmedUp.Load() {
		return errCleanupNotWarmedUp
	}
	return nil
}
