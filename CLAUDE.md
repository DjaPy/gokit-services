# gokit-services

Переиспользуемый Go-тулкит для микросервисов. Go 1.26. Module: `github.com/DjaPy/gokit-services`.

## Структура

```
entrypoint/   — жизненный цикл приложения
httpserver/   — HTTP сервер с Prometheus и panic recovery
httpclient/   — HTTP клиент с middleware chain
grpcserver/   — gRPC сервер
grpcclient/   — gRPC клиент
service/      — интерфейсы Service и Shutdown
```

## Интерфейсы (service/)

```go
type Service interface {
    Start(ctx context.Context) error  // блокирует до остановки; ctx отменяется при shutdown
}

type Shutdown interface {
    Stop(ctx context.Context, cause error) error  // опциональный graceful stop
}
```

## entrypoint

Управляет жизненным циклом нескольких сервисов. Shutdown триггерится: SIGINT/SIGTERM, отмена ctx, ошибка сервиса, `ep.Shutdown()`.

Порядок: PreStart hooks → Start (параллельно) → PostStart hooks → ожидание → PreStop hooks → Stop (параллельно) → PostStop hooks.

```go
ep := entrypoint.New(
    entrypoint.WithServices(httpSrv, grpcSrv),
    entrypoint.WithShutdownTimeout(30 * time.Second),
    entrypoint.WithPreStart(func(ctx context.Context) error { ... }),
)
ep.Run(ctx)
```

## httpserver

HTTP сервер реализует `service.Service` и `service.Shutdown`. Автоматически собирает Prometheus метрики и восстанавливается после паники.

**Метрики:** `http_request_duration_seconds`, `http_response_size_bytes`, `http_requests_inflight`, `http_panic_recovery_total`. Labels: `http_service`, `http_handler`, `http_method`, `http_code`.

**Важно:** Всегда передавать `WithPrometheusRegisterer(prometheus.NewRegistry())` в тестах — иначе второй `NewServer` с дефолтным регистратором паникует на дублирующейся регистрации метрик. В продакшне использовать один `NewServer` на процесс или свой `Registerer`.

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /health", healthHandler)

srv := httpserver.NewServer(mux,
    httpserver.WithPort(8080),
    httpserver.WithAppName("my-svc"),
)
```

Паника в хендлере возвращает клиенту RFC 7807 Problem JSON (`application/problem+json`) со статусом 500 — только если ответ ещё не начал отправляться.

`responseWriter` пробрасывает `http.Flusher` и `http.Hijacker` — SSE и WebSocket работают корректно.

## httpclient

HTTP клиент с фиксированным base URL и middleware chain. Дженерик `Do[T]` декодирует JSON-ответ в T.

```go
c, err := httpclient.New("https://api.example.com",
    httpclient.WithTimeout(10 * time.Second),
    httpclient.WithMiddleware(authMiddleware, tracingMiddleware),
)

type User struct { Name string `json:"name"` }
user, err := httpclient.Do[User](ctx, c, http.MethodGet, "/users/42")
```

**`Do[T]`** возвращает ошибку при non-2xx. Для пустого тела (204 No Content) возвращает zero value без ошибки.

**Middleware:** первый в списке — внешний (выполняется первым). Применяется после всех Option'ов, поэтому `WithMiddleware` и `WithTransport` можно передавать в любом порядке.

**`WithBody`** выставляет `Content-Length` автоматически для типов, реализующих `Len() int` (`*bytes.Buffer`, `*strings.Reader`).

## Паттерны

- **Functional options** везде: `Option func(*T)` для конструктора, `RequestOption func(*http.Request)` для запросов.
- **slog** для логирования — `slog.Default()` по умолчанию, переопределяется через `WithLogger`.
- **Prometheus** — изолированные регистраторы для тестов, дефолтный для продакшна.
- Тесты используют `httptest.NewServer` / `httptest.NewRecorder`, реальные HTTP соединения (никаких моков транспорта).

## Команды

```bash
go test ./...          # все тесты
go test -race ./...    # с детектором гонок
```

## Версионирование и релизы

Модуль — чистая библиотека (нет `cmd/`), поэтому версия задаётся исключительно git-тегами;
никакого build-time инжектирования версии (`-ldflags`) не требуется. Потребители фиксируют
версию через `go get github.com/DjaPy/gokit-services@vX.Y.Z`.

**Политика (SemVer, `vMAJOR.MINOR.PATCH`):**
- Пока проект в `v0.x.y` (первого релиза ещё не было): `MINOR` — новые фичи и breaking changes,
  `PATCH` — только багфиксы без изменения API. Обратная совместимость между `0.x` релизами
  не гарантируется — это ожидаемо для pre-1.0 по духу SemVer.
- После первого `v1.0.0`: `PATCH` — багфиксы, `MINOR` — обратно совместимые фичи,
  `MAJOR` — breaking changes.
- **Переход на `v2.0.0`+**: путь модуля должен получить суффикс `/v2` (`module
  github.com/DjaPy/gokit-services/v2` в `go.mod`), это требование Go modules, не опция.

**Процесс релиза:**
1. Обновить `CHANGELOG.md`: перенести содержимое `[Unreleased]` под новый заголовок
   `[X.Y.Z] - YYYY-MM-DD`, оставить `[Unreleased]` пустым для следующих изменений
2. Закоммитить: `git commit -m "chore: release vX.Y.Z"`
3. Запушить в `main`, затем создать и запушить тег:
   ```bash
   git tag vX.Y.Z
   git push origin main
   git push origin vX.Y.Z
   ```
4. `.github/workflows/release.yml` реагирует на push тега `v*`: прогоняет CI и создаёт
   GitHub Release с auto-generated notes — вручную ничего создавать не нужно

**Breaking changes до `v1.0.0`**: допустимы в `MINOR`-релизах, но должны быть явно отмечены
в `CHANGELOG.md` под заголовком `### Changed` с пометкой **BREAKING**.