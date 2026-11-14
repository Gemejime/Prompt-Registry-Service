package promptsdk

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultTagFallbackTTL is the 5-minute safety fallback TTL for mutable tag pointers (ADR-002).
	DefaultTagFallbackTTL = 5 * time.Minute

	// DefaultOfflineRetryCooldown is the cooldown window before re-attempting control plane tag refresh
	// when serving stale-while-offline during a control plane outage.
	DefaultOfflineRetryCooldown = 5 * time.Second

	// DefaultInvalidationChannel is the canonical Redis Pub/Sub channel for tag pointer invalidation (ADR-002).
	DefaultInvalidationChannel = "channel:prompt:invalidation"
)

// versionCacheKey is a value-typed composite key for L1_version lookups with 0 heap allocations.
type versionCacheKey struct {
	templateID string
	version    int
}

// tagCacheKey is a value-typed composite key for L1_tag lookups with 0 heap allocations.
type tagCacheKey struct {
	templateID string
	tag        string
}

// tagCacheEntry maps a mutable tag pointer to an immutable version number with a fallback TTL.
type tagCacheEntry struct {
	templateID    string
	targetVersion int
	expiresAt     time.Time
	retryAfter    time.Time
}

// InvalidationEvent represents the JSON payload broadcast on channel:prompt:invalidation (.ai/doc-api.md Section 6).
type InvalidationEvent struct {
	EventID    string `json:"event_id"`
	TenantID   string `json:"tenant_id"`
	TemplateID string `json:"template_id"`
	Tag        string `json:"tag"`
	NewVersion int    `json:"new_version"`
	Timestamp  string `json:"timestamp"`
}

// CacheStats captures point-in-time L1 cache hit/miss and eviction counters.
type CacheStats struct {
	VersionHits     uint64
	VersionMisses   uint64
	TagHits         uint64
	TagMisses       uint64
	EvictionsTotal  uint64
	StaleServes     uint64
	VersionHitRatio float64
	TagHitRatio     float64
	OverallHitRatio float64
}

// L1Cache implements the thread-safe two-tier in-memory cache (`L1_version` with unbounded TTL
// and `L1_tag` with 5-minute safety TTL) and bidirectional template name <-> UUID alias resolution.
type L1Cache struct {
	mu            sync.RWMutex
	tenantID      string
	versions      map[versionCacheKey]*CompiledTemplate
	tags          map[tagCacheKey]tagCacheEntry
	schemas       map[string]*SchemaContract
	nameToUUID    map[string]string
	uuidToName    map[string]string
	tagTTL        time.Duration
	retryCooldown time.Duration
	nowFunc       func() time.Time

	versionHits   atomic.Uint64
	versionMisses atomic.Uint64
	tagHits       atomic.Uint64
	tagMisses     atomic.Uint64
	evictions     atomic.Uint64
	staleServes   atomic.Uint64
}

// NewL1Cache creates a new two-tier L1Cache with the given fallback tag TTL.
func NewL1Cache(tagTTL time.Duration) *L1Cache {
	if tagTTL <= 0 {
		tagTTL = DefaultTagFallbackTTL
	}
	return &L1Cache{
		versions:      make(map[versionCacheKey]*CompiledTemplate, 64),
		tags:          make(map[tagCacheKey]tagCacheEntry, 64),
		schemas:       make(map[string]*SchemaContract, 32),
		nameToUUID:    make(map[string]string, 32),
		uuidToName:    make(map[string]string, 32),
		tagTTL:        tagTTL,
		retryCooldown: DefaultOfflineRetryCooldown,
		nowFunc:       time.Now,
	}
}

// SetClock overrides the internal clock function for deterministic TTL and cooldown testing.
func (c *L1Cache) SetClock(nowFunc func() time.Time) {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	c.mu.Lock()
	c.nowFunc = nowFunc
	c.mu.Unlock()
}

// SetTenantID sets the tenant UUID for tenant boundary isolation (Layer 4).
func (c *L1Cache) SetTenantID(tenantID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.tenantID = strings.TrimSpace(tenantID)
	c.mu.Unlock()
}

// TenantID returns the configured tenant UUID.
func (c *L1Cache) TenantID() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tenantID
}

// SetOfflineRetryCooldown configures the retry cooldown applied when control-plane tag refresh fails.
func (c *L1Cache) SetOfflineRetryCooldown(cooldown time.Duration) {
	if cooldown <= 0 {
		cooldown = DefaultOfflineRetryCooldown
	}
	c.mu.Lock()
	c.retryCooldown = cooldown
	c.mu.Unlock()
}

// FormatVersionKey returns the canonical logical key string `template:{id}:v{version}` (.ai/architecture.md).
func FormatVersionKey(templateID string, version int) string {
	return "template:" + templateID + ":v" + strconv.Itoa(version)
}

// FormatTagKey returns the canonical logical key string `template:{id}:tag:{tag}` (.ai/architecture.md).
func FormatTagKey(templateID, tag string) string {
	return "template:" + templateID + ":tag:" + tag
}

// RegisterAlias records a bidirectional mapping between a template's UUID and its tenant-unique name.
func (c *L1Cache) RegisterAlias(templateUUID, templateName string) {
	templateUUID = strings.TrimSpace(templateUUID)
	templateName = strings.TrimSpace(templateName)
	if templateUUID == "" || templateName == "" || templateUUID == templateName {
		return
	}
	c.mu.Lock()
	c.nameToUUID[templateName] = templateUUID
	c.uuidToName[templateUUID] = templateName
	c.mu.Unlock()
}

// PutSchema stores an immutable SchemaContract in the L1 cache by schema ID.
func (c *L1Cache) PutSchema(schema *SchemaContract) {
	if c == nil || schema == nil || schema.ID == "" {
		return
	}
	c.mu.Lock()
	c.schemas[schema.ID] = schema
	c.mu.Unlock()
}

// GetSchema retrieves a cached SchemaContract by schema ID.
func (c *L1Cache) GetSchema(schemaID string) (*SchemaContract, bool) {
	if c == nil || schemaID == "" {
		return nil, false
	}
	c.mu.RLock()
	sc, ok := c.schemas[schemaID]
	c.mu.RUnlock()
	return sc, ok
}

// PutVersion stores an immutable pre-compiled template version in L1_version with unbounded TTL.
func (c *L1Cache) PutVersion(compiled *CompiledTemplate) {
	if c == nil || compiled == nil || compiled.Version < 1 {
		return
	}
	c.mu.Lock()
	if compiled.TemplateID != "" {
		c.versions[versionCacheKey{templateID: compiled.TemplateID, version: compiled.Version}] = compiled
	}
	if compiled.TemplateName != "" {
		c.versions[versionCacheKey{templateID: compiled.TemplateName, version: compiled.Version}] = compiled
	}
	if compiled.TemplateID != "" && compiled.TemplateName != "" && compiled.TemplateID != compiled.TemplateName {
		c.nameToUUID[compiled.TemplateName] = compiled.TemplateID
		c.uuidToName[compiled.TemplateID] = compiled.TemplateName
	}
	if compiled.Schema != nil && compiled.Schema.ID != "" {
		c.schemas[compiled.Schema.ID] = compiled.Schema
	}
	c.mu.Unlock()
}

// GetVersion looks up an immutable template version in L1_version with 0 heap allocations.
func (c *L1Cache) GetVersion(idOrName string, version int) (*CompiledTemplate, bool) {
	if c == nil || idOrName == "" || version < 1 {
		return nil, false
	}
	c.mu.RLock()
	compiled, ok := c.versions[versionCacheKey{templateID: idOrName, version: version}]
	if !ok {
		if uuid, hasUUID := c.nameToUUID[idOrName]; hasUUID {
			compiled, ok = c.versions[versionCacheKey{templateID: uuid, version: version}]
		} else if name, hasName := c.uuidToName[idOrName]; hasName {
			compiled, ok = c.versions[versionCacheKey{templateID: name, version: version}]
		}
	}
	c.mu.RUnlock()

	if ok {
		c.versionHits.Add(1)
		return compiled, true
	}
	c.versionMisses.Add(1)
	return nil, false
}

// PutTag binds a mutable semantic tag pointer (`@production`, `@staging`, `@canary`) to targetVersion
// in L1_tag with the configured 5-minute safety fallback TTL, and ensures compiled is stored in L1_version.
func (c *L1Cache) PutTag(idOrName, tag string, compiled *CompiledTemplate) {
	if c == nil || compiled == nil || tag == "" {
		return
	}
	c.mu.Lock()
	now := c.nowFunc()
	exp := now.Add(c.tagTTL)

	canonicalID := compiled.TemplateID
	if canonicalID == "" {
		canonicalID = idOrName
	}

	c.versions[versionCacheKey{templateID: canonicalID, version: compiled.Version}] = compiled
	if idOrName != "" && idOrName != canonicalID {
		c.versions[versionCacheKey{templateID: idOrName, version: compiled.Version}] = compiled
		c.nameToUUID[idOrName] = canonicalID
		c.uuidToName[canonicalID] = idOrName
	}
	if compiled.TemplateName != "" && compiled.TemplateName != canonicalID {
		c.versions[versionCacheKey{templateID: compiled.TemplateName, version: compiled.Version}] = compiled
		c.nameToUUID[compiled.TemplateName] = canonicalID
		c.uuidToName[canonicalID] = compiled.TemplateName
	}
	if compiled.Schema != nil && compiled.Schema.ID != "" {
		c.schemas[compiled.Schema.ID] = compiled.Schema
	}

	entry := tagCacheEntry{
		templateID:    canonicalID,
		targetVersion: compiled.Version,
		expiresAt:     exp,
	}
	c.tags[tagCacheKey{templateID: canonicalID, tag: tag}] = entry
	if idOrName != "" {
		c.tags[tagCacheKey{templateID: idOrName, tag: tag}] = entry
	}
	if compiled.TemplateName != "" {
		c.tags[tagCacheKey{templateID: compiled.TemplateName, tag: tag}] = entry
	}
	if aliasName, ok := c.uuidToName[canonicalID]; ok && aliasName != "" {
		c.tags[tagCacheKey{templateID: aliasName, tag: tag}] = entry
	}
	c.mu.Unlock()
}

// GetTag resolves a semantic tag pointer against L1_tag and L1_version with 0 heap allocations.
// Returns:
//   - compiled: the resolved *CompiledTemplate (non-nil on both fresh hits and stale-while-offline hits)
//   - needsRefresh: true when the 5-minute fallback TTL has elapsed and the retry cooldown has passed
//   - ok: true if an entry exists in L1
func (c *L1Cache) GetTag(idOrName, tag string) (compiled *CompiledTemplate, needsRefresh bool, ok bool) {
	if c == nil || idOrName == "" || tag == "" {
		return nil, false, false
	}

	c.mu.RLock()
	entry, found := c.tags[tagCacheKey{templateID: idOrName, tag: tag}]
	if !found {
		if uuid, hasUUID := c.nameToUUID[idOrName]; hasUUID {
			entry, found = c.tags[tagCacheKey{templateID: uuid, tag: tag}]
		} else if name, hasName := c.uuidToName[idOrName]; hasName {
			entry, found = c.tags[tagCacheKey{templateID: name, tag: tag}]
		}
	}
	if !found {
		c.mu.RUnlock()
		c.tagMisses.Add(1)
		return nil, false, false
	}

	verCompiled, verFound := c.versions[versionCacheKey{templateID: entry.templateID, version: entry.targetVersion}]
	if !verFound {
		verCompiled, verFound = c.versions[versionCacheKey{templateID: idOrName, version: entry.targetVersion}]
	}
	if !verFound {
		c.mu.RUnlock()
		c.tagMisses.Add(1)
		return nil, false, false
	}

	now := c.nowFunc()
	expired := !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt)
	inCooldown := !entry.retryAfter.IsZero() && now.Before(entry.retryAfter)
	c.mu.RUnlock()

	if !expired {
		c.tagHits.Add(1)
		c.versionHits.Add(1)
		return verCompiled, false, true
	}

	if inCooldown {
		// Control plane was recently unreachable; serve last-known-good version from memory immediately.
		c.tagHits.Add(1)
		c.versionHits.Add(1)
		c.staleServes.Add(1)
		return verCompiled, false, true
	}

	// TTL expired and not in cooldown: caller should attempt control plane refresh and fall back to verCompiled on failure.
	return verCompiled, true, true
}

// MarkTagRefreshFailed records a control-plane refresh failure on an expired tag entry so subsequent
// renders within the retry cooldown window serve the cached version immediately without network stalls.
func (c *L1Cache) MarkTagRefreshFailed(idOrName, tag string) {
	if c == nil || idOrName == "" || tag == "" {
		return
	}
	c.mu.Lock()
	now := c.nowFunc()
	cooldownUntil := now.Add(c.retryCooldown)

	keys := c.collectTagKeysLocked(idOrName, tag)
	for _, k := range keys {
		if entry, exists := c.tags[k]; exists {
			entry.retryAfter = cooldownUntil
			c.tags[k] = entry
		}
	}
	c.mu.Unlock()
	c.tagHits.Add(1)
	c.versionHits.Add(1)
	c.staleServes.Add(1)
}

// collectTagKeysLocked returns all alias tagCacheKeys for (idOrName, tag) while c.mu is held.
func (c *L1Cache) collectTagKeysLocked(idOrName, tag string) []tagCacheKey {
	keys := make([]tagCacheKey, 0, 3)
	keys = append(keys, tagCacheKey{templateID: idOrName, tag: tag})
	if uuid, ok := c.nameToUUID[idOrName]; ok && uuid != "" {
		keys = append(keys, tagCacheKey{templateID: uuid, tag: tag})
		if name, ok2 := c.uuidToName[uuid]; ok2 && name != "" && name != idOrName {
			keys = append(keys, tagCacheKey{templateID: name, tag: tag})
		}
	}
	if name, ok := c.uuidToName[idOrName]; ok && name != "" {
		keys = append(keys, tagCacheKey{templateID: name, tag: tag})
	}
	return keys
}

// Invalidate immediately evicts the mutable tag pointer (and any name <-> UUID alias) from L1_tag
// while preserving immutable versions in L1_version.
func (c *L1Cache) Invalidate(templateID, tag string) {
	if c == nil {
		return
	}
	templateID = strings.TrimSpace(templateID)
	tag = strings.TrimSpace(tag)
	if templateID == "" {
		return
	}

	c.mu.Lock()
	if tag == "" || tag == "*" {
		ids := map[string]struct{}{templateID: {}}
		if uuid, ok := c.nameToUUID[templateID]; ok {
			ids[uuid] = struct{}{}
		}
		if name, ok := c.uuidToName[templateID]; ok {
			ids[name] = struct{}{}
		}
		for k := range c.tags {
			if _, match := ids[k.templateID]; match {
				delete(c.tags, k)
			}
		}
	} else {
		for _, k := range c.collectTagKeysLocked(templateID, tag) {
			delete(c.tags, k)
		}
	}
	c.mu.Unlock()
	c.evictions.Add(1)
}

// PurgeTags wipes all mutable tag entries from L1_tag while preserving immutable template versions in L1_version (Runbook 2).
func (c *L1Cache) PurgeTags() {
	if c == nil {
		return
	}
	c.mu.Lock()
	for k := range c.tags {
		delete(c.tags, k)
	}
	c.mu.Unlock()
	c.evictions.Add(1)
}

// HandleInvalidationPayload parses a Redis Pub/Sub InvalidationEvent JSON payload and evicts the matching tag.
// If the cache has a configured TenantID and evt.TenantID != "", events targeting different tenants are safely ignored
// without evicting local cache keys (Tenant Boundary Isolation, Layer 4).
func (c *L1Cache) HandleInvalidationPayload(rawJSON string) (InvalidationEvent, error) {
	var evt InvalidationEvent
	if err := json.Unmarshal([]byte(rawJSON), &evt); err != nil {
		return InvalidationEvent{}, err
	}
	c.mu.RLock()
	configuredTenant := c.tenantID
	c.mu.RUnlock()
	if configuredTenant != "" && evt.TenantID != "" && configuredTenant != evt.TenantID {
		// Event targets a different tenant; safely ignore without evicting local entries.
		return evt, nil
	}
	if strings.TrimSpace(evt.TemplateID) != "" {
		c.Invalidate(evt.TemplateID, evt.Tag)
	}
	return evt, nil
}

// HitRatio returns the cache hit ratio [0.0, 1.0] for the specified tier ("L1_version", "L1_tag", or "all").
func (c *L1Cache) HitRatio(tier string) float64 {
	if c == nil {
		return 1.0
	}
	switch tier {
	case "L1_version":
		hits := c.versionHits.Load()
		misses := c.versionMisses.Load()
		total := hits + misses
		if total == 0 {
			return 1.0
		}
		return float64(hits) / float64(total)
	case "L1_tag":
		hits := c.tagHits.Load()
		misses := c.tagMisses.Load()
		total := hits + misses
		if total == 0 {
			return 1.0
		}
		return float64(hits) / float64(total)
	default:
		hits := c.versionHits.Load() + c.tagHits.Load()
		misses := c.versionMisses.Load() + c.tagMisses.Load()
		total := hits + misses
		if total == 0 {
			return 1.0
		}
		return float64(hits) / float64(total)
	}
}

// Stats returns a point-in-time snapshot of L1 cache hit/miss and eviction metrics.
func (c *L1Cache) Stats() CacheStats {
	vHits := c.versionHits.Load()
	vMisses := c.versionMisses.Load()
	tHits := c.tagHits.Load()
	tMisses := c.tagMisses.Load()

	return CacheStats{
		VersionHits:     vHits,
		VersionMisses:   vMisses,
		TagHits:         tHits,
		TagMisses:       tMisses,
		EvictionsTotal:  c.evictions.Load(),
		StaleServes:     c.staleServes.Load(),
		VersionHitRatio: c.HitRatio("L1_version"),
		TagHitRatio:     c.HitRatio("L1_tag"),
		OverallHitRatio: c.HitRatio("all"),
	}
}
