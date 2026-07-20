# gokit-services

Переиспользуемый Go-тулкит для микросервисов. Go 1.26. Module: `github.com/DjaPy/gokit-services`.

## Структура

```
core/
  entrypoint/   — жизненный цикл приложения
  service/      — интерфейсы Service, Shutdown, Prober
http/
  server/       — HTTP сервер с Prometheus и panic recovery (package server)
  client/       — HTTP клиент с middleware chain (package client)
grpc/
  server/       — gRPC сервер (package server)
  client/       — gRPC клиент (package client)
kafka/          — dialer/TLS/SASL + подпакеты producer/ и consumer/
```

Транспорты сгруппированы по протоколу, а контрактно-оркестрационный слой
вынесен в `core/`. Подпакеты `server`/`client` рекомендуется импортировать с алиасами
(`httpsrv`, `httpcli`, `grpcsrv`, `grpccli`) — это снимает коллизии генеричных имён и
конфликт с stdlib `net/http`. Инфраструктурные сервисы (`healthserver`, `periodic`,
`workerpool`, `dbservice`, `redisservice`) остаются пакетами верхнего уровня.

## Интерфейсы (core/service)

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
import "github.com/DjaPy/gokit-services/core/entrypoint"

ep := entrypoint.New(
    entrypoint.WithServices(httpSrv, grpcSrv),
    entrypoint.WithShutdownTimeout(30 * time.Second),
    entrypoint.WithPreStart(func(ctx context.Context) error { ... }),
)
ep.Run(ctx)
```

## http/server

HTTP сервер реализует `service.Service` и `service.Shutdown`. Автоматически собирает Prometheus метрики и восстанавливается после паники.

**Метрики:** `http_request_duration_seconds`, `http_response_size_bytes`, `http_requests_inflight`, `http_panic_recovery_total`. Labels: `http_service`, `http_handler`, `http_method`, `http_code`.

**Важно:** Всегда передавать `WithPrometheusRegisterer(prometheus.NewRegistry())` в тестах — иначе второй `NewServer` с дефолтным регистратором паникует на дублирующейся регистрации метрик. В продакшне использовать один `NewServer` на процесс или свой `Registerer`.

```go
import httpsrv "github.com/DjaPy/gokit-services/http/server"

mux := http.NewServeMux()
mux.HandleFunc("GET /health", healthHandler)

srv := httpsrv.NewServer(mux,
    httpsrv.WithPort(8080),
    httpsrv.WithAppName("my-svc"),
)
```

Паника в хендлере возвращает клиенту RFC 7807 Problem JSON (`application/problem+json`) со статусом 500 — только если ответ ещё не начал отправляться.

`responseWriter` пробрасывает `http.Flusher` и `http.Hijacker` — SSE и WebSocket работают корректно.

## http/client

HTTP клиент с фиксированным base URL и middleware chain. Дженерик `Do[T]` декодирует JSON-ответ в T.

```go
import httpcli "github.com/DjaPy/gokit-services/http/client"

c, err := httpcli.New("https://api.example.com",
    httpcli.WithTimeout(10 * time.Second),
    httpcli.WithMiddleware(authMiddleware, tracingMiddleware),
)

type User struct { Name string `json:"name"` }
user, err := httpcli.Do[User](ctx, c, http.MethodGet, "/users/42")
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

**Процесс релиза** (через `justfile`, git-теги — единственный источник версии, `.version`-файла нет):
1. Обновить `CHANGELOG.md`: перенести содержимое `[Unreleased]` под новый заголовок
   `[X.Y.Z] - YYYY-MM-DD`, оставить `[Unreleased]` пустым для следующих изменений; закоммитить
   (`git commit -m "chore: release vX.Y.Z"`) и запушить в `main` — этот шаг justfile не автоматизирует
2. Запустить один из рецептов (гоняет `all-check`: build+lint+test-coverage, затем тегает и пушит):
   ```bash
   just release-patch   # X.Y.Z -> X.Y.(Z+1) — багфиксы
   just release-minor   # X.Y.Z -> X.(Y+1).0 — новые фичи / breaking changes до v1.0.0
   just release-major   # X.Y.Z -> (X+1).0.0 — breaking changes после v1.0.0
   ```
   Текущая версия читается из `git describe --tags --abbrev=0` — новый тег всегда считается
   от последнего существующего, вручную ничего не вводится. Для тега без пуша — `just bump-patch`
   и т.п. (тег создаётся локально, пушить `git push --tags` отдельно).
3. `.github/workflows/release.yml` реагирует на push тега `v*`: прогоняет CI (переиспользует
   `ci.yml` как reusable workflow — тот объявляет `workflow_call`) и создаёт GitHub Release
   с auto-generated notes — вручную ничего создавать не нужно

**Breaking changes до `v1.0.0`**: допустимы в `MINOR`-релизах, но должны быть явно отмечены
в `CHANGELOG.md` под заголовком `### Changed` с пометкой **BREAKING**.