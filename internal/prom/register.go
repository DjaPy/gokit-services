package prom

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// RegisterOrReuse registers collector with reg. If the same collector is
// already registered, it returns the existing one cast to T. Any other
// registration error causes a panic.
func RegisterOrReuse[T prometheus.Collector](reg prometheus.Registerer, collector T) T {
	if err := reg.Register(collector); err != nil {
		if are, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			return are.ExistingCollector.(T)
		}
		panic(err)
	}
	return collector
}
