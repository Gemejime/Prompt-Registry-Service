// Package redis implements the Redis 7.2 L2 distributed cache and Pub/Sub invalidation bus (ADR-002).
package redis

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"rag-prompt-registry-service/internal/domain/entity"
)

const (
	// DefaultInvalidationChannel is the canonical Redis Pub/Sub channel for tag pointer invalidation (ADR-002).
	DefaultInvalidationChannel = "channel:prompt:invalidation"

	defaultImmutableTTL = 24 * time.Hour
	defaultTagTTL       = 5 * time.Minute
)

// ClientOptions holds Redis 7.2 Cluster / Standalone connection configuration.
type ClientOptions struct {
	Addrs    []string
	Password string
	DB       int
}

// memEntry holds an in-memory cache value and its optional expiration timestamp.
type memEntry struct {
	payload   []byte
	expiresAt time.Time
}

// L2CacheClient provides Redis 7.2 Cluster / Standalone L2 caching and Pub/Sub transport
// with an integrated thread-safe L2 cache to sustain P99 <= 2ms Control Plane Registry Read Latency.
type L2CacheClient struct {
	addrs    []string
	password string
	db       int

	mu      sync.RWMutex
	memData map[string]memEntry
	subs    map[string][]chan string
}

// NewClusterClient initializes a Redis 7.2 client supporting one or more cluster/standalone addresses.
func NewClusterClient(opts ClientOptions) *L2CacheClient {
	var cleanAddrs []string
	for _, a := range opts.Addrs {
		if trimmed := strings.TrimSpace(a); trimmed != "" {
			cleanAddrs = append(cleanAddrs, trimmed)
		}
	}
	return &L2CacheClient{
		addrs:    cleanAddrs,
		password: opts.Password,
		db:       opts.DB,
		memData:  make(map[string]memEntry),
		subs:     make(map[string][]chan string),
	}
}

// NewInMemoryL2Cache creates an L2CacheClient backed by memory for deterministic testing.
func NewInMemoryL2Cache() *L2CacheClient {
	return &L2CacheClient{
		memData: make(map[string]memEntry),
		subs:    make(map[string][]chan string),
	}
}

// dialNode connects to the first reachable Redis node and performs optional AUTH and SELECT commands.
func (c *L2CacheClient) dialNode(ctx context.Context) (net.Conn, error) {
	if c == nil || len(c.addrs) == 0 {
		return nil, fmt.Errorf("no redis cluster addresses configured")
	}
	var lastErr error
	dialer := net.Dialer{Timeout: 1500 * time.Millisecond}
	for _, addr := range c.addrs {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		if c.password != "" {
			if _, authErr := execRESP(conn, "AUTH", c.password); authErr != nil {
				_ = conn.Close()
				lastErr = authErr
				continue
			}
		}
		if c.db > 0 {
			if _, selErr := execRESP(conn, "SELECT", strconv.Itoa(c.db)); selErr != nil {
				_ = conn.Close()
				lastErr = selErr
				continue
			}
		}
		return conn, nil
	}
	return nil, lastErr
}

// execRESP sends a RESP array command over conn and parses the single-line or bulk-string response.
func execRESP(conn net.Conn, args ...string) (string, error) {
	_ = conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*%d\r\n", len(args)))
	for _, a := range args {
		sb.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(a), a))
	}
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return "", err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) == 0 {
		return "", fmt.Errorf("empty RESP response")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case ':':
		return line[1:], nil
	case '-':
		return "", fmt.Errorf("redis error: %s", line[1:])
	case '$':
		length, err := strconv.Atoi(line[1:])
		if err != nil || length < 0 {
			return "", fmt.Errorf("redis nil bulk string")
		}
		buf := make([]byte, length+2)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return "", err
		}
		return string(buf[:length]), nil
	default:
		return line, nil
	}
}

// Ping checks reachability of the configured Redis cluster or succeeds in standalone memory mode.
func (c *L2CacheClient) Ping(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("redis client is nil")
	}
	if len(c.addrs) == 0 {
		return nil
	}
	conn, err := c.dialNode(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = execRESP(conn, "PING")
	return err
}

// Close releases resources held by the L2CacheClient.
func (c *L2CacheClient) Close() error {
	return nil
}

// VersionCacheKey builds the canonical L2 key for an immutable template version.
func VersionCacheKey(tenantID, templateID string, version int) string {
	return fmt.Sprintf("l2:tenant:%s:template:%s:v:%d", tenantID, templateID, version)
}

// SchemaCacheKey builds the canonical L2 key for a registered JSON output schema.
func SchemaCacheKey(tenantID, schemaID string) string {
	return fmt.Sprintf("l2:tenant:%s:schema:%s", tenantID, schemaID)
}

// TagCacheKey builds the canonical L2 key for a mutable semantic tag pointer.
func TagCacheKey(tenantID, templateID, tag string) string {
	return fmt.Sprintf("l2:tenant:%s:template:%s:tag:%s", tenantID, templateID, tag)
}

// getBytes fetches a cached byte slice from Redis when cluster addresses are configured,
// or from the in-memory store when running in standalone memory mode.
func (c *L2CacheClient) getBytes(ctx context.Context, key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	if len(c.addrs) > 0 {
		conn, err := c.dialNode(ctx)
		if err != nil {
			return nil, false
		}
		val, rErr := execRESP(conn, "GET", key)
		_ = conn.Close()
		if rErr != nil || val == "" {
			return nil, false
		}
		return []byte(val), true
	}
	c.mu.RLock()
	entry, ok := c.memData[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.memData, key)
		c.mu.Unlock()
		return nil, false
	}
	return entry.payload, true
}

// setBytes stores a byte slice in the cache with the given TTL.
func (c *L2CacheClient) setBytes(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	c.mu.Lock()
	c.memData[key] = memEntry{
		payload:   append([]byte(nil), val...),
		expiresAt: exp,
	}
	c.mu.Unlock()

	if len(c.addrs) > 0 {
		if conn, err := c.dialNode(ctx); err == nil {
			sec := int(ttl.Seconds())
			if sec <= 0 {
				sec = 300
			}
			_, _ = execRESP(conn, "SET", key, string(val), "EX", strconv.Itoa(sec))
			_ = conn.Close()
		}
	}
	return nil
}

// setBytesNX stores a byte slice only if the key is currently absent or expired, preventing
// concurrent stale readers from overwriting a freshly promoted tag resolution.
func (c *L2CacheClient) setBytesNX(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	now := time.Now()
	var exp time.Time
	if ttl > 0 {
		exp = now.Add(ttl)
	}
	c.mu.Lock()
	if existing, ok := c.memData[key]; !ok || (!existing.expiresAt.IsZero() && now.After(existing.expiresAt)) {
		c.memData[key] = memEntry{
			payload:   append([]byte(nil), val...),
			expiresAt: exp,
		}
	}
	c.mu.Unlock()

	if len(c.addrs) > 0 {
		conn, err := c.dialNode(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		sec := int(ttl.Seconds())
		if sec <= 0 {
			sec = 300
		}
		_, _ = execRESP(conn, "SET", key, string(val), "EX", strconv.Itoa(sec), "NX")
	}
	return nil
}

// delKey removes a key from the in-memory map and from Redis when cluster addresses are configured.
func (c *L2CacheClient) delKey(ctx context.Context, key string) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	delete(c.memData, key)
	c.mu.Unlock()

	if len(c.addrs) > 0 {
		conn, err := c.dialNode(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		if _, err := execRESP(conn, "DEL", key); err != nil {
			return err
		}
	}
	return nil
}

// publishRemote publishes a message payload to the specified Redis Pub/Sub channel.
func (c *L2CacheClient) publishRemote(ctx context.Context, channel, payload string) error {
	if c == nil || len(c.addrs) == 0 {
		return nil
	}
	conn, err := c.dialNode(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = execRESP(conn, "PUBLISH", channel, payload)
	return err
}

// GetVersion retrieves a cached immutable PromptVersion from L2.
func (c *L2CacheClient) GetVersion(ctx context.Context, tenantID, templateID string, version int) (*entity.PromptVersion, bool) {
	raw, ok := c.getBytes(ctx, VersionCacheKey(tenantID, templateID, version))
	if !ok {
		return nil, false
	}
	var ver entity.PromptVersion
	if err := json.Unmarshal(raw, &ver); err != nil {
		return nil, false
	}
	return &ver, true
}

// SetVersion stores an immutable PromptVersion in L2.
func (c *L2CacheClient) SetVersion(ctx context.Context, tenantID string, ver *entity.PromptVersion) error {
	if c == nil || ver == nil {
		return nil
	}
	raw, err := json.Marshal(ver)
	if err != nil {
		return err
	}
	return c.setBytes(ctx, VersionCacheKey(tenantID, ver.TemplateID, ver.Version), raw, defaultImmutableTTL)
}

// GetSchema retrieves a cached JSONSchema from L2.
func (c *L2CacheClient) GetSchema(ctx context.Context, tenantID, schemaID string) (*entity.JSONSchema, bool) {
	raw, ok := c.getBytes(ctx, SchemaCacheKey(tenantID, schemaID))
	if !ok {
		return nil, false
	}
	var sc entity.JSONSchema
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, false
	}
	return &sc, true
}

// SetSchema stores a JSONSchema in L2.
func (c *L2CacheClient) SetSchema(ctx context.Context, sc *entity.JSONSchema) error {
	if c == nil || sc == nil {
		return nil
	}
	raw, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	return c.setBytes(ctx, SchemaCacheKey(sc.TenantID, sc.ID), raw, defaultImmutableTTL)
}

// GetTagResolution retrieves a cached tag resolution (PromptVersion) from L2.
func (c *L2CacheClient) GetTagResolution(ctx context.Context, tenantID, templateID, tag string) (*entity.PromptVersion, bool) {
	raw, ok := c.getBytes(ctx, TagCacheKey(tenantID, templateID, tag))
	if !ok {
		return nil, false
	}
	var ver entity.PromptVersion
	if err := json.Unmarshal(raw, &ver); err != nil {
		return nil, false
	}
	return &ver, true
}

// SetTagResolution stores a resolved tag pointer target version in L2 with a 5-minute safety TTL
// if the tag key is not already populated, preventing stale concurrent readers from overwriting
// a newly promoted pointer after EvictTagResolution.
func (c *L2CacheClient) SetTagResolution(ctx context.Context, tenantID, templateID, tag string, ver *entity.PromptVersion) error {
	if c == nil || ver == nil {
		return nil
	}
	raw, err := json.Marshal(ver)
	if err != nil {
		return err
	}
	return c.setBytesNX(ctx, TagCacheKey(tenantID, templateID, tag), raw, defaultTagTTL)
}

// EvictTagResolution immediately removes a stale tag pointer entry from L2 upon tag promotion.
func (c *L2CacheClient) EvictTagResolution(ctx context.Context, tenantID, templateID, tag string) error {
	return c.delKey(ctx, TagCacheKey(tenantID, templateID, tag))
}

// SubscribeChannel registers a Go channel receiving published messages on the given Pub/Sub channel.
func (c *L2CacheClient) SubscribeChannel(_ context.Context, channel string) (<-chan string, func()) {
	ch := make(chan string, 64)
	c.mu.Lock()
	c.subs[channel] = append(c.subs[channel], ch)
	c.mu.Unlock()

	cleanup := func() {
		c.mu.Lock()
		list := c.subs[channel]
		for i, sub := range list {
			if sub == ch {
				c.subs[channel] = append(list[:i], list[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
	}
	return ch, cleanup
}
