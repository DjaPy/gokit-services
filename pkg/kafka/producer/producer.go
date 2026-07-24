// Package producer provides a managed Kafka producer implementing
// service.Service, service.Shutdown, and service.Prober. A single writer
// serves every topic — the topic is given per Produce call rather than
// fixed on the writer — so one producer can publish to many topics.
package producer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/segmentio/kafka-go"

	"github.com/DjaPy/gokit-services/pkg/internal/prom"
	"github.com/DjaPy/gokit-services/pkg/internal/retry"
	kafkanet "github.com/DjaPy/gokit-services/pkg/kafka"
)

// Compression selects the codec applied to produced messages. The zero value
// (CompressionNone) leaves messages uncompressed.
type Compression string

const (
	CompressionNone   Compression = ""
	CompressionGzip   Compression = "gzip"
	CompressionSnappy Compression = "snappy"
	CompressionLz4    Compression = "lz4"
	CompressionZstd   Compression = "zstd"
)

// Message is one record for ProduceBatch. Topic is required per message so a
// single batch may span topics.
type Message struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers map[string][]byte
}

// Producer is a lifecycle-managed Kafka producer.
type Producer struct {
	brokers      []string
	appName      string
	tlsCfg       kafkanet.TLSSASLConfig
	retry        retry.Config
	compression  Compression
	writeTimeout time.Duration
	maxAttempts  int
	logger       *slog.Logger
	registerer   prometheus.Registerer

	mu        sync.RWMutex
	writer    *kafka.Writer
	started   atomic.Bool
	closeOnce sync.Once

	produceTotal    *prometheus.CounterVec
	produceErrors   *prometheus.CounterVec
	produceDuration *prometheus.HistogramVec
}

// Option configures a Producer.
type Option func(*Producer)

func WithTLS(cfg *tls.Config) Option {
	return func(p *Producer) { p.tlsCfg.TLS = cfg }
}

func WithSASL(user, pass string) Option {
	return func(p *Producer) {
		p.tlsCfg.SASLUser = user
		p.tlsCfg.SASLPass = pass
	}
}

// WithSASLMechanism selects the SASL mechanism (kafka.SASLPlain,
// kafka.SASLScramSHA256, kafka.SASLScramSHA512). Defaults to PLAIN when SASL
// credentials are set.
func WithSASLMechanism(mech kafkanet.SASLMechanism) Option {
	return func(p *Producer) { p.tlsCfg.SASLMech = mech }
}

func WithRetry(cfg retry.Config) Option {
	return func(p *Producer) { p.retry = cfg }
}

// WithCompression sets the compression codec for produced messages.
func WithCompression(c Compression) Option {
	return func(p *Producer) { p.compression = c }
}

// WithWriteTimeout bounds a single WriteMessages call. Zero keeps the
// kafka-go default.
func WithWriteTimeout(d time.Duration) Option {
	return func(p *Producer) { p.writeTimeout = d }
}

// WithMaxAttempts sets how many times the writer retries a failed write
// before giving up. Zero keeps the kafka-go default.
func WithMaxAttempts(n int) Option {
	return func(p *Producer) { p.maxAttempts = n }
}

func WithAppName(name string) Option {
	return func(p *Producer) { p.appName = name }
}

func WithLogger(l *slog.Logger) Option {
	return func(p *Producer) { p.logger = l }
}

func WithPrometheusRegisterer(r prometheus.Registerer) Option {
	return func(p *Producer) { p.registerer = r }
}

// New creates a managed Kafka producer for the given brokers. The connection
// is established in Start, not New.
func New(brokers []string, opts ...Option) *Producer {
	p := &Producer{
		brokers:    brokers,
		retry:      retry.DefaultConfig(),
		logger:     slog.Default(),
		registerer: prometheus.DefaultRegisterer,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Producer) initMetrics() {
	p.produceTotal = prom.RegisterOrReuse(p.registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kafka_produce_messages_total",
			Help: "Total number of successfully produced messages.",
		},
		[]string{"kafka_service", "topic"},
	))
	p.produceErrors = prom.RegisterOrReuse(p.registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kafka_produce_errors_total",
			Help: "Total number of failed produce attempts.",
		},
		[]string{"kafka_service", "topic"},
	))
	p.produceDuration = prom.RegisterOrReuse(p.registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kafka_produce_duration_seconds",
			Help:    "The latency of produce calls.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"kafka_service", "topic"},
	))
}

// Start connects to the brokers, retrying with exponential backoff on
// failure, then blocks until ctx is canceled.
func (p *Producer) Start(ctx context.Context) error {
	dialer, err := kafkanet.NewDialer(p.tlsCfg)
	if err != nil {
		return fmt.Errorf("kafkaproducer: dialer: %w", err)
	}
	if _, err := retry.Do(ctx, p.retry, func(attemptCtx context.Context) (struct{}, error) {
		return struct{}{}, kafkanet.Probe(attemptCtx, p.brokers, dialer)
	}); err != nil {
		return fmt.Errorf("kafkaproducer: connect: %w", err)
	}

	p.initMetrics()

	writer := &kafka.Writer{
		Addr:        kafka.TCP(p.brokers...),
		Balancer:    &kafka.Hash{},
		Compression: kafkaCompression(p.compression),
		Transport: &kafka.Transport{
			Dial: dialer.DialFunc,
			SASL: dialer.SASLMechanism,
			TLS:  dialer.TLS,
		},
	}
	if p.writeTimeout > 0 {
		writer.WriteTimeout = p.writeTimeout
	}
	if p.maxAttempts > 0 {
		writer.MaxAttempts = p.maxAttempts
	}

	p.mu.Lock()
	p.writer = writer
	p.mu.Unlock()
	p.started.Store(true)
	p.logger.Info("kafkaproducer: connected")

	<-ctx.Done()
	return nil
}

// Stop implements service.Shutdown. It closes the writer, flushing pending
// messages. Repeated calls are safe.
func (p *Producer) Stop(_ context.Context) error {
	p.mu.RLock()
	writer := p.writer
	p.mu.RUnlock()

	var errClose error
	p.closeOnce.Do(func() {
		if writer == nil {
			return
		}
		if err := writer.Close(); err != nil {
			errClose = fmt.Errorf("kafkaproducer: close writer: %w", err)
		}
	})
	return errClose
}

// Probe implements service.Prober. It dials a broker to verify connectivity.
// Returns an error if Start has not yet connected.
func (p *Producer) Probe(ctx context.Context) error {
	if !p.started.Load() {
		return errors.New("kafkaproducer: not started")
	}
	dialer, err := kafkanet.NewDialer(p.tlsCfg)
	if err != nil {
		return fmt.Errorf("kafkaproducer: dialer: %w", err)
	}
	//nolint:wrapcheck // kafkanet.Probe already reports "kafkanet: probe <broker>"
	return kafkanet.Probe(ctx, p.brokers, dialer)
}

// Produce publishes a single message to topic. It returns an error if Start
// has not yet connected.
func (p *Producer) Produce(ctx context.Context, topic string, key, value []byte, headers map[string][]byte) error {
	p.mu.RLock()
	writer := p.writer
	p.mu.RUnlock()

	if writer == nil {
		return errors.New("kafkaproducer: not started")
	}

	msg := kafka.Message{
		Topic:   topic,
		Key:     toKafkaKey(key),
		Value:   value,
		Headers: toKafkaHeaders(headers),
	}

	start := time.Now()
	err := writer.WriteMessages(ctx, msg)
	p.produceDuration.WithLabelValues(p.appName, topic).Observe(time.Since(start).Seconds())

	if err != nil {
		p.produceErrors.WithLabelValues(p.appName, topic).Inc()
		return fmt.Errorf("kafkaproducer: produce to %s: %w", topic, err)
	}

	p.produceTotal.WithLabelValues(p.appName, topic).Inc()
	return nil
}

// ProduceBatch publishes several messages in a single WriteMessages call,
// which is more efficient than repeated Produce calls. Each message carries
// its own topic. It returns an error if Start has not yet connected or any
// message has an empty topic; on a write failure the whole batch is reported
// as failed.
func (p *Producer) ProduceBatch(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}

	p.mu.RLock()
	writer := p.writer
	p.mu.RUnlock()

	if writer == nil {
		return errors.New("kafkaproducer: not started")
	}

	kmsgs := make([]kafka.Message, 0, len(msgs))
	for i, m := range msgs {
		if m.Topic == "" {
			return fmt.Errorf("kafkaproducer: message at index %d has empty topic", i)
		}
		kmsgs = append(kmsgs, kafka.Message{
			Topic:   m.Topic,
			Key:     toKafkaKey(m.Key),
			Value:   m.Value,
			Headers: toKafkaHeaders(m.Headers),
		})
	}

	start := time.Now()
	err := writer.WriteMessages(ctx, kmsgs...)
	elapsed := time.Since(start).Seconds()
	for _, m := range msgs {
		p.produceDuration.WithLabelValues(p.appName, m.Topic).Observe(elapsed)
	}

	if err != nil {
		for _, m := range msgs {
			p.produceErrors.WithLabelValues(p.appName, m.Topic).Inc()
		}
		return fmt.Errorf("kafkaproducer: produce batch of %d: %w", len(msgs), err)
	}

	for _, m := range msgs {
		p.produceTotal.WithLabelValues(p.appName, m.Topic).Inc()
	}
	return nil
}

func kafkaCompression(c Compression) kafka.Compression {
	switch c {
	case CompressionGzip:
		return kafka.Gzip
	case CompressionSnappy:
		return kafka.Snappy
	case CompressionLz4:
		return kafka.Lz4
	case CompressionZstd:
		return kafka.Zstd
	case CompressionNone:
		return 0
	default:
		return 0
	}
}

// toKafkaKey normalizes the partition key. An empty key becomes nil so the
// Hash balancer spreads such messages across partitions instead of hashing
// them all onto partition 0.
func toKafkaKey(key []byte) []byte {
	if len(key) == 0 {
		return nil
	}
	return key
}

func toKafkaHeaders(headers map[string][]byte) []kafka.Header {
	if len(headers) == 0 {
		return nil
	}
	out := make([]kafka.Header, 0, len(headers))
	for k, v := range headers {
		out = append(out, kafka.Header{Key: k, Value: v})
	}
	return out
}
