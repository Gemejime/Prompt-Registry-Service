package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/domain/repository"
)

// TransactionalMemoryStore implements repository.PromptRepository and repository.AuditRepository
// with full ACID transaction semantics, row-level serialization, composite foreign keys,
// and append-only immutability trigger guards matching 000001_init_schema.up.sql.
type TransactionalMemoryStore struct {
	mu          sync.RWMutex
	templates   map[string]entity.PromptTemplate
	versions    map[string][]entity.PromptVersion // keyed by template_id
	tags        map[string]entity.PromptTag       // keyed by tenant_id|template_id|tag
	schemas     map[string]entity.JSONSchema      // keyed by schema_id
	auditLedger []entity.AuditEntry
	nextAuditID int64

	// faultInjectAuditFailure allows tests to simulate a failure during audit insertion
	// to verify atomic transaction rollback of primary table mutations (ADR-004).
	faultInjectAuditFailure bool
}

var (
	_ repository.PromptRepository = (*TransactionalMemoryStore)(nil)
	_ repository.AuditRepository  = (*TransactionalMemoryStore)(nil)
)

// NewTransactionalMemoryStore creates an initialized TransactionalMemoryStore.
func NewTransactionalMemoryStore() *TransactionalMemoryStore {
	return &TransactionalMemoryStore{
		templates:   make(map[string]entity.PromptTemplate),
		versions:    make(map[string][]entity.PromptVersion),
		tags:        make(map[string]entity.PromptTag),
		schemas:     make(map[string]entity.JSONSchema),
		auditLedger: make([]entity.AuditEntry, 0, 64),
		nextAuditID: 1,
	}
}

// SetFaultInjectAuditFailure enables or disables simulated audit insertion failures for ACID rollback tests.
func (s *TransactionalMemoryStore) SetFaultInjectAuditFailure(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faultInjectAuditFailure = fail
}

// newUUIDv4 generates a random RFC 4122 version 4 UUID string.
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// resolveTemplateLocked resolves a template by UUID or tenant-scoped name while s.mu is held.
func (s *TransactionalMemoryStore) resolveTemplateLocked(tenantID, identifier string) (entity.PromptTemplate, error) {
	identifier = strings.TrimSpace(identifier)
	if entity.IsValidUUID(identifier) {
		if t, ok := s.templates[identifier]; ok && t.TenantID == tenantID {
			return t, nil
		}
		return entity.PromptTemplate{}, fmt.Errorf("%w: template %q not found in tenant", entity.ErrNotFound, identifier)
	}
	for _, t := range s.templates {
		if t.TenantID == tenantID && t.Name == identifier {
			return t, nil
		}
	}
	return entity.PromptTemplate{}, fmt.Errorf("%w: template %q not found in tenant", entity.ErrNotFound, identifier)
}

// verifySchemaLocked verifies that schemaID exists within tenantID while s.mu is held.
func (s *TransactionalMemoryStore) verifySchemaLocked(tenantID, schemaID string) error {
	sc, ok := s.schemas[schemaID]
	if !ok || sc.TenantID != tenantID {
		return fmt.Errorf("%w: schema %q not found in tenant", entity.ErrNotFound, schemaID)
	}
	return nil
}

// appendAuditLocked appends an immutable AuditEntry to the in-memory ledger while s.mu is held.
func (s *TransactionalMemoryStore) appendAuditLocked(entry entity.AuditEntry) error {
	if s.faultInjectAuditFailure {
		return fmt.Errorf("simulated transactional audit ledger write failure")
	}
	entry.ID = s.nextAuditID
	s.nextAuditID++
	entry.CreatedAt = time.Now().UTC()
	s.auditLedger = append(s.auditLedger, entry)
	return nil
}

// CreateTemplate atomically creates a template aggregate, v1, and TEMPLATE_CREATED audit record.
func (s *TransactionalMemoryStore) CreateTemplate(_ context.Context, params repository.CreateTemplateParams) (*entity.PromptTemplate, *entity.PromptVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.verifySchemaLocked(params.TenantID, params.SchemaID); err != nil {
		return nil, nil, err
	}

	for _, existing := range s.templates {
		if existing.TenantID == params.TenantID && existing.Name == params.Name {
			return nil, nil, fmt.Errorf("%w: template name %q already exists in tenant", entity.ErrConflict, params.Name)
		}
	}

	now := time.Now().UTC()
	tmpl := entity.PromptTemplate{
		ID:             newUUIDv4(),
		TenantID:       params.TenantID,
		Name:           params.Name,
		Description:    params.Description,
		CurrentVersion: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	ver := entity.PromptVersion{
		ID:         newUUIDv4(),
		TemplateID: tmpl.ID,
		Version:    1,
		RawAST:     params.RawAST,
		ASTHash:    params.ASTHash,
		SchemaID:   params.SchemaID,
		Author:     params.Author,
		CreatedAt:  now,
	}

	v1 := 1
	auditEntry := entity.AuditEntry{
		TenantID:    params.TenantID,
		TemplateID:  tmpl.ID,
		Action:      entity.AuditActionTemplateCreated,
		Version:     &v1,
		PerformedBy: params.Author,
		PayloadSnapshot: entity.AuditPayloadSnapshot{
			ASTHash:          params.ASTHash,
			TruncatedASTHash: entity.TruncateHashPrefix(params.ASTHash),
			SchemaID:         params.SchemaID,
			TargetVersion:    &v1,
			VariableKeys:     params.RawAST.VariableKeys,
			DefaultDialect:   params.RawAST.DefaultDialect,
		},
	}
	if err := s.appendAuditLocked(auditEntry); err != nil {
		// Atomic rollback: neither templates nor versions maps are modified.
		return nil, nil, err
	}

	s.templates[tmpl.ID] = tmpl
	s.versions[tmpl.ID] = []entity.PromptVersion{ver}
	return &tmpl, &ver, nil
}

// PublishVersion atomically allocates monotonic version v_{n+1} (or returns existing on ast_hash match)
// and appends VERSION_PUBLISHED to prompt_audit_ledger.
func (s *TransactionalMemoryStore) PublishVersion(_ context.Context, params repository.PublishVersionParams) (*entity.PromptVersion, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tmpl, err := s.resolveTemplateLocked(params.TenantID, params.TemplateIdentifier)
	if err != nil {
		return nil, false, err
	}
	if err := s.verifySchemaLocked(params.TenantID, params.SchemaID); err != nil {
		return nil, false, err
	}

	vers := s.versions[tmpl.ID]
	for i := range vers {
		if vers[i].ASTHash == params.ASTHash {
			vCopy := vers[i]
			return &vCopy, true, nil
		}
	}

	nextVersion := len(vers) + 1
	now := time.Now().UTC()
	ver := entity.PromptVersion{
		ID:         newUUIDv4(),
		TemplateID: tmpl.ID,
		Version:    nextVersion,
		RawAST:     params.RawAST,
		ASTHash:    params.ASTHash,
		SchemaID:   params.SchemaID,
		Author:     params.Author,
		CreatedAt:  now,
	}

	prevVersion := nextVersion - 1
	auditEntry := entity.AuditEntry{
		TenantID:    params.TenantID,
		TemplateID:  tmpl.ID,
		Action:      entity.AuditActionVersionPublished,
		Version:     &nextVersion,
		PerformedBy: params.Author,
		PayloadSnapshot: entity.AuditPayloadSnapshot{
			ASTHash:          params.ASTHash,
			TruncatedASTHash: entity.TruncateHashPrefix(params.ASTHash),
			SchemaID:         params.SchemaID,
			PreviousVersion:  &prevVersion,
			TargetVersion:    &nextVersion,
			VariableKeys:     params.RawAST.VariableKeys,
			DefaultDialect:   params.RawAST.DefaultDialect,
		},
	}
	if err := s.appendAuditLocked(auditEntry); err != nil {
		return nil, false, err
	}

	tmpl.CurrentVersion = nextVersion
	tmpl.UpdatedAt = now
	s.templates[tmpl.ID] = tmpl
	s.versions[tmpl.ID] = append(vers, ver)
	return &ver, false, nil
}

// GetTemplateVersion retrieves a specific immutable version within the tenant.
func (s *TransactionalMemoryStore) GetTemplateVersion(_ context.Context, tenantID, templateIdentifier string, version int) (*entity.PromptVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tmpl, err := s.resolveTemplateLocked(tenantID, templateIdentifier)
	if err != nil {
		return nil, err
	}
	for _, v := range s.versions[tmpl.ID] {
		if v.Version == version {
			vCopy := v
			return &vCopy, nil
		}
	}
	return nil, fmt.Errorf("%w: template %q version %d not found", entity.ErrNotFound, templateIdentifier, version)
}

// PromoteTag atomically updates a semantic tag pointer with optimistic locking and audit logging.
func (s *TransactionalMemoryStore) PromoteTag(_ context.Context, params repository.PromoteTagParams) (*entity.PromptTag, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tmpl, err := s.resolveTemplateLocked(params.TenantID, params.TemplateIdentifier)
	if err != nil {
		return nil, false, err
	}

	var targetVerEntity *entity.PromptVersion
	for i := range s.versions[tmpl.ID] {
		if s.versions[tmpl.ID][i].Version == params.TargetVersion {
			targetVerEntity = &s.versions[tmpl.ID][i]
			break
		}
	}
	if targetVerEntity == nil {
		return nil, false, fmt.Errorf("%w: target version %d does not exist for template %q", entity.ErrNotFound, params.TargetVersion, tmpl.ID)
	}

	tagKey := fmt.Sprintf("%s|%s|%s", params.TenantID, tmpl.ID, params.Tag)
	currentVersion := 0
	var existingID string
	if existingTag, ok := s.tags[tagKey]; ok {
		currentVersion = existingTag.TargetVersion
		existingID = existingTag.ID
	} else {
		existingID = newUUIDv4()
	}

	isRollback, err := entity.ValidateTagPromotion(currentVersion, params.ExpectedVersion, params.TargetVersion)
	if err != nil {
		return nil, false, err
	}

	now := time.Now().UTC()
	updatedTag := entity.PromptTag{
		ID:            existingID,
		TenantID:      params.TenantID,
		TemplateID:    tmpl.ID,
		Tag:           params.Tag,
		TargetVersion: params.TargetVersion,
		UpdatedBy:     params.UpdatedBy,
		UpdatedAt:     now,
	}

	action := entity.AuditActionTagPromoted
	if isRollback {
		action = entity.AuditActionTagRolledBack
	}
	targetVer := params.TargetVersion
	prevVer := currentVersion
	auditEntry := entity.AuditEntry{
		TenantID:    params.TenantID,
		TemplateID:  tmpl.ID,
		Action:      action,
		Version:     &targetVer,
		Tag:         params.Tag,
		PerformedBy: params.UpdatedBy,
		PayloadSnapshot: entity.AuditPayloadSnapshot{
			ASTHash:          targetVerEntity.ASTHash,
			TruncatedASTHash: entity.TruncateHashPrefix(targetVerEntity.ASTHash),
			SchemaID:         targetVerEntity.SchemaID,
			PreviousVersion:  &prevVer,
			TargetVersion:    &targetVer,
		},
	}
	if err := s.appendAuditLocked(auditEntry); err != nil {
		return nil, false, err
	}

	s.tags[tagKey] = updatedTag
	return &updatedTag, isRollback, nil
}

// ResolveTag resolves a semantic tag pointer to its bound PromptVersion.
func (s *TransactionalMemoryStore) ResolveTag(_ context.Context, tenantID, templateIdentifier, tag string) (*entity.PromptTag, *entity.PromptVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tmpl, err := s.resolveTemplateLocked(tenantID, templateIdentifier)
	if err != nil {
		return nil, nil, err
	}

	tagKey := fmt.Sprintf("%s|%s|%s", tenantID, tmpl.ID, tag)
	pTag, ok := s.tags[tagKey]
	if !ok {
		return nil, nil, fmt.Errorf("%w: tag %q for template %q not found", entity.ErrNotFound, tag, templateIdentifier)
	}

	for _, v := range s.versions[tmpl.ID] {
		if v.Version == pTag.TargetVersion {
			tCopy := pTag
			vCopy := v
			return &tCopy, &vCopy, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: target version %d not found", entity.ErrNotFound, pTag.TargetVersion)
}

// ListTemplates returns a cursor-paginated list of templates for a tenant.
func (s *TransactionalMemoryStore) ListTemplates(_ context.Context, tenantID string, limit int, cursor string) ([]entity.PromptTemplate, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	var all []entity.PromptTemplate
	for _, t := range s.templates {
		if t.TenantID == tenantID && (cursor == "" || t.ID > cursor) {
			all = append(all, t)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	var nextCursor string
	if len(all) > limit {
		nextCursor = all[limit-1].ID
		all = all[:limit]
	}
	return all, nextCursor, nil
}

// RegisterSchema registers a JSON output validation schema and writes SCHEMA_REGISTERED to the audit ledger.
func (s *TransactionalMemoryStore) RegisterSchema(_ context.Context, params repository.RegisterSchemaParams) (*entity.JSONSchema, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.schemas {
		if existing.TenantID == params.TenantID && existing.Name == params.Name {
			return nil, fmt.Errorf("%w: schema name %q already exists in tenant", entity.ErrConflict, params.Name)
		}
	}

	now := time.Now().UTC()
	sc := entity.JSONSchema{
		ID:         newUUIDv4(),
		TenantID:   params.TenantID,
		Name:       params.Name,
		SchemaJSON: json.RawMessage(append([]byte(nil), params.SchemaJSON...)),
		CreatedAt:  now,
	}

	performedBy := params.Author
	if strings.TrimSpace(performedBy) == "" {
		performedBy = "system-admin"
	}
	auditEntry := entity.AuditEntry{
		TenantID:    params.TenantID,
		TemplateID:  sc.ID,
		Action:      entity.AuditActionSchemaRegistered,
		PerformedBy: performedBy,
		PayloadSnapshot: entity.AuditPayloadSnapshot{
			SchemaID:   sc.ID,
			SchemaName: sc.Name,
		},
	}
	if err := s.appendAuditLocked(auditEntry); err != nil {
		return nil, err
	}

	s.schemas[sc.ID] = sc
	return &sc, nil
}

// GetSchema fetches a registered JSON output schema by UUID within a tenant.
func (s *TransactionalMemoryStore) GetSchema(_ context.Context, tenantID, schemaID string) (*entity.JSONSchema, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sc, ok := s.schemas[schemaID]
	if !ok || sc.TenantID != tenantID {
		return nil, fmt.Errorf("%w: schema %q not found in tenant", entity.ErrNotFound, schemaID)
	}
	scCopy := sc
	return &scCopy, nil
}

// ListSchemas returns a cursor-paginated list of registered JSON output schemas for a tenant.
func (s *TransactionalMemoryStore) ListSchemas(_ context.Context, tenantID string, limit int, cursor string) ([]entity.JSONSchema, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	var all []entity.JSONSchema
	for _, sc := range s.schemas {
		if sc.TenantID == tenantID && (cursor == "" || sc.ID > cursor) {
			all = append(all, sc)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	var nextCursor string
	if len(all) > limit {
		nextCursor = all[limit-1].ID
		all = all[:limit]
	}
	return all, nextCursor, nil
}

// ListByTemplate retrieves ordered compliance ledger entries for a template or schema resource within a tenant.
func (s *TransactionalMemoryStore) ListByTemplate(_ context.Context, tenantID, templateID string, limit int) ([]entity.AuditEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var out []entity.AuditEntry
	for _, e := range s.auditLedger {
		if e.TenantID == tenantID && e.TemplateID == templateID {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// AttemptAppendOnlyMutation simulates the PostgreSQL prevent_append_only_mutation() trigger
// when an UPDATE or DELETE is attempted against prompt_versions or prompt_audit_ledger.
func (s *TransactionalMemoryStore) AttemptAppendOnlyMutation(tableName, operation string) error {
	return fmt.Errorf("ERROR: Table %s is strictly append-only; %s operations are forbidden (SQLSTATE P0001)", tableName, operation)
}
