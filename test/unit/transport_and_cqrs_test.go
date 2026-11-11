package unit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
	"rag-prompt-registry-service/internal/infrastructure/persistence/postgres"
	grpctransport "rag-prompt-registry-service/internal/transport/grpc"
	httptransport "rag-prompt-registry-service/internal/transport/http"
	promptv1 "rag-prompt-registry-service/proto/v1"
)

// TestRESTControlPlaneLifecycleAndSecuredLogging verifies the HTTP REST endpoints, JWT authentication,
// idempotency-key payload fingerprinting, and secured logging redaction invariants.
func TestRESTControlPlaneLifecycleAndSecuredLogging(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := postgres.NewTransactionalMemoryStore()
	l2Cache := redis.NewInMemoryL2Cache()
	invalidator := redis.NewInvalidationBroadcaster(l2Cache, redis.DefaultInvalidationChannel)
	metrics := command.NewMutationMetricsRecorder()

	pubHandler := command.NewPublishVersionHandler(store, l2Cache, logger, metrics)
	promHandler := command.NewPromoteTagHandler(store, l2Cache, invalidator, logger, metrics)
	getHandler := query.NewGetTemplateASTHandler(store, l2Cache)
	listHandler := query.NewListTemplatesHandler(store)

	tenantA := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	tenantB := "b1ffcd88-8d1a-4ef8-aa5c-5aa8ac270b22"

	router := httptransport.NewRouter(httptransport.RouterConfig{
		PublishHandler: pubHandler,
		PromoteHandler: promHandler,
		GetASTHandler:  getHandler,
		ListHandler:    listHandler,
		MutationMetric: metrics,
		Invalidator:    invalidator,
		DefaultTenant:  tenantA,
		OpenAPIPath:    "../../openapi/openapi.yaml",
		WriteRateRPS:   1000,
		WriteRateBurst: 1000,
		Logger:         logger,
	})

	ts := httptest.NewServer(router)
	defer ts.Close()

	// 1. Verify Doc API & Operational Probe endpoints (/docs, /redoc, /api/docs, /openapi.yaml, /healthz, /ready, /metrics)
	for _, path := range []string{"/docs", "/redoc", "/api/docs", "/openapi.yaml", "/healthz", "/ready", "/metrics"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned status %d, expected 200", path, resp.StatusCode)
		}
	}

	// 2. Register a JSON Output Schema
	schemaPayload := map[string]any{
		"tenant_id": tenantA,
		"name":      "financial-metrics-output",
		"json_schema": map[string]any{
			"type":     "object",
			"required": []string{"revenue_summary"},
			"properties": map[string]any{
				"revenue_summary": map[string]any{"type": "string"},
			},
		},
	}
	resp, body := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/schemas", tenantA, "", schemaPayload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/schemas expected 201, got %d: %s", resp.StatusCode, string(body))
	}
	var regSchemaResp struct {
		SchemaID string `json:"schema_id"`
	}
	if err := json.Unmarshal(body, &regSchemaResp); err != nil || !entity.IsValidUUID(regSchemaResp.SchemaID) {
		t.Fatalf("invalid RegisterSchemaResponse: %s", string(body))
	}

	// Duplicate schema name within tenantA must return 409 Conflict
	dupResp, _ := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/schemas", tenantA, "", schemaPayload)
	if dupResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on duplicate schema name, got %d", dupResp.StatusCode)
	}

	// 3. Create Prompt Template (with confidential prompt text that must NEVER appear in logs)
	secretPromptSentinel := "CONFIDENTIAL_PROPRIETARY_SYSTEM_PROMPT_DO_NOT_LOG_98765"
	createTmplPayload := map[string]any{
		"tenant_id":   tenantA,
		"name":        "rag-financial-summary",
		"description": "Synthesizes quarterly SEC filings",
		"schema_id":   regSchemaResp.SchemaID,
		"author":      "platform-architect",
		"raw_ast": map[string]any{
			"system_prompt":   secretPromptSentinel,
			"variable_keys":   []string{"context", "query"},
			"default_dialect": "DIALECT_ANTHROPIC_XML",
			"messages": []map[string]any{
				{
					"role":            "user",
					"content_pattern": "Context:\n<context>{{context}}</context>\nQuery: {{query}}",
				},
			},
			"persona_overrides": map[string]string{
				"cfo": "Focus on EBITDA for {{query}}.",
			},
		},
	}
	resp, body = doJSONRequest(t, http.MethodPost, ts.URL+"/v1/templates", tenantA, "idemp-create-1", createTmplPayload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/templates expected 201, got %d: %s", resp.StatusCode, string(body))
	}
	var tmplResp struct {
		ID             string `json:"id"`
		CurrentVersion int    `json:"current_version"`
	}
	_ = json.Unmarshal(body, &tmplResp)
	if tmplResp.CurrentVersion != 1 {
		t.Fatalf("expected initial version 1, got %d", tmplResp.CurrentVersion)
	}

	// Verify Idempotency-Key header replay
	replayResp, _ := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/templates", tenantA, "idemp-create-1", createTmplPayload)
	if replayResp.StatusCode != http.StatusCreated || replayResp.Header.Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("expected X-Idempotent-Replay=true with 201, got status=%d header=%q",
			replayResp.StatusCode, replayResp.Header.Get("X-Idempotent-Replay"))
	}

	// Verify reusing the same Idempotency-Key with a modified payload returns 422 Unprocessable Entity
	differentPayload := map[string]any{
		"tenant_id":   tenantA,
		"name":        "rag-financial-summary-different",
		"description": "Different payload on same idempotency key",
		"schema_id":   regSchemaResp.SchemaID,
		"author":      "platform-architect",
	}
	mismatchResp, _ := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/templates", tenantA, "idemp-create-1", differentPayload)
	if mismatchResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity when Idempotency-Key is reused with different payload, got %d", mismatchResp.StatusCode)
	}

	// Verify unauthenticated request to administrative endpoint returns 401 Unauthorized
	unauthReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/templates", nil)
	unauthReq.Header.Set("X-Tenant-ID", tenantA)
	unauthResp, err := http.DefaultClient.Do(unauthReq)
	if err != nil {
		t.Fatalf("unauthenticated GET /v1/templates failed: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without verified Bearer JWT, got %d", unauthResp.StatusCode)
	}

	// 4. Publish Version v2 using template name (dual identifier resolution)
	publishV2Payload := map[string]any{
		"schema_id": regSchemaResp.SchemaID,
		"author":    "platform-architect",
		"raw_ast": map[string]any{
			"system_prompt":   secretPromptSentinel + " v2",
			"variable_keys":   []string{"context", "query"},
			"default_dialect": "DIALECT_LLAMA_SPECIAL",
			"messages": []map[string]any{
				{
					"role":            "user",
					"content_pattern": "Context: {{context}}\nQuestion: {{query}}",
				},
			},
		},
	}
	resp, body = doJSONRequest(t, http.MethodPost, ts.URL+"/v1/templates/rag-financial-summary/versions", tenantA, "", publishV2Payload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/templates/{name}/versions expected 201, got %d: %s", resp.StatusCode, string(body))
	}
	var verResp struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(body, &verResp)
	if verResp.Version != 2 {
		t.Fatalf("expected version 2, got %d", verResp.Version)
	}

	// Publishing the exact same AST + schema_id again must be idempotent and return version 2
	resp, body = doJSONRequest(t, http.MethodPost, ts.URL+"/v1/templates/"+tmplResp.ID+"/versions", tenantA, "", publishV2Payload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 on idempotent version publish, got %d", resp.StatusCode)
	}
	_ = json.Unmarshal(body, &verResp)
	if verResp.Version != 2 {
		t.Fatalf("expected idempotent version replay to return version 2, got %d", verResp.Version)
	}

	// 5. Promote @production tag: v1 -> v2 -> rollback to v1 (Runbook 1)
	promoteV1 := map[string]any{"target_version": 1, "expected_version": 0, "updated_by": "release-bot"}
	resp, _ = doJSONRequest(t, http.MethodPut, ts.URL+"/v1/templates/"+tmplResp.ID+"/tags/@production", tenantA, "", promoteV1)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on initial tag promotion, got %d", resp.StatusCode)
	}

	promoteV2 := map[string]any{"target_version": 2, "expected_version": 1, "updated_by": "release-bot"}
	resp, _ = doJSONRequest(t, http.MethodPut, ts.URL+"/v1/templates/rag-financial-summary/tags/@production", tenantA, "", promoteV2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on v2 tag promotion by template name, got %d", resp.StatusCode)
	}

	// Stale expected_version must return 409 Conflict
	stalePromote := map[string]any{"target_version": 1, "expected_version": 1, "updated_by": "release-bot"}
	resp, _ = doJSONRequest(t, http.MethodPut, ts.URL+"/v1/templates/"+tmplResp.ID+"/tags/@production", tenantA, "", stalePromote)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on stale expected_version, got %d", resp.StatusCode)
	}

	// Rollback from v2 -> v1
	rollbackV1 := map[string]any{"target_version": 1, "expected_version": 2, "updated_by": "admin-rollback"}
	resp, _ = doJSONRequest(t, http.MethodPut, ts.URL+"/v1/templates/rag-financial-summary/tags/@production", tenantA, "", rollbackV1)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on rollback promotion, got %d", resp.StatusCode)
	}

	// Resolve @production tag and verify it points to v1
	resp, body = doJSONRequest(t, http.MethodGet, ts.URL+"/v1/templates/rag-financial-summary/tags/@production", tenantA, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET resolveTag expected 200, got %d: %s", resp.StatusCode, string(body))
	}
	var resolvedResp struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(body, &resolvedResp)
	if resolvedResp.Version != 1 {
		t.Fatalf("expected resolved @production version=1 after rollback, got %d", resolvedResp.Version)
	}

	// Verify audit ledger recorded TEMPLATE_CREATED, VERSION_PUBLISHED, TAG_PROMOTED, and TAG_ROLLED_BACK
	audits, err := store.ListByTemplate(context.Background(), tenantA, tmplResp.ID, 50)
	if err != nil || len(audits) != 5 {
		t.Fatalf("expected 5 audit entries for template, got len=%d, err=%v", len(audits), err)
	}
	if audits[4].Action != entity.AuditActionTagRolledBack {
		t.Fatalf("expected final audit action to be TAG_ROLLED_BACK, got %s", audits[4].Action)
	}

	// Verify /metrics exports prompt_registry_pointer_rollbacks_total == 1 after rollback
	metricsResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	metricsBytes, _ := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	if !strings.Contains(string(metricsBytes), "prompt_registry_pointer_rollbacks_total 1") {
		t.Fatalf("expected prompt_registry_pointer_rollbacks_total 1 on /metrics, got:\n%s", string(metricsBytes))
	}

	// 6. Cross-Tenant Isolation Check: Tenant B cannot read Tenant A's template
	resp, _ = doJSONRequest(t, http.MethodGet, ts.URL+"/v1/templates/"+tmplResp.ID+"/versions/1", tenantB, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 NotFound when Tenant B queries Tenant A template, got %d", resp.StatusCode)
	}

	// Cross-Tenant Header vs Body mismatch must return 403 Forbidden
	resp, _ = doJSONRequest(t, http.MethodPost, ts.URL+"/v1/schemas", tenantB, "", schemaPayload)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on header/body tenant mismatch, got %d", resp.StatusCode)
	}

	// 7. Secured Logging Invariant Audit: verify secretPromptSentinel is completely absent from JSON logs
	logsOutput := logBuf.String()
	if strings.Contains(logsOutput, secretPromptSentinel) {
		t.Fatalf("CRITICAL SECURED LOGGING VIOLATION: raw system_prompt leaked in logs:\n%s", logsOutput)
	}
	if !strings.Contains(logsOutput, `"template_hash"`) || !strings.Contains(logsOutput, `"trace_id"`) {
		t.Fatalf("expected structured logs to contain template_hash and trace_id:\n%s", logsOutput)
	}
}

// TestGRPCControlPlaneServerEndToEnd verifies the gRPC control plane service, JWT authentication, and optimistic locking.
func TestGRPCControlPlaneServerEndToEnd(t *testing.T) {
	store := postgres.NewTransactionalMemoryStore()
	l2Cache := redis.NewInMemoryL2Cache()
	invalidator := redis.NewInvalidationBroadcaster(l2Cache, redis.DefaultInvalidationChannel)
	metrics := command.NewMutationMetricsRecorder()

	pubHandler := command.NewPublishVersionHandler(store, l2Cache, nil, metrics)
	promHandler := command.NewPromoteTagHandler(store, l2Cache, invalidator, nil, metrics)
	getHandler := query.NewGetTemplateASTHandler(store, l2Cache)
	listHandler := query.NewListTemplatesHandler(store)

	tenantID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	adminSrv := grpctransport.NewPromptAdminServer(pubHandler, promHandler, getHandler, listHandler, tenantID, nil)
	grpcServer := grpctransport.NewGRPCServer(adminSrv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create gRPC client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := promptv1.NewPromptRegistryServiceClient(conn)

	// Unauthenticated gRPC request (even with x-tenant-id metadata) must be rejected with codes.Unauthenticated
	unauthCtx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-tenant-id", tenantID))
	if _, unauthErr := client.ListTemplates(unauthCtx, &promptv1.ListTemplatesRequest{TenantId: tenantID}); status.Code(unauthErr) != codes.Unauthenticated {
		t.Fatalf("expected codes.Unauthenticated without verified Bearer JWT, got %v", status.Code(unauthErr))
	}

	jwtToken, err := grpctransport.IssueHS256JWT(tenantID, grpctransport.DefaultJWTIssuer, grpctransport.DefaultJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("failed to issue test JWT: %v", err)
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer "+jwtToken,
		"x-tenant-id", tenantID,
	))

	// 1. RegisterSchema via gRPC
	regRes, err := client.RegisterSchema(ctx, &promptv1.RegisterSchemaRequest{
		TenantId:   tenantID,
		Name:       "grpc-schema-contract",
		JsonSchema: `{"type":"object","properties":{"answer":{"type":"string"}}}`,
	})
	if err != nil {
		t.Fatalf("gRPC RegisterSchema failed: %v", err)
	}

	// 2. CreateTemplate via gRPC
	createRes, err := client.CreateTemplate(ctx, &promptv1.CreateTemplateRequest{
		TenantId:    tenantID,
		Name:        "grpc-rag-prompt",
		SchemaId:    regRes.SchemaId,
		Author:      "grpc-tester",
		Description: "gRPC template test",
		RawAst: &promptv1.TemplateAST{
			SystemPrompt:   "Answer using {{context}}.",
			VariableKeys:   []string{"context", "query"},
			DefaultDialect: promptv1.DialectTarget_DIALECT_OPENAI_STANDARD,
			Messages: []*promptv1.MessageBlock{
				{Role: "user", ContentPattern: "Context: {{context}}\nQuery: {{query}}"},
			},
		},
	})
	if err != nil {
		t.Fatalf("gRPC CreateTemplate failed: %v", err)
	}
	if createRes.InitialVersion != 1 {
		t.Fatalf("expected InitialVersion=1, got %d", createRes.InitialVersion)
	}

	// 3. PromoteTag via gRPC & verify optimistic lock conflict returns codes.Aborted
	_, err = client.PromoteTag(ctx, &promptv1.PromoteTagRequest{
		TemplateId:      createRes.TemplateId,
		Tag:             "@production",
		TargetVersion:   1,
		ExpectedVersion: 0,
		UpdatedBy:       "grpc-tester",
		TenantId:        tenantID,
	})
	if err != nil {
		t.Fatalf("gRPC PromoteTag failed: %v", err)
	}

	_, err = client.PromoteTag(ctx, &promptv1.PromoteTagRequest{
		TemplateId:      createRes.TemplateId,
		Tag:             "@production",
		TargetVersion:   1,
		ExpectedVersion: 99, // mismatch
		UpdatedBy:       "grpc-tester",
		TenantId:        tenantID,
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("expected gRPC codes.Aborted on expected_version conflict, got %v", status.Code(err))
	}

	// 4. ResolveTag via gRPC
	resTag, err := client.ResolveTag(ctx, &promptv1.ResolveTagRequest{
		TemplateId: "grpc-rag-prompt",
		Tag:        "@production",
		TenantId:   tenantID,
	})
	if err != nil || resTag.Version != 1 {
		t.Fatalf("gRPC ResolveTag failed: res=%+v, err=%v", resTag, err)
	}
}

// doJSONRequest executes an authenticated HTTP request with a signed Bearer JWT for tenantHeader.
func doJSONRequest(t *testing.T, method, url, tenantHeader, idempKey string, payload any) (*http.Response, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("failed to marshal request payload: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tenantHeader != "" {
		jwtToken, jwtErr := httptransport.IssueHS256JWT(tenantHeader, httptransport.DefaultJWTIssuer, httptransport.DefaultJWTSecret, time.Hour)
		if jwtErr != nil {
			t.Fatalf("failed to sign test JWT: %v", jwtErr)
		}
		req.Header.Set("Authorization", "Bearer "+jwtToken)
		req.Header.Set("X-Tenant-ID", tenantHeader)
	}
	if idempKey != "" {
		req.Header.Set("Idempotency-Key", idempKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request %s %s failed: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	return resp, respBytes
}
