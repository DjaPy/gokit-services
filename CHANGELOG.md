# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/) (pre-1.0.0 —
see the versioning policy in `CLAUDE.md`).

## [Unreleased]

## [0.5.0] - 2026-07-25

### Changed
- **BREAKING**: all library packages moved under `pkg/`. `example/` and `docs/` stay at the
  repo root. The module path and versioning are unchanged, but every import path gains a
  `pkg/` segment — update imports accordingly:

  | Old path | New path |
  |----------|----------|
  | `github.com/DjaPy/gokit-services/core/…` | `github.com/DjaPy/gokit-services/pkg/core/…` |
  | `github.com/DjaPy/gokit-services/http/…` | `github.com/DjaPy/gokit-services/pkg/http/…` |
  | `github.com/DjaPy/gokit-services/grpc/…` | `github.com/DjaPy/gokit-services/pkg/grpc/…` |
  | `github.com/DjaPy/gokit-services/kafka/…` | `github.com/DjaPy/gokit-services/pkg/kafka/…` |
  | `github.com/DjaPy/gokit-services/dbservice` | `github.com/DjaPy/gokit-services/pkg/dbservice` |
  | `github.com/DjaPy/gokit-services/redisservice` | `github.com/DjaPy/gokit-services/pkg/redisservice` |
  | `github.com/DjaPy/gokit-services/healthserver` | `github.com/DjaPy/gokit-services/pkg/healthserver` |
  | `github.com/DjaPy/gokit-services/periodic` | `github.com/DjaPy/gokit-services/pkg/periodic` |
  | `github.com/DjaPy/gokit-services/workerpool` | `github.com/DjaPy/gokit-services/pkg/workerpool` |

  Package names, public APIs, signatures and metrics are unchanged — only the paths move.
- **BREAKING**: `service.Shutdown.Stop` drops its `cause error` parameter — the signature is
  now `Stop(ctx context.Context) error`. No implementation ever consumed `cause`, and the
  reason shutdown began is already available as `entrypoint.Run`'s return value. Update any
  `Stop(ctx, cause)` implementations and calls to `Stop(ctx)`.
- `http/server` — on context cancellation `Start` now drains in-flight requests without a
  deadline of its own; the shutdown deadline is supplied by `Stop`'s context, mirroring
  `grpc/server`. Previously the ctx-cancel path applied a hardcoded 5s timeout. Standalone
  callers relying on ctx cancellation alone now get an unbounded graceful drain — call
  `Stop(ctx)` with a deadline (or run under `entrypoint`) to bound it.

### Fixed
- `http/server` — `entrypoint.WithShutdownTimeout` now actually governs HTTP server shutdown.
  Previously `Start`'s ctx-cancel handler won the race against `Stop` and applied a hardcoded
  5s deadline, so the configured timeout was ignored. `Stop` now bounds the graceful drain by
  forcing connections closed (`server.Close`) when its context expires.
- `http/server` — fixed a goroutine leak: `Start`'s shutdown watcher blocked forever on
  `ctx.Done()` when the server was stopped via `Stop` without the context being canceled. It
  is now released when `Serve` returns.

## [0.4.1] - 2026-07-21

### Fixed
- `grpc/server` — `Stop` now handles an already-expired context deterministically:
  when `ctx.Err() != nil` it performs a forced stop immediately and returns the
  context error instead of taking the graceful path. Previously a graceful stop of
  a server with no active connections completed instantly and could win the race in
  the `select` against `ctx.Done()`, so `Stop` occasionally returned `nil` instead
  of the context error.

## [0.4.0] - 2026-07-20

### Changed
- **BREAKING**: package layout reorganized — transports are grouped by protocol
  (following the `kafka/` model), and the contract/orchestration layer is moved into
  `core/`. Behavior, public APIs, signatures, and metrics are unchanged — only the
  import paths need updating:

  | Old path | New path |
  |----------|----------|
  | `github.com/DjaPy/gokit-services/service` | `github.com/DjaPy/gokit-services/core/service` |
  | `github.com/DjaPy/gokit-services/entrypoint` | `github.com/DjaPy/gokit-services/core/entrypoint` |
  | `github.com/DjaPy/gokit-services/httpserver` | `github.com/DjaPy/gokit-services/http/server` |
  | `github.com/DjaPy/gokit-services/httpclient` | `github.com/DjaPy/gokit-services/http/client` |
  | `github.com/DjaPy/gokit-services/grpcserver` | `github.com/DjaPy/gokit-services/grpc/server` |
  | `github.com/DjaPy/gokit-services/grpcclient` | `github.com/DjaPy/gokit-services/grpc/client` |

  The `service` and `entrypoint` package names are preserved (only the path changes).
  The transport subpackages are now named `server`/`client` — importing them with
  aliases (`httpsrv`, `httpcli`, `grpcsrv`, `grpccli`) is recommended to avoid name
  collisions and the clash with stdlib `net/http`. `kafka/` and the infrastructure
  services (`healthserver`, `periodic`, `workerpool`, `dbservice`, `redisservice`)
  are unaffected.

## [0.3.0] - 2026-07-18

### Added
- `dbservice` — managed PostgreSQL connection pool (`pgxpool`) implementing
  `service.Service`/`Shutdown`/`Prober` with retry-backed startup and polled pool metrics
- `kafka` — shared package with Kafka dialer/probe and TLS + SASL (PLAIN, SCRAM-SHA-256/512)
- `kafka/producer` — managed Kafka producer (`Produce`/`ProduceBatch`, compression, write
  timeout, max attempts)
- `kafka/consumer` — managed Kafka consumer group with bounded worker-pool dispatch,
  at-least-once commit, per-handler timeout, and fetch backoff
- `redisservice` — managed Redis client (`go-redis/v9`) with retry-backed startup and polled
  pool metrics
- `example/orders-service` — PostgreSQL store backend (`ORDERS_STORE=postgres`), opt-in Redis
  read-through cache (`ORDERS_REDIS=on`) and Kafka event publishing/consumption (`ORDERS_KAFKA=on`)
- Integration test infrastructure — Postgres/Kafka/Redis service containers in `ci.yml` and
  `docker-compose.test.yml`, plus `just test-infra-up`/`test-integration`/`test-infra-down`;
  tests skip when `TEST_POSTGRES_DSN`/`TEST_KAFKA_BROKERS`/`TEST_REDIS_DSN` are unset

### Changed
- `example/orders-service` — `Store` is now an interface with in-memory and Postgres backends
  (in-memory remains the default); dashboard UI redesign

## [0.2.0] - 2026-07-11

### Added
- `example/orders-service` — a runnable example microservice combining every gokit-services
  primitive (`entrypoint`, `httpserver`, `httpclient`, `grpcserver`, `grpcclient`,
  `healthserver`, `periodic`, `workerpool`): an in-memory order store exposed over both REST
  and gRPC, async order confirmation via `workerpool`, a `periodic` job that expires stale
  orders and gates `healthserver` readiness, and an htmx dashboard that exercises all of it
  over real network calls (`httpclient` for REST/health, a live `grpcclient` connection for gRPC)

## [0.1.1] - 2026-07-10

### Fixed
- `release.yml` failed on every tag push (including `v0.1.0`) because `ci.yml` didn't declare
  `workflow_call` — no GitHub Release was ever created for `v0.1.0`
- `justfile` bump/release recipes referenced a non-existent `.version` file — switched to
  `git describe --tags` as the sole version source

## [0.1.0] - 2026-07-10

### Added
- `entrypoint` — service lifecycle management: SIGINT/SIGTERM, lifecycle hooks, graceful shutdown
- `httpserver` — HTTP server with Prometheus metrics and panic recovery
- `httpclient` — HTTP client with a middleware chain and generic `Do[T]`
- `grpcserver` — managed gRPC server (`service.Service` + `service.Shutdown`)
- `grpcclient` — managed gRPC client (`service.Service` + `service.Shutdown`)
- `healthserver` — `/healthz` and `/readyz` endpoints with concurrent `service.Prober` polling
- `periodic` — periodic background service (overlapping/non-overlapping modes)
- `workerpool` — bounded goroutine pool with backpressure via `Submit(ctx, task)`
- `service` — base interfaces `Service`, `Shutdown`, `Prober`

[Unreleased]: https://github.com/DjaPy/gokit-services/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/DjaPy/gokit-services/compare/v0.4.1...v0.5.0
[0.4.1]: https://github.com/DjaPy/gokit-services/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/DjaPy/gokit-services/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/DjaPy/gokit-services/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/DjaPy/gokit-services/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/DjaPy/gokit-services/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/DjaPy/gokit-services/releases/tag/v0.1.0