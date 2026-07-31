// Package redisx wraps a go-redis client for voicx. Redis backs later-phase
// features (pub/sub fan-out, rate limiting) and is treated as optional: the
// caller pings at startup and, on failure, logs a warning and continues
// without Redis rather than failing the server.
package redisx

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Client wraps a *redis.Client with the voicx logger.
type Client struct {
	rdb    *redis.Client
	logger *zap.Logger
}

// New constructs a Client for the given address and password. It does not
// connect; call Ping to verify connectivity.
func New(addr, password string, logger *zap.Logger) *Client {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Client{
		rdb: redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: password,
		}),
		logger: logger,
	}
}

// Ping verifies connectivity to the Redis server.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}

// Raw exposes the underlying *redis.Client for later phases (pub/sub, rate
// limiting).
func (c *Client) Raw() *redis.Client {
	return c.rdb
}

// Close releases the client connection pool.
func (c *Client) Close() error {
	return c.rdb.Close()
}
