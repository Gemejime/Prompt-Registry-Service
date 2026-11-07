// Package repository defines persistence contracts for the control plane domain.
package repository

import (
	"context"

	"rag-prompt-registry-service/internal/domain/entity"
)

// CreateTemplateParams holds inputs for initializing a template aggregate and initial version v1.
type CreateTemplateParams struct {
	TenantID    string
	Name        string
	Description string
	RawAST      entity.TemplateAST
	ASTHash     string
	SchemaID    string
	Author      string
}

// PublishVersionParams holds inputs for publishing an immutable version v_{n+1}.
type PublishVersionParams struct {
	TenantID           string
	TemplateIdentifier string // Template UUID or unique name within tenant
	RawAST             entity.TemplateAST
	ASTHash            string
	SchemaID           string
	Author             string
}

// PromoteTagParams holds inputs for atomically updating a semantic tag pointer.
type PromoteTagParams struct {
	TenantID           string
	TemplateIdentifier string // Template UUID or unique name within tenant
	Tag                string
	TargetVersion      int
	ExpectedVersion    int
	UpdatedBy          string
}

// RegisterSchemaParams holds inputs for registering a JSON output schema contract.
type RegisterSchemaParams struct {
	TenantID   string
	Name       string
	SchemaJSON []byte
	Author     string
}

// PromptRepository defines the transactional persistence operations for prompt templates,
// versions, semantic tags, and JSON output schemas.
// All write operations atomically commit their corresponding AuditEntry in the same transaction (ADR-004).
type PromptRepository interface {
	// CreateTemplate initializes the template aggregate root and initial version v1,
	// and inserts a TEMPLATE_CREATED entry into prompt_audit_ledger atomically.
	CreateTemplate(ctx context.Context, params CreateTemplateParams) (*entity.PromptTemplate, *entity.PromptVersion, error)

	// PublishVersion allocates monotonic version v_{n+1} (or returns existing version on idempotent ast_hash match)
	// under a row-level lock, and inserts a VERSION_PUBLISHED entry into prompt_audit_ledger atomically.
	PublishVersion(ctx context.Context, params PublishVersionParams) (*entity.PromptVersion, bool, error)

	// GetTemplateVersion retrieves a specific immutable version by template UUID or name within a tenant.
	GetTemplateVersion(ctx context.Context, tenantID, templateIdentifier string, version int) (*entity.PromptVersion, error)

	// PromoteTag atomically updates a semantic tag pointer (@production, @staging, @canary)
	// using optimistic locking on ExpectedVersion, and writes TAG_PROMOTED or TAG_ROLLED_BACK to prompt_audit_ledger.
	PromoteTag(ctx context.Context, params PromoteTagParams) (*entity.PromptTag, bool, error)

	// ResolveTag resolves a semantic tag pointer to its currently bound PromptVersion.
	ResolveTag(ctx context.Context, tenantID, templateIdentifier, tag string) (*entity.PromptTag, *entity.PromptVersion, error)

	// ListTemplates returns a cursor-paginated list of templates for a tenant (limit <= 100).
	ListTemplates(ctx context.Context, tenantID string, limit int, cursor string) ([]entity.PromptTemplate, string, error)

	// RegisterSchema registers a new JSON output schema contract and writes SCHEMA_REGISTERED to prompt_audit_ledger.
	RegisterSchema(ctx context.Context, params RegisterSchemaParams) (*entity.JSONSchema, error)

	// GetSchema fetches a registered JSON output schema by UUID within a tenant.
	GetSchema(ctx context.Context, tenantID, schemaID string) (*entity.JSONSchema, error)

	// ListSchemas returns a cursor-paginated list of registered JSON schemas for a tenant (limit <= 100).
	ListSchemas(ctx context.Context, tenantID string, limit int, cursor string) ([]entity.JSONSchema, string, error)
}
