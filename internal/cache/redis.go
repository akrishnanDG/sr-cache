package cache

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// Options bundles the connection knobs. Username + TLS support let customers
// use Redis ACLs and encrypted connections without code changes.
type Options struct {
	Addr          string
	Username      string // optional — for Redis 6+ ACLs
	Password      string
	DB            int
	PoolSize      int
	TLS           bool
	TLSCAFile     string // optional — path to a CA bundle to verify the Redis cert
	TLSSkipVerify bool   // dev only
}

// Entry is what we persist for a cached upstream response.
type Entry struct {
	Status      int    `json:"s"`
	ContentType string `json:"ct"`
	Body        []byte `json:"b"`
}

type Cache struct {
	rdb *redis.Client
}

// NewWithOptions constructs a Cache, honoring TLS + ACL settings.
func NewWithOptions(o Options) (*Cache, error) {
	opts := &redis.Options{
		Addr:     o.Addr,
		Username: o.Username,
		Password: o.Password,
		DB:       o.DB,
		PoolSize: o.PoolSize,
	}
	if o.TLS {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if o.TLSCAFile != "" {
			pem, err := os.ReadFile(o.TLSCAFile)
			if err != nil {
				return nil, fmt.Errorf("read REDIS_TLS_CA_FILE: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("REDIS_TLS_CA_FILE %q contained no PEM certs", o.TLSCAFile)
			}
			tc.RootCAs = pool
		}
		if o.TLSSkipVerify {
			tc.InsecureSkipVerify = true
		}
		opts.TLSConfig = tc
	}
	return &Cache{rdb: redis.NewClient(opts)}, nil
}

func (c *Cache) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *Cache) Close() error { return c.rdb.Close() }

func (c *Cache) Get(ctx context.Context, key string) (*Entry, bool, error) {
	b, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, false, err
	}
	return &e, true, nil
}

// Set stores an entry. ttl == 0 means no expiration.
func (c *Cache) Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, key, b, ttl).Err()
}

func (c *Cache) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.rdb.Del(ctx, keys...).Err()
}

// DelByPattern deletes keys matching the glob (uses SCAN; safe on large dbs).
func (c *Cache) DelByPattern(ctx context.Context, pattern string) error {
	iter := c.rdb.Scan(ctx, 0, pattern, 200).Iterator()
	var batch []string
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) >= 200 {
			if err := c.rdb.Del(ctx, batch...).Err(); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if err := iter.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		return c.rdb.Del(ctx, batch...).Err()
	}
	return nil
}
