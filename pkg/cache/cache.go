// Package cache is a thin, failure-tolerant wrapper over Redis.
//
// Redis is never the source of truth here - MySQL is. Every method is safe on
// a nil or disabled *Cache and on a Redis outage: callers get a "miss" / error
// and fall back to their normal path, so Redis being down makes the app
// slower, never broken. After any Redis error the client stops calling Redis
// for a short cool-off, so an outage costs one timeout, not one per request.
package cache

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrUnavailable is returned when Redis is disabled or cooling off after an
// error. Callers treat it the same as any other Redis error: fall back.
var ErrUnavailable = errors.New("cache unavailable")

const coolOff = 10 * time.Second

type Cache struct {
	rdb       *redis.Client
	downUntil atomic.Int64 // unix nanos; Redis is skipped until then
}

// New connects to redisURL (redis://:password@host:port/db). An empty URL
// disables caching entirely; a bad URL or an unreachable server is logged and
// the app carries on without Redis.
func New(redisURL string) *Cache {
	if redisURL == "" {
		log.Println("cache: REDIS_URL not set - running without Redis")
		return &Cache{}
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Println("cache: invalid REDIS_URL - running without Redis:", err)
		return &Cache{}
	}
	// Tight timeouts: a slow Redis must not hold up a request for long.
	opts.DialTimeout = 500 * time.Millisecond
	opts.ReadTimeout = 300 * time.Millisecond
	opts.WriteTimeout = 300 * time.Millisecond
	opts.MaxRetries = 0
	// go-redis retries a failed dial 5 times with backoff by default, which
	// stalled the first request after an outage by ~1.8s. One attempt only.
	opts.DialerRetries = 1

	c := &Cache{rdb: redis.NewClient(opts)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		log.Println("cache: Redis not reachable at startup (will keep retrying):", err)
		c.markDown()
	} else {
		log.Println("cache: connected to Redis")
	}
	return c
}

// Enabled reports whether a Redis client is configured (it may still be
// temporarily unreachable).
func (c *Cache) Enabled() bool { return c != nil && c.rdb != nil }

func (c *Cache) available() bool {
	return c.Enabled() && time.Now().UnixNano() >= c.downUntil.Load()
}

func (c *Cache) markDown() {
	c.downUntil.Store(time.Now().Add(coolOff).UnixNano())
}

// check records a Redis failure (redis.Nil is a normal miss, not a failure).
func (c *Cache) check(err error) error {
	if err != nil && !errors.Is(err, redis.Nil) {
		if c.available() {
			log.Println("cache: Redis error, bypassing for", coolOff, "-", err)
		}
		c.markDown()
	}
	return err
}

// Get returns the value and whether it was found. A Redis error is returned
// as err (with found=false) so the caller can fall back.
func (c *Cache) Get(ctx context.Context, key string) (string, bool, error) {
	if !c.available() {
		return "", false, ErrUnavailable
	}
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if c.check(err) != nil {
		return "", false, err
	}
	return v, true, nil
}

func (c *Cache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if !c.available() {
		return ErrUnavailable
	}
	return c.check(c.rdb.Set(ctx, key, value, ttl).Err())
}

func (c *Cache) Del(ctx context.Context, keys ...string) error {
	if !c.available() {
		return ErrUnavailable
	}
	return c.check(c.rdb.Del(ctx, keys...).Err())
}

// IncrWindow increments key and, on the first increment, starts its expiry
// window - a fixed-window counter (e.g. failed logins per IP).
func (c *Cache) IncrWindow(ctx context.Context, key string, window time.Duration) (int64, error) {
	if !c.available() {
		return 0, ErrUnavailable
	}
	pipe := c.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, window)
	if _, err := pipe.Exec(ctx); c.check(err) != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// ShouldRun takes a best-effort lock for a periodic job so only one app
// instance runs it per interval. Without Redis it always returns true - the
// jobs it guards are safe to run more than once, the lock only avoids waste.
func (c *Cache) ShouldRun(ctx context.Context, job string, ttl time.Duration) bool {
	if !c.available() {
		return true
	}
	ok, err := c.rdb.SetNX(ctx, "lock:"+job, "1", ttl).Result()
	if c.check(err) != nil {
		return true
	}
	return ok
}

func (c *Cache) Close() {
	if c.Enabled() {
		_ = c.rdb.Close()
	}
}
