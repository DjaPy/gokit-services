# gokit-services

[![Build](https://github.com/DjaPy/gokit-services/actions/workflows/ci.yml/badge.svg)](https://github.com/DjaPy/gokit-services/actions/workflows/ci.yml)
[![Coverage](https://codecov.io/gh/DjaPy/gokit-services/branch/main/graph/badge.svg)](https://codecov.io/gh/DjaPy/gokit-services)
[![Go Report Card](https://goreportcard.com/badge/github.com/DjaPy/gokit-services)](https://goreportcard.com/report/github.com/DjaPy/gokit-services)
[![Go Reference](https://pkg.go.dev/badge/github.com/DjaPy/gokit-services.svg)](https://pkg.go.dev/github.com/DjaPy/gokit-services)

`gokit-services` is a small, opinionated toolkit for building Go microservices — the plumbing every service
needs, behind minimal, consistent APIs built on functional options. There is no framework to buy into: each
package is usable on its own, and any mix of them composes under a single `entrypoint`.

## What it does

- **Runs a set of services under one lifecycle** — `entrypoint` handles OS signals, ordered pre/post hooks,
  concurrent start/stop and a bounded graceful shutdown.
- **One contract for everything** — every component implements the same `Service` / `Shutdown` / `Prober`
  interfaces, so they wire together without glue.
- **HTTP server and client** — Prometheus metrics and RFC 7807 panic recovery on the server; base URL,
  middleware chain and a generic `Do[T]` JSON decode on the client.
- **gRPC server and client** — managed, with a deterministic graceful stop.
- **Kafka producer and consumer** — TLS + SASL, batching and compression on the producer; a consumer group
  with bounded worker-pool dispatch and at-least-once commit.
- **Managed datastores** — Postgres (`pgxpool`) and Redis pools with retry-backed startup and pool metrics.
- **Operational building blocks** — `/healthz` + `/readyz` health server, periodic jobs, and a bounded worker
  pool with backpressure.

Every metric-owning component takes its own `prometheus.Registerer`, logs through `slog`, and is configured
with functional options.

## Install

```
go get github.com/DjaPy/gokit-services@latest
```

Requires Go 1.26+. The module is a pure library (no `cmd/`), versioned by git tags — pin a release with
`@vX.Y.Z`.

## Concepts

A handful of terms recur across every package:

- **Service** — anything with `Start(ctx) error`. `Start` blocks until the service is fully stopped; the ctx is
  canceled when shutdown begins. This is the only interface a component must implement.
- **Shutdown** — the optional `Stop(ctx) error` for graceful cleanup, called with a context bounded by the
  shutdown timeout. A component without it stops purely on ctx cancellation.
- **Prober** — the optional `Probe(ctx) error` for readiness. `healthserver` polls all registered probers on
  `/readyz`.
- **entrypoint** — the lifecycle manager that owns a set of services and drives them through startup, waiting
  and shutdown. Which optional interfaces a component satisfies is discovered by type assertion — no
  registration.
- **Options** — every constructor takes `Option func(*T)` values; requests take `RequestOption`. All are
  order-independent.

See [ARCHITECTURE.md](ARCHITECTURE.md) for how these fit together and the invariants each package upholds.

## Usage

A minimal service — an HTTP server driven by the lifecycle manager:

```go
package main

import (
	"context"
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
		entrypoint.WithShutdownTimeout(30*time.Second),
	)
	ep.Run(context.Background())
}
```

`ep.Run` blocks until shutdown, which is triggered by `SIGINT`/`SIGTERM`, ctx cancellation, a failing service,
or an explicit `ep.Shutdown()`. The lifecycle runs strictly in order: PreStart hooks → Start (concurrently) →
PostStart hooks → wait → PreStop hooks → Stop (concurrently) → PostStop hooks.

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

The server implements `service.Service` and `service.Shutdown`, collects Prometheus metrics
(`http_request_duration_seconds`, `http_response_size_bytes`, `http_requests_inflight`,
`http_panic_recovery_total`) and recovers from handler panics, replying with an RFC 7807 Problem document
(`application/problem+json`, status 500) as long as the response hasn't started. `responseWriter` forwards
`http.Flusher` and `http.Hijacker`, so SSE and WebSocket work.

> In tests always pass `WithPrometheusRegisterer(prometheus.NewRegistry())` — otherwise a second `NewServer`
> panics on duplicate metric registration. In production use one server per process, or supply your own
> `Registerer`.
</details>

## Packages

| Import path | What it is |
|-------------|------------|
| `pkg/core/entrypoint` | Application lifecycle: signal handling, lifecycle hooks, concurrent start/stop, graceful shutdown |
| `pkg/core/service` | Base interfaces: `Service`, `Shutdown`, `Prober` |
| `pkg/http/server` | HTTP server with Prometheus metrics and panic recovery (RFC 7807) |
| `pkg/http/client` | HTTP client with a base URL, middleware chain and generic `Do[T]` |
| `pkg/grpc/server` | Managed gRPC server |
| `pkg/grpc/client` | Managed gRPC client |
| `pkg/kafka` | Kafka dialer/probe with TLS + SASL (PLAIN, SCRAM-SHA-256/512) |
| `pkg/kafka/producer` | Managed producer (`Produce`/`ProduceBatch`, compression, retries) |
| `pkg/kafka/consumer` | Managed consumer group with bounded worker-pool dispatch, at-least-once commit |
| `pkg/dbservice` | Managed PostgreSQL pool (`pgxpool`) with retry-backed startup and pool metrics |
| `pkg/redisservice` | Managed Redis client (`go-redis/v9`) with retry-backed startup and pool metrics |
| `pkg/healthserver` | `/healthz` and `/readyz` endpoints polling `service.Prober`s concurrently |
| `pkg/periodic` | Periodic background service (overlapping / non-overlapping modes) |
| `pkg/workerpool` | Bounded goroutine pool with backpressure via `Submit(ctx, task)` |

## Details

- **Functional options everywhere.** `Option func(*T)` for constructors, `RequestOption func(*http.Request)` for
  requests. Options are order-independent.
- **`slog` for logging.** `slog.Default()` out of the box, overridable per component with `WithLogger`.
- **Prometheus by default.** Isolated registerers in tests, the default one in production.
- **Real connections in tests.** The suite uses `httptest` and real HTTP — no transport mocks.
- **`example/orders-service`** is a runnable microservice that wires every primitive together: an order store
  exposed over REST and gRPC, async confirmation via `workerpool`, a `periodic` job expiring stale orders and
  gating `healthserver` readiness, optional Postgres/Redis/Kafka backends, and an htmx dashboard driving it all
  over real network calls.

## Development

```
go test ./...          # all tests
go test -race ./...    # with the race detector
```

Integration tests for Postgres/Kafka/Redis are skipped unless `TEST_POSTGRES_DSN`, `TEST_KAFKA_BROKERS` and
`TEST_REDIS_DSN` are set. `just test-infra-up` / `test-integration` / `test-infra-down` spin the backing
containers up and down.

## Architecture

The design — the shared service contract, how `entrypoint` drives the lifecycle, and the load-bearing
invariants each package upholds (shutdown-context handling, deterministic gRPC stop, at-least-once Kafka
commits, Prometheus registerer isolation) — is documented in [ARCHITECTURE.md](ARCHITECTURE.md).

## Status

The project is pre-1.0 and under active development. It follows SemVer, but while on `v0.x` a `MINOR` bump may
introduce new features and breaking changes (marked in the [changelog](CHANGELOG.md)), while `PATCH` is bugfixes
only. Backward compatibility between `0.x` releases is not guaranteed. See [`CLAUDE.md`](CLAUDE.md) for the full
versioning policy and release process.

## License

This project is licensed under the MIT License — see the [LICENSE](LICENSE) file for details.