# Changelog

Все заметные изменения этого проекта документируются в этом файле.

Формат основан на [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
проект придерживается [Semantic Versioning](https://semver.org/) (до v1.0.0 —
см. политику версионирования в `CLAUDE.md`).

## [Unreleased]

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

[Unreleased]: https://github.com/DjaPy/gokit-services/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/DjaPy/gokit-services/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/DjaPy/gokit-services/releases/tag/v0.1.0
