# Managed PostgreSQL, Kafka и Redis сервисы

## Requirements

Расширить `gokit-services` четырьмя новыми managed-обёртками инфраструктурных зависимостей — `dbservice` (PostgreSQL через `pgxpool`), `kafkaproducer`/`kafkaconsumer` (раздельные Kafka producer и consumer через `segmentio/kafka-go`, по аналогии с разделением `grpcserver`/`grpcclient`), `redisservice` (Redis через `go-redis/v9`) — каждая следует установленному контракту `service.Service`/`service.Shutdown`/`service.Prober`, ретраит подключение с backoff при старте вместо fail-fast, экспонирует Prometheus-метрики через injectable `Registerer`, и покрыта реальными интеграционными тестами против настоящих service-контейнеров в CI (без моков транспорта). Distributed locks, Redis Streams и БД-backed task queue — явно вне объёма. Каждый пакет реализуется отдельным проходом (`dbservice` → `kafkaconsumer` → `kafkaproducer` → `redisservice`) и в рамках того же прохода интегрируется в `example/orders-service` — это решение отменяет более раннее «example не трогаем» из фазы анализа. Для `dbservice` интеграция означает: `Store` в `orders-service` становится интерфейсом с двумя реализациями — `InMemoryStore` (текущее поведение) и `PostgresStore` (через `dbservice.Pool()`), выбор — через флаг/переменную окружения в `main.go`.

## Entities

```mermaid
classDiagram
direction TB

class Service {
    <<interface>>
    +Start(ctx Context) error
}

class Shutdown {
    <<interface>>
    +Stop(ctx Context, cause error) error
}

class Prober {
    <<interface>>
    +Probe(ctx Context) error
}

class DBService {
    +dsn string
    +maxConns int32
    +minConns int32
    +retry RetryConfig
    +logger Logger
    +registerer Registerer
    +pool pgxpool_Pool
    +shutdownOnce sync_Once
    +New(opts ...Option) DBService
    +Start(ctx Context) error
    +Stop(ctx Context, cause error) error
    +Probe(ctx Context) error
    +Pool() pgxpool_Pool
}

class Store {
    <<interface>>
    +Create(ctx Context, customerID string, items []Item) (Order, error)
    +Get(ctx Context, id string) (Order, error)
    +List(ctx Context) ([]Order, error)
    +ConfirmPending(ctx Context, id string) error
    +CancelStalePending(ctx Context, cutoff Time) ([]string, error)
}

class InMemoryStore {
    +orders map~string,Order~
    +nextID atomic_Uint64
    +New() InMemoryStore
}

class PostgresStore {
    +pool poolProvider
    +New(pool poolProvider) PostgresStore
    +EnsureSchema(ctx Context) error
}

class poolProvider {
    <<interface>>
    +Pool() pgxpool_Pool
}

class KafkaProducer {
    +brokers []string
    +tls TLSConfig
    +retry RetryConfig
    +writer kafka_Writer
    +New(brokers []string, opts ...Option) KafkaProducer
    +Start(ctx Context) error
    +Stop(ctx Context, cause error) error
    +Probe(ctx Context) error
    +Produce(ctx Context, topic string, key []byte, value []byte, headers map) error
}

class KafkaConsumer {
    +brokers []string
    +consumerGroup string
    +topics map~string,Handler~
    +tls TLSConfig
    +retry RetryConfig
    +pool workerpool_Pool
    +reader kafka_Reader
    +New(brokers []string, opts ...Option) KafkaConsumer
    +Handle(topic string, h Handler)
    +Start(ctx Context) error
    +Stop(ctx Context, cause error) error
    +Probe(ctx Context) error
}

class RedisService {
    +dsn string
    +poolSize int
    +retry RetryConfig
    +logger Logger
    +registerer Registerer
    +client redis_Client
    +New(opts ...Option) RedisService
    +Start(ctx Context) error
    +Stop(ctx Context, cause error) error
    +Probe(ctx Context) error
    +Client() redis_Client
}

class RetryConfig {
    +maxAttempts int
    +backoffMin Duration
    +backoffMax Duration
}

class KafkaNet {
    <<internal helper>>
    +NewDialer(tls TLSConfig, user string, pass string) kafka_Dialer
    +Probe(ctx Context, brokers []string, dialer kafka_Dialer) error
}

class Handler {
    <<func>>
    Handler(ctx Context, msg Message) error
}

class Message {
    +Topic string
    +Partition int
    +Offset int64
    +Key []byte
    +Value []byte
    +Headers map~string,[]byte~
    +Time Time
}

class WorkerPool {
    +Submit(ctx Context, task Task) error
}

Service <|.. DBService : implements
Shutdown <|.. DBService : implements
Prober <|.. DBService : implements
Service <|.. KafkaProducer : implements
Shutdown <|.. KafkaProducer : implements
Prober <|.. KafkaProducer : implements
Service <|.. KafkaConsumer : implements
Shutdown <|.. KafkaConsumer : implements
Prober <|.. KafkaConsumer : implements
Service <|.. RedisService : implements
Shutdown <|.. RedisService : implements
Prober <|.. RedisService : implements

DBService --> RetryConfig : uses
KafkaProducer --> RetryConfig : uses
KafkaConsumer --> RetryConfig : uses
RedisService --> RetryConfig : uses
KafkaProducer --> KafkaNet : uses for dialer/probe
KafkaConsumer --> KafkaNet : uses for dialer/probe
KafkaConsumer --> WorkerPool : dispatches via
KafkaConsumer --> Handler : invokes per topic
Handler --> Message : receives

Store <|.. InMemoryStore : implements
Store <|.. PostgresStore : implements
PostgresStore --> poolProvider : reads pool via
DBService <|.. poolProvider : satisfies (Pool() accessor)
```

## Approach

1. **Общий шаблон четырёх пакетов**:
   - Каждый пакет (`dbservice`, `kafkaproducer`, `kafkaconsumer`, `redisservice`) — top-level, functional options, `slog.Default()` + `WithLogger`, `WithAppName` для label'а метрик (мирроринг `httpserver`), `WithPrometheusRegisterer`.
   - Единственное осознанное отклонение от `httpserver`/`grpcserver`: `Start(ctx)` не fail-fast, а ретраит подключение экспоненциальным backoff, ограниченным `ctx.Done()` — паттерн взят из `/Users/djapy/_past/infravision/golibs/kafka_connector/retry.go` (`WithRetries`), реализуется как общий generic-хелпер `internal/retry.Do[T]`, переиспользуемый всеми четырьмя пакетами — не дублировать логику backoff четырежды.
   - Каждый сервис отдаёт сконструированный клиент через явный метод-аксессор (`Pool()`, `Client()`), доступный только после успешного `Start()` — до этого возвращает `nil` (тот же паттерн, что `grpcclient.Client.Conn()`).
   - `kafkaproducer` и `kafkaconsumer` — два отдельных пакета, а не один `kafkaservice` с двумя типами: та же мотивация, что и разделение `grpcserver`/`grpcclient` — независимые `Option`-типы без коллизии имён (`WithTLS`, `WithSASL`, `WithLogger` нужны обоим), независимые `service.Service`-регистрации в `entrypoint` (producer-only процесс не обязан конфигурировать consumer group и наоборот).

2. **Техническая реализация**:
   - `dbservice`: `pgxpool.ParseConfig(dsn)` → применить `MaxConns`/`MinConns` → `pgxpool.NewWithConfig` в retry-цикле → `pool.Ping(ctx)` как часть успешной попытки подключения. Метрики пула собираются через периодический poll `pool.Stat()` (не event-хуки, каких у `pgxpool` нет — см. риск в анализе), интервал настраивается через `WithMetricsInterval` (по умолчанию 15s).
   - `internal/kafkanet` — общий для `kafkaproducer`/`kafkaconsumer` хелпер: строит `*kafka.Dialer` с TLS/SASL из общей конфигурации, и даёт `Probe(ctx, brokers, dialer)` — дёргает `dialer.DialContext` на первый брокер и сразу закрывает соединение. Избегает дублирования TLS/SASL-настройки dialer'а и dial-проверки связности между двумя пакетами.
   - `kafkaconsumer`: один `kafka.Reader` с `GroupTopics` (список ключей карты обработчиков) на весь процесс — не по reader на топик; входящие сообщения диспетчеризуются в зарегистрированный `Handler` по `msg.Topic` через `workerpool.Pool.Submit`; коммит оффсета — только после успешного выполнения `Handler` (at-least-once, без коммита при ошибке — сообщение будет передоставлено).
   - `kafkaproducer`: один `kafka.Writer` с пустым `Topic`-полем (топик указывается на каждый вызов `Produce`, а не фиксирован на уровне writer'а), чтобы один продюсер мог отправлять в разные топики.
   - `redisservice`: `redis.ParseURL(dsn)` → применить `PoolSize` → `redis.NewClient` в retry-цикле → `client.Ping(ctx).Err()` как проверка успешности попытки. Метрики пула — периодический poll `client.PoolStats()`.
   - Retry-политика для всех четырёх: экспоненциальный backoff (`backoff *= 1.5`, капается `backoffMax`), уважает `ctx.Done()` (прерывается немедленно, не проглатывает отмену), ограничен `maxAttempts`; при исчерпании попыток `Start()` возвращает финальную ошибку.

3. **Бизнес-логика и обработка ошибок**:
   - `service.Prober.Probe(ctx)` во всех четырёх — реальная проверка связности (`pool.Ping`, `kafkanet.Probe` на первый брокер, `client.Ping`), не факт «сконструирован без ошибки»; если `Start()` ещё ретраит или клиент/пул ещё `nil` — `Probe` явно возвращает ошибку «not connected», а не паникует на nil pointer.
   - `Stop(ctx, cause)` во всех четырёх идемпотентен через `sync.Once`/`atomic.Bool`-флаг закрытия (мирроринг `grpcserver.Server.shutdownOnce`).
   - Ошибки при обработке Kafka-сообщения в `kafkaconsumer` логируются через `slog.Error` и инкрементируют счётчик ошибок; сообщение не коммитится (гарантия at-least-once, не at-most-once).

## Structure

### Inheritance Relationships
1. `service.Service` — контракт: `Start(ctx context.Context) error`
2. `service.Shutdown` — опциональный graceful stop: `Stop(ctx context.Context, cause error) error`
3. `service.Prober` — опциональный readiness-опрос: `Probe(ctx context.Context) error`
4. `*dbservice.Service` реализует `service.Service`, `service.Shutdown`, `service.Prober`
5. `*kafkaproducer.Producer` реализует `service.Service`, `service.Shutdown`, `service.Prober`
6. `*kafkaconsumer.Consumer` реализует `service.Service`, `service.Shutdown`, `service.Prober`
7. `*redisservice.Service` реализует `service.Service`, `service.Shutdown`, `service.Prober`
8. `orders.Store` (`example/orders-service`) — контракт: `Create`/`Get`/`List`/`ConfirmPending`/`CancelStalePending`; `*orders.InMemoryStore` и `*orders.PostgresStore` — обе реализации
9. `*dbservice.Service` дополнительно удовлетворяет `orders.poolProvider` (`Pool() *pgxpool.Pool`) — приватный интерфейс `example/orders-service`, позволяющий `PostgresStore` не зависеть от конкретного типа `*dbservice.Service` напрямую

### Dependencies
1. `dbservice` → `github.com/jackc/pgx/v5/pgxpool`, `log/slog`, `sync`, `internal/prom`, `internal/retry`
2. `kafkaproducer` → `github.com/segmentio/kafka-go`, `log/slog`, `sync`, `internal/prom`, `internal/retry`, `internal/kafkanet`
3. `kafkaconsumer` → `github.com/segmentio/kafka-go`, `workerpool.Pool`, `log/slog`, `sync`, `internal/prom`, `internal/retry`, `internal/kafkanet`
4. `redisservice` → `github.com/redis/go-redis/v9`, `log/slog`, `sync`, `internal/prom`, `internal/retry`
5. `internal/retry` (новый) → только stdlib (`context`, `time`); используется всеми четырьмя новыми пакетами, не публичный API
6. `internal/kafkanet` (новый) → `github.com/segmentio/kafka-go`, `github.com/segmentio/kafka-go/sasl/plain`, `crypto/tls`; используется `kafkaproducer` и `kafkaconsumer`, не публичный API
7. Ни один из четырёх новых пакетов не зависит от `httpserver`/`grpcserver`/`healthserver` напрямую — подключение к readiness-агрегации происходит через `healthserver.WithProber(svc)` на стороне вызывающего кода (в `main`), не изнутри новых пакетов
8. `kafkaproducer` и `kafkaconsumer` не зависят друг от друга ни в каком направлении (production-код); тесты каждого пакета самодостаточны (используют сырой `kafka.Reader`/`kafka.Writer` для проверки встречной стороны, не импортируют друг друга)
9. `example/orders-service` → `dbservice` (только для postgres-бэкенда, через `orders.PostgresStore`/`newStore` в `main.go`), `github.com/jackc/pgx/v5`, `github.com/jackc/pgx/v5/pgxpool` (напрямую, для типов `pgx.Row`/`*pgxpool.Pool`, не только через `dbservice`); `orders.PostgresStore` не импортирует `dbservice` напрямую — зависит только от `poolProvider`, приватного интерфейса, которому `*dbservice.Service` удовлетворяет структурно

### Package Layout
```
dbservice/         — Service (service.go, service_test.go)
kafkaproducer/      — Producer (producer.go, producer_test.go)
kafkaconsumer/      — Consumer, Handler, Message (consumer.go, consumer_test.go)
redisservice/       — Service (service.go, service_test.go)
internal/retry/     — общий retry-with-backoff хелпер (retry.go, retry_test.go)
internal/kafkanet/  — общий dialer/probe хелпер для kafkaproducer/kafkaconsumer (kafkanet.go, kafkanet_test.go)
```
`internal/prom/register.go` не меняется — переиспользуется как есть.

`example/orders-service/` (существующий пакет, интеграция первого прохода — `dbservice`):
```
store_memory.go    — InMemoryStore (вынесен из прежнего единого файла со Store)
store_postgres.go  — PostgresStore, poolProvider
cmd/orders-service/main.go — newStore() выбирает бэкенд по ORDERS_STORE,
                              ensureSchemaOnceConnected() в PostStart
```

## Operations

### 1. Создать `internal/retry/retry.go`

1. Ответственность: общий generic-хелпер экспоненциального backoff с уважением к `ctx.Done()`, переиспользуемый `dbservice`/`kafkaproducer`/`kafkaconsumer`/`redisservice` вместо четырёхкратного дублирования логики из `kafka_connector/retry.go`.
2. Структура:
   ```go
   type Config struct {
       MaxAttempts int
       BackoffMin  time.Duration
       BackoffMax  time.Duration
   }
   ```
3. Функция:
   ```go
   func Do[T any](ctx context.Context, cfg Config, connect func(context.Context) (T, error)) (T, error)
   ```
   - `cfg.MaxAttempts <= 0` трактуется как 1 (одна попытка, без ретраев)
   - backoff стартует с `cfg.BackoffMin`, после каждой неудачной попытки умножается на 1.5, капается `cfg.BackoffMax`
   - между попытками — `select { case <-ctx.Done(): return zero, fmt.Errorf("retry: context canceled after %d attempts: %w", attempt, ctx.Err()); case <-time.After(backoff): }`
   - при исчерпании `MaxAttempts` — возвращает `fmt.Errorf("retry: failed after %d attempts: %w", cfg.MaxAttempts, lastErr)`
   - при успехе на любой попытке — возвращает результат немедленно; логирование не встроено в хелпер (хелпер stdlib-only, логирование — на вызывающей стороне, между попытками)
4. Default: `DefaultConfig() Config` → `Config{MaxAttempts: 10, BackoffMin: 500 * time.Millisecond, BackoffMax: 30 * time.Second}`

---

### 2. Создать `internal/retry/retry_test.go`

Пакет: `package retry_test`

1. **TestDo_SucceedsFirstAttempt**: `connect` возвращает успех сразу — `Do` возвращает результат без ожидания
2. **TestDo_SucceedsAfterRetries**: `connect` возвращает ошибку N раз, затем успех — `Do` возвращает успешный результат, счётчик вызовов `connect` = N+1
3. **TestDo_ExhaustsMaxAttempts**: `connect` всегда возвращает ошибку — `Do` возвращает обёрнутую ошибку после ровно `MaxAttempts` попыток
4. **TestDo_RespectsContextCancellation**: `connect` всегда возвращает ошибку; ctx отменяется до исчерпания `MaxAttempts` — `Do` возвращает раньше с ошибкой, содержащей `context.Canceled` (`errors.Is`)
5. **TestDo_BackoffCappedAtMax**: короткий `BackoffMax`, много попыток — суммарное время выполнения теста ограничено (не растёт неограниченно), проверяется через `require.Eventually` с разумным таймаутом

---

### 3. Реализовать `dbservice/service.go`

1. Ответственность: управляемый пул подключений PostgreSQL — `Start` подключается с retry-backoff, `Probe` проверяет связность через `Ping`, `Stop` закрывает пул, метрики пула собираются периодическим poll `pgxpool.Stat()`.

2. Структура:
   ```go
   type Service struct {
       dsn             string
       maxConns        int32
       minConns        int32
       appName         string
       retry           retry.Config
       metricsInterval time.Duration
       logger          *slog.Logger
       registerer      prometheus.Registerer

       mu           sync.RWMutex
       pool         *pgxpool.Pool
       shutdownOnce sync.Once

       poolConns       *prometheus.GaugeVec   // db_pool_conns{db_service,state}
       poolMaxConns    *prometheus.GaugeVec   // db_pool_max_conns{db_service}
       poolAcquireCnt  *prometheus.GaugeVec   // db_pool_acquire_count{db_service}
       probeDuration   *prometheus.HistogramVec // db_probe_duration_seconds{db_service}
   }
   ```
   `shutdownOnce` делает `Stop` идемпотентным без повторного вызова `pool.Close()` на закрытом пуле (мирроринг `grpcserver.shutdownOnce`).

3. Конструктор:
   ```go
   func New(dsn string, opts ...Option) *Service
   ```
   Дефолты: `maxConns=10`, `minConns=2`, `retry=retry.DefaultConfig()`, `metricsInterval=15*time.Second`, `logger=slog.Default()`, `registerer=prometheus.DefaultRegisterer`.

4. `Start(ctx context.Context) error`:
   - `cfg, err := pgxpool.ParseConfig(s.dsn)`; при ошибке — вернуть немедленно (`fmt.Errorf("dbservice: parse dsn: %w", err)`), retry здесь не применяется (ошибка конфигурации не транзиентна)
   - `cfg.MaxConns = s.maxConns; cfg.MinConns = s.minConns`
   - подключение делегируется приватной `connectOnce(ctx, cfg) (*pgxpool.Pool, error)`, а не инлайн-замыканию: `p, err := pgxpool.NewWithConfig(ctx, cfg)`; при ошибке — обёрнуть `fmt.Errorf("dbservice: create pool: %w", err)`; `p.Ping(ctx)`; при ошибке — `p.Close()` и обернуть `fmt.Errorf("dbservice: ping: %w", err)` (обёртка нужна, чтобы отличать в логах отказ создания пула от отказа пинга)
   - `pool, err := retry.Do(ctx, s.retry, func(attemptCtx context.Context) (*pgxpool.Pool, error) { return connectOnce(attemptCtx, cfg) })`
   - при ошибке после исчерпания retry — вернуть `fmt.Errorf("dbservice: connect: %w", err)`
   - вызвать приватный `s.initMetrics()` (регистрация всех коллекторов через `prom.RegisterOrReuse(s.registerer, ...)`), затем под `s.mu.Lock()` сохранить `s.pool = pool`
   - запустить горутину-поллер метрик: `ticker := time.NewTicker(s.metricsInterval); defer ticker.Stop(); for { select { case <-ctx.Done(): return; case <-ticker.C: stat := pool.Stat(); s.poolConns.WithLabelValues(s.appName, "total").Set(float64(stat.TotalConns())); ...idle/acquired/constructing аналогично; s.poolMaxConns.WithLabelValues(s.appName).Set(float64(stat.MaxConns())); s.poolAcquireCnt.WithLabelValues(s.appName).Set(float64(stat.AcquireCount())) } }`
   - блокировать на `<-ctx.Done()`, затем вернуть `nil`

5. `Stop(ctx context.Context, _ error) error`:
   - `s.mu.RLock()` прочитать `pool`; если `nil` — вернуть `nil`
   - `pool.Close()` не имеет ctx-параметра в API `pgxpool` и может блокировать до фактического дренажа пула, поэтому вызывается в отдельной горутине через `s.shutdownOnce.Do(pool.Close)`, с `close(done)` после завершения
   - `select { case <-done: return nil; case <-ctx.Done(): return fmt.Errorf("dbservice: stop: %w", ctx.Err()) }` — `Stop` возвращает управление по истечении `ctx`, даже если `Close()` продолжает выполняться в фоне (небезопасно относительно двойного использования `s.pool` после — но следующий `Probe`/`Pool()` в любом случае увидят пул, который вот-вот закроется)

6. `Probe(ctx context.Context) error`:
   - под `s.mu.RLock()` прочитать `pool`; если `nil` — вернуть `errors.New("dbservice: not connected")`
   - `start := time.Now(); err := pool.Ping(ctx); s.probeDuration.WithLabelValues(s.appName).Observe(time.Since(start).Seconds())`
   - если `err != nil` — вернуть `fmt.Errorf("dbservice: probe: %w", err)`

7. `Pool() *pgxpool.Pool`:
   - под `s.mu.RLock()` вернуть `pool` (может быть `nil` до успешного `Start`)

8. Options:
   - `WithMaxConns(n int32)`, `WithMinConns(n int32)`
   - `WithRetry(cfg retry.Config)`
   - `WithMetricsInterval(d time.Duration)`
   - `WithAppName(name string)`
   - `WithLogger(l *slog.Logger)`
   - `WithPrometheusRegisterer(r prometheus.Registerer)`

---

### 4. Создать `dbservice/service_test.go`

Пакет: `package dbservice_test`; во всех тестах: `WithPrometheusRegisterer(prometheus.NewRegistry())`; реальный Postgres (см. Operations #15 про CI-контейнер) — тесты, требующие живой БД, читают DSN из переменной окружения `TEST_POSTGRES_DSN` и **скипаются** (`t.Skip`), если переменная не установлена (позволяет `go test ./...` проходить локально без Docker, но полноценно гонять в CI).

1. **TestService_StartConnectsAndProbeSucceeds**: `New(dsn)`, `Start` в горутине с ctx-timeout, дождаться (`require.Eventually`) что `Pool()` не `nil`, вызвать `Probe(ctx)` — `require.NoError`
2. **TestService_ProbeBeforeStart_ReturnsError**: `New(dsn)` без `Start` — `Probe(ctx)` возвращает ошибку «not connected»
3. **TestService_StartRetriesOnInitialFailure**: `New` с заведомо недоступным DSN (`postgresql://localhost:1`) и `WithRetry(retry.Config{MaxAttempts: 3, BackoffMin: 10*time.Millisecond, BackoffMax: 50*time.Millisecond})` — `Start` возвращает ошибку после ровно 3 попыток (не паникует, не висит бесконечно)
4. **TestService_StopClosesPool**: `Start` → дождаться подключения → `Stop(ctx, nil)` → `Probe` после `Stop` возвращает ошибку (пул закрыт)
5. **TestService_StopIdempotent**: несколько вызовов `Stop` подряд не паникуют и не блокируют

---

### 5. Разделить `example/orders-service` Store на `store_memory.go`/`store_postgres.go`

1. Ответственность: выполняет решение из Requirements — `Store` в `orders-service` становится интерфейсом с двумя реализациями, чтобы пример мог работать как без внешних зависимостей (`go run` из коробки), так и против настоящего Postgres через `dbservice`.
2. `store_memory.go` — `InMemoryStore` вынесен из прежнего единого файла без изменения поведения: тот же `sync.RWMutex` + `atomic.Uint64`-счётчик id, та же семантика всех пяти методов `Store`.
3. `store_postgres.go` — `PostgresStore`:
   ```go
   type PostgresStore struct {
       pool poolProvider
   }

   type poolProvider interface {
       Pool() *pgxpool.Pool
   }

   func NewPostgresStore(pool poolProvider) *PostgresStore
   func (s *PostgresStore) EnsureSchema(ctx context.Context) error
   ```
   - `poolProvider` — приватный интерфейс с единственным методом `Pool() *pgxpool.Pool`; `*dbservice.Service` удовлетворяет ему структурно, `PostgresStore` не импортирует `dbservice` напрямую
   - `PostgresStore` не захватывает `*pgxpool.Pool` один раз в конструкторе — каждый вызов метода читает актуальный пул через `s.pool.Pool()` (приватный хелпер `s.db()`), возвращая `errStoreNotConnected`, если пул ещё `nil` (dbservice ещё ретраит `Start`), вместо разыменования nil-пула
   - Заказ хранится одной строкой: `id, customer_id, items (JSONB), status, created_at, updated_at` — line items сериализуются в JSONB одной колонкой, без отдельной таблицы/join'ов (пример остаётся однотабличным)
   - `EnsureSchema(ctx) error` — не часть контракта `Store`, отдельный метод; создаёт `CREATE SEQUENCE IF NOT EXISTS orders_id_seq` и `CREATE TABLE IF NOT EXISTS orders (...)`, идемпотентен, вызывается один раз при старте после подключения пула
   - `Create` генерирует id как `'ord_' || nextval('orders_id_seq')` на стороне Postgres — не в Go-коде, в отличие от `InMemoryStore.nextID`
   - `ConfirmPending` — `UPDATE ... WHERE id = $1 AND status = 'PENDING'`, тот же race-free паттерн, что и в `InMemoryStore`: конкурентный `CancelStalePending`, успевший перевести заказ из PENDING первым, делает этот `UPDATE` no-op (0 affected rows); при 0 affected rows дополнительно проверяется `EXISTS`, чтобы отличить «уже не PENDING» (no-op, `nil`) от «заказа нет» (`ErrOrderNotFound`)
   - `CancelStalePending` — `UPDATE ... WHERE status = 'PENDING' AND created_at < $cutoff RETURNING id`, симметрично `InMemoryStore`
   - Ошибки оборачиваются с префиксом `postgres store: ...`; `Get` транслирует `pgx.ErrNoRows` в `ErrOrderNotFound` (через `errors.Is`, не сравнение строк)

---

### 6. Wiring бэкенда в `cmd/orders-service/main.go`

1. Ответственность: выбор `Store`-реализации при старте процесса и подключение `dbservice` к жизненному циклу `entrypoint`/`healthserver` только когда выбран Postgres-бэкенд.
2. `newStore(registry prometheus.Registerer) (orders.Store, *dbservice.Service)`:
   - `ORDERS_STORE` не равен `"postgres"` (включая не установленную переменную) → `orders.NewInMemoryStore(), nil` — дефолт, без внешних зависимостей
   - `ORDERS_STORE=postgres` → DSN берётся из `ORDERS_POSTGRES_DSN`, а если не задан — из `defaultPostgresDSN` (совпадает с `docker-compose.test.yml`, см. Operations #15); строится `dbservice.New(dsn, dbservice.WithAppName("orders-service"), dbservice.WithPrometheusRegisterer(registry))`; возвращается `orders.NewPostgresStore(dbSvc), dbSvc`
3. В `main()`: `dbSvc` (если не `nil`) добавляется и в список `svcs` для `entrypoint.WithServices` (retry-подключение, poll метрик пула, graceful `Stop` управляются самим entrypoint), и как дополнительный `healthserver.WithProber(dbSvc)` — в Postgres-режиме `/readyz` остаётся красным, пока база физически не отвечает на `Ping`
4. `ensureSchemaOnceConnected(ctx, store orders.Store, dbSvc *dbservice.Service) error`, вызывается из `entrypoint.WithPostStart`:
   - если `store` не `*orders.PostgresStore` или `dbSvc == nil` — no-op (`nil`), это ветка in-memory
   - иначе ждёт (поллингом каждые 50ms, ограничено `dbConnectTimeout = 15 * time.Second` через `context.WithTimeout`) пока `dbSvc.Pool() != nil`, затем вызывает `pg.EnsureSchema(ctx)`
   - таймаут обязателен: `PostStart` выполняется до того, как `entrypoint` начинает слушать сигналы завершения — без ограничения по времени недоступная база сделала бы процесс неубиваемым на этом шаге

---

### 7. Создать `internal/kafkanet/kafkanet.go`

1. Ответственность: общий для `kafkaproducer`/`kafkaconsumer` хелпер построения `*kafka.Dialer` с TLS/SASL и dial-based проверки связности с брокером — избегает дублирования этой логики в двух независимых пакетах.
2. Структура:
   ```go
   type TLSSASLConfig struct {
       TLS      *tls.Config // nil = plaintext
       SASLUser string      // пусто = без SASL
       SASLPass string
   }
   ```
3. Функция:
   ```go
   func NewDialer(cfg TLSSASLConfig) *kafka.Dialer
   ```
   - `dialer := &kafka.Dialer{Timeout: 10 * time.Second, DualStack: true}`
   - если `cfg.TLS != nil` — `dialer.TLS = cfg.TLS`
   - если `cfg.SASLUser != ""` — `dialer.SASLMechanism = plain.Mechanism{Username: cfg.SASLUser, Password: cfg.SASLPass}` (пакет `github.com/segmentio/kafka-go/sasl/plain`)
   - вернуть `dialer`
4. Функция:
   ```go
   func Probe(ctx context.Context, brokers []string, dialer *kafka.Dialer) error
   ```
   - `if len(brokers) == 0 { return errors.New("kafkanet: no brokers configured") }`
   - `conn, err := dialer.DialContext(ctx, "tcp", brokers[0])`
   - при ошибке — `fmt.Errorf("kafkanet: probe %s: %w", brokers[0], err)`
   - при успехе — `defer conn.Close()`, вернуть `nil`

---

### 8. Создать `internal/kafkanet/kafkanet_test.go`

Пакет: `package kafkanet_test`; реальный Kafka (см. Operations #15) — тесты читают брокеры из `TEST_KAFKA_BROKERS` и **скипаются**, если переменная не установлена.

1. **TestNewDialer_PlaintextConfig**: `TLSSASLConfig{}` (нулевое значение) — `NewDialer` возвращает dialer без `TLS`/`SASLMechanism`
2. **TestNewDialer_WithSASL**: `TLSSASLConfig{SASLUser: "u", SASLPass: "p"}` — возвращённый dialer имеет ненулевой `SASLMechanism`
3. **TestProbe_Succeeds**: реальный доступный брокер — `Probe` возвращает `nil`
4. **TestProbe_FailsOnUnreachableBroker**: `Probe(ctx, []string{"localhost:1"}, dialer)` — возвращает ошибку в разумное время (не висит)
5. **TestProbe_NoBrokers_ReturnsError**: пустой `brokers` — возвращает ошибку немедленно, без сетевого вызова

---

### 9. Реализовать `kafkaconsumer/consumer.go`

1. Ответственность: жизненный цикл (`Service`/`Shutdown`/`Prober`) управляемого Kafka consumer'а, регистрация обработчиков по топику, consume-loop с диспетчеризацией через `workerpool.Pool`.

2. Структура:
   ```go
   type Handler func(ctx context.Context, msg Message) error

   type Message struct {
       Topic     string
       Partition int
       Offset    int64
       Key       []byte
       Value     []byte
       Headers   map[string][]byte
       Time      time.Time
   }

   type Consumer struct {
       brokers       []string
       consumerGroup string
       appName       string
       tlsCfg        kafkanet.TLSSASLConfig
       retry         retry.Config
       poolSize      int
       logger        *slog.Logger
       registerer    prometheus.Registerer

       mu      sync.RWMutex
       topics  map[string]Handler
       reader  *kafka.Reader
       pool    *workerpool.Pool
       started atomic.Bool
       closeOnce sync.Once

       consumedTotal   *prometheus.CounterVec   // kafka_consume_messages_total{kafka_service,topic}
       consumeErrors   *prometheus.CounterVec   // kafka_consume_errors_total{kafka_service,topic}
       consumeDuration *prometheus.HistogramVec // kafka_consume_duration_seconds{kafka_service,topic}
       consumeLag      *prometheus.GaugeVec     // kafka_consume_lag{kafka_service,topic}
   }
   ```

3. Конструктор:
   ```go
   func New(brokers []string, consumerGroup string, opts ...Option) *Consumer
   ```
   Дефолты: `retry=retry.DefaultConfig()`, `poolSize=8`, `logger=slog.Default()`, `registerer=prometheus.DefaultRegisterer`, `topics=make(map[string]Handler)`.

4. `func (c *Consumer) Handle(topic string, h Handler)`:
   - под `c.mu.Lock()` добавить `c.topics[topic] = h`
   - **Precondition** (задокументировать doc-комментарием, аналогично `grpcserver.GRPCServer()`): должен вызываться до `Start`; вызов после `Start` не паникует, но не подхватывается уже запущенным `kafka.Reader` (у которого `GroupTopics` фиксируется на момент создания в `Start`)

5. `Start(ctx context.Context) error`:
   - `dialer := kafkanet.NewDialer(c.tlsCfg)`
   - retry-проверка связности: `_, err := retry.Do(ctx, c.retry, func(attemptCtx context.Context) (struct{}, error) { return struct{}{}, kafkanet.Probe(attemptCtx, c.brokers, dialer) })`; при ошибке — `fmt.Errorf("kafkaconsumer: connect: %w", err)`
   - `c.pool = workerpool.New(c.poolSize)`; запустить `wg.Go(func() { c.pool.Start(ctx) })` (`workerpool.Pool.Start` блокирует до `ctx.Done()`, поэтому в отдельной горутине)
   - под `c.mu.RLock()` собрать `topicList := keys(c.topics)`; если `len(topicList) == 0` — вернуть `errors.New("kafkaconsumer: no topics registered, call Handle before Start")`
   - `c.reader = kafka.NewReader(kafka.ReaderConfig{Brokers: c.brokers, GroupID: c.consumerGroup, GroupTopics: topicList, Dialer: dialer, MinBytes: 10e3, MaxBytes: 10e6, MaxWait: 500*time.Millisecond})`
   - `c.started.Store(true)`
   - запустить метрики-поллер лага (`ticker` по аналогии с `dbservice`, читает `c.reader.Stats().Lag` каждые N секунд, останавливается на `ctx.Done()`) в отдельной горутине
   - выполнить consume-loop (блокирующий, в текущей горутине, до `ctx.Done()`): `for { msg, err := c.reader.FetchMessage(ctx); if err != nil { if errors.Is(err, context.Canceled) { break }; c.logger.Error("kafkaconsumer: fetch", "error", err); continue }; converted := toMessage(msg); handler, ok := c.topics[msg.Topic]; if !ok { _ = c.reader.CommitMessages(ctx, msg); continue }; if err := c.pool.Submit(ctx, func(taskCtx context.Context) { c.dispatch(ctx, taskCtx, msg, converted, handler) }); err != nil { break } }`
   - `c.dispatch` (приватный метод): вызывает `handler(taskCtx, converted)`, замеряет `consumeDuration`, при ошибке — инкремент `consumeErrors`, лог, **не коммитит**; при успехе — `reader.CommitMessages(ctx, msg)` (используя внешний `ctx` из `Start`, не `taskCtx`, чтобы коммит не отменялся по таймауту конкретной задачи), инкремент `consumedTotal`
   - вернуть `nil` после выхода из consume-loop

6. `Stop(ctx context.Context, _ error) error`:
   - через `c.closeOnce`: если `c.reader != nil` — `c.reader.Close()`
   - `c.pool.Stop(ctx, nil)` (реализует `service.Shutdown`; по умолчанию `WithDrainOnStop` не задан — `Stop` пула no-op, дренаж управляется отменой `ctx` в `Start`, зеркалирует контракт `workerpool.Pool`)
   - вернуть первую встреченную ошибку, обёрнутую через `%w`, или `nil`

7. `Probe(ctx context.Context) error`:
   - если `!c.started.Load()` — вернуть `errors.New("kafkaconsumer: not started")`
   - вернуть `kafkanet.Probe(ctx, c.brokers, kafkanet.NewDialer(c.tlsCfg))` (обёрнутая ошибка уже содержит контекст «kafkanet: probe ...», доп. обёртка не нужна)

8. Options:
   - `WithTLS(cfg *tls.Config)`
   - `WithSASL(user, pass string)`
   - `WithRetry(cfg retry.Config)`
   - `WithWorkerPoolSize(n int)`
   - `WithAppName(name string)`
   - `WithLogger(l *slog.Logger)`
   - `WithPrometheusRegisterer(r prometheus.Registerer)`

---

### 10. Создать `kafkaconsumer/consumer_test.go`

Пакет: `package kafkaconsumer_test`; во всех тестах: `WithPrometheusRegisterer(prometheus.NewRegistry())`; реальный Kafka (см. Operations #15) — тесты читают брокеры из `TEST_KAFKA_BROKERS` (запятая-разделённый список) и **скипаются**, если переменная не установлена. Для отправки тестовых сообщений используется сырой `kafka.Writer` (не `kafkaproducer` — тесты пакета не зависят от другого нового пакета).

1. **TestConsumer_StartWithoutHandle_ReturnsError**: `New(brokers, group)` без `Handle` — `Start` возвращает ошибку немедленно
2. **TestConsumer_HandlerReceivesProducedMessage**: `Handle(topic, handler)` записывает полученные `Message` в канал; `Start` в горутине; сырой `kafka.Writer` отправляет сообщение в тот же топик; `require.Eventually` — handler получил сообщение с ожидаемыми `Key`/`Value`
3. **TestConsumer_HandlerErrorDoesNotCommit**: handler всегда возвращает ошибку — сообщение передоставляется (повторно фетчится) при повторном чтении с того же consumer group
4. **TestConsumer_ProbeBeforeStart_ReturnsError**
5. **TestConsumer_StartRetriesOnInitialFailure**: заведомо недоступный broker, короткий `retry.Config` — `Start` возвращает ошибку после исчерпания попыток
6. **TestConsumer_StopIdempotent**

---

### 11. Реализовать `kafkaproducer/producer.go`

1. Ответственность: жизненный цикл (`Service`/`Shutdown`/`Prober`) управляемого Kafka producer'а, отправка сообщений в произвольные топики через один shared `kafka.Writer`.

2. Структура:
   ```go
   type Producer struct {
       brokers    []string
       appName    string
       tlsCfg     kafkanet.TLSSASLConfig
       retry      retry.Config
       logger     *slog.Logger
       registerer prometheus.Registerer

       mu      sync.RWMutex
       writer  *kafka.Writer
       started atomic.Bool
       closeOnce sync.Once

       produceTotal    *prometheus.CounterVec   // kafka_produce_messages_total{kafka_service,topic}
       produceErrors   *prometheus.CounterVec   // kafka_produce_errors_total{kafka_service,topic}
       produceDuration *prometheus.HistogramVec // kafka_produce_duration_seconds{kafka_service,topic}
   }
   ```

3. Конструктор:
   ```go
   func New(brokers []string, opts ...Option) *Producer
   ```
   Дефолты: `retry=retry.DefaultConfig()`, `logger=slog.Default()`, `registerer=prometheus.DefaultRegisterer`.

4. `Start(ctx context.Context) error`:
   - `dialer := kafkanet.NewDialer(p.tlsCfg)`
   - retry-проверка связности: `_, err := retry.Do(ctx, p.retry, func(attemptCtx context.Context) (struct{}, error) { return struct{}{}, kafkanet.Probe(attemptCtx, p.brokers, dialer) })`; при ошибке — `fmt.Errorf("kafkaproducer: connect: %w", err)`
   - под `p.mu.Lock()`: `p.writer = &kafka.Writer{Addr: kafka.TCP(p.brokers...), Balancer: &kafka.Hash{}, Transport: &kafka.Transport{Dial: dialer.DialFunc, SASL: dialer.SASLMechanism, TLS: dialer.TLS}}`
   - `p.started.Store(true)`
   - зарегистрировать метрики через `prom.RegisterOrReuse`
   - блокировать на `<-ctx.Done()`, вернуть `nil`

5. `Stop(ctx context.Context, _ error) error`:
   - через `p.closeOnce`: под `p.mu.RLock()` прочитать `writer`; если не `nil` — `writer.Close()`
   - вернуть обёрнутую ошибку или `nil`

6. `Probe(ctx context.Context) error`:
   - если `!p.started.Load()` — вернуть `errors.New("kafkaproducer: not started")`
   - вернуть `kafkanet.Probe(ctx, p.brokers, kafkanet.NewDialer(p.tlsCfg))`

7. `func (p *Producer) Produce(ctx context.Context, topic string, key, value []byte, headers map[string][]byte) error`:
   - под `p.mu.RLock()` прочитать `p.writer`; если `nil` — вернуть `errors.New("kafkaproducer: not started")`
   - собрать `kafka.Message{Topic: topic, Key: key, Value: value, Headers: toKafkaHeaders(headers)}`
   - `start := time.Now(); err := p.writer.WriteMessages(ctx, msg); p.produceDuration.WithLabelValues(p.appName, topic).Observe(time.Since(start).Seconds())`
   - при ошибке — инкремент `p.produceErrors.WithLabelValues(p.appName, topic)`, вернуть `fmt.Errorf("kafkaproducer: produce to %s: %w", topic, err)`
   - при успехе — инкремент `p.produceTotal.WithLabelValues(p.appName, topic)`, вернуть `nil`

8. Options:
   - `WithTLS(cfg *tls.Config)`
   - `WithSASL(user, pass string)`
   - `WithRetry(cfg retry.Config)`
   - `WithAppName(name string)`
   - `WithLogger(l *slog.Logger)`
   - `WithPrometheusRegisterer(r prometheus.Registerer)`

---

### 12. Создать `kafkaproducer/producer_test.go`

Пакет: `package kafkaproducer_test`; во всех тестах: `WithPrometheusRegisterer(prometheus.NewRegistry())`; реальный Kafka (см. Operations #15) — тесты читают брокеры из `TEST_KAFKA_BROKERS` и **скипаются**, если переменная не установлена. Для проверки доставленных сообщений используется сырой `kafka.Reader` (не `kafkaconsumer` — тесты пакета не зависят от другого нового пакета).

1. **TestProducer_ProduceBeforeStart_ReturnsError**
2. **TestProducer_ProbeBeforeStart_ReturnsError**
3. **TestProducer_StartRetriesOnInitialFailure**: заведомо недоступный broker, короткий `retry.Config` — `Start` возвращает ошибку после исчерпания попыток
4. **TestProducer_SendsToCorrectTopic**: `Produce(ctx, "topic-a", ...)` и `Produce(ctx, "topic-b", ...)` из одного продюсера — оба сообщения оказываются в соответствующих топиках (проверка через отдельный `kafka.Reader` в тесте)
5. **TestProducer_StopIdempotent**

---

### 13. Реализовать `redisservice/service.go`

1. Ответственность: управляемый Redis-клиент — `Start` подключается с retry-backoff через `PING`, `Probe` проверяет связность, `Stop` закрывает клиента, метрики пула — периодический poll `client.PoolStats()`.

2. Структура:
   ```go
   type Service struct {
       dsn             string
       poolSize        int
       appName         string
       retry           retry.Config
       metricsInterval time.Duration
       logger          *slog.Logger
       registerer      prometheus.Registerer

       mu     sync.RWMutex
       client *redis.Client

       poolConns     *prometheus.GaugeVec     // redis_pool_conns{redis_service,state}
       poolHits      *prometheus.GaugeVec     // redis_pool_hits{redis_service}
       poolMisses    *prometheus.GaugeVec     // redis_pool_misses{redis_service}
       probeDuration *prometheus.HistogramVec // redis_probe_duration_seconds{redis_service}
   }
   ```

3. Конструктор:
   ```go
   func New(dsn string, opts ...Option) *Service
   ```
   Дефолты: `poolSize=0` (0 = использовать дефолт `go-redis`, обычно `10 * runtime.NumCPU()`), `retry=retry.DefaultConfig()`, `metricsInterval=15*time.Second`, `logger=slog.Default()`, `registerer=prometheus.DefaultRegisterer`.

4. `Start(ctx context.Context) error`:
   - `redisOpts, err := redis.ParseURL(s.dsn)`; при ошибке — вернуть немедленно без retry (конфигурационная, не транзиентная ошибка)
   - если `s.poolSize > 0` — `redisOpts.PoolSize = s.poolSize`
   - `client, err := retry.Do(ctx, s.retry, func(attemptCtx context.Context) (*redis.Client, error) { c := redis.NewClient(redisOpts); if err := c.Ping(attemptCtx).Err(); err != nil { c.Close(); return nil, err }; return c, nil })`
   - при ошибке после исчерпания retry — `fmt.Errorf("redisservice: connect: %w", err)`
   - под `s.mu.Lock()` сохранить `s.client = client`
   - зарегистрировать метрики, запустить горутину-поллер `client.PoolStats()` (аналогично `dbservice`, лейблы `state=["total","idle"]` для `poolConns`, `poolHits`/`poolMisses` — кумулятивные счётчики из `Stats.Hits`/`Stats.Misses`, экспонируются как `Gauge`, не `Counter`, по той же причине, что и в `dbservice`: источник даёт снапшот, не дельту)
   - блокировать на `<-ctx.Done()`, вернуть `nil`

5. `Stop(ctx context.Context, _ error) error`:
   - под `s.mu.RLock()` прочитать `client`; если `nil` — вернуть `nil`
   - `client.Close()` через `sync.Once`

6. `Probe(ctx context.Context) error`:
   - под `s.mu.RLock()` прочитать `client`; если `nil` — вернуть `errors.New("redisservice: not connected")`
   - `start := time.Now(); err := client.Ping(ctx).Err(); s.probeDuration.WithLabelValues(s.appName).Observe(time.Since(start).Seconds())`
   - при ошибке — вернуть `fmt.Errorf("redisservice: probe: %w", err)`

7. `Client() *redis.Client`:
   - под `s.mu.RLock()` вернуть `client` (может быть `nil` до успешного `Start`)

8. Options:
   - `WithPoolSize(n int)`
   - `WithRetry(cfg retry.Config)`
   - `WithMetricsInterval(d time.Duration)`
   - `WithAppName(name string)`
   - `WithLogger(l *slog.Logger)`
   - `WithPrometheusRegisterer(r prometheus.Registerer)`

---

### 14. Создать `redisservice/service_test.go`

Пакет: `package redisservice_test`; во всех тестах: `WithPrometheusRegisterer(prometheus.NewRegistry())`; реальный Redis — тесты читают DSN из `TEST_REDIS_DSN` и **скипаются**, если переменная не установлена.

1. **TestService_StartConnectsAndProbeSucceeds**
2. **TestService_ProbeBeforeStart_ReturnsError**
3. **TestService_StartRetriesOnInitialFailure**: заведомо недоступный DSN (`redis://localhost:1`), короткий `retry.Config` — ошибка после исчерпания попыток
4. **TestService_StopClosesClient**: после `Stop` — `Client().Ping(ctx)` возвращает ошибку (закрытый клиент)
5. **TestService_StopIdempotent**

---

### 15. Обновить `.github/workflows/ci.yml`: добавить service-контейнеры

1. Ответственность: реальные Postgres/Kafka/Redis для интеграционных тестов новых пакетов, без моков транспорта (соответствует конвенции `CLAUDE.md`).
2. В job `test` добавить `services:`:
   ```yaml
   services:
     postgres:
       image: postgres:16
       env:
         POSTGRES_USER: gokit
         POSTGRES_PASSWORD: gokit
         POSTGRES_DB: gokit_test
       ports: ["5432:5432"]
       options: >-
         --health-cmd="pg_isready -U gokit"
         --health-interval=5s --health-timeout=3s --health-retries=10
     redis:
       image: redis:7
       ports: ["6379:6379"]
       options: >-
         --health-cmd="redis-cli ping"
         --health-interval=5s --health-timeout=3s --health-retries=10
     kafka:
       image: apache/kafka:latest
       ports: ["9092:9092"]
       env:
         KAFKA_NODE_ID: "1"
         KAFKA_PROCESS_ROLES: "broker,controller"
         KAFKA_LISTENERS: "PLAINTEXT://:9092,CONTROLLER://:9093"
         KAFKA_ADVERTISED_LISTENERS: "PLAINTEXT://localhost:9092"
         KAFKA_CONTROLLER_LISTENER_NAMES: "CONTROLLER"
         KAFKA_CONTROLLER_QUORUM_VOTERS: "1@localhost:9093"
         KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: "CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT"
         KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR: "1"
   ```
3. Добавить шаг `env:` для самого шага `Test with race detector`, экспортирующий переменные, которые читают тесты:
   ```yaml
   env:
     TEST_POSTGRES_DSN: postgresql://gokit:gokit@localhost:5432/gokit_test
     TEST_REDIS_DSN: redis://localhost:6379/0
     TEST_KAFKA_BROKERS: localhost:9092
   ```
4. Проверить: `go test -race ./...` в CI реально подключается к контейнерам (тесты новых пакетов не скипаются в CI-логах — отсутствие строк `--- SKIP`), а локальный `go test ./...` без переменных окружения продолжает проходить (скип, не провал).

---

### 16. Создать `docker-compose.test.yml` и recipes в `justfile` для локального запуска интеграционных тестов

1. Ответственность: дать локальный эквивалент CI service-контейнеров (Operations #15), чтобы разработчик мог гонять `TestService_StartConnectsAndProbeSucceeds`-подобные тесты не только в CI — без этого интеграционные тесты новых пакетов фактически недостижимы локально (переменные `TEST_*_DSN`/`TEST_KAFKA_BROKERS` просто не с чем сопоставить).
2. `docker-compose.test.yml` в корне репозитория — зеркалирует `services:` из `.github/workflows/ci.yml` один в один (те же образы `postgres:16`/`redis:7`/`apache/kafka:latest`, те же переменные окружения, те же порты), но в `docker compose`-синтаксисе (`environment:` вместо `env:`, `healthcheck:` вместо `options: --health-cmd=...`); для Kafka healthcheck использует `kafka-broker-api-versions.sh` вместо `redis-cli ping`/`pg_isready`, так как у образа `apache/kafka` нет отдельной CLI-проверки готовности брокера короче.
3. Новые recipes в `justfile`:
   ```just
   test-infra-up:
       docker compose -f docker-compose.test.yml up -d --wait

   test-infra-down:
       docker compose -f docker-compose.test.yml down -v

   test-integration: test-infra-up
       #!/usr/bin/env bash
       set -e
       export TEST_POSTGRES_DSN="postgresql://gokit:gokit@localhost:5432/gokit_test"
       export TEST_REDIS_DSN="redis://localhost:6379/0"
       export TEST_KAFKA_BROKERS="localhost:9092"
       go test -race ./...
   ```
   `test-integration` зависит от `test-infra-up` (запускает контейнеры и ждёт `--wait` их healthy-статуса), затем экспортирует те же переменные, что и CI-шаг, и гоняет `go test -race ./...` — тот же набор тестов, что в CI, без дублирования логики выбора DSN.
4. `test-infra-down` — отдельная recipe, не вызывается автоматически после `test-integration`: контейнеры намеренно остаются подняты между локальными прогонами (быстрее повторный `just test-integration`), разработчик останавливает их явно.

## Norms

1. **Именование пакетов**: `dbservice`, `kafkaproducer`, `kafkaconsumer`, `redisservice` — строчные, одно слово; `dbservice`/`redisservice` — суффикс `service` (соответствует `httpserver`/`grpcserver`/`healthserver`), `kafkaproducer`/`kafkaconsumer` — суффикс по роли (соответствует `grpcserver`/`grpcclient`).

2. **Functional options**: единый `type Option func(*T)` на пакет; поля структуры не экспортируются; опции применяются до первого использования внутренних зависимостей в конструкторе (как во всех существующих пакетах). `kafkaproducer.Option` и `kafkaconsumer.Option` — независимые типы, коллизия имён (`WithTLS`, `WithSASL`, `WithLogger`) невозможна по построению, поскольку пакеты разные.

3. **Retry-with-backoff вместо fail-fast для `Start()`**: специфично для этих четырёх пакетов (отклонение от `httpserver`/`grpcserver`), через общий `internal/retry.Do[T]`; ошибки конфигурации (невалидный DSN — `pgxpool.ParseConfig`/`redis.ParseURL` возвращают ошибку до попытки сети) **не** ретраятся — возвращаются немедленно, так как это не транзиентный отказ.

4. **`service.Prober.Probe(ctx)` — единый метод, без разделения ready/health**: следует уже устоявшемуся в `gokit-services` контракту (не импортировать двухметодный `ready()`/`health()` сплит из ptpylibs); `Probe` до успешного `Start` явно возвращает ошибку «not connected»/«not started», не паникует на nil.

5. **Идемпотентный `Stop`**: через `sync.Once` (мирроринг `grpcserver.shutdownOnce`) или `atomic.Bool`-флаг — второй и последующие вызовы `Stop` не паникуют и не блокируют.

6. **Метрики только через `internal/prom.RegisterOrReuse`** с injectable `Registerer`; лейбл `<domain>_service` (`db_service`/`kafka_service`/`redis_service`) заполняется из `WithAppName`, мирроринг `http_service` в `httpserver`. `kafkaproducer` и `kafkaconsumer` используют общий префикс `kafka_` в именах метрик, различаясь по типу метрики (`kafka_produce_*` vs `kafka_consume_*`), не по лейблу пакета.

7. **Poll-based метрики пула, не event-based**: `pgxpool`/`go-redis` не предоставляют connect/checkout/checkin-хуков (в отличие от SQLAlchemy в ptpylibs) — метрики пула собираются периодическим `Stat()`/`PoolStats()`-poll в отдельной горутине, интервал настраивается через `WithMetricsInterval`, останавливается вместе с `ctx.Done()` в `Start`.

8. **Общая TLS/SASL-логика вынесена в `internal/kafkanet`**: `kafkaproducer` и `kafkaconsumer` не дублируют построение `*kafka.Dialer` и dial-based `Probe` — оба вызывают `kafkanet.NewDialer`/`kafkanet.Probe`, изменения в TLS/SASL-обработке вносятся в одном месте.

9. **Логирование**: `slog.Default()` по умолчанию, `WithLogger` для переопределения; уровни: `Error` — ошибки подключения/обработки сообщений, `Warn` — неудачная попытка retry (не последняя), `Info` — успешное подключение/переподключение.

10. **Тесты**: `package X_test` (black-box), реальные Postgres/Kafka/Redis через переменные окружения (`TEST_POSTGRES_DSN`/`TEST_KAFKA_BROKERS`/`TEST_REDIS_DSN`), `t.Skip`, если переменная не установлена (локальный `go test ./...` без Docker проходит, CI — гоняет полноценно), `require.Eventually` вместо `time.Sleep`, `WithPrometheusRegisterer(prometheus.NewRegistry())` во всех тестах. Тесты `kafkaproducer` и `kafkaconsumer` не импортируют друг друга — каждый использует сырой `kafka.Writer`/`kafka.Reader` для проверки встречной стороны.

11. **Разграничение пакетов**: `kafkaconsumer` зависит от `workerpool` (единственная зависимость на публичный пакет `gokit-services` среди четырёх новых) для диспетчеризации сообщений; `dbservice`/`redisservice`/`kafkaproducer` ни от чего внутри `gokit-services`, кроме `internal/prom`/`internal/retry`/`internal/kafkanet`, не зависят.

## Safeguards

1. **Функциональные ограничения**:
   - `Start()` при недоступности внешней инфраструктуры ретраит согласно `retry.Config`, не блокирует бесконечно — обязательно завершается либо успехом, либо ошибкой после `MaxAttempts` попыток, либо отменой `ctx`
   - `Probe()` вызванный до успешного `Start()` (или пока `Start()` ещё ретраит) не паникует — возвращает явную ошибку «not connected»/«not started»
   - Ошибки конфигурации (невалидный DSN, невалидный broker-адрес) не ретраятся — возвращаются немедленно из `Start()`
   - `kafkaconsumer.Start()` без хотя бы одного зарегистрированного через `Handle` топика возвращает ошибку немедленно, не запускает пустой consume-loop

2. **Ограничения производительности**:
   - `kafkaconsumer`: конкурентность обработки сообщений ограничена `workerpool.Pool` (`WithWorkerPoolSize`, по умолчанию 8) — нет неограниченного роста горутин на burst входящих сообщений
   - Poll-интервал метрик пула (`WithMetricsInterval`, по умолчанию 15s) не создаёт заметной нагрузки на `Stat()`/`PoolStats()` (обе операции — O(1) чтение внутреннего состояния, не сетевой вызов)

3. **Ограничения безопасности**:
   - `kafkaproducer`/`kafkaconsumer` поддерживают TLS (`WithTLS(*tls.Config)`) и SASL/PLAIN (`WithSASL(user, pass)`) через общий `internal/kafkanet` — секреты (`SASLPass`) не логируются ни на одном уровне
   - DSN-строки (`dbservice`/`redisservice`) могут содержать пароль в открытом виде — не логировать DSN целиком, только хост/порт при необходимости диагностики

4. **Ограничения совместимости**:
   - Четыре новых пакета — аддитивное изменение, не трогают публичный API существующих пакетов (`httpserver`, `grpcserver`, `grpcclient`, `healthserver`, `periodic`, `workerpool`, `entrypoint`, `service`)
   - `internal/retry`, `internal/kafkanet` — новые internal-пакеты, не публичный API, могут свободно меняться без учёта обратной совместимости

5. **Ограничения тестирования**:
   - Каждый новый пакет имеет `_test.go` с покрытием: happy path подключения, `Probe` до/после `Start`, retry на первичном отказе, идемпотентность `Stop`
   - Тесты, требующие реальной инфраструктуры, скипаются (не падают) при отсутствии переменной окружения — `go test ./...` без Docker остаётся зелёным
   - `golangci-lint run ./...` — `0 issues` после добавления новых пакетов

6. **Ограничения зависимостей**:
   - `dbservice` добавляет `github.com/jackc/pgx/v5` (прямая зависимость)
   - `kafkaproducer`/`kafkaconsumer`/`internal/kafkanet` вместе добавляют одну прямую зависимость `github.com/segmentio/kafka-go` (используется всеми тремя, а не дублируется в `go.mod`)
   - `redisservice` добавляет `github.com/redis/go-redis/v9` (прямая зависимость)
   - Ни один из новых пакетов не добавляет cgo-зависимостей — весь граф остаётся чисто-Go, кросс-компиляция не усложняется

7. **API-контракты**:
   - `Pool()`/`Client()` возвращают `nil` до успешного `Start()` — задокументировано как precondition (мирроринг `grpcclient.Client.Conn()`)
   - `kafkaconsumer.Handle(topic, handler)` должен вызываться до `Start()` — вызов после не паникует, но не подхватывается уже сконструированным `kafka.Reader`
   - `kafkaproducer.Produce()` вызванный до `Start()` возвращает ошибку, не паникует на nil `writer`

8. **Ограничения CI**:
   - Service-контейнеры (Postgres/Kafka/Redis) добавляются только в job `test` — job'ы `check` (build) и `lint` не требуют инфраструктуры и не замедляются
   - CI-время `test`-job'а может заметно вырасти (запуск трёх контейнеров + health-check ожидание) — приемлемый трейд-офф за реальные интеграционные тесты вместо моков
