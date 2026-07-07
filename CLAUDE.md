# gokit-services

Переиспользуемый Go-тулкит для микросервисов. Go 1.26. Module: `github.com/DjaPy/gokit-services`.

## Структура

```
entrypoint/   — жизненный цикл приложения
httpserver/   — HTTP сервер с Prometheus и panic recovery
httpclient/   — HTTP клиент с middleware chain
grpc/         — gRPC клиент (в разработке)
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