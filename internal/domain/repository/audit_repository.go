package repository

import (
	"context"

	"rag-prompt-registry-service/internal/domain/entity"
)

// AuditRepository defines read and transactional write access to the immutable prompt_audit_ledger (ADR-004).
type AuditRepository interface {
	// ListByTemplate retrieves ordered compliance ledger entries for a template or schema resource within a tenant.
	ListByTemplate(ctx context.Context, tenantID, templateID string, limit int) ([]entity.AuditEntry, error)
}
