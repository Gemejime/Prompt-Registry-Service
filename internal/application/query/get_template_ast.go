// Package query implements CQRS read handlers for the Prompt Registry control plane.
package query

import (
	"context"
	"fmt"
	"strings"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/domain/repository"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
)

// GetTemplateASTHandler executes read-through L2 cached lookups for template versions,
// semantic tag resolutions, and JSON output schemas.
type GetTemplateASTHandler struct {
	repo    repository.PromptRepository
	l2Cache *redis.L2CacheClient
}

// NewGetTemplateASTHandler creates a new GetTemplateASTHandler.
func NewGetTemplateASTHandler(repo repository.PromptRepository, l2Cache *redis.L2CacheClient) *GetTemplateASTHandler {
	return &GetTemplateASTHandler{
		repo:    repo,
		l2Cache: l2Cache,
	}
}

// GetTemplateVersion fetches a specific immutable template version (checking Redis L2 first when identifier is a UUID).
func (h *GetTemplateASTHandler) GetTemplateVersion(ctx context.Context, tenantID, templateIdentifier string, version int) (*entity.PromptVersion, error) {
	if !entity.IsValidUUID(tenantID) {
		return nil, fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	templateIdentifier = strings.TrimSpace(templateIdentifier)
	if templateIdentifier == "" {
		return nil, fmt.Errorf("%w: template_id is required", entity.ErrInvalidInput)
	}
	if version < 1 {
		return nil, fmt.Errorf("%w: version must be >= 1", entity.ErrInvalidInput)
	}

	if h.l2Cache != nil && entity.IsValidUUID(templateIdentifier) {
		if cached, ok := h.l2Cache.GetVersion(ctx, tenantID, templateIdentifier, version); ok {
			return cached, nil
		}
	}

	ver, err := h.repo.GetTemplateVersion(ctx, tenantID, templateIdentifier, version)
	if err != nil {
		return nil, err
	}

	if h.l2Cache != nil {
		_ = h.l2Cache.SetVersion(ctx, tenantID, ver)
	}
	return ver, nil
}

// ResolveTag resolves a semantic tag pointer (@production, @staging, @canary) to its target version and canonical AST.
func (h *GetTemplateASTHandler) ResolveTag(ctx context.Context, tenantID, templateIdentifier, tag string) (*entity.PromptVersion, error) {
	if !entity.IsValidUUID(tenantID) {
		return nil, fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	templateIdentifier = strings.TrimSpace(templateIdentifier)
	if templateIdentifier == "" {
		return nil, fmt.Errorf("%w: template_id is required", entity.ErrInvalidInput)
	}
	tag = strings.TrimSpace(tag)
	if err := entity.ValidateTagName(tag); err != nil {
		return nil, err
	}

	if h.l2Cache != nil && entity.IsValidUUID(templateIdentifier) {
		if cached, ok := h.l2Cache.GetTagResolution(ctx, tenantID, templateIdentifier, tag); ok {
			return cached, nil
		}
	}

	_, ver, err := h.repo.ResolveTag(ctx, tenantID, templateIdentifier, tag)
	if err != nil {
		return nil, err
	}

	if h.l2Cache != nil {
		_ = h.l2Cache.SetTagResolution(ctx, tenantID, ver.TemplateID, tag, ver)
		_ = h.l2Cache.SetVersion(ctx, tenantID, ver)
	}
	return ver, nil
}

// GetSchema retrieves a registered JSON output schema by UUID within the tenant (backed by Redis L2 cache).
func (h *GetTemplateASTHandler) GetSchema(ctx context.Context, tenantID, schemaID string) (*entity.JSONSchema, error) {
	if !entity.IsValidUUID(tenantID) {
		return nil, fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	schemaID = strings.TrimSpace(schemaID)
	if !entity.IsValidUUID(schemaID) {
		return nil, fmt.Errorf("%w: schema_id must be a valid UUID", entity.ErrInvalidInput)
	}

	if h.l2Cache != nil {
		if cached, ok := h.l2Cache.GetSchema(ctx, tenantID, schemaID); ok {
			return cached, nil
		}
	}

	sc, err := h.repo.GetSchema(ctx, tenantID, schemaID)
	if err != nil {
		return nil, err
	}

	if h.l2Cache != nil {
		_ = h.l2Cache.SetSchema(ctx, sc)
	}
	return sc, nil
}
