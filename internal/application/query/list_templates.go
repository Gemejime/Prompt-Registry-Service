package query

import (
	"context"
	"fmt"
	"strings"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/domain/repository"
)

// ListTemplatesHandler executes cursor-paginated queries for prompt templates and JSON schemas.
type ListTemplatesHandler struct {
	repo repository.PromptRepository
}

// NewListTemplatesHandler creates a new ListTemplatesHandler.
func NewListTemplatesHandler(repo repository.PromptRepository) *ListTemplatesHandler {
	return &ListTemplatesHandler{repo: repo}
}

// ListTemplates returns a cursor-paginated list of prompt templates within a tenant (limit <= 100).
func (h *ListTemplatesHandler) ListTemplates(ctx context.Context, tenantID string, limit int, cursor string) ([]entity.PromptTemplate, string, error) {
	if !entity.IsValidUUID(tenantID) {
		return nil, "", fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	cursor = strings.TrimSpace(cursor)
	if cursor != "" && !entity.IsValidUUID(cursor) {
		return nil, "", fmt.Errorf("%w: cursor must be a valid UUID", entity.ErrInvalidInput)
	}
	return h.repo.ListTemplates(ctx, tenantID, limit, cursor)
}

// ListSchemas returns a cursor-paginated list of registered JSON output schemas within a tenant (limit <= 100).
func (h *ListTemplatesHandler) ListSchemas(ctx context.Context, tenantID string, limit int, cursor string) ([]entity.JSONSchema, string, error) {
	if !entity.IsValidUUID(tenantID) {
		return nil, "", fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	cursor = strings.TrimSpace(cursor)
	if cursor != "" && !entity.IsValidUUID(cursor) {
		return nil, "", fmt.Errorf("%w: cursor must be a valid UUID", entity.ErrInvalidInput)
	}
	return h.repo.ListSchemas(ctx, tenantID, limit, cursor)
}
