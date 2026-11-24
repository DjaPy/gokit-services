# gokit-services

[![Build](https://github.com/DjaPy/gokit-services/actions/workflows/ci.yml/badge.svg)](https://github.com/DjaPy/gokit-services/actions/workflows/ci.yml)
[![Coverage](https://codecov.io/gh/DjaPy/gokit-services/branch/main/graph/badge.svg)](https://codecov.io/gh/DjaPy/gokit-services)
[![Go Report Card](https://goreportcard.com/badge/github.com/DjaPy/gokit-services)](https://goreportcard.com/report/github.com/DjaPy/gokit-services)
[![Go Reference](https://pkg.go.dev/badge/github.com/DjaPy/gokit-services.svg)](https://pkg.go.dev/github.com/DjaPy/gokit-services)

`gokit-services` is a small, opinionated toolkit for building Go microservices — the plumbing every service
needs, behind minimal, consistent APIs built on functional options. Each package is usable on its own.
Register lifecycle components with a single `entrypoint` to run them together.

## What it does

- **Runs a set of services under one lifecycle** — `entrypoint` handles OS signals, startup readiness,
  lifecycle hooks, and graceful shutdown.
- **Shared service interfaces** — `Service` defines the lifecycle. `ReadyService`, `Shutdown`, and
  `Prober` add optional capabilities.
- **HTTP server and client** — Prometheus metrics and RFC 7807 panic recovery on the server; base URL,
  middleware chain and a generic `Do[T]` JSON decode on the client.
- **gRPC server and client** — managed, with a deterministic graceful stop.
- **Kafka producer and consumer** — TLS and SASL, batching and compression on the producer,
  and bounded worker-pool dispatch with offset commits on handler success on the consumer.
- **Managed datastores** — Postgres (`pgxpool`) and Redis pools with retry-backed startup and pool metrics.
- **Operational building blocks** — `/healthz` + `/readyz` health server, periodic jobs, and a bounded worker
  pool with backpressure.

Every metric-owning component takes its own `prometheus.Registerer`, logs through `slog`, and is configured
with functional options.

## Install

```
go get github.com/DjaPy/gokit-services@latest
```

Requires Go 1.26.1 or later. The library is versioned by git tags. Pin a release with `@vX.Y.Z`.

## Concepts

A handful of terms recur across every package:

- **Service** — anything with `Start(ctx) error`. `Start` blocks until the service stops. With ordered
  shutdown, `entrypoint` cancels each service's context when its group reaches its turn. Otherwise
  it cancels all service contexts when shutdown begins.
- **Shutdown** — the optional `Stop(ctx) error` for graceful cleanup, called with a context bounded by the
  shutdown timeout. A component without it stops purely on ctx cancellation.
- **ReadyService** — an optional startup contract with `StartWithReady(ctx, ready) error`. The service
  calls `ready()` once it can do its work. With `WithStartupReadiness`, `entrypoint` waits for these
  signals before running `PostStart`.
- **Prober** — the optional `Probe(ctx) error` for readiness. `healthserver` polls all registered probers on
  `/readyz`.
- **entrypoint** — the lifecycle manager that drives registered services through startup and shutdown.
  It detects `ReadyService` and `Shutdown` on those services.
- **Options** — constructors use functional options, and HTTP requests use `RequestOption`.

See [ARCHITECTURE.md](ARCHITECTURE.md) for how these fit together and the invariants each package upholds.

## Usage

A minimal service — an HTTP server driven by the lifecycle manager:

```go
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/DjaPy/gokit-services/pkg/core/entrypoint"
	httpsrv "github.com/DjaPy/gokit-services/pkg/http/server"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httpsrv.NewServer(mux,
		httpsrv.WithPort(8080),
		httpsrv.WithAppName("my-svc"),
	)

	ep := entrypoint.New(
		entrypoint.WithServices(srv),
		entrypoint.WithStartupReadiness(15*time.Second),
		entrypoint.WithShutdownTimeout(30*time.Second),
	)
	result := ep.RunWithResult(context.Background())
	if err := errors.Join(result.Cause, result.ShutdownErr); err != nil {
		log.Fatal(err)
	}
}
```

An OS signal, caller context cancellation, a startup or service error, or an explicit
`ep.Shutdown()` can start shutdown. During `PreStart` or `PostStart`, an interruption cancels the
active hook's context and skips any remaining startup hooks.

Services start concurrently after `PreStart`. By default, `PostStart` follows goroutine launch.
`WithStartupReadiness` requires every registered service to implement `service.ReadyService` and
waits for every startup signal before `PostStart`. A failure or timeout enters the shutdown path.

Add `entrypoint.WithFailOnUnexpectedExit()` when every registered service must stay running
until shutdown. A service that returns `nil` early is reported as a failure and the remaining
services are stopped. This option works with both startup modes.

Use `entrypoint.WithShutdownGroups` to register services in shutdown order.
`entrypoint.WithShutdownGroups([]service.Service{httpSrv}, []service.Service{db})` gives the
server time to drain before canceling the database service. `PreStop` runs while the services
are still active. Services registered through `WithServices` or `AddService` form the final group.
`PreStop`, all groups combined, and `PostStop` each get the configured shutdown timeout. If
the service shutdown budget expires, the remaining groups are canceled and their stops are
attempted with the expired context.

Call `ep.RunWithResult(ctx)` to handle the OS signal, cause, and cleanup outcome separately.
`Signal` is set when an OS signal initiated shutdown. `Cause` reports a startup or service
failure or caller context cancellation. `ShutdownErr` joins shutdown hook errors, service stop
errors, and timeouts observed before the call returns. A clean signal or programmatic shutdown
has nil errors. `Run(ctx)` keeps its original error return behavior.

Transports are grouped by protocol and the contract/orchestration layer lives in `pkg/core/`. Import the
`server`/`client` subpackages with aliases (`httpsrv`, `httpcli`, `grpcsrv`, `grpccli`) to sidestep the generic
names and the clash with stdlib `net/http`.

<details>
<summary><b>HTTP client — base URL, middleware chain, generic decode</b></summary>

```go
import httpcli "github.com/DjaPy/gokit-services/pkg/http/client"

c, err := httpcli.New("https://api.example.com",
	httpcli.WithTimeout(10*time.Second),
	httpcli.WithMiddleware(authMiddleware, tracingMiddleware),
)

type User struct {
	Name string `json:"name"`
}
user, err := httpcli.Do[User](ctx, c, http.MethodGet, "/users/42")
```

`Do[T]` returns an error on any non-2xx status, and the zero value (without an error) for an empty body such as
`204 No Content`. Middleware is applied outer-to-inner in list order and is independent of Option order.
</details>

<details>
<summary><b>HTTP server — metrics and panic recovery</b></summary>

The server implements `service.ReadyService` and `service.Shutdown`, collects Prometheus metrics
(`http_request_duration_seconds`, `http_response_size_bytes`, `http_requests_inflight`,
`http_panic_recovery_total`) and recovers from handler panics, replying with an RFC 7807 Problem document
(`application/problem+json`, status 500) as long as the response hasn't started. `responseWriter` forwards
`http.Flusher` and `http.Hijacker`, so SSE and WebSocket work.

> Servers on the same Prometheus registerer reuse compatible collectors. Pass
> `WithPrometheusRegisterer(prometheus.NewRegistry())` to isolate metrics in tests.
</details>

## Packages

| Import path | What it is |
|-------------|------------|
| `pkg/core/entrypoint` | Application lifecycle with signal handling, readiness, hooks, and ordered shutdown |
| `pkg/core/service` | Base interfaces: `Service`, `ReadyService`, `Shutdown`, `Prober` |
| `pkg/http/server` | HTTP server with Prometheus metrics and panic recovery (RFC 7807) |
| `pkg/http/client` | HTTP client with a base URL, middleware chain and generic `Do[T]` |
| `pkg/grpc/server` | Managed gRPC server |
| `pkg/grpc/client` | Managed gRPC client |
| `pkg/kafka` | Kafka dialer/probe with TLS + SASL (PLAIN, SCRAM-SHA-256/512) |
| `pkg/kafka/producer` | Managed producer (`Produce`/`ProduceBatch`, compression, retries) |
| `pkg/kafka/consumer` | Managed consumer group with bounded worker-pool dispatch and commits on handler success |
| `pkg/dbservice` | Managed PostgreSQL pool (`pgxpool`) with retry-backed startup and pool metrics |
| `pkg/redisservice` | Managed Redis client (`go-redis/v9`) with retry-backed startup and pool metrics |
| `pkg/healthserver` | `/healthz` and `/readyz` endpoints polling `service.Prober`s concurrently |
| `pkg/periodic` | Periodic background service (overlapping / non-overlapping modes) |
| `pkg/workerpool` | Bounded goroutine pool with backpressure via `Submit(ctx, task)` |

Kafka commits advance a partition's offset. A successful later message can move that offset
past an earlier message whose handler failed or is still running. This can prevent redelivery
of the earlier message.

## Details

- **Functional options everywhere.** `Option func(*T)` for constructors and
  `RequestOption func(*http.Request)` for requests.
- **`slog` for logging.** `slog.Default()` out of the box, overridable per component with `WithLogger`.
- **Prometheus by default.** Isolated registerers in tests, the default one in production.
- **Real connections in tests.** The suite uses `httptest` and real HTTP — no transport mocks.
- **`example/orders-service`** is a runnable microservice using the default entrypoint lifecycle. It
  exposes an order store over REST and gRPC, confirms orders via `workerpool`, and uses a `periodic` job
  to expire stale orders and gate `healthserver` readiness. It includes optional Postgres, Redis, and Kafka
  backends and an htmx dashboard.

## Development

```
go test ./...
go test -race ./...
```

Integration tests for Postgres/Kafka/Redis are skipped unless `TEST_POSTGRES_DSN`, `TEST_KAFKA_BROKERS` and
`TEST_REDIS_DSN` are set. `just test-infra-up` / `test-integration` / `test-infra-down` spin the backing
containers up and down.

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the service interfaces, shutdown contexts, gRPC stop
behavior, Kafka offset handling, and Prometheus registerer isolation.

## Status

The project is pre-1.0 and under active development. It follows SemVer, but while on `v0.x` a `MINOR` bump may
introduce new features and breaking changes (marked in the [changelog](CHANGELOG.md)), while `PATCH` is bugfixes
only. Backward compatibility between `0.x` releases is not guaranteed. See [`agent.md`](agent.md) for the full
versioning policy and release process.

## License

This project is licensed under the MIT License — see the [LICENSE](LICENSE) file for details.