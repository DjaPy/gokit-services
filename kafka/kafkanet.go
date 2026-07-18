package kafka

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
)

const dialTimeout = 10 * time.Second

// TLSSASLConfig configures transport security for a Kafka connection.
// The zero value means plaintext without authentication.
type TLSSASLConfig struct {
	TLS      *tls.Config   // nil = plaintext
	SASLUser string        // empty = no SASL
	SASLPass string        //
	SASLMech SASLMechanism // empty = PLAIN when SASLUser is set
}

// NewDialer builds a *kafka.Dialer with TLS and SASL applied from cfg. It
// returns an error only if the SASL mechanism is unknown or cannot be built.
func NewDialer(cfg TLSSASLConfig) (*kafka.Dialer, error) {
	dialer := &kafka.Dialer{
		Timeout:   dialTimeout,
		DualStack: true,
	}
	if cfg.TLS != nil {
		dialer.TLS = cfg.TLS
	}
	if cfg.SASLUser != "" {
		mech, err := saslMechanism(cfg)
		if err != nil {
			return nil, err
		}
		dialer.SASLMechanism = mech
	}
	return dialer, nil
}

func saslMechanism(cfg TLSSASLConfig) (sasl.Mechanism, error) {
	switch cfg.SASLMech {
	case "", SASLPlain:
		return plain.Mechanism{Username: cfg.SASLUser, Password: cfg.SASLPass}, nil
	case SASLScramSHA256:
		mech, err := scram.Mechanism(scram.SHA256, cfg.SASLUser, cfg.SASLPass)
		if err != nil {
			return nil, fmt.Errorf("kafkanet: scram-sha-256: %w", err)
		}
		return mech, nil
	case SASLScramSHA512:
		mech, err := scram.Mechanism(scram.SHA512, cfg.SASLUser, cfg.SASLPass)
		if err != nil {
			return nil, fmt.Errorf("kafkanet: scram-sha-512: %w", err)
		}
		return mech, nil
	default:
		return nil, fmt.Errorf("kafkanet: unknown SASL mechanism %q", cfg.SASLMech)
	}
}

// Probe checks broker connectivity by dialing the first broker and closing
// the connection immediately. It is a real connectivity check, not an
// assertion that a client was constructed without error.
func Probe(ctx context.Context, brokers []string, dialer *kafka.Dialer) error {
	if len(brokers) == 0 {
		return errors.New("kafkanet: no brokers configured")
	}

	conn, err := dialer.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("kafkanet: probe %s: %w", brokers[0], err)
	}
	defer conn.Close()

	return nil
}
