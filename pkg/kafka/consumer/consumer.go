// Package consumer provides a managed Kafka consumer implementing
// service.Service, service.Shutdown, and service.Prober. A single reader
// consumes all registered topics as part of a consumer group and dispatches
// each message to its handler through a bounded worker pool. Offsets are
// committed only after a handler succeeds, giving at-least-once delivery.
package consumer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/segmentio/kafka-go"

	"github.com/DjaPy/gokit-services/pkg/internal/prom"
	"github.com/DjaPy/gokit-services/pkg/internal/retry"
	kafkanet "github.com/DjaPy/gokit-services/pkg/kafka"
	"github.com/DjaPy/gokit-services/pkg/workerpool"
)

const (
	defaultPoolSize        = 8
	defaultMetricsInterval = 15 * time.Second

	readerMinBytes = 10e3
	readerMaxBytes = 10e6
	readerMaxWait  = 500 * time.Millisecond

	fetchErrorBackoff = 1 * time.Second
)

// Handler processes a single consumed message. Returning an error prevents
// the offset from being committed, so the message is redelivered.
type Handler func(ctx context.Context, msg Message) error

// Message is a consumed Kafka message, decoupled from the kafka-go types.
type Message struct {
	Topic     string
	Partition int
	Offset    int64
	Key       []byte
	Value     []byte
	Headers   map[string][]byte
	Time      time.Time
}

// Consumer is a lifecycle-managed Kafka consumer.
type Consumer struct {
	brokers         []string
	consumerGroup   string
	appName         string
	tlsCfg          kafkanet.TLSSASLConfig
	retry           retry.Config
	poolSize        int
	metricsInterval time.Duration
	handlerTimeout  time.Duration
	logger          *slog.Logger
	registerer      prometheus.Registerer

	mu        sync.RWMutex
	topics    map[string]Handler
	reader    *kafka.Reader
	pool      *workerpool.Pool
	started   atomic.Bool
	closeOnce sync.Once

	consumedTotal   *prometheus.CounterVec
	consumeErrors   *prometheus.CounterVec
	consumeDuration *prometheus.HistogramVec
	consumeLag      *prometheus.GaugeVec
}

// Option configures a Consumer.
type Option func(*Consumer)

func WithTLS(cfg *tls.Config) Option {
	return func(c *Consumer) { c.tlsCfg.TLS = cfg }
}

func WithSASL(user, pass string) Option {
	return func(c *Consumer) {
		c.tlsCfg.SASLUser = user
		c.tlsCfg.SASLPass = pass
	}
}

// WithSASLMechanism selects the SASL mechanism (kafka.SASLPlain,
// kafka.SASLScramSHA256, kafka.SASLScramSHA512). Defaults to PLAIN when SASL
// credentials are set.
func WithSASLMechanism(mech kafkanet.SASLMechanism) Option {
	return func(c *Consumer) { c.tlsCfg.SASLMech = mech }
}

func WithRetry(cfg retry.Config) Option {
	return func(c *Consumer) { c.retry = cfg }
}

func WithWorkerPoolSize(n int) Option {
	return func(c *Consumer) { c.poolSize = n }
}

func WithMetricsInterval(d time.Duration) Option {
	return func(c *Consumer) { c.metricsInterval = d }
}

// WithHandlerTimeout bounds each handler invocation. A handler that exceeds d
// has its context canceled, freeing the worker-pool slot. Zero (the default)
// means no timeout — a hung handler holds its worker until Start's ctx ends.
func WithHandlerTimeout(d time.Duration) Option {
	return func(c *Consumer) { c.handlerTimeout = d }
}

func WithAppName(name string) Option {
	return func(c *Consumer) { c.appName = name }
}

func WithLogger(l *slog.Logger) Option {
	return func(c *Consumer) { c.logger = l }
}

func WithPrometheusRegisterer(r prometheus.Registerer) Option {
	return func(c *Consumer) { c.registerer = r }
}

// New creates a managed Kafka consumer for the given brokers and consumer
// group. The connection is established in Start, not New.
func New(brokers []string, consumerGroup string, opts ...Option) *Consumer {
	c := &Consumer{
		brokers:         brokers,
		consumerGroup:   consumerGroup,
		retry:           retry.DefaultConfig(),
		poolSize:        defaultPoolSize,
		metricsInterval: defaultMetricsInterval,
		logger:          slog.Default(),
		registerer:      prometheus.DefaultRegisterer,
		topics:          make(map[string]Handler),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Handle registers h as the handler for topic.
//
// Precondition: Handle must be called before Start. A topic registered
// afterwards does not panic but is never consumed — the reader's topic list
// is fixed when the reader is constructed in Start.
func (c *Consumer) Handle(topic string, h Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.topics[topic] = h
}

func (c *Consumer) initMetrics() {
	c.consumedTotal = prom.RegisterOrReuse(c.registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kafka_consume_messages_total",
			Help: "Total number of successfully handled and committed messages.",
		},
		[]string{"kafka_service", "topic"},
	))
	c.consumeErrors = prom.RegisterOrReuse(c.registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kafka_consume_errors_total",
			Help: "Total number of messages whose handler returned an error (not committed).",
		},
		[]string{"kafka_service", "topic"},
	))
	c.consumeDuration = prom.RegisterOrReuse(c.registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kafka_consume_duration_seconds",
			Help:    "The latency of message handlers.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"kafka_service", "topic"},
	))
	c.consumeLag = prom.RegisterOrReuse(c.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kafka_consume_lag",
			Help: "Consumer lag as reported by the reader (polled snapshot).",
		},
		[]string{"kafka_service", "topic"},
	))
}

// Start connects to the brokers, retrying with exponential backoff on
// failure, then consumes registered topics until ctx is canceled.
func (c *Consumer) Start(ctx context.Context) error {
	topicList := c.topicList()
	if len(topicList) == 0 {
		return errors.New("kafkaconsumer: no topics registered, call Handle before Start")
	}

	dialer, err := kafkanet.NewDialer(c.tlsCfg)
	if err != nil {
		return fmt.Errorf("kafkaconsumer: dialer: %w", err)
	}
	if _, err := retry.Do(ctx, c.retry, func(attemptCtx context.Context) (struct{}, error) {
		return struct{}{}, kafkanet.Probe(attemptCtx, c.brokers, dialer)
	}); err != nil {
		return fmt.Errorf("kafkaconsumer: connect: %w", err)
	}

	c.initMetrics()

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     c.brokers,
		GroupID:     c.consumerGroup,
		GroupTopics: topicList,
		Dialer:      dialer,
		MinBytes:    readerMinBytes,
		MaxBytes:    readerMaxBytes,
		MaxWait:     readerMaxWait,
	})
	pool := workerpool.New(c.poolSize, workerpool.WithLogger(c.logger))

	c.mu.Lock()
	c.reader = reader
	c.pool = pool
	c.mu.Unlock()
	c.started.Store(true)
	c.logger.Info("kafkaconsumer: connected", "group", c.consumerGroup, "topics", topicList)

	var wg sync.WaitGroup

	wg.Go(func() { _ = pool.Start(ctx) })
	wg.Go(func() { c.pollLag(ctx, reader) })

	c.consume(ctx, reader, pool)

	wg.Wait()
	return nil
}

func (c *Consumer) topicList() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	topics := make([]string, 0, len(c.topics))
	for topic := range c.topics {
		topics = append(topics, topic)
	}
	return topics
}

func (c *Consumer) handlerFor(topic string) (Handler, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.topics[topic]
	return h, ok
}

// pollLag samples reader statistics on an interval. kafka-go exposes lag as
// a snapshot rather than an event stream, so it is polled like the pgxpool
// stats in dbservice.
func (c *Consumer) pollLag(ctx context.Context, reader *kafka.Reader) {
	ticker := time.NewTicker(c.metricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := reader.Stats()
			c.consumeLag.WithLabelValues(c.appName, stats.Topic).Set(float64(stats.Lag))
		}
	}
}

// consume fetches messages until ctx is canceled or the reader is closed,
// dispatching each one to its handler through the worker pool.
func (c *Consumer) consume(ctx context.Context, reader *kafka.Reader, pool *workerpool.Pool) {
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.ErrClosedPipe) {
				return
			}
			c.logger.Error("kafkaconsumer: fetch", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(fetchErrorBackoff):
			}
			continue
		}

		handler, ok := c.handlerFor(msg.Topic)
		if !ok {
			if errCommit := reader.CommitMessages(ctx, msg); errCommit != nil {
				c.logger.Error("kafkaconsumer: commit unhandled topic", "topic", msg.Topic, "error", errCommit)
			}
			continue
		}

		converted := toMessage(msg)
		if err := pool.Submit(ctx, func(taskCtx context.Context) {
			c.dispatch(ctx, taskCtx, reader, msg, converted, handler)
		}); err != nil {
			return
		}
	}
}

// dispatch runs handler and commits the offset only on success, so a failed
// message is redelivered (at-least-once). The commit uses the Start context
// rather than the task context so that it is not bound to a single task's
// lifetime.
func (c *Consumer) dispatch(
	ctx, taskCtx context.Context,
	reader *kafka.Reader,
	raw kafka.Message,
	msg Message,
	handler Handler,
) {
	handlerCtx := taskCtx
	if c.handlerTimeout > 0 {
		var cancel context.CancelFunc
		handlerCtx, cancel = context.WithTimeout(taskCtx, c.handlerTimeout)
		defer cancel()
	}

	start := time.Now()
	err := handler(handlerCtx, msg)
	c.consumeDuration.WithLabelValues(c.appName, msg.Topic).Observe(time.Since(start).Seconds())

	if err != nil {
		c.consumeErrors.WithLabelValues(c.appName, msg.Topic).Inc()
		c.logger.Error("kafkaconsumer: handler",
			"topic", msg.Topic, "partition", msg.Partition, "offset", msg.Offset, "error", err)
		return
	}

	if errCommit := reader.CommitMessages(ctx, raw); errCommit != nil {
		c.logger.Error("kafkaconsumer: commit", "topic", msg.Topic, "error", errCommit)
		return
	}
	c.consumedTotal.WithLabelValues(c.appName, msg.Topic).Inc()
}

func toMessage(msg kafka.Message) Message {
	headers := make(map[string][]byte, len(msg.Headers))
	for _, h := range msg.Headers {
		headers[h.Key] = h.Value
	}
	return Message{
		Topic:     msg.Topic,
		Partition: msg.Partition,
		Offset:    msg.Offset,
		Key:       msg.Key,
		Value:     msg.Value,
		Headers:   headers,
		Time:      msg.Time,
	}
}

// Stop implements service.Shutdown. It closes the reader and stops the
// worker pool. Repeated calls are safe.
func (c *Consumer) Stop(ctx context.Context) error {
	c.mu.RLock()
	reader := c.reader
	pool := c.pool
	c.mu.RUnlock()

	var errClose error
	c.closeOnce.Do(func() {
		if reader == nil {
			return
		}
		if err := reader.Close(); err != nil {
			errClose = fmt.Errorf("kafkaconsumer: close reader: %w", err)
		}
	})

	if pool != nil {
		if err := pool.Stop(ctx); err != nil && errClose == nil {
			errClose = fmt.Errorf("kafkaconsumer: stop pool: %w", err)
		}
	}
	return errClose
}

// Probe implements service.Prober. It dials a broker to verify connectivity.
// Returns an error if Start has not yet connected.
func (c *Consumer) Probe(ctx context.Context) error {
	if !c.started.Load() {
		return errors.New("kafkaconsumer: not started")
	}
	dialer, err := kafkanet.NewDialer(c.tlsCfg)
	if err != nil {
		return fmt.Errorf("kafkaconsumer: dialer: %w", err)
	}
	//nolint:wrapcheck // kafkanet.Probe already reports "kafkanet: probe <broker>"
	return kafkanet.Probe(ctx, c.brokers, dialer)
}
