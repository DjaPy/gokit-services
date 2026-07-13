# SPDD-анализ: Добавление сервисов PostgreSQL, Kafka, Redis

## Исходное бизнес-требование

необходимо добавить теперь сервисы posgresql, kafka, redis - можешь подсмотреть примеры в /Users/djapy/projects/ptpylibs/ptpylibs

---

## Идентификация доменных концепций

### Существующие концепции (из кодовой базы)

- **`service.Service` / `service.Shutdown` / `service.Prober`** (`service/service.go`): жизненные контракты, которым обязан следовать любой новый managed-компонент — блокирующий `Start(ctx)`, опциональный graceful `Stop(ctx, cause)`, опциональный readiness-опрос `Probe(ctx) error`.

- **`entrypoint.Entrypoint`** (`entrypoint/entrypoint.go`): единственный оркестратор; новые сервисы подключаются через `WithServices(...)` наравне с `httpserver`/`grpcserver`.

- **`httpserver` / `grpcserver` / `healthserver`**: эталонный шаблон для нового кода — конструктор через functional options с разумными дефолтами, `slog.Default()` + `WithLogger`, метрики Prometheus через injectable `Registerer` (`WithPrometheusRegisterer`), `Start(ctx)` блокирует до отмены ctx, `Stop(ctx, cause)` — graceful с `sync.Once`-защитой от двойного закрытия (см. `grpcserver.Server.shutdownOnce`).

- **`internal/prom.RegisterOrReuse`**: общий хелпер регистрации метрик, обязателен для любого нового metric-bearing пакета — уже используется `httpserver` и `healthserver`.

- **`service.Prober` + `healthserver.WithProber`**: точка агрегации readiness — новые Postgres/Kafka/Redis сервисы ожидаемо реализуют `Probe(ctx) error` с реальной проверкой связности (аналог `SELECT 1` / `PING`), а не просто «сконструирован без ошибки». Прецедент такого паттерна уже есть в `example/orders-service/cleanup.go` (`CleanupJob.Probe`, флаг `warmedUp`, readiness не «сразу true»).

- **`workerpool.Pool`**: ограниченный пул горутин с backpressure через `Submit(ctx, task)` — переиспользуемый механизм для конкурентной обработки Kafka-сообщений, вместо изобретения нового шедулера (подтверждено).

- **`example/orders-service`**: демонстрирует паттерн явного constructor injection (не через общий DI-контейнер) — `Store` передаётся напрямую в `HTTPAPI`, `GRPCAPI`, `OrderProcessor`. Обновление этого демо для реального использования Postgres/Kafka/Redis — подтверждённо отдельная будущая задача, не входит в объём этой итерации.

- **Референс `ptpylibs/ptpylibs`** (Python, `aiomisc.Service`-based тулкит, концептуальный аналог `gokit-services`): изучены `database_service/` (SQLAlchemy async engine, pool-метрики через `event.listens_for`, `PgLock` — advisory lock), `kafka_service/` (обёртка над `aiokafka` продюсером/консьюмером, метрики лагов/длительностей, диспетчеризация сообщений через `aiojobs.Scheduler`), `redis_service/` (обёртка над `redis.asyncio`, pool-метрики, `RedisLock`, подпапка `redis_stream/`), `ready_health_service/` (mixin `ReadyHealthCheck` с раздельными `ready()`/`health()`, но фактически ни один из трёх сервисов их не переопределяет — реальное подключение проб происходит через отдельные функции `db_probe`/`redis_probe`/`rabbit_probe` в `probes.py`, регистрируемые извне через `add_ready_check_external_function`). Служит источником *каких метрик/возможностей* ожидать от managed-обёртки, но не источником Go-идиом (Python-специфичные паттерны — DI-контекст, Pydantic-валидаторы — не переносятся напрямую).

- **Референс `/Users/djapy/_past/infravision/golibs/kafka_connector`** (Go, готовая реализация Kafka-клиента на `segmentio/kafka-go`): более прямой источник Go-идиом для `kafkaservice`, чем ptpylibs. Содержит `Consumer`/`ConsumerManager` (батчинг сообщений, тикер для частичных батчей, per-handler timeout, graceful shutdown с `atomic.Bool`-флагом закрытия), `Producer` (persistent writer, батчинг, опциональное шифрование payload'а), `retry.go` (`WithRetries` — реконнект с экспоненциальным backoff, ровно то, что нужно для решения по `Start()`, см. ниже), `circuit_breaker.go` (state-machine на consecutive-failures + cooldown), `tls.go` (TLS/SASL dialer с fallback), `Config` (плоская структура: `Host`/`Port`/`ConnectionRetries`/`ConsumerGroup`/`User`/`Pass`/`UseSSL`/`PathCert`/`PathKey`/`PathCA` — без Pydantic-подобного авто-вывода полей). Подтверждён пользователем как источник вдохновения для объёма Kafka-аутентификации.

### Новые концепции, которые нужно добавить

- **`dbservice`** (имя подтверждено пользователем — движок-агностичное, как `database_service` в ptpylibs, оставляет пространство для будущих SQL-движков без переименования пакета) — управляемый пул подключений к PostgreSQL: `service.Service` (Start открывает пул, с retry-with-backoff при недоступности БД — см. решение ниже), `service.Shutdown` (Close пула), `service.Prober` (реальный ping/`SELECT 1`), метрики пула (открытые/idle/in-use соединения — аналог `DB_CONNECTIONS_*` gauges из ptpylibs, но собираемые через `pgxpool.Stat()`-polling, а не connect/checkout-события).

- **`kafkaservice`** — управляемый Kafka producer + consumer group на `segmentio/kafka-go` (подтверждено): `service.Service` (consumer loop блокирует до отмены ctx; retry-with-backoff при недоступности брокеров на старте — паттерн зеркалирует `kafka_connector/retry.go`), `service.Shutdown` (graceful close продюсера и консьюмера), `service.Prober` (проверка связности с брокерами), регистрация обработчиков по топику (аналог `KafkaService.add_topic_handler`/`topic_handler` в ptpylibs и `ConsumerManager.Register` в `kafka_connector`), метрики консьюминга (лаг, длительность, ошибки — аналог `KAFKA_CONSUME_*`).

- **`redisservice`** — управляемый Redis-клиент на `redis/go-redis/v9`: `service.Service`/`service.Shutdown` (Close клиента), `service.Prober` (`PING`), метрики команд/пула (аналог `REDIS_CLIENT_COMMANDS_*`, `REDIS_CONNECTIONS_*`).

- **Retry-with-backoff при старте** — общий для всех трёх новых сервисов паттерн (подтверждено пользователем: «Retry с backoff внутри Start()»), в отличие от fail-fast поведения `httpserver`/`grpcserver` при ошибке bind порта. Обоснование: недоступность БД/брокера/Redis при старте процесса — транзиентный, ожидаемо самовосстанавливающийся отказ (например, под k8s под и его зависимости стартуют не строго последовательно), в отличие от «порт уже занят». Готовый Go-паттерн для этого уже есть в `kafka_connector/retry.go` (`WithRetries`, экспоненциальный backoff, уважает отмену ctx).

### Ключевые бизнес-правила

- **Соответствие `service.Service`/`service.Shutdown`**: как и все существующие managed-компоненты, три новых сервиса обязаны блокировать в `Start` до отмены ctx и поддерживать идемпотентный graceful `Stop` — управляет всеми тремя пакетами.

- **`Start()` ретраит с backoff, а не fail-fast**: подтверждённое отклонение от паттерна `httpserver`/`grpcserver` (которые быстро падают на ошибке bind) — специфично для трёх новых пакетов, так как недоступность внешней инфраструктуры при старте — принципиально иной (транзиентный) режим отказа. Управляет реализацией `Start()` во всех трёх пакетах.

- **Readiness — реальная проверка связности, не факт успешного конструирования**: `Probe(ctx)` должен реально дёргать `SELECT 1`/`PING`/broker-metadata-запрос, зеркалируя `db_probe`/`redis_probe` из ptpylibs — управляет `service.Prober`-реализациями всех трёх пакетов.

- **Метрики только через `internal/prom.RegisterOrReuse` с injectable `Registerer`**: обязательное условие для тестовой изоляции (см. `CLAUDE.md`: «Всегда передавать `WithPrometheusRegisterer` в тестах») — управляет всеми тремя пакетами.

- **Явный constructor injection, без общего DI-контекста**: в отличие от `aiomisc`-паттерна ptpylibs (`self.context[name]` — общий реестр, из которого зависимости достаются по строковому имени), `gokit-services` последовательно использует явную передачу зависимостей через конструкторы (см. `entrypoint.WithServices`, `orders-service.Store`) — управляет публичным API всех трёх новых пакетов: они должны возвращать сконструированный клиент/пул через явный метод-аксессор (`Pool()`, `Client()`, `Producer()`), а не прятать его в скрытый реестр.

- **Конкурентность обработки Kafka-сообщений через существующий `workerpool.Pool`**, а не через новый шедулер — управляет внутренним устройством `kafkaservice`.

- **Объём этой итерации — только managed-клиенты** (подтверждено пользователем): `Service`+`Shutdown`+`Prober`+метрики для трёх пакетов. Distributed locks (`PgLock`/`RedisLock`), Redis Streams, БД-backed task queue — явно вне объёма, будущие отдельные задачи.

---

## Стратегический подход

### Направление решения

Три новых top-level пакета — `dbservice`, `kafkaservice`, `redisservice` — строятся строго по уже устоявшемуся в проекте шаблону (`httpserver`/`grpcserver`/`healthserver`): конструктор через functional options с дефолтами, `slog`-логирование с переопределением, реализация `service.Service`+`service.Shutdown`, реализация `service.Prober` с реальной проверкой связности (подключаемой в `healthserver` через `WithProber`), метрики Prometheus за injectable `Registerer`. Единственное осознанное отклонение от шаблона `httpserver`/`grpcserver` — политика `Start()`: вместо fail-fast эти три сервиса ретраят подключение с экспоненциальным backoff (паттерн `kafka_connector/retry.go`), поскольку недоступность внешней инфраструктуры при старте — транзиентный отказ, а не «порт занят». Диспетчеризация Kafka-сообщений строится поверх уже существующего `workerpool.Pool`. Поток данных: конструктор принимает Settings-структуру → `Start()` устанавливает пул/клиента/соединение с retry (и для Kafka — запускает consume-loop) → сконструированный клиент/пул отдаётся вызывающей стороне через явный метод-аксессор, а не через неявный общий реестр.

### Ключевые решения дизайна

- **Postgres-драйвер**: `database/sql` + драйвер vs нативный `jackc/pgx/v5/pgxpool` → **рекомендация: `pgxpool`** — нативный пул с собственным API интроспекции (`Stat()`), без накладных расходов абстракции `database/sql`, де-факто стандарт современной Go-экосистемы для Postgres.

- **Kafka-клиент**: `segmentio/kafka-go` **(подтверждено)** — простой io-идиоматичный API, чистый Go (без cgo), уже провалидирован рабочим Go-референсом `kafka_connector`, который построен именно на нём.

- **Redis-клиент**: `redis/go-redis/v9` — реального альтернативного варианта в Go-экосистеме сравнимой зрелости нет; прямой аналог выбора `redis.asyncio` в ptpylibs.

- **Kafka auth/TLS конфигурация**: не «минимальный PLAINTEXT-only» и не полный перенос Pydantic-валидаторов ptpylibs, а **объём по образцу `kafka_connector/config.go`** (подтверждено пользователем) — плоская `Config`-структура (`Host`/`Port`/`ConnectionRetries`/`ConsumerGroup`/`User`/`Pass`/`UseSSL`/`PathCert`/`PathKey`/`PathCA`) с TLS-dialer'ом при `UseSSL=true`, без авто-вывода `security_protocol` из наличия других полей (это специфика Pydantic-валидаторов ptpylibs, не Go-идиома).

- **Distributed locks (`PgLock`/`RedisLock`)**: присутствуют в референсе как бонус-утилиты поверх managed-клиентов → **подтверждено: вне объёма этой итерации** — первый проход фокусируется на трёх managed-клиентах с их `Prober`/метриками, по аналогии с тем, как `grpcserver`/`grpcclient` были выделены в минимальный первый срез до появления `healthserver`/`periodic`/`workerpool`.

- **Конкурентность обработки Kafka-сообщений**: переиспользовать `workerpool.Pool` (уже написан, протестирован, race-checked) vs Kafka-специфичный шедулер по образцу `aiojobs.Scheduler`/`kafka_connector.ConsumerManager` → **рекомендация: переиспользовать `workerpool.Pool`** — избегает дублирования логики ограниченной конкурентности; `kafkaservice` берёт у `kafka_connector` идею батчинга/тикера на уровне одного consumer'а, но не собственный шедулер для fan-out между топиками.

- **Гранулярность health-проверки**: ptpylibs разделяет `ready()`/`health()` (readiness vs liveness) в mixin'е, но ни один из прочитанных `service.py` файлов их фактически не переопределяет — реальное подключение проб идёт через отдельные функции в `probes.py`, регистрируемые извне → **рекомендация: следовать уже устоявшемуся в `gokit-services` единому `service.Prober.Probe(ctx) error`** (уже используется `healthserver`, `CleanupJob` в `orders-service`), не импортировать ни двухметодный сплит ptpylibs, ни паттерн внешних probe-функций — сохраняет единый readiness-контракт по всему проекту.

- **Именование пакетов**: `dbservice`/`kafkaservice`/`redisservice` **(подтверждено)** — соответствует конвенции `httpserver`/`grpcserver`/`healthserver` (строчные, одно слово, суффикс `service`/`server`), избегает коллизий алиасинга с драйверами (особенно `pgx`, `redis`), а движок-агностичное `dbservice` (не `postgresservice`) оставляет пространство для будущих SQL-движков без переименования пакета.

- **Retry-политика при старте**: fail-fast (как `httpserver`/`grpcserver`) vs retry с экспоненциальным backoff внутри `Start()` → **подтверждено: retry с backoff**, ограниченный отменой ctx (паттерн `kafka_connector/retry.go`: `WithRetries` с `InitialBackoff`/`MaxBackoff`/`MaxRetries`, уважает `ctx.Done()`).

### Рассмотренные альтернативы

- **Общий `context`-реестр (DI в стиле aiomisc)** для межсервисной передачи зависимостей, по аналогии с `self.context[name]` в ptpylibs → **отклонено**: `gokit-services` последовательно использует явную передачу зависимостей через конструкторы (`entrypoint.WithServices`, `Store` в `orders-service`, передаваемый напрямую в `HTTPAPI`/`GRPCAPI`/`OrderProcessor`); введение неявного реестра стало бы первым в проекте отступлением от этого паттерна.

- **cgo-based Kafka-клиент (`confluent-kafka-go`/librdkafka)** → **отклонено**: добавляет зависимость от C-тулчейна в сборку, ломает простую кросс-компиляцию, не соответствует нынешнему чисто-Go графу зависимостей проекта; также отклонён `IBM/sarama` как альтернативный чисто-Go клиент — референс `kafka_connector` уже валидирует `segmentio/kafka-go` как рабочий выбор.

- **`database/sql` + `pq`/`pgx`-stdlib-драйвер** для Postgres → **отклонено в пользу нативного `pgxpool`**: теряется нативная интроспекция пула на уровне pgx (нужна для зеркалирования `DB_CONNECTIONS_*`-gauge'ов ptpylibs), добавляется слой абстракции без выгоды — проекту не нужна портируемость между разными SQL-движками через единый интерфейс `database/sql`.

- **Полный перенос Pydantic-стиля авто-вывода `security_protocol`** из `sasl_dsn`/`sasl_cert` (ptpylibs) → **отклонено**: `kafka_connector`'s плоский `Config` без авто-вывода полей — более Go-идиоматичный и уже рабочий референс; авто-вывод из ptpylibs добавил бы неявную логику конструирования без явного запроса пользователя на такую сложность.

---

## Анализ рисков и пробелов

### Неоднозначности требования

Все ранее выявленные неоднозначности объёма и выбора технологий **разрешены** в ходе обсуждения (см. подтверждённые решения выше: объём = только managed-клиенты; Kafka-клиент = `segmentio/kafka-go`; auth-конфиг = по образцу `kafka_connector`; нейминг = `dbservice`/`kafkaservice`/`redisservice`; `Start()` = retry-with-backoff; CI = добавить service-контейнеры в эту же итерацию; `example/orders-service` = не трогать, отдельная будущая задача). Остаётся одна открытая точка:

- **Конкретные версии/API-детали `pgxpool` и `go-redis/v9`**: выбор библиотек подтверждён на уровне «какую взять», но не проверялся построчно (в отличие от `kafkaservice`, для которого есть готовый рабочий Go-референс `kafka_connector`). Уровень детализации API pgxpool/go-redis — задача REASONS Canvas, не блокер анализа.

### Граничные случаи

- **Kafka consumer group rebalance посреди shutdown**: у `entrypoint`'s `stopCtx` фиксированный таймаут (`WithShutdownTimeout`, по умолчанию 60s) — leave/rebalance consumer group под нагрузкой иногда превышает типичные окна shutdown; требуется тот же паттерн «graceful, затем forceful fallback», что уже применён в `grpcserver.Server.Stop`. `kafka_connector.Consumer.Close()` уже реализует нечто похожее (сигнал отмены + короткая пауза перед закрытием reader'а) — стоит свериться при проектировании.

- **Вызов `Probe` до завершения `Start()`**: `healthserver`'s `/readyz` опрашивает `service.Prober.Probe(ctx)` независимо от жизненного цикла `entrypoint` — если Prober вызван до инициализации пула/клиента (nil pointer) или пока `Start()` ещё ретраит подключение, все три новых сервиса должны либо безопасно обрабатывать nil, либо явно возвращать «ещё не готов» — паттерн уже есть (`warmedUp`-флаг в `CleanupJob` из `orders-service`). Ретраящийся `Start()` делает это окно «не готов» шире, чем у fail-fast `httpserver`/`grpcserver` — стоит явно спроектировать.

- **Идемпотентный `Stop()` / двойное закрытие**: `pgxpool.Close()`, закрытие Kafka producer/consumer, `go-redis` `Client.Close()` — все должны переносить повторный вызов после того как отмена ctx уже запустила внутренний путь закрытия (зеркалирует `sync.Once`-паттерн `grpcserver` для `GracefulStop`/`Stop`; в `kafka_connector` для этого используется `atomic.Bool`-флаг `closed`).

- **Ретрай vs `ctx` с коротким таймаутом**: если вызывающий код передаёт в `Start()` уже почти истёкший ctx (например, из-за `WithShutdownTimeout` на entrypoint, применённого не туда), retry-цикл должен корректно прерваться по `ctx.Done()`, а не проглотить отмену — `kafka_connector.WithRetries` уже поступает так; важно перенести это поведение, а не только «общую идею ретрая».

### Технические риски

- **Новый объём third-party зависимостей**: добавление `pgx`, `segmentio/kafka-go` и `go-redis` тянет заметное поддерево зависимостей (сейчас в `go.mod` ровно 3 прямые зависимости: prometheus client, testify, grpc). Это существенный сдвиг от нынешней минимально-зависимой позиции проекта — стоит явно отметить как осознанный трейд-офф, а не тихо «проскользнувшее» изменение.

- **CI: добавление service-контейнеров (подтверждено в объёме этой итерации)**: устоявшаяся конвенция тестирования проекта (`CLAUDE.md`: «реальные HTTP соединения, никаких моков транспорта») для Postgres/Kafka/Redis означает service-контейнеры в `.github/workflows/ci.yml`, которого сейчас нет (сервис-контейнеры не сконфигурированы). Это не просто риск, а подтверждённая часть объёма работ: нужно расширить `ci.yml` (или reusable `ci.yml`, вызываемый из `release.yml`) job'ами с `services:` для Postgres/Kafka(+Zookeeper или KRaft)/Redis, что заметно увеличит время и сложность CI-пайплайна по сравнению с нынешним чисто-Go набором проверок.

- **Сложность Kafka producer/consumer поверх batching-логики `kafka-go`**: `kafka_connector` показывает, что простого «обернуть kafka.Writer/Reader» недостаточно для продакшн-качества — там уже есть батчинг с тикером, per-handler timeout, header-based фильтрация, опциональное шифрование payload'а, circuit breaker для продюсер-пула. Портирование части этой сложности (как минимум батчинг+timeout, вероятно не шифрование/circuit-breaker в первой итерации) — нетривиальный объём работы, а не «обернуть клиент в 50 строк», как для Redis.

- **Кардинальность метрик и механизм сбора**: Postgres/Kafka/Redis-метрики ptpylibs используют лейблы `addr`/`pod`/`app` на события пула соединений — воспроизвести такую же гранулярность в Go (через `client_golang`) технически прямолинейно, но событийные хуки пула SQLAlchemy (`event.listens_for` на connect/checkout/checkin/close) не имеют прямого 1:1 аналога в API `pgxpool`; `pgxpool` вместо этого предоставляет polling через `Stat()`, а не connect/checkout-события — значит, *механизм* сбора метрик будет отличаться, даже если *имена* метрик можно зеркалировать. Решение уровня REASONS Canvas, не блокер, но стоит отметить сейчас, чтобы не считалось прямым переносом «один в один».

### Покрытие критериев приёмки

Требование не содержит явных Acceptance Criteria — ниже выведенные (inferred) AC с учётом всех решённых в обсуждении вопросов.

| AC# | Описание | Реализуемо? | Пробелы / примечания |
|-----|----------|-------------|----------------------|
| 1 | Postgres-подключение (`dbservice`) управляется через `service.Service`+`service.Shutdown`, с реальным `Probe` (`SELECT 1`) и retry-with-backoff при недоступности БД на старте | Да | Драйвер (`pgxpool`) и политика retry подтверждены; детали API — задача REASONS Canvas |
| 2 | Kafka producer+consumer (`kafkaservice`) на `segmentio/kafka-go` управляются через `service.Service`+`service.Shutdown`, с реальным `Probe` и retry-with-backoff | Да | Клиент, auth-объём (по образцу `kafka_connector`) и retry-политика подтверждены; объём батчинга/circuit-breaker — уточнить в REASONS Canvas |
| 3 | Redis-клиент (`redisservice`) на `go-redis/v9` управляется через `service.Service`+`service.Shutdown`, с реальным `Probe` (`PING`) и retry-with-backoff | Да | Наиболее прямолинейная из трёх |
| 4 | Метрики трёх новых пакетов идут через `internal/prom.RegisterOrReuse` с injectable `Registerer` | Да | Механизм сбора pool-метрик у Postgres потребует адаптации (`Stat()`-polling вместо event-хуков) |
| 5 | Диспетчеризация Kafka-сообщений переиспользует `workerpool.Pool` | Да | Архитектурное решение, подтверждённое как обязательное для этой итерации |
| 6 | CI поднимает реальные Postgres/Kafka/Redis service-контейнеры для интеграционных тестов без моков | Да | Подтверждено в объёме итерации; требует доработки `.github/workflows/ci.yml`, заметный рост сложности пайплайна |
| 7 | Distributed locks (`PgLock`/`RedisLock`-аналоги), Redis Streams, `example/orders-service` с реальной персистентностью | Нет (вне объёма) | Явно подтверждено как отдельные будущие задачи, не входят в эту итерацию |
