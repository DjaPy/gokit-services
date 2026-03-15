package service

import "context"

// Service is the interface all services must implement.
// Start should block until the service is fully stopped.
// The ctx is canceled when the entrypoint initiates shutdown.
type Service interface {
	Start(ctx context.Context) error
}

// Stopper is an optional interface a service may implement to perform
// cleanup during graceful shutdown. cause is the error that triggered
// the shutdown (nil for a clean stop).
//
// If a service does not implement Stopper, context cancellation in Start
// is the only shutdown mechanism.
type Stopper interface {
	Stop(ctx context.Context, cause error) error
}
