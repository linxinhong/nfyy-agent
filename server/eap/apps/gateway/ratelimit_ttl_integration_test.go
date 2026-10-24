//go:build integration

// TASK-040: rate-limit counter keys must expire with their window so Redis
// memory stays bounded.
package main_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	gateway "enterprise-agent-platform/apps/gateway"
)

func TestRateLimitKeysExpire(t *testing.T) {
	addr := os.Getenv("EAP_TEST_REDIS_ADDR")
	if addr == "" {
		t.Fatal("EAP_TEST_REDIS_ADDR must point at Redis")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()

	counter := gateway.RedisCounter{Client: rdb, TTL: time.Minute}
	key := "ratelimit:test-ttl:" + time.Now().Format("150405.000000000")
	if _, err := counter.Incr(ctx, key); err != nil {
		t.Fatalf("incr: %v", err)
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("counter key must carry a window-sized TTL, got %v", ttl)
	}
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
