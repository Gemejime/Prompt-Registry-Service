package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/domain/repository"
)

// PostgresPromptRepository implements repository.PromptRepository backed by PostgreSQL 16+ via pgx/v5.
type PostgresPromptRepository struct {
	pool *pgxpool.Pool
}

var _ repository.PromptRepository = (*PostgresPromptRepository)(nil)

// NewPromptRepository creates a new PostgresPromptRepository.
func NewPromptRepository(pool *pgxpool.Pool) *PostgresPromptRepository {
	return &PostgresPromptRepository{pool: pool}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// resolveTemplateIDTx resolves a template identifier (either canonical UUID or tenant-unique name)
// within the caller's tenant boundary. When forUpdate is true, acquires a row-level lock on prompt_templates.
func resolveTemplateIDTx(ctx context.Context, tx pgx.Tx, tenantID, identifier string, forUpdate bool) (string, error) {
	identifier = strings.TrimSpace(identifier)
	var query string
	if entity.IsValidUUID(identifier) {
		query = `SELECT id::text FROM prompt_templates WHERE tenant_id = $1 AND id = $2`
	} else {
		query = `SELECT id::text FROM prompt_templates WHERE tenant_id = $1 AND name = $2`
	}
	if forUpdate {
		query += ` FOR UPDATE`
	}

	var templateID string
	if err := tx.QueryRow(ctx, query, tenantID, identifier).Scan(&templateID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: template %q not found in tenant", entity.ErrNotFound, identifier)
		}
		return "", fmt.Errorf("failed to resolve template %q: %w", identifier, err)
	}
	return templateID, nil
}

// verifySchemaExistsTx ensures the referenced schema_id exists and belongs to the same tenant.
func verifySchemaExistsTx(ctx context.Context, tx pgx.Tx, tenantID, schemaID string) error {
	const q = `SELECT id::text FROM json_schemas WHERE tenant_id = $1 AND id = $2`
	var foundID string
	if err := tx.QueryRow(ctx, q, tenantID, schemaID).Scan(&foundID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: schema %q not found in tenant", entity.ErrNotFound, schemaID)
		}
		return fmt.Errorf("failed to verify schema %q: %w", schemaID, err)
	}
	return nil
}

// CreateTemplate initializes a template aggregate root, inserts initial version v1,
// and commits a TEMPLATE_CREATED entry to prompt_audit_ledger in one ACID transaction.
func (r *PostgresPromptRepository) CreateTemplate(ctx context.Context, params repository.CreateTemplateParams) (*entity.PromptTemplate, *entity.PromptVersion, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := verifySchemaExistsTx(ctx, tx, params.TenantID, params.SchemaID); err != nil {
		return nil, nil, err
	}

	const insertTemplateSQL = `
		INSERT INTO prompt_templates (tenant_id, name, description)
		VALUES ($1, $2, $3)
		RETURNING id::text, tenant_id::text, name, COALESCE(description, ''), created_at, updated_at
	`
	var tmpl entity.PromptTemplate
	if err := tx.QueryRow(ctx, insertTemplateSQL, params.TenantID, params.Name, params.Description).Scan(
		&tmpl.ID,
		&tmpl.TenantID,
		&tmpl.Name,
		&tmpl.Description,
		&tmpl.CreatedAt,
		&tmpl.UpdatedAt,
	); err != nil {
		if isUniqueViolation(err) {
			return nil, nil, fmt.Errorf("%w: template name %q already exists in tenant", entity.ErrConflict, params.Name)
		}
		return nil, nil, fmt.Errorf("failed to insert prompt_templates: %w", err)
	}
	tmpl.CurrentVersion = 1

	astBytes, err := json.Marshal(params.RawAST)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal raw_ast: %w", err)
	}

	const insertVersionSQL = `
		INSERT INTO prompt_versions (template_id, version, raw_ast, ast_hash, schema_id, author)
		VALUES ($1, 1, $2, $3, $4, $5)
		RETURNING id::text, template_id::text, version, schema_id::text, author, created_at
	`
	var ver entity.PromptVersion
	if err := tx.QueryRow(ctx, insertVersionSQL, tmpl.ID, astBytes, params.ASTHash, params.SchemaID, params.Author).Scan(
		&ver.ID,
		&ver.TemplateID,
		&ver.Version,
		&ver.SchemaID,
		&ver.Author,
		&ver.CreatedAt,
	); err != nil {
		return nil, nil, fmt.Errorf("failed to insert initial prompt_versions v1: %w", err)
	}
	ver.RawAST = params.RawAST
	ver.ASTHash = params.ASTHash

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
	if err := insertAuditEntryTx(ctx, tx, auditEntry); err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("failed to commit CreateTemplate transaction: %w", err)
	}
	return &tmpl, &ver, nil
}

// PublishVersion locks the template aggregate root (SELECT ... FOR UPDATE), checks for an idempotent
// ast_hash match, allocates monotonic version v_{n+1}, and commits VERSION_PUBLISHED to prompt_audit_ledger.
func (r *PostgresPromptRepository) PublishVersion(ctx context.Context, params repository.PublishVersionParams) (*entity.PromptVersion, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	templateID, err := resolveTemplateIDTx(ctx, tx, params.TenantID, params.TemplateIdentifier, true)
	if err != nil {
		return nil, false, err
	}

	if err := verifySchemaExistsTx(ctx, tx, params.TenantID, params.SchemaID); err != nil {
		return nil, false, err
	}

	// Idempotency check: if (template_id, ast_hash) already exists, return the existing version without duplication.
	const checkHashSQL = `
		SELECT id::text, template_id::text, version, raw_ast, ast_hash, schema_id::text, author, created_at
		FROM prompt_versions
		WHERE template_id = $1 AND ast_hash = $2
		LIMIT 1
	`
	var existing entity.PromptVersion
	var rawASTBytes []byte
	err = tx.QueryRow(ctx, checkHashSQL, templateID, params.ASTHash).Scan(
		&existing.ID,
		&existing.TemplateID,
		&existing.Version,
		&rawASTBytes,
		&existing.ASTHash,
		&existing.SchemaID,
		&existing.Author,
		&existing.CreatedAt,
	)
	if err == nil {
		if err := json.Unmarshal(rawASTBytes, &existing.RawAST); err != nil {
			return nil, false, fmt.Errorf("failed to unmarshal existing AST: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("failed to commit idempotent PublishVersion: %w", err)
		}
		return &existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("failed to query existing version hash: %w", err)
	}

	const nextVersionSQL = `SELECT COALESCE(MAX(version), 0) + 1 FROM prompt_versions WHERE template_id = $1`
	var nextVersion int
	if err := tx.QueryRow(ctx, nextVersionSQL, templateID).Scan(&nextVersion); err != nil {
		return nil, false, fmt.Errorf("failed to compute next monotonic version: %w", err)
	}

	astBytes, err := json.Marshal(params.RawAST)
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal raw_ast: %w", err)
	}

	const insertVersionSQL = `
		INSERT INTO prompt_versions (template_id, version, raw_ast, ast_hash, schema_id, author)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, template_id::text, version, schema_id::text, author, created_at
	`
	var ver entity.PromptVersion
	if err := tx.QueryRow(ctx, insertVersionSQL, templateID, nextVersion, astBytes, params.ASTHash, params.SchemaID, params.Author).Scan(
		&ver.ID,
		&ver.TemplateID,
		&ver.Version,
		&ver.SchemaID,
		&ver.Author,
		&ver.CreatedAt,
	); err != nil {
		return nil, false, fmt.Errorf("failed to insert prompt_versions v%d: %w", nextVersion, err)
	}
	ver.RawAST = params.RawAST
	ver.ASTHash = params.ASTHash

	const updateTemplateSQL = `UPDATE prompt_templates SET updated_at = NOW() WHERE tenant_id = $1 AND id = $2`
	if _, err := tx.Exec(ctx, updateTemplateSQL, params.TenantID, templateID); err != nil {
		return nil, false, fmt.Errorf("failed to update prompt_templates timestamp: %w", err)
	}

	prevVersion := nextVersion - 1
	auditEntry := entity.AuditEntry{
		TenantID:    params.TenantID,
		TemplateID:  templateID,
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
	if err := insertAuditEntryTx(ctx, tx, auditEntry); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("failed to commit PublishVersion transaction: %w", err)
	}
	return &ver, false, nil
}

// GetTemplateVersion fetches a specific immutable version by template UUID or name within the tenant.
func (r *PostgresPromptRepository) GetTemplateVersion(ctx context.Context, tenantID, templateIdentifier string, version int) (*entity.PromptVersion, error) {
	templateIdentifier = strings.TrimSpace(templateIdentifier)
	var query string
	if entity.IsValidUUID(templateIdentifier) {
		query = `
			SELECT pv.id::text, pv.template_id::text, pv.version, pv.raw_ast, pv.ast_hash, pv.schema_id::text, pv.author, pv.created_at
			FROM prompt_versions pv
			JOIN prompt_templates pt ON pt.id = pv.template_id
			WHERE pt.tenant_id = $1 AND pt.id = $2 AND pv.version = $3
		`
	} else {
		query = `
			SELECT pv.id::text, pv.template_id::text, pv.version, pv.raw_ast, pv.ast_hash, pv.schema_id::text, pv.author, pv.created_at
			FROM prompt_versions pv
			JOIN prompt_templates pt ON pt.id = pv.template_id
			WHERE pt.tenant_id = $1 AND pt.name = $2 AND pv.version = $3
		`
	}

	var ver entity.PromptVersion
	var rawASTBytes []byte
	if err := r.pool.QueryRow(ctx, query, tenantID, templateIdentifier, version).Scan(
		&ver.ID,
		&ver.TemplateID,
		&ver.Version,
		&rawASTBytes,
		&ver.ASTHash,
		&ver.SchemaID,
		&ver.Author,
		&ver.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: template %q version %d not found", entity.ErrNotFound, templateIdentifier, version)
		}
		return nil, fmt.Errorf("failed to get template version: %w", err)
	}
	if err := json.Unmarshal(rawASTBytes, &ver.RawAST); err != nil {
		return nil, fmt.Errorf("failed to unmarshal template AST: %w", err)
	}
	return &ver, nil
}

// PromoteTag atomically updates a semantic tag pointer (@production, @staging, @canary)
// with optimistic locking on ExpectedVersion and writes TAG_PROMOTED or TAG_ROLLED_BACK to prompt_audit_ledger.
func (r *PostgresPromptRepository) PromoteTag(ctx context.Context, params repository.PromoteTagParams) (*entity.PromptTag, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	templateID, err := resolveTemplateIDTx(ctx, tx, params.TenantID, params.TemplateIdentifier, true)
	if err != nil {
		return nil, false, err
	}

	// Verify target_version exists on this template.
	const verifyVerSQL = `SELECT ast_hash, schema_id::text FROM prompt_versions WHERE template_id = $1 AND version = $2`
	var astHash, schemaID string
	if err := tx.QueryRow(ctx, verifyVerSQL, templateID, params.TargetVersion).Scan(&astHash, &schemaID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("%w: target version %d does not exist for template %q", entity.ErrNotFound, params.TargetVersion, templateID)
		}
		return nil, false, fmt.Errorf("failed to verify target version: %w", err)
	}

	// Lock and inspect current tag pointer version (0 if unassigned).
	const currentTagSQL = `
		SELECT target_version
		FROM prompt_tags
		WHERE tenant_id = $1 AND template_id = $2 AND tag = $3
		FOR UPDATE
	`
	currentVersion := 0
	err = tx.QueryRow(ctx, currentTagSQL, params.TenantID, templateID, params.Tag).Scan(&currentVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("failed to query current tag pointer: %w", err)
	}

	isRollback, err := entity.ValidateTagPromotion(currentVersion, params.ExpectedVersion, params.TargetVersion)
	if err != nil {
		return nil, false, err
	}

	const upsertTagSQL = `
		INSERT INTO prompt_tags (tenant_id, template_id, tag, target_version, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (tenant_id, template_id, tag)
		DO UPDATE SET target_version = EXCLUDED.target_version,
		              updated_by = EXCLUDED.updated_by,
		              updated_at = NOW()
		RETURNING id::text, tenant_id::text, template_id::text, tag, target_version, updated_by, updated_at
	`
	var tagEntity entity.PromptTag
	if err := tx.QueryRow(ctx, upsertTagSQL,
		params.TenantID,
		templateID,
		params.Tag,
		params.TargetVersion,
		params.UpdatedBy,
	).Scan(
		&tagEntity.ID,
		&tagEntity.TenantID,
		&tagEntity.TemplateID,
		&tagEntity.Tag,
		&tagEntity.TargetVersion,
		&tagEntity.UpdatedBy,
		&tagEntity.UpdatedAt,
	); err != nil {
		return nil, false, fmt.Errorf("failed to upsert prompt_tags: %w", err)
	}

	action := entity.AuditActionTagPromoted
	if isRollback {
		action = entity.AuditActionTagRolledBack
	}
	targetVer := params.TargetVersion
	prevVer := currentVersion
	auditEntry := entity.AuditEntry{
		TenantID:    params.TenantID,
		TemplateID:  templateID,
		Action:      action,
		Version:     &targetVer,
		Tag:         params.Tag,
		PerformedBy: params.UpdatedBy,
		PayloadSnapshot: entity.AuditPayloadSnapshot{
			ASTHash:          astHash,
			TruncatedASTHash: entity.TruncateHashPrefix(astHash),
			SchemaID:         schemaID,
			PreviousVersion:  &prevVer,
			TargetVersion:    &targetVer,
		},
	}
	if err := insertAuditEntryTx(ctx, tx, auditEntry); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("failed to commit PromoteTag transaction: %w", err)
	}
	return &tagEntity, isRollback, nil
}

// ResolveTag resolves a semantic tag pointer to its bound PromptVersion within the tenant.
func (r *PostgresPromptRepository) ResolveTag(ctx context.Context, tenantID, templateIdentifier, tag string) (*entity.PromptTag, *entity.PromptVersion, error) {
	templateIdentifier = strings.TrimSpace(templateIdentifier)
	var query string
	if entity.IsValidUUID(templateIdentifier) {
		query = `
			SELECT
				ptag.id::text, ptag.tenant_id::text, ptag.template_id::text, ptag.tag, ptag.target_version, ptag.updated_by, ptag.updated_at,
				pv.id::text, pv.version, pv.raw_ast, pv.ast_hash, pv.schema_id::text, pv.author, pv.created_at
			FROM prompt_tags ptag
			JOIN prompt_templates pt ON pt.id = ptag.template_id AND pt.tenant_id = ptag.tenant_id
			JOIN prompt_versions pv ON pv.template_id = ptag.template_id AND pv.version = ptag.target_version
			WHERE ptag.tenant_id = $1 AND pt.id = $2 AND ptag.tag = $3
		`
	} else {
		query = `
			SELECT
				ptag.id::text, ptag.tenant_id::text, ptag.template_id::text, ptag.tag, ptag.target_version, ptag.updated_by, ptag.updated_at,
				pv.id::text, pv.version, pv.raw_ast, pv.ast_hash, pv.schema_id::text, pv.author, pv.created_at
			FROM prompt_tags ptag
			JOIN prompt_templates pt ON pt.id = ptag.template_id AND pt.tenant_id = ptag.tenant_id
			JOIN prompt_versions pv ON pv.template_id = ptag.template_id AND pv.version = ptag.target_version
			WHERE ptag.tenant_id = $1 AND pt.name = $2 AND ptag.tag = $3
		`
	}

	var pTag entity.PromptTag
	var pVer entity.PromptVersion
	var rawASTBytes []byte
	if err := r.pool.QueryRow(ctx, query, tenantID, templateIdentifier, tag).Scan(
		&pTag.ID,
		&pTag.TenantID,
		&pTag.TemplateID,
		&pTag.Tag,
		&pTag.TargetVersion,
		&pTag.UpdatedBy,
		&pTag.UpdatedAt,
		&pVer.ID,
		&pVer.Version,
		&rawASTBytes,
		&pVer.ASTHash,
		&pVer.SchemaID,
		&pVer.Author,
		&pVer.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("%w: tag %q for template %q not found", entity.ErrNotFound, tag, templateIdentifier)
		}
		return nil, nil, fmt.Errorf("failed to resolve tag %q: %w", tag, err)
	}
	pVer.TemplateID = pTag.TemplateID
	if err := json.Unmarshal(rawASTBytes, &pVer.RawAST); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal resolved AST: %w", err)
	}
	return &pTag, &pVer, nil
}

// ListTemplates returns a cursor-paginated list of prompt templates within the tenant (limit <= 100).
func (r *PostgresPromptRepository) ListTemplates(ctx context.Context, tenantID string, limit int, cursor string) ([]entity.PromptTemplate, string, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	fetchLimit := limit + 1
	var rows pgx.Rows
	var err error
	if strings.TrimSpace(cursor) == "" {
		const q = `
			SELECT
				pt.id::text, pt.tenant_id::text, pt.name, COALESCE(pt.description, ''),
				COALESCE((SELECT MAX(pv.version) FROM prompt_versions pv WHERE pv.template_id = pt.id), 1) AS current_version,
				pt.created_at, pt.updated_at
			FROM prompt_templates pt
			WHERE pt.tenant_id = $1
			ORDER BY pt.id ASC
			LIMIT $2
		`
		rows, err = r.pool.Query(ctx, q, tenantID, fetchLimit)
	} else {
		const q = `
			SELECT
				pt.id::text, pt.tenant_id::text, pt.name, COALESCE(pt.description, ''),
				COALESCE((SELECT MAX(pv.version) FROM prompt_versions pv WHERE pv.template_id = pt.id), 1) AS current_version,
				pt.created_at, pt.updated_at
			FROM prompt_templates pt
			WHERE pt.tenant_id = $1 AND pt.id > $2
			ORDER BY pt.id ASC
			LIMIT $3
		`
		rows, err = r.pool.Query(ctx, q, tenantID, cursor, fetchLimit)
	}
	if err != nil {
		return nil, "", fmt.Errorf("failed to list templates: %w", err)
	}
	defer rows.Close()

	var list []entity.PromptTemplate
	for rows.Next() {
		var t entity.PromptTemplate
		if err := rows.Scan(
			&t.ID,
			&t.TenantID,
			&t.Name,
			&t.Description,
			&t.CurrentVersion,
			&t.CreatedAt,
			&t.UpdatedAt,
		); err != nil {
			return nil, "", fmt.Errorf("failed to scan template row: %w", err)
		}
		list = append(list, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("template rows iteration error: %w", err)
	}

	var nextCursor string
	if len(list) > limit {
		nextCursor = list[limit-1].ID
		list = list[:limit]
	}
	return list, nextCursor, nil
}

// RegisterSchema registers a new JSON output validation schema and commits SCHEMA_REGISTERED to prompt_audit_ledger.
func (r *PostgresPromptRepository) RegisterSchema(ctx context.Context, params repository.RegisterSchemaParams) (*entity.JSONSchema, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insertSQL = `
		INSERT INTO json_schemas (tenant_id, name, schema_json)
		VALUES ($1, $2, $3)
		RETURNING id::text, tenant_id::text, name, schema_json, created_at
	`
	var s entity.JSONSchema
	var rawBytes []byte
	if err := tx.QueryRow(ctx, insertSQL, params.TenantID, params.Name, params.SchemaJSON).Scan(
		&s.ID,
		&s.TenantID,
		&s.Name,
		&rawBytes,
		&s.CreatedAt,
	); err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: schema name %q already exists in tenant", entity.ErrConflict, params.Name)
		}
		return nil, fmt.Errorf("failed to insert json_schemas: %w", err)
	}
	s.SchemaJSON = json.RawMessage(rawBytes)

	performedBy := params.Author
	if strings.TrimSpace(performedBy) == "" {
		performedBy = "system-admin"
	}
	auditEntry := entity.AuditEntry{
		TenantID:    params.TenantID,
		TemplateID:  s.ID,
		Action:      entity.AuditActionSchemaRegistered,
		PerformedBy: performedBy,
		PayloadSnapshot: entity.AuditPayloadSnapshot{
			SchemaID:   s.ID,
			SchemaName: s.Name,
		},
	}
	if err := insertAuditEntryTx(ctx, tx, auditEntry); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("failed to commit RegisterSchema transaction: %w", err)
	}
	return &s, nil
}

// GetSchema fetches a registered JSON output schema by UUID within the tenant.
func (r *PostgresPromptRepository) GetSchema(ctx context.Context, tenantID, schemaID string) (*entity.JSONSchema, error) {
	const q = `
		SELECT id::text, tenant_id::text, name, schema_json, created_at
		FROM json_schemas
		WHERE tenant_id = $1 AND id = $2
	`
	var s entity.JSONSchema
	var rawBytes []byte
	if err := r.pool.QueryRow(ctx, q, tenantID, schemaID).Scan(
		&s.ID,
		&s.TenantID,
		&s.Name,
		&rawBytes,
		&s.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: schema %q not found in tenant", entity.ErrNotFound, schemaID)
		}
		return nil, fmt.Errorf("failed to get schema: %w", err)
	}
	s.SchemaJSON = json.RawMessage(rawBytes)
	return &s, nil
}

// ListSchemas returns a cursor-paginated list of registered JSON output schemas for a tenant (limit <= 100).
func (r *PostgresPromptRepository) ListSchemas(ctx context.Context, tenantID string, limit int, cursor string) ([]entity.JSONSchema, string, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	fetchLimit := limit + 1
	var rows pgx.Rows
	var err error
	if strings.TrimSpace(cursor) == "" {
		const q = `
			SELECT id::text, tenant_id::text, name, schema_json, created_at
			FROM json_schemas
			WHERE tenant_id = $1
			ORDER BY id ASC
			LIMIT $2
		`
		rows, err = r.pool.Query(ctx, q, tenantID, fetchLimit)
	} else {
		const q = `
			SELECT id::text, tenant_id::text, name, schema_json, created_at
			FROM json_schemas
			WHERE tenant_id = $1 AND id > $2
			ORDER BY id ASC
			LIMIT $3
		`
		rows, err = r.pool.Query(ctx, q, tenantID, cursor, fetchLimit)
	}
	if err != nil {
		return nil, "", fmt.Errorf("failed to list schemas: %w", err)
	}
	defer rows.Close()

	var list []entity.JSONSchema
	for rows.Next() {
		var s entity.JSONSchema
		var rawBytes []byte
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Name, &rawBytes, &s.CreatedAt); err != nil {
			return nil, "", fmt.Errorf("failed to scan schema row: %w", err)
		}
		s.SchemaJSON = json.RawMessage(rawBytes)
		list = append(list, s)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("schema rows iteration error: %w", err)
	}

	var nextCursor string
	if len(list) > limit {
		nextCursor = list[limit-1].ID
		list = list[:limit]
	}
	return list, nextCursor, nil
}
