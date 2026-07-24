package producer

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

	"github.com/DjaPy/gokit-services/pkg/internal/retry"
)

func testBrokers(t *testing.T) []string {
	t.Helper()
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS not set; skipping test requiring a real Kafka broker")
	}
	return strings.Split(brokers, ",")
}

// createTopic provisions the topic up front: kafka.Writer does not request
// auto-creation, so producing to an unknown topic would fail. It waits until
// the broker reports the topic's partitions, since CreateTopics returns before
// the topic is visible in metadata.
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

// uniqueName keeps concurrent test runs from sharing topics.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// readOne consumes through a raw kafka.Reader: the producer's tests must not
// depend on the kafkaconsumer package.
func readOne(t *testing.T, brokers []string, topic string) kafka.Message {
	t.Helper()
	readTimeout := 45 * time.Second

	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:   brokers,
		Topic:     topic,
		Partition: 0,
		MinBytes:  1,
		MaxBytes:  10e6,
	})
	defer r.Close()

	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()

	msg, err := r.ReadMessage(ctx)
	require.NoError(t, err)
	return msg
}

func startedProducer(t *testing.T, brokers []string) *Producer {
	t.Helper()
	startTimeout := 15 * time.Second
	pollInterval := 50 * time.Millisecond

	p := New(brokers, WithPrometheusRegisterer(prometheus.NewRegistry()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Start(ctx) //nolint:errcheck // readiness is asserted via Probe below

	require.Eventually(t, func() bool {
		return p.Probe(context.Background()) == nil
	}, startTimeout, pollInterval)

	return p
}

func TestProducer_ProduceBeforeStart_ReturnsError(t *testing.T) {
	unreachableBroker := "localhost:1"

	p := New([]string{unreachableBroker},
		WithPrometheusRegisterer(prometheus.NewRegistry()))

	err := p.Produce(context.Background(), "topic", nil, []byte("v"), nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

func TestProducer_ProbeBeforeStart_ReturnsError(t *testing.T) {
	unreachableBroker := "localhost:1"

	p := New([]string{unreachableBroker},
		WithPrometheusRegisterer(prometheus.NewRegistry()))

	err := p.Probe(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not started")
}

func TestProducer_StartRetriesOnInitialFailure(t *testing.T) {
	unreachableBroker := "localhost:1"
	maxAttempts := 3
	startTimeout := 10 * time.Second

	p := New([]string{unreachableBroker},
		WithPrometheusRegisterer(prometheus.NewRegistry()),
		WithRetry(retry.Config{
			MaxAttempts: maxAttempts,
			BackoffMin:  10 * time.Millisecond,
			BackoffMax:  50 * time.Millisecond,
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	err := p.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kafkaproducer: connect")
}

func TestProducer_SendsToCorrectTopic(t *testing.T) {
	produceTimeout := 30 * time.Second
	brokers := testBrokers(t)
	topicA := uniqueName(t, "topic-a")
	topicB := uniqueName(t, "topic-b")
	valueA := []byte("value-a")
	valueB := []byte("value-b")

	createTopic(t, brokers, topicA)
	createTopic(t, brokers, topicB)

	p := startedProducer(t, brokers)

	ctx, cancel := context.WithTimeout(context.Background(), produceTimeout)
	defer cancel()

	require.NoError(t, p.Produce(ctx, topicA, []byte("key-a"), valueA, map[string][]byte{"h": []byte("1")}))
	require.NoError(t, p.Produce(ctx, topicB, []byte("key-b"), valueB, nil))

	assert.Equal(t, valueA, readOne(t, brokers, topicA).Value)
	assert.Equal(t, valueB, readOne(t, brokers, topicB).Value)
}

func TestProducer_StopIdempotent(t *testing.T) {
	brokers := testBrokers(t)

	p := startedProducer(t, brokers)

	require.NoError(t, p.Stop(context.Background()))
	require.NoError(t, p.Stop(context.Background()))
	require.NoError(t, p.Stop(context.Background()))
}
