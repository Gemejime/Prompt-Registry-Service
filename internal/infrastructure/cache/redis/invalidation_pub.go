package redis

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID generates a monotonically sortable 26-character Crockford Base32 ULID string.
func NewULID() string {
	ms := uint64(time.Now().UTC().UnixMilli())
	var rnd [10]byte
	_, _ = rand.Read(rnd[:])

	var out [26]byte
	out[0] = crockfordAlphabet[(ms>>45)&0x1F]
	out[1] = crockfordAlphabet[(ms>>40)&0x1F]
	out[2] = crockfordAlphabet[(ms>>35)&0x1F]
	out[3] = crockfordAlphabet[(ms>>30)&0x1F]
	out[4] = crockfordAlphabet[(ms>>25)&0x1F]
	out[5] = crockfordAlphabet[(ms>>20)&0x1F]
	out[6] = crockfordAlphabet[(ms>>15)&0x1F]
	out[7] = crockfordAlphabet[(ms>>10)&0x1F]
	out[8] = crockfordAlphabet[(ms>>5)&0x1F]
	out[9] = crockfordAlphabet[ms&0x1F]

	var bitBuf uint64
	var bits uint
	idx := 10
	for _, b := range rnd {
		bitBuf = (bitBuf << 8) | uint64(b)
		bits += 8
		for bits >= 5 && idx < 26 {
			bits -= 5
			out[idx] = crockfordAlphabet[(bitBuf>>bits)&0x1F]
			idx++
		}
	}
	return string(out[:])
}

// InvalidationEvent matches the Redis Pub/Sub payload contract in .ai/doc-api.md Section 6.
type InvalidationEvent struct {
	EventID    string `json:"event_id"`
	TenantID   string `json:"tenant_id"`
	TemplateID string `json:"template_id"`
	Tag        string `json:"tag"`
	NewVersion int    `json:"new_version"`
	Timestamp  string `json:"timestamp"`
}

// InvalidationPublisher defines the interface for broadcasting tag pointer mutations over Redis Pub/Sub (ADR-002).
type InvalidationPublisher interface {
	PublishTagInvalidation(ctx context.Context, tenantID, templateID, tag string, newVersion int) (InvalidationEvent, error)
}

// PubSubInvalidationBroadcaster publishes eviction signals to channel:prompt:invalidation
// and tracks publication metrics.
type PubSubInvalidationBroadcaster struct {
	client       *L2CacheClient
	channel      string
	successCount atomic.Uint64
	errorCount   atomic.Uint64
	forceFail    atomic.Bool
}

var _ InvalidationPublisher = (*PubSubInvalidationBroadcaster)(nil)

// NewInvalidationBroadcaster creates a new PubSubInvalidationBroadcaster.
func NewInvalidationBroadcaster(client *L2CacheClient, channel string) *PubSubInvalidationBroadcaster {
	if channel == "" {
		channel = DefaultInvalidationChannel
	}
	return &PubSubInvalidationBroadcaster{
		client:  client,
		channel: channel,
	}
}

// SetForceFail enables or disables simulated publish failures for alert/error path testing.
func (b *PubSubInvalidationBroadcaster) SetForceFail(fail bool) {
	b.forceFail.Store(fail)
}

// Metrics returns the total published success and error counts for Prometheus exposition.
func (b *PubSubInvalidationBroadcaster) Metrics() (success uint64, errCount uint64) {
	if b == nil {
		return 0, 0
	}
	return b.successCount.Load(), b.errorCount.Load()
}

// PublishTagInvalidation constructs a canonical InvalidationEvent and broadcasts it to Redis Pub/Sub.
func (b *PubSubInvalidationBroadcaster) PublishTagInvalidation(ctx context.Context, tenantID, templateID, tag string, newVersion int) (InvalidationEvent, error) {
	evt := InvalidationEvent{
		EventID:    NewULID(),
		TenantID:   tenantID,
		TemplateID: templateID,
		Tag:        tag,
		NewVersion: newVersion,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}

	if b.forceFail.Load() {
		b.errorCount.Add(1)
		return evt, fmt.Errorf("simulated redis pub/sub publish failure")
	}

	payloadBytes, err := json.Marshal(evt)
	if err != nil {
		b.errorCount.Add(1)
		return evt, fmt.Errorf("failed to marshal invalidation event: %w", err)
	}
	payloadStr := string(payloadBytes)

	var remoteErr error
	if b.client != nil {
		if len(b.client.addrs) > 0 {
			if err := b.client.publishRemote(ctx, b.channel, payloadStr); err != nil {
				remoteErr = err
			}
		}

		b.client.mu.RLock()
		subs := append([]chan string(nil), b.client.subs[b.channel]...)
		b.client.mu.RUnlock()
		for _, ch := range subs {
			select {
			case ch <- payloadStr:
			default:
			}
		}
	}

	if remoteErr != nil {
		b.errorCount.Add(1)
		return evt, fmt.Errorf("failed to publish invalidation event to redis: %w", remoteErr)
	}

	b.successCount.Add(1)
	return evt, nil
}
