package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisProvider yields the currently-connected Redis client, or nil before
// redisservice has finished its retrying Start. *redisservice.Service
// satisfies it via its Client() accessor — mirroring how PostgresStore reads
// its pool through poolProvider, so CachingStore need not import redisservice
// and tolerates the client being established asynchronously after Start.
type redisProvider interface {
	Client() *redis.Client
}

// CachingStore decorates a Store with a Redis read-through cache for single
// Get lookups. A Redis outage never breaks a read: every cache path falls
// back to the inner Store. Cache consistency is best-effort — mutations
// invalidate the affected keys and the TTL is the backstop. The embedded
// Store provides Create/List unchanged; only Get and the invalidating
// mutations are overridden.
type CachingStore struct {
	Store
	provider redisProvider
	ttl      time.Duration
	logger   *slog.Logger
}

// NewCachingStore wraps inner with a Redis read-through cache drawn from
// provider, expiring entries after ttl.
func NewCachingStore(inner Store, provider redisProvider, ttl time.Duration) *CachingStore {
	return &CachingStore{Store: inner, provider: provider, ttl: ttl, logger: slog.Default()}
}

func cacheKey(id string) string { return "order:" + id }

// client returns the live Redis client, or nil while redisservice is still
// connecting — in which case the cache layer is skipped and calls fall
// through to the inner Store.
func (s *CachingStore) client() *redis.Client { return s.provider.Client() }

func (s *CachingStore) Get(ctx context.Context, id string) (Order, error) {
	c := s.client()
	if c == nil {
		return s.getFromStore(ctx, id)
	}

	key := cacheKey(id)
	if cached, err := c.Get(ctx, key).Bytes(); err == nil {
		var o Order
		if json.Unmarshal(cached, &o) == nil {
			return o, nil
		}
		// Corrupt cache entry: fall through to the store and refresh below.
	} else if !errors.Is(err, redis.Nil) {
		s.logger.Warn("orders: cache get", "order_id", id, "error", err)
	}

	o, err := s.getFromStore(ctx, id)
	if err != nil {
		return Order{}, err
	}
	if payload, mErr := json.Marshal(o); mErr == nil {
		if sErr := c.Set(ctx, key, payload, s.ttl).Err(); sErr != nil {
			s.logger.Warn("orders: cache set", "order_id", id, "error", sErr)
		}
	}
	return o, nil
}

// getFromStore reads through to the embedded Store. Its error (including
// ErrOrderNotFound, which is not cached) is wrapped once here.
func (s *CachingStore) getFromStore(ctx context.Context, id string) (Order, error) {
	o, err := s.Store.Get(ctx, id)
	if err != nil {
		return Order{}, fmt.Errorf("caching store: get: %w", err)
	}
	return o, nil
}

func (s *CachingStore) ConfirmPending(ctx context.Context, id string) error {
	if err := s.Store.ConfirmPending(ctx, id); err != nil {
		return fmt.Errorf("caching store: confirm: %w", err)
	}
	s.invalidate(ctx, id)
	return nil
}

func (s *CachingStore) CancelStalePending(ctx context.Context, cutoff time.Time) ([]string, error) {
	ids, err := s.Store.CancelStalePending(ctx, cutoff)
	if err != nil {
		return nil, fmt.Errorf("caching store: cancel stale: %w", err)
	}
	s.invalidate(ctx, ids...)
	return ids, nil
}

// invalidate drops cached entries for the given order IDs. Errors are logged,
// not returned — the TTL guarantees eventual consistency regardless.
func (s *CachingStore) invalidate(ctx context.Context, ids ...string) {
	c := s.client()
	if c == nil || len(ids) == 0 {
		return
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = cacheKey(id)
	}
	if err := c.Del(ctx, keys...).Err(); err != nil {
		s.logger.Warn("orders: cache invalidate", "error", err)
	}
}
