// Package kafka provides the transport primitives (dialer, connectivity
// probe, TLS/SASL config) shared by its producer and consumer subpackages,
// so neither depends on the other — a producer-only service must not pull in
// the consumer and its worker pool, and vice versa.
package kafka

// SASLMechanism selects the SASL authentication mechanism. Managed Kafka
// (Confluent, MSK, Yandex) typically requires SCRAM rather than PLAIN.
type SASLMechanism string

const (
	SASLPlain       SASLMechanism = "PLAIN"
	SASLScramSHA256 SASLMechanism = "SCRAM-SHA-256"
	SASLScramSHA512 SASLMechanism = "SCRAM-SHA-512"
)
