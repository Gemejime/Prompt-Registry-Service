package entity

import (
	"time"
)

// AuditAction enumerates compliance ledger actions stored in prompt_audit_ledger (ADR-004).
type AuditAction string

const (
	AuditActionTemplateCreated  AuditAction = "TEMPLATE_CREATED"
	AuditActionVersionPublished AuditAction = "VERSION_PUBLISHED"
	AuditActionTagPromoted      AuditAction = "TAG_PROMOTED"
	AuditActionTagRolledBack    AuditAction = "TAG_ROLLED_BACK"
	AuditActionSchemaRegistered AuditAction = "SCHEMA_REGISTERED"
)

// AuditPayloadSnapshot contains strictly redacted operational metadata for the compliance ledger.
// Never store raw prompt templates or raw context strings in the audit snapshot (Invariant 2).
type AuditPayloadSnapshot struct {
	ASTHash          string      `json:"ast_hash,omitempty"`
	TruncatedASTHash string      `json:"template_hash,omitempty"`
	SchemaID         string      `json:"schema_id,omitempty"`
	SchemaName       string      `json:"schema_name,omitempty"`
	PreviousVersion  *int        `json:"previous_version,omitempty"`
	TargetVersion    *int        `json:"target_version,omitempty"`
	VariableKeys     []string    `json:"variable_keys,omitempty"`
	DefaultDialect   DialectType `json:"default_dialect,omitempty"`
}

// AuditEntry represents an immutable row in prompt_audit_ledger committed atomically
// within the same PostgreSQL ACID transaction as the primary mutation (ADR-004).
type AuditEntry struct {
	ID              int64                `json:"id"`
	TenantID        string               `json:"tenant_id"`
	TemplateID      string               `json:"template_id"`
	Action          AuditAction          `json:"action"`
	Version         *int                 `json:"version,omitempty"`
	Tag             string               `json:"tag,omitempty"`
	PerformedBy     string               `json:"performed_by"`
	PayloadSnapshot AuditPayloadSnapshot `json:"payload_snapshot"`
	CreatedAt       time.Time            `json:"created_at"`
}
