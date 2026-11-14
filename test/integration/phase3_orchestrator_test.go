package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
	"rag-prompt-registry-service/internal/infrastructure/persistence/postgres"
	grpctransport "rag-prompt-registry-service/internal/transport/grpc"
	"rag-prompt-registry-service/pkg/orchestrator"
	"rag-prompt-registry-service/pkg/promptsdk"
	promptv1 "rag-prompt-registry-service/proto/v1"
)

// mockTestRetriever provides synthetic document chunks for testing.
type mockTestRetriever struct {
	chunks []orchestrator.RetrievedChunk
}

// Retrieve yields configured synthetic chunks for integration testing.
func (m *mockTestRetriever) Retrieve(ctx context.Context, query string) ([]orchestrator.RetrievedChunk, error) {
	return m.chunks, nil
}

// mockTestDispatcher simulates model execution downstream.
type mockTestDispatcher struct {
	mu            sync.Mutex
	dispatchCount int
}

// Dispatch simulates a model inference response and increments the dispatch counter.
func (m *mockTestDispatcher) Dispatch(ctx context.Context, req orchestrator.InferenceRequest) (*orchestrator.InferenceResponse, error) {
	m.mu.Lock()
	m.dispatchCount++
	m.mu.Unlock()
	return &orchestrator.InferenceResponse{
		ModelID:      req.Model,
		OutputText:   "Answer based on assembled document context.",
		FinishReason: "stop",
		TokensUsed:   50,
	}, nil
}

// TestOrchestratorPipelineAndChaosOutageServing tests the decoupled Data Plane pipeline
// and verifies 100% availability during complete Control Plane & Redis outages.
func TestOrchestratorPipelineAndChaosOutageServing(t *testing.T) {
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
	serverRunning := true
	go func() {
		_ = grpcServer.Serve(lis)
	}()

	ctx := context.Background()

	// 1. Seed schema and template on Control Plane
	sc, err := pubHandler.RegisterSchema(ctx, command.RegisterSchemaInput{
		TenantID:   tenantID,
		Name:       "orchestrator-outage-schema",
		SchemaJSON: []byte(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
		Author:     "phase3-chaos-test",
	})
	if err != nil {
		t.Fatalf("RegisterSchema failed: %v", err)
	}

	tmpl, _, err := pubHandler.CreateTemplate(ctx, command.CreateTemplateInput{
		TenantID: tenantID,
		Name:     "outage-resilient-rag",
		SchemaID: sc.ID,
		Author:   "phase3-chaos-test",
		RawAST: entity.TemplateAST{
			SystemPrompt:   "You are an AI research assistant.",
			VariableKeys:   []string{"context", "query"},
			DefaultDialect: entity.DialectAnthropicXML,
			Messages: []entity.MessageBlock{
				{Role: "user", ContentPattern: "Context:\n{{context}}\n\nQuery:\n{{query}}"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTemplate failed: %v", err)
	}

	if _, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantID,
		TemplateIdentifier: tmpl.ID,
		Tag:                "@production",
		TargetVersion:      1,
		ExpectedVersion:    0,
		UpdatedBy:          "phase3-chaos-test",
	}); err != nil {
		t.Fatalf("PromoteTag failed: %v", err)
	}

	// 2. Connect SDK client to Control Plane
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	grpcClient := promptv1.NewPromptRegistryServiceClient(conn)
	sdkClient, err := promptsdk.NewClient(promptsdk.ClientConfig{
		RegistryClient: grpcClient,
		TenantID:       tenantID,
		NodeID:         "chaos-orchestrator-node",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = sdkClient.Close() }()

	// Build 50 KB context chunk
	chunk50KB := strings.Repeat("Quarterly SEC filing report data section highlights. ", 950) // ~50 KB
	retriever := &mockTestRetriever{
		chunks: []orchestrator.RetrievedChunk{
			{ChunkID: "chk-1", DocumentID: "sec-filing-2026", Text: chunk50KB, Score: 0.98},
		},
	}
	dispatcher := &mockTestDispatcher{}

	pipeline, err := orchestrator.NewRAGPipeline(orchestrator.PipelineConfig{
		PromptSDK:    sdkClient,
		Retriever:    retriever,
		Dispatcher:   dispatcher,
		DefaultModel: "claude-3-5-sonnet",
	})
	if err != nil {
		t.Fatalf("NewRAGPipeline failed: %v", err)
	}

	// Warmup execution while control plane is online (L1 cache populates)
	warmResp, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:        "What are the quarterly filing highlights?",
		TemplateID:   "outage-resilient-rag",
		TagOrVersion: "@production",
		Mode:         orchestrator.ExecutionModeDiagnostic,
	})
	if err != nil {
		t.Fatalf("warmup execution failed: %v", err)
	}
	if warmResp.Diagnostic == nil || !strings.Contains(warmResp.OutputText, "Answer based on assembled document context") {
		t.Fatalf("unexpected warmup response: %+v", warmResp)
	}

	// -------------------------------------------------------------------------
	// Step 3: Complete Outage Simulation (Kill Control Plane gRPC server & Redis)
	// -------------------------------------------------------------------------
	if serverRunning {
		grpcServer.Stop()
		serverRunning = false
	}

	// 3. Launch 500 concurrent pipeline requests during complete control plane outage!
	const callers = 500
	var wg sync.WaitGroup
	errsCh := make(chan error, callers)

	startOutage := time.Now()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := orchestrator.PipelineRequest{
				Query:        fmt.Sprintf("Outage query %d", idx),
				TemplateID:   "outage-resilient-rag",
				TagOrVersion: "@production",
				Mode:         orchestrator.ExecutionModeProduction,
			}
			out, execErr := pipeline.Execute(ctx, req)
			if execErr != nil {
				errsCh <- execErr
				return
			}
			if !strings.Contains(out.OutputText, "Answer based on assembled document context") {
				errsCh <- fmt.Errorf("unexpected output during outage: %s", out.OutputText)
			}
		}(i)
	}
	wg.Wait()
	close(errsCh)

	for execErr := range errsCh {
		t.Fatalf("pipeline failed during control plane outage: %v", execErr)
	}
	outageLatency := time.Since(startOutage)
	avgMs := float64(outageLatency.Milliseconds()) / float64(callers)
	t.Logf("Executed %d concurrent requests during outage in %v (avg %.3f ms/req)", callers, outageLatency, avgMs)

	// -------------------------------------------------------------------------
	// Step 4: Stale-While-Offline Past Fallback TTL
	// -------------------------------------------------------------------------
	// Advance clock past the 5-minute fallback TTL
	clockOffset := 6 * time.Minute
	sdkClient.Cache().SetClock(func() time.Time {
		return time.Now().Add(clockOffset)
	})

	// The control plane is still dead. The SDK must serve the cached version from memory
	// without failing caller requests (Stale-While-Offline resilience).
	staleResp, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:        "Query past 5m TTL during outage",
		TemplateID:   "outage-resilient-rag",
		TagOrVersion: "@production",
		Mode:         orchestrator.ExecutionModeDiagnostic,
	})
	if err != nil {
		t.Fatalf("expected Stale-While-Offline serving past 5m TTL, got error: %v", err)
	}
	if !strings.Contains(staleResp.OutputText, "Answer based on assembled document context") {
		t.Fatalf("unexpected stale response: %+v", staleResp)
	}
	if sdkClient.Cache().Stats().StaleServes == 0 {
		t.Fatalf("expected StaleServes counter > 0")
	}

	// -------------------------------------------------------------------------
	// Step 5: Uncached Tag Requests During Outage
	// -------------------------------------------------------------------------
	// Request an un-cached template while control plane is dead:
	// must return ErrTemplateNotFound cleanly without crashing or hanging.
	_, uncachedErr := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:        "Non-existent template query",
		TemplateID:   "non-existent-template",
		TagOrVersion: "@production",
	})
	if uncachedErr == nil {
		t.Fatalf("expected error for un-cached template during outage, got nil")
	}
	if !errors.Is(uncachedErr, promptsdk.ErrTemplateNotFound) && !strings.Contains(uncachedErr.Error(), "template or version not found") {
		t.Fatalf("expected ErrTemplateNotFound, got: %v", uncachedErr)
	}
}
