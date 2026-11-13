package main

import (
	"context"
	"testing"

	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
)

// TestServerReadinessCheckRedis verifies that empty Redis configuration fails readiness
// unless ALLOW_IN_MEMORY_STORE local-only mode is explicitly active.
func TestServerReadinessCheckRedis(t *testing.T) {
	ctx := context.Background()
	emptyClient := redis.NewClusterClient(redis.ClientOptions{})

	strict := &serverReadiness{
		redisClient:     emptyClient,
		redisConfigured: false,
		allowInMemory:   false,
	}
	if err := strict.CheckRedis(ctx); err == nil {
		t.Fatalf("expected CheckRedis to fail when Redis addresses are empty and allowInMemory is false")
	}

	localOnly := &serverReadiness{
		redisClient:     emptyClient,
		redisConfigured: false,
		allowInMemory:   true,
	}
	if err := localOnly.CheckRedis(ctx); err != nil {
		t.Fatalf("expected CheckRedis to allow empty Redis only in explicit allowInMemory mode, got: %v", err)
	}

	uninit := &serverReadiness{
		redisClient:     nil,
		redisConfigured: true,
		allowInMemory:   false,
	}
	if err := uninit.CheckRedis(ctx); err == nil {
		t.Fatalf("expected CheckRedis to fail when redisClient is nil")
	}
}
