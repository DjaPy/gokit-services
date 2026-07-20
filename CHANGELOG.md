# Changelog

Все заметные изменения этого проекта документируются в этом файле.

Формат основан на [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
проект придерживается [Semantic Versioning](https://semver.org/) (до v1.0.0 —
см. политику версионирования в `CLAUDE.md`).

## [Unreleased]

### Changed
- **BREAKING**: реорганизована раскладка пакетов — транспорты сгруппированы по протоколу
  (по образцу `kafka/`), а контрактно-оркестрационный слой вынесен в `core/`. Поведение,
  публичные API, сигнатуры и метрики не изменились — обновить нужно только пути импорта:

  | Старый путь | Новый путь |
  |-------------|------------|
  | `github.com/DjaPy/gokit-services/service` | `github.com/DjaPy/gokit-services/core/service` |
  | `github.com/DjaPy/gokit-services/entrypoint` | `github.com/DjaPy/gokit-services/core/entrypoint` |
  | `github.com/DjaPy/gokit-services/httpserver` | `github.com/DjaPy/gokit-services/http/server` |
  | `github.com/DjaPy/gokit-services/httpclient` | `github.com/DjaPy/gokit-services/http/client` |
  | `github.com/DjaPy/gokit-services/grpcserver` | `github.com/DjaPy/gokit-services/grpc/server` |
  | `github.com/DjaPy/gokit-services/grpcclient` | `github.com/DjaPy/gokit-services/grpc/client` |

  Имена пакетов `service` и `entrypoint` сохранены (меняется только путь). Транспортные
  подпакеты теперь называются `server`/`client` — импортировать их рекомендуется с алиасами
  (`httpsrv`, `httpcli`, `grpcsrv`, `grpccli`), чтобы избежать коллизий и конфликта имени с
  stdlib `net/http`. `kafka/` и инфраструктурные сервисы (`healthserver`, `periodic`,
  `workerpool`, `dbservice`, `redisservice`) не затронуты.

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
- `entrypoint` — управление жизненным циклом сервисов: SIGINT/SIGTERM, lifecycle-хуки, graceful shutdown
- `httpserver` — HTTP-сервер с Prometheus-метриками и panic recovery
- `httpclient` — HTTP-клиент с middleware chain и generic `Do[T]`
- `grpcserver` — управляемый gRPC-сервер (`service.Service` + `service.Shutdown`)
- `grpcclient` — управляемый gRPC-клиент (`service.Service` + `service.Shutdown`)
- `healthserver` — `/healthz` и `/readyz` эндпоинты с параллельным опросом `service.Prober`
- `periodic` — периодический фоновый сервис (overlapping/non-overlapping режимы)
- `workerpool` — ограниченный пул горутин с backpressure через `Submit(ctx, task)`
- `service` — базовые интерфейсы `Service`, `Shutdown`, `Prober`

[Unreleased]: https://github.com/DjaPy/gokit-services/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/DjaPy/gokit-services/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/DjaPy/gokit-services/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/DjaPy/gokit-services/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/DjaPy/gokit-services/releases/tag/v0.1.0
