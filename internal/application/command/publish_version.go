// Package command implements CQRS write handlers for the Prompt Registry control plane.
package command

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/domain/repository"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
)

// traceIDKeyType represents an unexported context key type for storing correlated trace IDs.
type traceIDKeyType struct{}

var traceIDContextKey = traceIDKeyType{}

// WithTraceID attaches a correlated trace_id to the context for structured logging.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDContextKey, traceID)
}

// TraceIDFromContext extracts the correlated trace_id from the context.
func TraceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDContextKey).(string); ok && v != "" {
		return v
	}
	return "00000000000000000000000000"
}

// MutationMetricsRecorder tracks control plane write latencies for Prometheus exposition.
type MutationMetricsRecorder struct {
	mu           sync.RWMutex
	counts       map[string]uint64
	durationSums map[string]float64
}

// NewMutationMetricsRecorder creates a new MutationMetricsRecorder.
func NewMutationMetricsRecorder() *MutationMetricsRecorder {
	return &MutationMetricsRecorder{
		counts:       make(map[string]uint64),
		durationSums: make(map[string]float64),
	}
}

// Observe records a control plane write operation latency in seconds.
func (m *MutationMetricsRecorder) Observe(operation, status string, seconds float64) {
	if m == nil {
		return
	}
	key := operation + "|" + status
	m.mu.Lock()
	m.counts[key]++
	m.durationSums[key] += seconds
	m.mu.Unlock()
}

// Snapshot returns copies of the recorded counts and duration sums.
func (m *MutationMetricsRecorder) Snapshot() (map[string]uint64, map[string]float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := make(map[string]uint64, len(m.counts))
	s := make(map[string]float64, len(m.durationSums))
	for k, v := range m.counts {
		c[k] = v
	}
	for k, v := range m.durationSums {
		s[k] = v
	}
	return c, s
}

// CreateTemplateInput holds parameters for CreateTemplate.
type CreateTemplateInput struct {
	TenantID    string
	Name        string
	Description string
	RawAST      entity.TemplateAST
	SchemaID    string
	Author      string
}

// PublishVersionInput holds parameters for PublishVersion.
type PublishVersionInput struct {
	TenantID           string
	TemplateIdentifier string
	RawAST             entity.TemplateAST
	SchemaID           string
	Author             string
}

// RegisterSchemaInput holds parameters for RegisterSchema.
type RegisterSchemaInput struct {
	TenantID   string
	Name       string
	SchemaJSON []byte
	Author     string
}

// PublishVersionHandler orchestrates template creation, immutable version publishing,
// and JSON output schema registration.
type PublishVersionHandler struct {
	repo    repository.PromptRepository
	l2Cache *redis.L2CacheClient
	logger  *slog.Logger
	metrics *MutationMetricsRecorder
}

// NewPublishVersionHandler creates a new PublishVersionHandler.
func NewPublishVersionHandler(
	repo repository.PromptRepository,
	l2Cache *redis.L2CacheClient,
	logger *slog.Logger,
	metrics *MutationMetricsRecorder,
) *PublishVersionHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &PublishVersionHandler{
		repo:    repo,
		l2Cache: l2Cache,
		logger:  logger,
		metrics: metrics,
	}
}

// CreateTemplate validates the AST and schema contract, persists the aggregate root + initial version v1,
// populates L2 cache, and emits a secured JSON log entry.
func (h *PublishVersionHandler) CreateTemplate(ctx context.Context, in CreateTemplateInput) (*entity.PromptTemplate, *entity.PromptVersion, error) {
	start := time.Now()
	status := "success"
	defer func() {
		h.metrics.Observe("create_template", status, time.Since(start).Seconds())
	}()

	if !entity.IsValidUUID(in.TenantID) {
		status = "error"
		return nil, nil, fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	if err := entity.ValidateResourceName(in.Name); err != nil {
		status = "error"
		return nil, nil, err
	}
	if !entity.IsValidUUID(in.SchemaID) {
		status = "error"
		return nil, nil, fmt.Errorf("%w: schema_id is required and must be a valid UUID", entity.ErrInvalidInput)
	}
	if strings.TrimSpace(in.Author) == "" {
		status = "error"
		return nil, nil, fmt.Errorf("%w: author is required", entity.ErrInvalidInput)
	}
	if err := in.RawAST.Validate(); err != nil {
		status = "error"
		return nil, nil, err
	}

	astHash, err := in.RawAST.CanonicalHash(in.SchemaID)
	if err != nil {
		status = "error"
		return nil, nil, err
	}

	tmpl, ver, err := h.repo.CreateTemplate(ctx, repository.CreateTemplateParams{
		TenantID:    in.TenantID,
		Name:        strings.TrimSpace(in.Name),
		Description: strings.TrimSpace(in.Description),
		RawAST:      in.RawAST,
		ASTHash:     astHash,
		SchemaID:    strings.TrimSpace(in.SchemaID),
		Author:      strings.TrimSpace(in.Author),
	})
	if err != nil {
		status = "error"
		return nil, nil, err
	}

	if h.l2Cache != nil {
		_ = h.l2Cache.SetVersion(ctx, in.TenantID, ver)
	}

	// Secured Logging Invariant 2: log only operational metadata and truncated SHA-256 hash.
	h.logger.InfoContext(ctx, "template created with initial version",
		slog.String("trace_id", TraceIDFromContext(ctx)),
		slog.String("tenant_id", tmpl.TenantID),
		slog.String("template_id", tmpl.ID),
		slog.Int("version", ver.Version),
		slog.String("template_hash", entity.TruncateHashPrefix(astHash)),
		slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000.0),
	)

	return tmpl, ver, nil
}

// PublishVersion validates the AST and schema contract, allocates monotonic version v_{n+1}
// (or returns the existing version if ast_hash matches idempotently), populates L2 cache,
// and emits a secured JSON log entry.
func (h *PublishVersionHandler) PublishVersion(ctx context.Context, in PublishVersionInput) (*entity.PromptVersion, bool, error) {
	start := time.Now()
	status := "success"
	defer func() {
		h.metrics.Observe("publish_version", status, time.Since(start).Seconds())
	}()

	if !entity.IsValidUUID(in.TenantID) {
		status = "error"
		return nil, false, fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	if strings.TrimSpace(in.TemplateIdentifier) == "" {
		status = "error"
		return nil, false, fmt.Errorf("%w: template_id is required", entity.ErrInvalidInput)
	}
	if !entity.IsValidUUID(in.SchemaID) {
		status = "error"
		return nil, false, fmt.Errorf("%w: schema_id is required and must be a valid UUID", entity.ErrInvalidInput)
	}
	if strings.TrimSpace(in.Author) == "" {
		status = "error"
		return nil, false, fmt.Errorf("%w: author is required", entity.ErrInvalidInput)
	}
	if err := in.RawAST.Validate(); err != nil {
		status = "error"
		return nil, false, err
	}

	astHash, err := in.RawAST.CanonicalHash(in.SchemaID)
	if err != nil {
		status = "error"
		return nil, false, err
	}

	ver, idempotentReplay, err := h.repo.PublishVersion(ctx, repository.PublishVersionParams{
		TenantID:           in.TenantID,
		TemplateIdentifier: strings.TrimSpace(in.TemplateIdentifier),
		RawAST:             in.RawAST,
		ASTHash:            astHash,
		SchemaID:           strings.TrimSpace(in.SchemaID),
		Author:             strings.TrimSpace(in.Author),
	})
	if err != nil {
		status = "error"
		return nil, false, err
	}

	if h.l2Cache != nil {
		_ = h.l2Cache.SetVersion(ctx, in.TenantID, ver)
	}

	h.logger.InfoContext(ctx, "immutable template version published",
		slog.String("trace_id", TraceIDFromContext(ctx)),
		slog.String("tenant_id", in.TenantID),
		slog.String("template_id", ver.TemplateID),
		slog.Int("version", ver.Version),
		slog.Bool("idempotent_replay", idempotentReplay),
		slog.String("template_hash", entity.TruncateHashPrefix(astHash)),
		slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000.0),
	)

	return ver, idempotentReplay, nil
}

// RegisterSchema validates the JSON Schema draft, persists it atomically with a SCHEMA_REGISTERED
// audit record, populates L2 cache, and emits a secured JSON log entry.
func (h *PublishVersionHandler) RegisterSchema(ctx context.Context, in RegisterSchemaInput) (*entity.JSONSchema, error) {
	start := time.Now()
	status := "success"
	defer func() {
		h.metrics.Observe("register_schema", status, time.Since(start).Seconds())
	}()

	if !entity.IsValidUUID(in.TenantID) {
		status = "error"
		return nil, fmt.Errorf("%w: tenant_id must be a valid UUID", entity.ErrInvalidInput)
	}
	if err := entity.ValidateResourceName(in.Name); err != nil {
		status = "error"
		return nil, err
	}

	normalizedSchema, err := entity.ValidateJSONSchemaDraft(in.SchemaJSON)
	if err != nil {
		status = "error"
		return nil, err
	}

	sc, err := h.repo.RegisterSchema(ctx, repository.RegisterSchemaParams{
		TenantID:   in.TenantID,
		Name:       strings.TrimSpace(in.Name),
		SchemaJSON: normalizedSchema,
		Author:     strings.TrimSpace(in.Author),
	})
	if err != nil {
		status = "error"
		return nil, err
	}

	if h.l2Cache != nil {
		_ = h.l2Cache.SetSchema(ctx, sc)
	}

	h.logger.InfoContext(ctx, "json output schema registered",
		slog.String("trace_id", TraceIDFromContext(ctx)),
		slog.String("tenant_id", sc.TenantID),
		slog.String("template_id", sc.ID),
		slog.Int("version", 1),
		slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000.0),
	)

	return sc, nil
}
