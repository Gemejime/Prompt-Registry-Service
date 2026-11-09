package command

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/domain/repository"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
)

// PromoteTagInput holds parameters for atomically promoting or rolling back a semantic tag pointer.
type PromoteTagInput struct {
	TenantID           string
	TemplateIdentifier string
	Tag                string
	TargetVersion      int
	ExpectedVersion    int
	UpdatedBy          string
}

// PromoteTagOutput contains the result of a semantic tag promotion.
type PromoteTagOutput struct {
	TemplateID       string
	Tag              string
	TargetVersion    int
	BroadcastEmitted bool
	IsRollback       bool
}

// PromoteTagHandler orchestrates optimistic pointer mutations and Redis Pub/Sub invalidation broadcasts (ADR-002).
type PromoteTagHandler struct {
	repo      repository.PromptRepository
	l2Cache   *redis.L2CacheClient
	publisher redis.InvalidationPublisher
	logger    *slog.Logger
	metrics   *MutationMetricsRecorder
}

// NewPromoteTagHandler creates a new PromoteTagHandler.
func NewPromoteTagHandler(
	repo repository.PromptRepository,
	l2Cache *redis.L2CacheClient,
	publisher redis.InvalidationPublisher,
	logger *slog.Logger,
	metrics *MutationMetricsRecorder,
) *PromoteTagHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &PromoteTagHandler{
		repo:      repo,
		l2Cache:   l2Cache,
		publisher: publisher,
		logger:    logger,
		metrics:   metrics,
	}
}

// PromoteTag atomically updates a tag pointer in PostgreSQL (with optimistic locking on ExpectedVersion
// and transactional audit logging), evicts the stale L2 tag cache entry, and broadcasts an invalidation
// event to Redis Pub/Sub (channel:prompt:invalidation).
func (h *PromoteTagHandler) PromoteTag(ctx context.Context, in PromoteTagInput) (*PromoteTagOutput, error) {
	start := time.Now()
	status := "success"
	defer func() {
		h.metrics.Observe("promote_tag", status, time.Since(start).Seconds())
	}()

	if !entity.IsValidUUID(in.TenantID) {
		status = "error"
		return nil, fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	if strings.TrimSpace(in.TemplateIdentifier) == "" {
		status = "error"
		return nil, fmt.Errorf("%w: template_id is required", entity.ErrInvalidInput)
	}
	if err := entity.ValidateTagName(in.Tag); err != nil {
		status = "error"
		return nil, err
	}
	if in.TargetVersion < 1 {
		status = "error"
		return nil, fmt.Errorf("%w: target_version must be >= 1", entity.ErrInvalidInput)
	}
	if in.ExpectedVersion < 0 {
		status = "error"
		return nil, fmt.Errorf("%w: expected_version must be >= 0", entity.ErrInvalidInput)
	}
	if strings.TrimSpace(in.UpdatedBy) == "" {
		status = "error"
		return nil, fmt.Errorf("%w: updated_by is required", entity.ErrInvalidInput)
	}

	tagEntity, isRollback, err := h.repo.PromoteTag(ctx, repository.PromoteTagParams{
		TenantID:           in.TenantID,
		TemplateIdentifier: strings.TrimSpace(in.TemplateIdentifier),
		Tag:                strings.TrimSpace(in.Tag),
		TargetVersion:      in.TargetVersion,
		ExpectedVersion:    in.ExpectedVersion,
		UpdatedBy:          strings.TrimSpace(in.UpdatedBy),
	})
	if err != nil {
		status = "error"
		return nil, err
	}

	if h.l2Cache != nil {
		var promotedVer *entity.PromptVersion
		if cachedVer, ok := h.l2Cache.GetVersion(ctx, in.TenantID, tagEntity.TemplateID, tagEntity.TargetVersion); ok {
			promotedVer = cachedVer
		} else if repoVer, verErr := h.repo.GetTemplateVersion(ctx, in.TenantID, tagEntity.TemplateID, tagEntity.TargetVersion); verErr == nil {
			promotedVer = repoVer
			_ = h.l2Cache.SetVersion(ctx, in.TenantID, promotedVer)
		}

		if evictErr := h.l2Cache.EvictTagResolution(ctx, in.TenantID, tagEntity.TemplateID, tagEntity.Tag); evictErr != nil {
			h.logger.ErrorContext(ctx, "failed to evict l2 tag resolution cache",
				slog.String("trace_id", TraceIDFromContext(ctx)),
				slog.String("tenant_id", in.TenantID),
				slog.String("template_id", tagEntity.TemplateID),
				slog.String("tag", tagEntity.Tag),
				slog.Int("version", tagEntity.TargetVersion),
				slog.String("error_code", "L2_CACHE_EVICT_ERROR"),
			)
		} else if promotedVer != nil {
			if setErr := h.l2Cache.SetTagResolution(ctx, in.TenantID, tagEntity.TemplateID, tagEntity.Tag, promotedVer); setErr != nil {
				h.logger.ErrorContext(ctx, "failed to populate promoted l2 tag resolution cache",
					slog.String("trace_id", TraceIDFromContext(ctx)),
					slog.String("tenant_id", in.TenantID),
					slog.String("template_id", tagEntity.TemplateID),
					slog.String("tag", tagEntity.Tag),
					slog.Int("version", tagEntity.TargetVersion),
					slog.String("error_code", "L2_CACHE_SET_ERROR"),
				)
			}
		}
	}

	broadcastEmitted := false
	if h.publisher != nil {
		if _, pubErr := h.publisher.PublishTagInvalidation(ctx, in.TenantID, tagEntity.TemplateID, tagEntity.Tag, tagEntity.TargetVersion); pubErr != nil {
			h.logger.ErrorContext(ctx, "failed to publish redis tag invalidation event",
				slog.String("trace_id", TraceIDFromContext(ctx)),
				slog.String("tenant_id", in.TenantID),
				slog.String("template_id", tagEntity.TemplateID),
				slog.String("tag", tagEntity.Tag),
				slog.Int("version", tagEntity.TargetVersion),
				slog.String("error_code", "PUBSUB_PUBLISH_ERROR"),
			)
		} else {
			broadcastEmitted = true
		}
	}

	h.logger.InfoContext(ctx, "semantic tag pointer updated",
		slog.String("trace_id", TraceIDFromContext(ctx)),
		slog.String("tenant_id", in.TenantID),
		slog.String("template_id", tagEntity.TemplateID),
		slog.String("tag", tagEntity.Tag),
		slog.Int("version", tagEntity.TargetVersion),
		slog.Bool("is_rollback", isRollback),
		slog.Bool("broadcast_emitted", broadcastEmitted),
		slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000.0),
	)

	return &PromoteTagOutput{
		TemplateID:       tagEntity.TemplateID,
		Tag:              tagEntity.Tag,
		TargetVersion:    tagEntity.TargetVersion,
		BroadcastEmitted: broadcastEmitted,
		IsRollback:       isRollback,
	}, nil
}
