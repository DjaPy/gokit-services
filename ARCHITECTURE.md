# Architecture

This document describes how `gokit-services` is put together — the contract every
component shares, how the lifecycle manager drives them, and the non-obvious invariants
each package upholds. It targets contributors and anyone wiring the toolkit into a
service. For usage, see the [README](README.md).

## Package organization

All library code lives under `pkg/`; the repo root holds only `example/`, `docs/` and
project files. Import paths are therefore `github.com/DjaPy/gokit-services/pkg/…` (package
names below are given relative to `pkg/`). The layout separates the *contract* from the
*transports* from the *managed resources*:

- `core/` — the orchestration layer. `core/service` holds the interfaces everything else
  implements; `core/entrypoint` is the only package that knows how to run a set of
  services. Nothing in `core/` imports a transport.
- `http/`, `grpc/`, `kafka/` — transports grouped by protocol, each split into `server`
  and `client` (or `producer`/`consumer`) subpackages. They depend on `core/service` but
  not on each other.
- Infrastructure packages — `dbservice`, `redisservice`, `healthserver`, `periodic`,
  `workerpool` — are leaf components under `pkg/` that also implement the `core/service`
  contract and are consumed the same way. `internal/` (retry, prom helpers) is shared
  within `pkg/` and not importable by consumers.

The dependency direction is strictly one-way: leaves → `core/service` ← `core/entrypoint`.
`entrypoint` depends only on the interfaces, never on a concrete transport, so the set of
components is open-ended — anything satisfying `Service` composes without touching `core/`.

The `server`/`client` subpackage names repeat across protocols on purpose; consumers alias
them at import (`httpsrv`, `httpcli`, `grpcsrv`, `grpccli`) to disambiguate and to avoid
shadowing stdlib `net/http`.

## Service contract (`core/service`)

Three small interfaces, composed by capability rather than inheritance:

- `Service` — `Start(ctx) error`. The one required method. `Start` **blocks** until the
  service is fully stopped; the `ctx` is canceled when the entrypoint begins shutdown. A
  service that only implements `Service` treats ctx-cancellation as its sole stop signal.
- `Shutdown` — `Stop(ctx) error`. Optional graceful stop, called with a context bounded by
  the shutdown timeout. Implemented via a type assertion, so it is purely additive.
- `Prober` — `Probe(ctx) error`. Optional readiness participation; `nil` means ready.

Every component is a plain struct constructed with functional options (`Option func(*T)`)
and a `New`/`NewServer` constructor. Which of the three interfaces a component satisfies is
discovered at runtime by `entrypoint` and `healthserver` through type assertions — there
are no registration calls and no compile-time interface assertions (`var _ Service = …`) in
the tree; interface conformance is asserted by the test suite instead.

## Lifecycle (`core/entrypoint`)

`Entrypoint` owns a `[]service.Service`, a shutdown timeout, the set of OS signals to catch,
four hook slices (pre/post × start/stop), and an internal `context.CancelCauseFunc` that
backs the programmatic `Shutdown()` trigger.

`Run(ctx)` executes a fixed sequence:

1. **PreStart hooks** — run sequentially; a failure aborts before anything starts.
2. **Start** — every service's `Start` is launched concurrently under a derived
   `svcCtx` (child of the caller's `ctx`) using `sync.WaitGroup.Go`. A service error is
   forwarded to a buffered channel *only* if `svcCtx` is still live — errors observed after
   cancellation are shutdown noise and dropped.
3. **PostStart hooks** — sequential; a failure cancels `svcCtx` and returns.
4. **Wait** — a `select` blocks on the first of: caller `ctx` cancellation, an OS signal,
   the first service error, or the programmatic shutdown ctx. The winner becomes
   `shutdownCause`.
5. **PreStop hooks**, then **Stop** — every service that also implements `Shutdown` has
   `Stop(stopCtx)` called concurrently; stop errors are logged, not propagated. The reason
   shutdown began (`shutdownCause`) is not passed to `Stop` — it is `Run`'s return value.
6. **PostStop hooks** — always run.

`Run` returns `shutdownCause`, so the process exit reflects *why* it stopped.

**Shutdown-context invariant.** Once shutdown begins, the service `ctx` is canceled, but the
stop phase must still be able to do bounded cleanup. The stop context is therefore built as
`context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)` — it inherits the caller's
values but is detached from its cancellation, so a `Stop` that needs to flush or drain gets
its full timeout even though the parent `ctx` is already done. PostStop hooks run on a fresh
`context.Background()` for the same reason. Getting this wrong (using the live `ctx`, or
`context.Background()` without the parent's values) is the classic regression here.

## HTTP (`http/server`, `http/client`)

**Server.** `Server` wraps a stdlib `*http.Server` and implements both `Service` and
`Shutdown`. `Start` binds the listener and serves; `Stop` calls `http.Server.Shutdown`.
Every request passes through a middleware chain that records four Prometheus series
(`http_request_duration_seconds`, `http_response_size_bytes`, `http_requests_inflight`,
`http_panic_recovery_total`, labeled by service/handler/method/code) and recovers panics.
On a recovered panic the client gets an RFC 7807 `application/problem+json` 500 — but only
when the response hasn't started; once bytes are on the wire the middleware can only log.

The metrics live behind a `prometheus.Registerer` chosen at construction. The default
registerer is process-global, so two servers built with the default panic on duplicate
registration — hence `WithPrometheusRegisterer` for tests and multi-server processes.

`responseWriter` is a thin wrapper that captures status and byte count for the metrics. It
deliberately forwards `http.Flusher` and `http.Hijacker`, so streaming responses (SSE) and
connection upgrades (WebSocket) keep working through the instrumentation.

**Client.** A `Client` binds a base URL plus a `RoundTripper` middleware chain. The generic
`Do[T]` performs the request and decodes JSON into `T`: it returns an error on any non-2xx,
and the zero value without error for an empty body (`204`). Middleware is ordered
outer-to-inner as listed and is applied after all Options, so `WithMiddleware`/`WithTransport`
are order-independent. `WithBody` sets `Content-Length` automatically for bodies exposing
`Len() int` (`*bytes.Buffer`, `*strings.Reader`).

## gRPC (`grpc/server`, `grpc/client`)

`grpc/server.Server` wraps a `*grpc.Server`, exposed via `GRPCServer()` so callers register
their services before `Start`. `Start` binds and serves; `Addr()` returns the configured
address before start and the actual bound address after.

**Deterministic stop.** `Stop` distinguishes two cases up front. If the passed `ctx` is
already expired (`ctx.Err() != nil`), it forces `srv.Stop()` immediately and returns the
context error — it never enters the graceful path. Otherwise it races `GracefulStop` (guarded
by a `sync.Once`) against `ctx.Done()`; if the deadline wins first it falls back to a forced
`Stop`. The early-return matters: a graceful stop of a server with no open connections
completes instantly and could otherwise win the `select` against an already-done context,
making `Stop` return `nil` when it should have reported the context error.

## Kafka (`kafka`, `kafka/producer`, `kafka/consumer`)

`kafka` is the shared transport layer: a dialer/probe with TLS and SASL (PLAIN,
SCRAM-SHA-256/512). Producer and consumer both build on it and both implement
`Service`/`Shutdown`/`Prober`.

**Producer.** `Produce`/`ProduceBatch` with configurable compression, write timeout and max
attempts. `Probe` reports broker reachability.

**Consumer.** A consumer group with per-topic `Handler`s registered via `Handle`. `Start`
fetches messages in a loop and dispatches each to a bounded `workerpool.Pool`
(`WithWorkerPoolSize`), giving concurrency with backpressure — when the pool is saturated,
`Submit` blocks the fetch loop rather than unbounded buffering.

**At-least-once, deliberately.** The consumer commits an offset *only after* its handler
returns success; a failing handler leaves the offset uncommitted so the message is
redelivered. Two context choices enforce this: the handler runs under the task context
(optionally bounded by `WithHandlerTimeout`), but the **commit uses the `Start` context**,
not the per-task context, so a commit is never tied to a single task's lifetime. Messages on
topics with no registered handler are committed immediately (skipped, not redelivered).
Fetch errors back off before retrying; a canceled context or closed reader ends the loop.

## Managed resources (`dbservice`, `redisservice`)

Both wrap a connection pool (`pgxpool` / `go-redis`) and implement
`Service`/`Shutdown`/`Prober` with the same shape:

- `Start` establishes the pool with a **retry-backed** connect (`WithRetry`), so a service
  can boot before its datastore is reachable and converge once it is.
- A background goroutine polls pool statistics on `WithMetricsInterval` and exports them as
  Prometheus gauges.
- `Probe` pings the backend for readiness; `Pool()` / `Client()` expose the live handle for
  application code.
- `Stop` closes the pool. (Redis's close is synchronous and ignores the stop ctx; Postgres
  closes its pool under the stop ctx.)

## Health & readiness (`healthserver`)

A tiny HTTP server exposing `/healthz` (liveness — always `200` once serving) and `/readyz`
(readiness). `/readyz` fans out to every registered `service.Prober` concurrently and returns
`200` only when all probes pass, otherwise `503`. This is the consumer side of the `Prober`
interface: `dbservice`, `redisservice` and the Kafka components plug in as probers, and
`healthserver` gates traffic on their collective readiness. Probers are registered explicitly
via `WithProber` rather than auto-discovered.

## Concurrency primitives (`periodic`, `workerpool`)

**`periodic`** runs `fn(ctx)` on an interval and implements `Service`. Two modes:
*non-overlapping* (default) skips a tick if the previous run is still going;
*overlapping* (`WithOverlapping`) launches every tick regardless. `WithImmediateStart` fires
once at `t0` before the first interval elapses. Shutdown is ctx-driven.

**`workerpool`** is a bounded goroutine pool. `New(size, …)` sets worker count;
`WithQueueSize` sizes the task channel (backpressure buffer). `Submit(ctx, task)` enqueues,
blocking until there's room or `ctx` is done, and returns an error if the pool is stopped —
a send on a closed channel is recovered and surfaced as `pool stopped` (or the ctx error if
that fired). `Task` is `func(ctx)`; the task ctx is the pool's, not the submitter's.
`WithDrainOnStop` finishes queued tasks before `Stop` returns instead of dropping them.

## Cross-cutting concurrency contract

- **Context is the stop signal.** `Start(ctx)` blocks; ctx cancellation is the universal
  "wind down" instruction. Long-running loops select on `ctx.Done()`.
- **`sync.WaitGroup.Go`** is used for fan-out (entrypoint start/stop, gRPC graceful stop,
  metrics pollers) rather than manual `Add`/`Done`.
- **Cleanup outlives cancellation.** Any post-cancellation work uses
  `context.WithoutCancel` (+ a fresh timeout) so it isn't killed by the same cancellation
  that triggered it — see the entrypoint stop phase and the Kafka commit path.
- **Metrics isolation.** Every metric-owning component accepts a `prometheus.Registerer`;
  the default is global. Tests must pass an isolated `prometheus.NewRegistry()`.

## Load-bearing fragile points

Non-obvious spots where a plausible-looking change reintroduces a real bug:

- **Duplicate Prometheus registration.** Two components on the default registerer panic at
  construction. Always thread `WithPrometheusRegisterer` in tests and multi-instance
  processes.
- **gRPC `Stop` on an expired context.** The early `ctx.Err() != nil` branch must stay — a
  graceful stop with no connections finishes instantly and would otherwise race
  `ctx.Done()`, returning `nil` instead of the context error. (Regression fixed in v0.4.1.)
- **Entrypoint stop context.** It must be `WithoutCancel(ctx)` + timeout, never the live
  `ctx` (killed the instant shutdown starts) and never a bare `Background()` (drops the
  caller's values). PostStop hooks run on `Background()` by design.
- **Kafka commit context.** The offset commit uses the *Start* context, not the per-task
  context; binding it to the task would let a handler timeout also abort the commit and
  corrupt at-least-once semantics.
- **`workerpool.Submit` after stop.** Sending on the closed task channel is intentionally
  recovered; the recover-to-error path is how a stopped pool reports back instead of
  panicking the caller.