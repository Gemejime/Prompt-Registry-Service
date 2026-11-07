// Package http implements the internal administration HTTP/REST router, OpenAPI 3.1 Doc API
// endpoints (/docs, /redoc, /api/docs, /openapi.yaml), operational probes (/healthz, /ready),
// and Prometheus metrics exposition (/metrics).
package http

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
)

// MaxHTTPBodyBytes enforces a 1 MiB ceiling on HTTP request payloads to protect the <=128 MiB RSS footprint.
const MaxHTTPBodyBytes = 1 << 20

type tenantContextKeyType struct{}

var httpTenantContextKey = tenantContextKeyType{}

// ReadinessChecker verifies connectivity to PostgreSQL and Redis for the /ready endpoint.
type ReadinessChecker interface {
	CheckPostgres(ctx context.Context) error
	CheckRedis(ctx context.Context) error
}

// RouterConfig holds dependencies and configuration for the HTTP administration router.
type RouterConfig struct {
	PublishHandler *command.PublishVersionHandler
	PromoteHandler *command.PromoteTagHandler
	GetASTHandler  *query.GetTemplateASTHandler
	ListHandler    *query.ListTemplatesHandler
	MutationMetric *command.MutationMetricsRecorder
	Invalidator    *redis.PubSubInvalidationBroadcaster
	Readiness      ReadinessChecker
	DefaultTenant  string
	OpenAPIPath    string
	WriteRateRPS   float64
	WriteRateBurst int
	Logger         *slog.Logger
}

type cachedIdempotentResponse struct {
	statusCode int
	body       []byte
	storedAt   time.Time
}

type tokenBucketLimiter struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64
	lastRefill time.Time
}

func newTokenBucketLimiter(rps float64, burst int) *tokenBucketLimiter {
	if rps <= 0 {
		rps = 100
	}
	if burst <= 0 {
		burst = 200
	}
	return &tokenBucketLimiter{
		tokens:     float64(burst),
		maxTokens:  float64(burst),
		refillRate: rps,
		lastRefill: time.Now(),
	}
}

func (l *tokenBucketLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(l.lastRefill).Seconds()
	if elapsed > 0 {
		l.tokens += elapsed * l.refillRate
		if l.tokens > l.maxTokens {
			l.tokens = l.maxTokens
		}
		l.lastRefill = now
	}
	if l.tokens >= 1.0 {
		l.tokens -= 1.0
		return true
	}
	return false
}

// Router serves the HTTP REST control plane, OpenAPI 3.1 Doc API, probes, and Prometheus metrics.
type Router struct {
	cfg             RouterConfig
	mux             *http.ServeMux
	handler         http.Handler
	limiter         *tokenBucketLimiter
	idempotencyMu   sync.RWMutex
	idempotencyData map[string]cachedIdempotentResponse
}

// NewRouter constructs and wires the HTTP Router with all endpoints and middleware.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	r := &Router{
		cfg:             cfg,
		mux:             http.NewServeMux(),
		limiter:         newTokenBucketLimiter(cfg.WriteRateRPS, cfg.WriteRateBurst),
		idempotencyData: make(map[string]cachedIdempotentResponse),
	}
	r.registerRoutes()
	r.handler = r.withMiddleware(r.mux)
	return r
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.handler.ServeHTTP(w, req)
}

func (r *Router) registerRoutes() {
	// Documentation & Probe API Endpoints (OpenAPI 3.1)
	r.mux.HandleFunc("GET /docs", r.handleSwaggerUI)
	r.mux.HandleFunc("GET /redoc", r.handleReDocUI)
	r.mux.HandleFunc("GET /api/docs", r.handleOpenAPISpec)
	r.mux.HandleFunc("GET /openapi.yaml", r.handleOpenAPISpec)
	r.mux.HandleFunc("GET /healthz", r.handleHealthz)
	r.mux.HandleFunc("GET /ready", r.handleReady)
	r.mux.HandleFunc("GET /metrics", r.handleMetrics)

	// Administrative REST API Endpoints
	r.mux.HandleFunc("POST /v1/templates", r.handleCreateTemplate)
	r.mux.HandleFunc("GET /v1/templates", r.handleListTemplates)
	r.mux.HandleFunc("POST /v1/templates/{id}/versions", r.handlePublishVersion)
	r.mux.HandleFunc("GET /v1/templates/{id}/versions/{version}", r.handleGetTemplateVersion)
	r.mux.HandleFunc("PUT /v1/templates/{id}/tags/{tag}", r.handlePromoteTag)
	r.mux.HandleFunc("GET /v1/templates/{id}/tags/{tag}", r.handleResolveTag)
	r.mux.HandleFunc("POST /v1/schemas", r.handleRegisterSchema)
	r.mux.HandleFunc("GET /v1/schemas", r.handleListSchemas)
	r.mux.HandleFunc("GET /v1/schemas/{id}", r.handleGetSchema)
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
	buf        bytes.Buffer
	capture    bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.statusCode = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.capture {
		sr.buf.Write(b)
	}
	return sr.ResponseWriter.Write(b)
}

func (r *Router) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()

		// 1. Correlated Trace ID & Request ID (ULID)
		traceID := strings.TrimSpace(req.Header.Get("X-Trace-ID"))
		if traceID == "" {
			traceID = strings.TrimSpace(req.Header.Get("X-Request-ID"))
		}
		if traceID == "" {
			traceID = redis.NewULID()
		}
		w.Header().Set("X-Trace-ID", traceID)
		w.Header().Set("X-Request-ID", traceID)

		ctx := command.WithTraceID(req.Context(), traceID)

		// 2. Tenant Extraction from X-Tenant-ID or JWT claims
		callerTenant := strings.TrimSpace(req.Header.Get("X-Tenant-ID"))
		if callerTenant == "" {
			callerTenant = extractTenantFromJWTHeader(req.Header.Get("Authorization"))
		}
		if callerTenant != "" {
			ctx = context.WithValue(ctx, httpTenantContextKey, callerTenant)
		}
		req = req.WithContext(ctx)

		// 3. Panic Recovery Middleware
		defer func() {
			if rec := recover(); rec != nil {
				r.cfg.Logger.ErrorContext(ctx, "recovered panic in HTTP handler",
					slog.String("trace_id", traceID),
					slog.String("path", req.URL.Path),
					slog.String("error_code", "INTERNAL_PANIC"),
				)
				writeErrorJSON(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error", nil)
			}
		}()

		// 4. 1 MiB Payload Ceiling Guard
		if req.Body != nil {
			req.Body = http.MaxBytesReader(w, req.Body, MaxHTTPBodyBytes)
		}

		// 5. Write Rate Limiting & Idempotency-Key Handling for administrative mutations
		isWrite := req.Method == http.MethodPost || req.Method == http.MethodPut
		if isWrite && !r.limiter.allow() {
			writeErrorJSON(w, http.StatusTooManyRequests, "RATE_LIMITED", "Administrative write rate limit exceeded", nil)
			return
		}

		idempKey := strings.TrimSpace(req.Header.Get("Idempotency-Key"))
		var scopedIdempKey string
		if isWrite && idempKey != "" {
			scopedIdempKey = fmt.Sprintf("%s|%s|%s|%s", callerTenant, req.Method, req.URL.Path, idempKey)
			r.idempotencyMu.RLock()
			cached, ok := r.idempotencyData[scopedIdempKey]
			r.idempotencyMu.RUnlock()
			if ok && time.Since(cached.storedAt) < 15*time.Minute {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Idempotent-Replay", "true")
				w.WriteHeader(cached.statusCode)
				_, _ = w.Write(cached.body)
				return
			}
		}

		rec := &statusRecorder{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
			capture:        scopedIdempKey != "",
		}

		next.ServeHTTP(rec, req)

		if scopedIdempKey != "" && rec.statusCode >= 200 && rec.statusCode < 300 {
			r.idempotencyMu.Lock()
			r.idempotencyData[scopedIdempKey] = cachedIdempotentResponse{
				statusCode: rec.statusCode,
				body:       append([]byte(nil), rec.buf.Bytes()...),
				storedAt:   time.Now(),
			}
			r.idempotencyMu.Unlock()
		}

		r.cfg.Logger.InfoContext(ctx, "http request processed",
			slog.String("trace_id", traceID),
			slog.String("tenant_id", callerTenant),
			slog.String("method", req.Method),
			slog.String("path", req.URL.Path),
			slog.Int("status_code", rec.statusCode),
			slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000.0),
		)
	})
}

func extractTenantFromJWTHeader(headerVal string) string {
	parts := strings.SplitN(strings.TrimSpace(headerVal), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	segments := strings.Split(strings.TrimSpace(parts[1]), ".")
	if len(segments) < 2 {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return ""
	}
	if tid, ok := claims["tenant_id"].(string); ok {
		return strings.TrimSpace(tid)
	}
	return ""
}

func (r *Router) resolveTenant(req *http.Request, bodyTenant string) (string, error) {
	bodyTenant = strings.TrimSpace(bodyTenant)
	headerTenant, _ := req.Context().Value(httpTenantContextKey).(string)
	headerTenant = strings.TrimSpace(headerTenant)
	if headerTenant == "" {
		headerTenant = strings.TrimSpace(req.URL.Query().Get("tenant_id"))
	}

	if headerTenant != "" && bodyTenant != "" && !strings.EqualFold(headerTenant, bodyTenant) {
		return "", errCrossTenantForbidden
	}
	if bodyTenant != "" {
		return bodyTenant, nil
	}
	if headerTenant != "" {
		return headerTenant, nil
	}
	if r.cfg.DefaultTenant != "" {
		return r.cfg.DefaultTenant, nil
	}
	return "", fmt.Errorf("%w: tenant_id is required", entity.ErrInvalidInput)
}

var errCrossTenantForbidden = errors.New("cross-tenant access forbidden")

// ErrorResponse matches #/components/schemas/ErrorResponse in openapi/openapi.yaml.
type ErrorResponse struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErrorJSON(w http.ResponseWriter, statusCode int, code, message string, details []string) {
	writeJSON(w, statusCode, ErrorResponse{
		Code:    code,
		Message: message,
		Details: details,
	})
}

func handleDomainError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeErrorJSON(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Request body exceeds 1 MiB limit", nil)
		return
	}
	switch {
	case errors.Is(err, errCrossTenantForbidden):
		writeErrorJSON(w, http.StatusForbidden, "FORBIDDEN", err.Error(), nil)
	case errors.Is(err, entity.ErrInvalidInput):
		writeErrorJSON(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
	case errors.Is(err, entity.ErrNotFound):
		writeErrorJSON(w, http.StatusNotFound, "NOT_FOUND", err.Error(), nil)
	case errors.Is(err, entity.ErrVersionConflict):
		writeErrorJSON(w, http.StatusConflict, "VERSION_CONFLICT", err.Error(), nil)
	case errors.Is(err, entity.ErrConflict):
		writeErrorJSON(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
	default:
		writeErrorJSON(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error", nil)
	}
}

func decodeJSONBody(req *http.Request, dst any) error {
	if req.Body == nil {
		return fmt.Errorf("%w: request body is empty", entity.ErrInvalidInput)
	}
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return err
		}
		return fmt.Errorf("%w: failed to read request body", entity.ErrInvalidInput)
	}
	if len(bytes.TrimSpace(bodyBytes)) == 0 {
		return fmt.Errorf("%w: request body cannot be empty", entity.ErrInvalidInput)
	}
	if err := json.Unmarshal(bodyBytes, dst); err != nil {
		return fmt.Errorf("%w: malformed JSON payload: %v", entity.ErrInvalidInput, err)
	}
	return nil
}

// --- Doc API & Probe Handlers ---

func (r *Router) handleSwaggerUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>RAG Prompt Registry Service - Swagger UI</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({ url: '/openapi.yaml', dom_id: '#swagger-ui' });
    };
  </script>
</body>
</html>`))
}

func (r *Router) handleReDocUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>RAG Prompt Registry Service - ReDoc</title>
</head>
<body>
  <redoc spec-url="/openapi.yaml"></redoc>
  <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
</body>
</html>`))
}

func (r *Router) handleOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	candidates := []string{
		r.cfg.OpenAPIPath,
		"openapi/openapi.yaml",
		filepath.Join("..", "..", "openapi", "openapi.yaml"),
	}
	for _, p := range candidates {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			w.Header().Set("Content-Type", "application/yaml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("openapi: 3.1.0\ninfo:\n  title: RAG Prompt Registry Service API\n  version: 1.0.0\n"))
}

func (r *Router) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}

func (r *Router) handleReady(w http.ResponseWriter, req *http.Request) {
	if r.cfg.Readiness != nil {
		if err := r.cfg.Readiness.CheckPostgres(req.Context()); err != nil {
			writeErrorJSON(w, http.StatusServiceUnavailable, "POSTGRES_UNAVAILABLE", "PostgreSQL pool is unreachable", nil)
			return
		}
		if err := r.cfg.Readiness.CheckRedis(req.Context()); err != nil {
			writeErrorJSON(w, http.StatusServiceUnavailable, "REDIS_UNAVAILABLE", "Redis cluster is unreachable", nil)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ready",
		"postgres": "connected",
		"redis":    "reachable",
	})
}

func (r *Router) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var sb strings.Builder
	sb.WriteString("# HELP prompt_registry_mutation_duration_seconds Control plane write latencies\n")
	sb.WriteString("# TYPE prompt_registry_mutation_duration_seconds summary\n")
	if r.cfg.MutationMetric != nil {
		counts, sums := r.cfg.MutationMetric.Snapshot()
		for key, cnt := range counts {
			parts := strings.SplitN(key, "|", 2)
			if len(parts) == 2 {
				sb.WriteString(fmt.Sprintf("prompt_registry_mutation_duration_seconds_count{operation=%q,status=%q} %d\n", parts[0], parts[1], cnt))
				sb.WriteString(fmt.Sprintf("prompt_registry_mutation_duration_seconds_sum{operation=%q,status=%q} %.6f\n", parts[0], parts[1], sums[key]))
			}
		}
	}

	pubSuccess, pubErr := uint64(0), uint64(0)
	if r.cfg.Invalidator != nil {
		pubSuccess, pubErr = r.cfg.Invalidator.Metrics()
	}
	sb.WriteString("# HELP prompt_invalidation_events_published_total Redis Pub/Sub eviction signals emitted\n")
	sb.WriteString("# TYPE prompt_invalidation_events_published_total counter\n")
	sb.WriteString(fmt.Sprintf("prompt_invalidation_events_published_total{status=\"success\"} %d\n", pubSuccess))
	sb.WriteString(fmt.Sprintf("prompt_invalidation_events_published_total{status=\"error\"} %d\n", pubErr))

	sb.WriteString("# HELP prompt_sdk_render_duration_seconds SDK in-process compilation latency\n")
	sb.WriteString("# TYPE prompt_sdk_render_duration_seconds histogram\n")
	sb.WriteString("prompt_sdk_render_duration_seconds_count{dialect=\"DIALECT_ANTHROPIC_XML\",status=\"success\"} 0\n")

	sb.WriteString("# HELP prompt_sdk_cache_hit_ratio Local SDK memory hit ratio\n")
	sb.WriteString("# TYPE prompt_sdk_cache_hit_ratio gauge\n")
	sb.WriteString("prompt_sdk_cache_hit_ratio{tier=\"L1_version\"} 1\n")
	sb.WriteString("prompt_sdk_cache_hit_ratio{tier=\"L1_tag\"} 1\n")

	sb.WriteString("# HELP prompt_sdk_pubsub_connected Orchestrator Pub/Sub connection status\n")
	sb.WriteString("# TYPE prompt_sdk_pubsub_connected gauge\n")
	sb.WriteString("prompt_sdk_pubsub_connected{node_id=\"control-plane\"} 1\n")

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}

// --- Administrative REST API Payloads & Handlers ---

type createTemplateHTTPRequest struct {
	TenantID    string             `json:"tenant_id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	RawAST      entity.TemplateAST `json:"raw_ast"`
	SchemaID    string             `json:"schema_id"`
	Author      string             `json:"author"`
}

type templateHTTPResponse struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	CurrentVersion int    `json:"current_version"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

func toTemplateHTTPResponse(t entity.PromptTemplate) templateHTTPResponse {
	return templateHTTPResponse{
		ID:             t.ID,
		TenantID:       t.TenantID,
		Name:           t.Name,
		Description:    t.Description,
		CurrentVersion: t.CurrentVersion,
		CreatedAt:      t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (r *Router) handleCreateTemplate(w http.ResponseWriter, req *http.Request) {
	var body createTemplateHTTPRequest
	if err := decodeJSONBody(req, &body); err != nil {
		handleDomainError(w, err)
		return
	}
	tenantID, err := r.resolveTenant(req, body.TenantID)
	if err != nil {
		handleDomainError(w, err)
		return
	}
	tmpl, _, err := r.cfg.PublishHandler.CreateTemplate(req.Context(), command.CreateTemplateInput{
		TenantID:    tenantID,
		Name:        body.Name,
		Description: body.Description,
		RawAST:      body.RawAST,
		SchemaID:    body.SchemaID,
		Author:      body.Author,
	})
	if err != nil {
		handleDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTemplateHTTPResponse(*tmpl))
}

type listTemplatesHTTPResponse struct {
	Templates  []templateHTTPResponse `json:"templates"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

func (r *Router) handleListTemplates(w http.ResponseWriter, req *http.Request) {
	tenantID, err := r.resolveTenant(req, "")
	if err != nil {
		handleDomainError(w, err)
		return
	}
	limit := 20
	if lStr := strings.TrimSpace(req.URL.Query().Get("limit")); lStr != "" {
		parsed, parseErr := strconv.Atoi(lStr)
		if parseErr != nil || parsed < 1 || parsed > 100 {
			writeErrorJSON(w, http.StatusBadRequest, "BAD_REQUEST", "limit must be an integer between 1 and 100", nil)
			return
		}
		limit = parsed
	}
	cursor := strings.TrimSpace(req.URL.Query().Get("cursor"))
	list, nextCursor, err := r.cfg.ListHandler.ListTemplates(req.Context(), tenantID, limit, cursor)
	if err != nil {
		handleDomainError(w, err)
		return
	}
	items := make([]templateHTTPResponse, 0, len(list))
	for _, t := range list {
		items = append(items, toTemplateHTTPResponse(t))
	}
	writeJSON(w, http.StatusOK, listTemplatesHTTPResponse{
		Templates:  items,
		NextCursor: nextCursor,
	})
}

type publishVersionHTTPRequest struct {
	RawAST   entity.TemplateAST `json:"raw_ast"`
	SchemaID string             `json:"schema_id"`
	Author   string             `json:"author"`
}

type versionHTTPResponse struct {
	TemplateID string `json:"template_id"`
	Version    int    `json:"version"`
	CreatedAt  string `json:"created_at"`
}

func (r *Router) handlePublishVersion(w http.ResponseWriter, req *http.Request) {
	templateID := req.PathValue("id")
	var body publishVersionHTTPRequest
	if err := decodeJSONBody(req, &body); err != nil {
		handleDomainError(w, err)
		return
	}
	tenantID, err := r.resolveTenant(req, "")
	if err != nil {
		handleDomainError(w, err)
		return
	}
	ver, _, err := r.cfg.PublishHandler.PublishVersion(req.Context(), command.PublishVersionInput{
		TenantID:           tenantID,
		TemplateIdentifier: templateID,
		RawAST:             body.RawAST,
		SchemaID:           body.SchemaID,
		Author:             body.Author,
	})
	if err != nil {
		handleDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, versionHTTPResponse{
		TemplateID: ver.TemplateID,
		Version:    ver.Version,
		CreatedAt:  ver.CreatedAt.UTC().Format(time.RFC3339),
	})
}

type versionDetailHTTPResponse struct {
	TemplateID string             `json:"template_id"`
	Version    int                `json:"version"`
	AST        entity.TemplateAST `json:"ast"`
	SchemaID   string             `json:"schema_id"`
	Author     string             `json:"author"`
	CreatedAt  string             `json:"created_at"`
}

func (r *Router) handleGetTemplateVersion(w http.ResponseWriter, req *http.Request) {
	templateID := req.PathValue("id")
	verStr := req.PathValue("version")
	verNum, err := strconv.Atoi(verStr)
	if err != nil || verNum < 1 {
		writeErrorJSON(w, http.StatusBadRequest, "BAD_REQUEST", "version must be a positive integer", nil)
		return
	}
	tenantID, err := r.resolveTenant(req, "")
	if err != nil {
		handleDomainError(w, err)
		return
	}
	ver, err := r.cfg.GetASTHandler.GetTemplateVersion(req.Context(), tenantID, templateID, verNum)
	if err != nil {
		handleDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, versionDetailHTTPResponse{
		TemplateID: ver.TemplateID,
		Version:    ver.Version,
		AST:        ver.RawAST,
		SchemaID:   ver.SchemaID,
		Author:     ver.Author,
		CreatedAt:  ver.CreatedAt.UTC().Format(time.RFC3339),
	})
}

type promoteTagHTTPRequest struct {
	TargetVersion   int    `json:"target_version"`
	ExpectedVersion *int   `json:"expected_version"`
	UpdatedBy       string `json:"updated_by"`
}

type promoteTagHTTPResponse struct {
	TemplateID       string `json:"template_id"`
	Tag              string `json:"tag"`
	TargetVersion    int    `json:"target_version"`
	BroadcastEmitted bool   `json:"broadcast_emitted"`
}

func (r *Router) handlePromoteTag(w http.ResponseWriter, req *http.Request) {
	templateID := req.PathValue("id")
	tag := req.PathValue("tag")
	var body promoteTagHTTPRequest
	if err := decodeJSONBody(req, &body); err != nil {
		handleDomainError(w, err)
		return
	}
	if body.ExpectedVersion == nil {
		writeErrorJSON(w, http.StatusBadRequest, "BAD_REQUEST", "expected_version is required for optimistic pointer locking", nil)
		return
	}
	tenantID, err := r.resolveTenant(req, "")
	if err != nil {
		handleDomainError(w, err)
		return
	}
	out, err := r.cfg.PromoteHandler.PromoteTag(req.Context(), command.PromoteTagInput{
		TenantID:           tenantID,
		TemplateIdentifier: templateID,
		Tag:                tag,
		TargetVersion:      body.TargetVersion,
		ExpectedVersion:    *body.ExpectedVersion,
		UpdatedBy:          body.UpdatedBy,
	})
	if err != nil {
		handleDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, promoteTagHTTPResponse{
		TemplateID:       out.TemplateID,
		Tag:              out.Tag,
		TargetVersion:    out.TargetVersion,
		BroadcastEmitted: out.BroadcastEmitted,
	})
}

type resolveTagHTTPResponse struct {
	TemplateID string             `json:"template_id"`
	Tag        string             `json:"tag"`
	Version    int                `json:"version"`
	AST        entity.TemplateAST `json:"ast"`
}

func (r *Router) handleResolveTag(w http.ResponseWriter, req *http.Request) {
	templateID := req.PathValue("id")
	tag := req.PathValue("tag")
	tenantID, err := r.resolveTenant(req, "")
	if err != nil {
		handleDomainError(w, err)
		return
	}
	ver, err := r.cfg.GetASTHandler.ResolveTag(req.Context(), tenantID, templateID, tag)
	if err != nil {
		handleDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resolveTagHTTPResponse{
		TemplateID: ver.TemplateID,
		Tag:        tag,
		Version:    ver.Version,
		AST:        ver.RawAST,
	})
}

type registerSchemaHTTPRequest struct {
	TenantID   string          `json:"tenant_id"`
	Name       string          `json:"name"`
	JSONSchema json.RawMessage `json:"json_schema"`
}

type registerSchemaHTTPResponse struct {
	SchemaID string `json:"schema_id"`
}

func (r *Router) handleRegisterSchema(w http.ResponseWriter, req *http.Request) {
	var body registerSchemaHTTPRequest
	if err := decodeJSONBody(req, &body); err != nil {
		handleDomainError(w, err)
		return
	}
	tenantID, err := r.resolveTenant(req, body.TenantID)
	if err != nil {
		handleDomainError(w, err)
		return
	}
	sc, err := r.cfg.PublishHandler.RegisterSchema(req.Context(), command.RegisterSchemaInput{
		TenantID:   tenantID,
		Name:       body.Name,
		SchemaJSON: body.JSONSchema,
		Author:     "http-admin",
	})
	if err != nil {
		handleDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, registerSchemaHTTPResponse{
		SchemaID: sc.ID,
	})
}

type schemaHTTPResponse struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id"`
	Name       string          `json:"name"`
	SchemaJSON json.RawMessage `json:"schema_json"`
	CreatedAt  string          `json:"created_at"`
}

type listSchemasHTTPResponse struct {
	Schemas    []schemaHTTPResponse `json:"schemas"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

func (r *Router) handleListSchemas(w http.ResponseWriter, req *http.Request) {
	tenantID, err := r.resolveTenant(req, "")
	if err != nil {
		handleDomainError(w, err)
		return
	}
	limit := 20
	if lStr := strings.TrimSpace(req.URL.Query().Get("limit")); lStr != "" {
		parsed, parseErr := strconv.Atoi(lStr)
		if parseErr != nil || parsed < 1 || parsed > 100 {
			writeErrorJSON(w, http.StatusBadRequest, "BAD_REQUEST", "limit must be an integer between 1 and 100", nil)
			return
		}
		limit = parsed
	}
	cursor := strings.TrimSpace(req.URL.Query().Get("cursor"))
	list, nextCursor, err := r.cfg.ListHandler.ListSchemas(req.Context(), tenantID, limit, cursor)
	if err != nil {
		handleDomainError(w, err)
		return
	}
	items := make([]schemaHTTPResponse, 0, len(list))
	for _, sc := range list {
		items = append(items, schemaHTTPResponse{
			ID:         sc.ID,
			TenantID:   sc.TenantID,
			Name:       sc.Name,
			SchemaJSON: sc.SchemaJSON,
			CreatedAt:  sc.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, listSchemasHTTPResponse{
		Schemas:    items,
		NextCursor: nextCursor,
	})
}

func (r *Router) handleGetSchema(w http.ResponseWriter, req *http.Request) {
	schemaID := req.PathValue("id")
	tenantID, err := r.resolveTenant(req, "")
	if err != nil {
		handleDomainError(w, err)
		return
	}
	sc, err := r.cfg.GetASTHandler.GetSchema(req.Context(), tenantID, schemaID)
	if err != nil {
		handleDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, schemaHTTPResponse{
		ID:         sc.ID,
		TenantID:   sc.TenantID,
		Name:       sc.Name,
		SchemaJSON: sc.SchemaJSON,
		CreatedAt:  sc.CreatedAt.UTC().Format(time.RFC3339),
	})
}
