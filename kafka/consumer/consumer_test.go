package consumer

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/internal/retry"
)

func testBrokers(t *testing.T) []string {
	t.Helper()
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS not set; skipping test requiring a real Kafka broker")
	}
	return strings.Split(brokers, ",")
}

// uniqueName keeps concurrent test runs from sharing topics or consumer groups.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// createTopic provisions the topic up front and waits until the broker
// reports its partitions: CreateTopics returns before the topic is visible in
// metadata, and a writer that races it sees UnknownTopicOrPartition.
func createTopic(t *testing.T, brokers []string, topic string) {
	t.Helper()
	topicReadyTimeout := 15 * time.Second
	pollInterval := 100 * time.Millisecond

	conn, err := kafka.Dial("tcp", brokers[0])
	require.NoError(t, err)
	defer conn.Close()

	controller, err := conn.Controller()
	require.NoError(t, err)

	ctrlConn, err := kafka.Dial("tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	require.NoError(t, err)
	defer ctrlConn.Close()

	require.NoError(t, ctrlConn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	}))

	require.Eventually(t, func() bool {
		partitions, errRead := conn.ReadPartitions(topic)
		return errRead == nil && len(partitions) > 0
	}, topicReadyTimeout, pollInterval)
}

// writeMessage produces through a raw kafka.Writer: the consumer's tests must
// not depend on the kafkaproducer package.
func writeMessage(t *testing.T, brokers []string, topic string, key, value []byte) {
	t.Helper()
	writeTimeout := 30 * time.Second

	createTopic(t, brokers, topic)

	w := &kafka.Writer{
		Addr:      kafka.TCP(brokers...),
		Topic:     topic,
		Balancer:  &kafka.Hash{},
		Transport: &kafka.Transport{},
	}
	defer w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	require.NoError(t, w.WriteMessages(ctx, kafka.Message{Key: key, Value: value}))
}

func TestConsumer_StartWithoutHandle_ReturnsError(t *testing.T) {
	unreachableBroker := "localhost:1"

	c := New([]string{unreachableBroker}, uniqueName(t, "group"),
		WithPrometheusRegisterer(prometheus.NewRegistry()))

	err := c.Start(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no topics registered")
}

func TestConsumer_ProbeBeforeStart_ReturnsError(t *testing.T) {
	unreachableBroker := "localhost:1"

	c := New([]string{unreachableBroker}, uniqueName(t, "group"),
		WithPrometheusRegisterer(prometheus.NewRegistry()))

	err := c.Probe(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

func TestConsumer_StartRetriesOnInitialFailure(t *testing.T) {
	unreachableBroker := "localhost:1"
	maxAttempts := 3
	startTimeout := 10 * time.Second

	c := New([]string{unreachableBroker}, uniqueName(t, "group"),
		WithPrometheusRegisterer(prometheus.NewRegistry()),
		WithRetry(retry.Config{
			MaxAttempts: maxAttempts,
			BackoffMin:  10 * time.Millisecond,
			BackoffMax:  50 * time.Millisecond,
		}),
	)
	c.Handle(uniqueName(t, "topic"), func(context.Context, Message) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	err := c.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kafkaconsumer: connect")
}

func TestConsumer_HandlerReceivesProducedMessage(t *testing.T) {
	consumeTimeout := 45 * time.Second
	brokers := testBrokers(t)
	topic := uniqueName(t, "topic")
	key := []byte("order-1")
	value := []byte(`{"id":"ord_1"}`)

	writeMessage(t, brokers, topic, key, value)

	received := make(chan Message, 1)
	c := New(brokers, uniqueName(t, "group"),
		WithPrometheusRegisterer(prometheus.NewRegistry()))
	c.Handle(topic, func(_ context.Context, msg Message) error {
		received <- msg
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx) //nolint:errcheck // failure surfaces as a missing message below

	select {
	case msg := <-received:
		assert.Equal(t, topic, msg.Topic)
		assert.Equal(t, key, msg.Key)
		assert.Equal(t, value, msg.Value)
	case <-time.After(consumeTimeout):
		t.Fatal("handler did not receive the produced message")
	}
}

func TestConsumer_HandlerErrorDoesNotCommit(t *testing.T) {
	consumeTimeout := 45 * time.Second
	brokers := testBrokers(t)
	topic := uniqueName(t, "topic")
	group := uniqueName(t, "group")
	value := []byte("redeliver-me")

	writeMessage(t, brokers, topic, nil, value)

	// A handler that always fails must not commit the offset, so a second
	// consumer in the same group starts from the same uncommitted position.
	failed := make(chan struct{}, 1)
	first := New(brokers, group,
		WithPrometheusRegisterer(prometheus.NewRegistry()))
	first.Handle(topic, func(context.Context, Message) error {
		select {
		case failed <- struct{}{}:
		default:
		}
		return assert.AnError
	})

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	go first.Start(firstCtx) //nolint:errcheck

	select {
	case <-failed:
	case <-time.After(consumeTimeout):
		cancelFirst()
		t.Fatal("failing handler was never invoked")
	}
	require.NoError(t, first.Stop(context.Background(), nil))
	cancelFirst()

	redelivered := make(chan Message, 1)
	second := New(brokers, group,
		WithPrometheusRegisterer(prometheus.NewRegistry()))
	second.Handle(topic, func(_ context.Context, msg Message) error {
		redelivered <- msg
		return nil
	})

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	go second.Start(secondCtx) //nolint:errcheck

	select {
	case msg := <-redelivered:
		assert.Equal(t, value, msg.Value)
	case <-time.After(consumeTimeout):
		t.Fatal("uncommitted message was not redelivered to a new consumer in the same group")
	}
}

func TestConsumer_StopIdempotent(t *testing.T) {
	startTimeout := 10 * time.Second
	pollInterval := 50 * time.Millisecond
	brokers := testBrokers(t)
	topic := uniqueName(t, "topic")

	createTopic(t, brokers, topic)

	c := New(brokers, uniqueName(t, "group"),
		WithPrometheusRegisterer(prometheus.NewRegistry()))
	c.Handle(topic, func(context.Context, Message) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return c.Probe(context.Background()) == nil
	}, startTimeout, pollInterval)

	require.NoError(t, c.Stop(context.Background(), nil))
	require.NoError(t, c.Stop(context.Background(), nil))
	require.NoError(t, c.Stop(context.Background(), nil))
}
