// Package redisx wraps a go-redis client for voicx. Redis backs later-phase
// features (pub/sub fan-out, rate limiting) and is treated as optional: a
// startup Ping failure leaves the client retained in degraded mode so later
// readiness probes can observe recovery without restarting the server.
package redisx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Client wraps a *redis.Client with the voicx logger.
type Client struct {
	rdb    *redis.Client
	logger *zap.Logger
}

const (
	defaultDialTimeout  = 5 * time.Second
	defaultReadTimeout  = 3 * time.Second
	defaultWriteTimeout = 3 * time.Second
)

// Options configures an optional Redis client. TLS defaults to disabled;
// callers must opt in explicitly and every enabled TLS connection verifies
// the server certificate.
type Options struct {
	Addr          string
	Password      string
	DialTimeout   time.Duration
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	TLSEnabled    bool
	TLSServerName string
	TLSCAFile     string
}

// New constructs a Client without contacting Redis. Invalid TLS material is
// returned as a startup error; callers may still treat a later Ping failure as
// optional-service degradation.
func New(options Options, logger *zap.Logger) (*Client, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if options.DialTimeout <= 0 {
		options.DialTimeout = defaultDialTimeout
	}
	if options.ReadTimeout <= 0 {
		options.ReadTimeout = defaultReadTimeout
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = defaultWriteTimeout
	}
	tlsConfig, err := buildTLSConfig(options)
	if err != nil {
		return nil, err
	}
	return &Client{
		rdb: redis.NewClient(&redis.Options{
			Addr:                  options.Addr,
			Password:              options.Password,
			DialTimeout:           options.DialTimeout,
			ReadTimeout:           options.ReadTimeout,
			WriteTimeout:          options.WriteTimeout,
			ContextTimeoutEnabled: true,
			TLSConfig:             tlsConfig,
		}),
		logger: logger,
	}, nil
}

func buildTLSConfig(options Options) (*tls.Config, error) {
	serverName := strings.TrimSpace(options.TLSServerName)
	caFile := strings.TrimSpace(options.TLSCAFile)
	if !options.TLSEnabled {
		if serverName != "" || caFile != "" {
			return nil, errors.New("redis TLS server name and CA file require TLS to be enabled")
		}
		return nil, nil
	}
	if serverName == "" {
		host, _, err := net.SplitHostPort(options.Addr)
		if err != nil {
			return nil, fmt.Errorf("deriving Redis TLS server name from address %q: %w", options.Addr, err)
		}
		if host == "" {
			return nil, errors.New("redis TLS address has no host; set redis_tls_server_name explicitly")
		}
		serverName = host
	}
	config := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
	}
	if caFile == "" {
		return config, nil
	}
	// #nosec G304 -- TLSCAFile is administrator-selected configuration.
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading Redis TLS CA file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("parsing Redis TLS CA file %q: no certificates found", caFile)
	}
	config.RootCAs = roots
	return config, nil
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
