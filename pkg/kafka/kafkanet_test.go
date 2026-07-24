package kafka_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kafkanet "github.com/DjaPy/gokit-services/pkg/kafka"
)

func testBrokers(t *testing.T) []string {
	t.Helper()
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS not set; skipping test requiring a real Kafka broker")
	}
	return strings.Split(brokers, ",")
}

func TestNewDialer_WithSASL(t *testing.T) {
	dialer, err := kafkanet.NewDialer(kafkanet.TLSSASLConfig{SASLUser: "u", SASLPass: "p"})

	require.NoError(t, err)
	require.NotNil(t, dialer.SASLMechanism)
	assert.Equal(t, "PLAIN", dialer.SASLMechanism.Name())
}

func TestProbe_Succeeds(t *testing.T) {
	brokers := testBrokers(t)
	dialer, err := kafkanet.NewDialer(kafkanet.TLSSASLConfig{})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, kafkanet.Probe(ctx, brokers, dialer))
}

func TestProbe_FailsOnUnreachableBroker(t *testing.T) {
	probeTimeout := 5 * time.Second
	unreachableBroker := "localhost:1"
	dialer, err := kafkanet.NewDialer(kafkanet.TLSSASLConfig{})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	err = kafkanet.Probe(ctx, []string{unreachableBroker}, dialer)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kafkanet: probe")
}

func TestProbe_NoBrokers_ReturnsError(t *testing.T) {
	dialer, err := kafkanet.NewDialer(kafkanet.TLSSASLConfig{})
	require.NoError(t, err)

	err = kafkanet.Probe(context.Background(), nil, dialer)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no brokers configured")
}
