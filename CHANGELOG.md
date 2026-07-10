# Changelog

Все заметные изменения этого проекта документируются в этом файле.

Формат основан на [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
проект придерживается [Semantic Versioning](https://semver.org/) (до v1.0.0 —
см. политику версионирования в `CLAUDE.md`).

## [Unreleased]

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

[Unreleased]: https://github.com/DjaPy/gokit-services/commits/main
