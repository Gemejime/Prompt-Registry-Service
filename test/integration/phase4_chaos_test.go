package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
	"rag-prompt-registry-service/internal/infrastructure/persistence/postgres"
	grpctransport "rag-prompt-registry-service/internal/transport/grpc"
	httptransport "rag-prompt-registry-service/internal/transport/http"
	"rag-prompt-registry-service/pkg/orchestrator"
	"rag-prompt-registry-service/pkg/promptsdk"
	promptv1 "rag-prompt-registry-service/proto/v1"
)

// chaosRetriever supplies a pre-assembled 50 KB RAG context chunk without external network calls.
type chaosRetriever struct {
	chunks []orchestrator.RetrievedChunk
}

// Retrieve returns the configured synthetic context chunks.
func (r *chaosRetriever) Retrieve(_ context.Context, _ string) ([]orchestrator.RetrievedChunk, error) {
	return r.chunks, nil
}

// chaosDispatcher simulates downstream LLM inference forwarding.
type chaosDispatcher struct{}

// Dispatch returns a deterministic synthetic inference completion.
func (d *chaosDispatcher) Dispatch(_ context.Context, req orchestrator.InferenceRequest) (*orchestrator.InferenceResponse, error) {
	return &orchestrator.InferenceResponse{
		ModelID:      req.Model,
		OutputText:   "Synthesized financial answer from cached prompt.",
		FinishReason: "stop",
		TokensUsed:   64,
	}, nil
}

// spyRegistryClient wraps a PromptRegistryServiceClient to count RPC invocations and simulate outages.
type spyRegistryClient struct {
	inner           promptv1.PromptRegistryServiceClient
	offline         atomic.Bool
	resolveTagCalls atomic.Int64
	getVersionCalls atomic.Int64
	getSchemaCalls  atomic.Int64
	artificialDelay time.Duration
}

// CreateTemplate delegates to the underlying gRPC client.
func (s *spyRegistryClient) CreateTemplate(ctx context.Context, in *promptv1.CreateTemplateRequest, opts ...grpc.CallOption) (*promptv1.CreateTemplateResponse, error) {
	return s.inner.CreateTemplate(ctx, in, opts...)
}

// PublishVersion delegates to the underlying gRPC client.
func (s *spyRegistryClient) PublishVersion(ctx context.Context, in *promptv1.PublishVersionRequest, opts ...grpc.CallOption) (*promptv1.PublishVersionResponse, error) {
	return s.inner.PublishVersion(ctx, in, opts...)
}

// GetTemplateVersion counts RPCs and returns Unavailable while offline is true.
func (s *spyRegistryClient) GetTemplateVersion(ctx context.Context, in *promptv1.GetTemplateVersionRequest, opts ...grpc.CallOption) (*promptv1.GetTemplateVersionResponse, error) {
	s.getVersionCalls.Add(1)
	if s.offline.Load() {
		return nil, status.Error(codes.Unavailable, "control plane pod down")
	}
	return s.inner.GetTemplateVersion(ctx, in, opts...)
}

// PromoteTag delegates to the underlying gRPC client.
func (s *spyRegistryClient) PromoteTag(ctx context.Context, in *promptv1.PromoteTagRequest, opts ...grpc.CallOption) (*promptv1.PromoteTagResponse, error) {
	return s.inner.PromoteTag(ctx, in, opts...)
}

// ResolveTag counts RPCs, injects optional backend latency, and returns Unavailable while offline is true.
func (s *spyRegistryClient) ResolveTag(ctx context.Context, in *promptv1.ResolveTagRequest, opts ...grpc.CallOption) (*promptv1.ResolveTagResponse, error) {
	s.resolveTagCalls.Add(1)
	if s.offline.Load() {
		return nil, status.Error(codes.Unavailable, "control plane pod down")
	}
	if s.artificialDelay > 0 {
		time.Sleep(s.artificialDelay)
	}
	return s.inner.ResolveTag(ctx, in, opts...)
}

// RegisterSchema delegates to the underlying gRPC client.
func (s *spyRegistryClient) RegisterSchema(ctx context.Context, in *promptv1.RegisterSchemaRequest, opts ...grpc.CallOption) (*promptv1.RegisterSchemaResponse, error) {
	return s.inner.RegisterSchema(ctx, in, opts...)
}

// GetSchema counts RPCs and returns Unavailable while offline is true.
func (s *spyRegistryClient) GetSchema(ctx context.Context, in *promptv1.GetSchemaRequest, opts ...grpc.CallOption) (*promptv1.GetSchemaResponse, error) {
	s.getSchemaCalls.Add(1)
	if s.offline.Load() {
		return nil, status.Error(codes.Unavailable, "control plane pod down")
	}
	return s.inner.GetSchema(ctx, in, opts...)
}

// ListTemplates delegates to the underlying gRPC client.
func (s *spyRegistryClient) ListTemplates(ctx context.Context, in *promptv1.ListTemplatesRequest, opts ...grpc.CallOption) (*promptv1.ListTemplatesResponse, error) {
	return s.inner.ListTemplates(ctx, in, opts...)
}

// ListSchemas delegates to the underlying gRPC client.
func (s *spyRegistryClient) ListSchemas(ctx context.Context, in *promptv1.ListSchemasRequest, opts ...grpc.CallOption) (*promptv1.ListSchemasResponse, error) {
	return s.inner.ListSchemas(ctx, in, opts...)
}

// setupPhase4ControlPlane boots an in-process gRPC control plane with schema, v1, v2, and @production -> v1.
func setupPhase4ControlPlane(t *testing.T) (
	*postgres.TransactionalMemoryStore,
	*redis.L2CacheClient,
	*redis.PubSubInvalidationBroadcaster,
	*command.MutationMetricsRecorder,
	*command.PromoteTagHandler,
	*grpc.Server,
	net.Listener,
	string,
	string,
) {
	t.Helper()
	store := postgres.NewTransactionalMemoryStore()
	l2Cache := redis.NewInMemoryL2Cache()
	invalidator := redis.NewInvalidationBroadcaster(l2Cache, redis.DefaultInvalidationChannel)
	metrics := command.NewMutationMetricsRecorder()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	pubHandler := command.NewPublishVersionHandler(store, l2Cache, logger, metrics)
	promHandler := command.NewPromoteTagHandler(store, l2Cache, invalidator, logger, metrics)
	getHandler := query.NewGetTemplateASTHandler(store, l2Cache)
	listHandler := query.NewListTemplatesHandler(store)

	tenantID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	adminSrv := grpctransport.NewPromptAdminServer(pubHandler, promHandler, getHandler, listHandler, tenantID, logger)
	grpcServer := grpctransport.NewGRPCServer(adminSrv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(lis) }()

	ctx := context.Background()
	sc, err := pubHandler.RegisterSchema(ctx, command.RegisterSchemaInput{
		TenantID:   tenantID,
		Name:       "phase4-chaos-schema",
		SchemaJSON: []byte(`{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`),
		Author:     "phase4-chaos",
	})
	if err != nil {
		t.Fatalf("RegisterSchema failed: %v", err)
	}

	tmpl, _, err := pubHandler.CreateTemplate(ctx, command.CreateTemplateInput{
		TenantID: tenantID,
		Name:     "phase4-resilient-rag",
		SchemaID: sc.ID,
		Author:   "phase4-chaos",
		RawAST: entity.TemplateAST{
			SystemPrompt:   "You are a financial RAG assistant (v1).",
			VariableKeys:   []string{"context", "query"},
			DefaultDialect: entity.DialectAnthropicXML,
			Messages: []entity.MessageBlock{
				{Role: "user", ContentPattern: "Context:\n<context>{{context}}</context>\n\nQuery: {{query}}"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTemplate failed: %v", err)
	}

	if _, _, err := pubHandler.PublishVersion(ctx, command.PublishVersionInput{
		TenantID:           tenantID,
		TemplateIdentifier: tmpl.ID,
		SchemaID:           sc.ID,
		Author:             "phase4-chaos",
		RawAST: entity.TemplateAST{
			SystemPrompt:   "You are a financial RAG assistant (v2).",
			VariableKeys:   []string{"context", "query"},
			DefaultDialect: entity.DialectAnthropicXML,
			Messages: []entity.MessageBlock{
				{Role: "user", ContentPattern: "Context v2:\n<context>{{context}}</context>\n\nQuery: {{query}}"},
			},
		},
	}); err != nil {
		t.Fatalf("PublishVersion v2 failed: %v", err)
	}

	if _, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantID,
		TemplateIdentifier: tmpl.ID,
		Tag:                "@production",
		TargetVersion:      1,
		ExpectedVersion:    0,
		UpdatedBy:          "phase4-chaos",
	}); err != nil {
		t.Fatalf("PromoteTag v1 failed: %v", err)
	}

	return store, l2Cache, invalidator, metrics, promHandler, grpcServer, lis, tenantID, tmpl.ID
}

// TestPhase4ChaosPodAndRedisOutageInjection verifies 100% orchestrator availability, zero dropped queries,
// and P99 <= 0.5ms rendering latency across 50 KB context blocks during total control plane and Redis outages.
func TestPhase4ChaosPodAndRedisOutageInjection(t *testing.T) {
	_, _, _, _, _, grpcServer, lis, tenantID, _ := setupPhase4ControlPlane(t)

	// Spin up a local TCP Redis Pub/Sub endpoint that will also be terminated during the outage.
	redisLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start mock redis listener: %v", err)
	}
	var redisConnMu sync.Mutex
	var activeRedisConns []net.Conn
	go func() {
		for {
			c, aErr := redisLis.Accept()
			if aErr != nil {
				return
			}
			redisConnMu.Lock()
			activeRedisConns = append(activeRedisConns, c)
			redisConnMu.Unlock()
			go func(conn net.Conn) {
				buf := make([]byte, 512)
				n, rErr := conn.Read(buf)
				if rErr == nil && strings.Contains(string(buf[:n]), "SUBSCRIBE") {
					_, _ = conn.Write([]byte(fmt.Sprintf("*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:1\r\n", len(redis.DefaultInvalidationChannel), redis.DefaultInvalidationChannel)))
				}
			}(c)
		}
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	sdkClient, err := promptsdk.NewClient(promptsdk.ClientConfig{
		RegistryClient: promptv1.NewPromptRegistryServiceClient(conn),
		TenantID:       tenantID,
		RedisAddrs:     []string{redisLis.Addr().String()},
		NodeID:         "phase4-outage-node",
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("promptsdk.NewClient failed: %v", err)
	}
	defer func() { _ = sdkClient.Close() }()

	// Wait for Redis Pub/Sub gauge == 1 before outage injection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !sdkClient.IsPubSubConnected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !sdkClient.IsPubSubConnected() {
		t.Fatalf("expected Pub/Sub to connect before outage injection")
	}

	chunk50KB := strings.Repeat("Enterprise RAG audited balance sheet segment. ", 1024) // ~48 KB
	pipeline, err := orchestrator.NewRAGPipeline(orchestrator.PipelineConfig{
		PromptSDK:    sdkClient,
		Retriever:    &chaosRetriever{chunks: []orchestrator.RetrievedChunk{{ChunkID: "c1", DocumentID: "doc1", Text: chunk50KB, Score: 0.99}}},
		Dispatcher:   &chaosDispatcher{},
		DefaultModel: "claude-3-5-sonnet",
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewRAGPipeline failed: %v", err)
	}

	ctx := context.Background()
	// Warmup while Control Plane & Redis are healthy.
	warmResp, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:        "Summarize audited balance sheet",
		TemplateID:   "phase4-resilient-rag",
		TagOrVersion: "@production",
		Mode:         orchestrator.ExecutionModeProduction,
	})
	if err != nil || !strings.Contains(warmResp.OutputText, "Synthesized financial answer") {
		t.Fatalf("warmup failed: resp=%+v, err=%v", warmResp, err)
	}

	// INJECT CHAOS: Kill both Control Plane gRPC pod and Redis server!
	grpcServer.Stop()
	_ = lis.Close()
	_ = redisLis.Close()
	redisConnMu.Lock()
	for _, c := range activeRedisConns {
		_ = c.Close()
	}
	redisConnMu.Unlock()

	// Confirm SDK Pub/Sub gauge transitions to 0 (fallback 5m TTL mode).
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sdkClient.IsPubSubConnected() {
		time.Sleep(5 * time.Millisecond)
	}
	if sdkClient.IsPubSubConnected() {
		t.Fatalf("expected IsPubSubConnected == false after Redis termination")
	}

	// Run sustained concurrent orchestrator workload during complete Control Plane + Redis outage.
	const workers = 16
	const reqsPerWorker = 100
	totalReqs := workers * reqsPerWorker

	var wg sync.WaitGroup
	errCh := make(chan error, totalReqs)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < reqsPerWorker; i++ {
				resp, execErr := pipeline.Execute(ctx, orchestrator.PipelineRequest{
					Query:        "Quarterly revenue breakdown",
					TemplateID:   "phase4-resilient-rag",
					TagOrVersion: "@production",
					Mode:         orchestrator.ExecutionModeProduction,
				})
				if execErr != nil {
					errCh <- execErr
					return
				}
				if !strings.Contains(resp.OutputText, "Synthesized financial answer") || len(resp.CompiledPrompt) < 40000 {
					errCh <- fmt.Errorf("corrupted outage response: len=%d", len(resp.CompiledPrompt))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for execErr := range errCh {
		t.Fatalf("dropped or failed request during total outage: %v", execErr)
	}

	// Measure SDK zero-allocation RenderToBuffer P99 latency across 1,000 50 KB renders during outage.
	outageVars := map[string]string{
		"context": chunk50KB,
		"query":   "Quarterly revenue breakdown",
	}
	outageOpts := promptsdk.RenderOptions{
		TargetDialect: promptsdk.DialectAnthropicXML,
		Variables:     outageVars,
	}
	for i := 0; i < 50; i++ {
		b := promptsdk.AcquireBuffer()
		_ = sdkClient.RenderToBuffer(ctx, b, "phase4-resilient-rag", "@production", outageOpts)
		promptsdk.ReleaseBuffer(b)
	}

	const latencyIters = 1000
	renderDurations := make([]time.Duration, latencyIters)
	var clk preciseClock
	for i := 0; i < latencyIters; i++ {
		t0 := clk.now()
		b := promptsdk.AcquireBuffer()
		rErr := sdkClient.RenderToBuffer(ctx, b, "phase4-resilient-rag", "@production", outageOpts)
		promptsdk.ReleaseBuffer(b)
		renderDurations[i] = time.Duration(clk.now() - t0)
		if rErr != nil {
			t.Fatalf("RenderToBuffer during outage failed: %v", rErr)
		}
	}

	sort.Slice(renderDurations, func(i, j int) bool { return renderDurations[i] < renderDurations[j] })
	p50 := renderDurations[latencyIters/2]
	p99 := renderDurations[int(float64(latencyIters)*0.99)]
	var totalRender time.Duration
	for _, d := range renderDurations {
		totalRender += d
	}
	avgRender := totalRender / latencyIters
	t.Logf("Outage %d concurrent orchestrator requests (100%% success) | %d SDK outage renders avg=%v, P50=%v, P99=%v",
		totalReqs, latencyIters, avgRender, p50, p99)
	if avgRender > 500*time.Microsecond {
		t.Fatalf("average SDK render latency during outage %v exceeded 0.5ms SLO", avgRender)
	}
	if os.Getenv("CI") == "" {
		if p99 > 500*time.Microsecond {
			t.Fatalf("P99 render latency during outage %v exceeded 0.5ms SLO", p99)
		}
	} else {
		if p99 > 5*time.Millisecond {
			t.Fatalf("P99 render latency during outage %v exceeded 5ms CI jitter guard", p99)
		}
		t.Logf("Skipping strict 0.5ms P99 assertion on CI runner (CI=%q); observed P99=%v", os.Getenv("CI"), p99)
	}
}

// TestPhase4ThunderingHerdSingleflightRecovery verifies that when 1,000 concurrent goroutines
// simultaneously request a refreshed tag pointer upon control plane restoration, singleflight
// coalescing issues exactly 1 gRPC ResolveTag call without thread starvation or socket leaks.
func TestPhase4ThunderingHerdSingleflightRecovery(t *testing.T) {
	_, _, _, _, promHandler, grpcServer, lis, tenantID, tmplID := setupPhase4ControlPlane(t)
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	spy := &spyRegistryClient{
		inner:           promptv1.NewPromptRegistryServiceClient(conn),
		artificialDelay: 25 * time.Millisecond,
	}

	sdkClient, err := promptsdk.NewClient(promptsdk.ClientConfig{
		RegistryClient:       spy,
		TenantID:             tenantID,
		TagTTL:               5 * time.Minute,
		OfflineRetryCooldown: 5 * time.Second,
		NodeID:               "phase4-singleflight-node",
		Logger:               slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("promptsdk.NewClient failed: %v", err)
	}
	defer func() { _ = sdkClient.Close() }()

	ctx := context.Background()
	// Warm v1 while online.
	outV1, err := sdkClient.Render(ctx, "phase4-resilient-rag", "@production", promptsdk.RenderOptions{
		Variables: map[string]string{"context": "ctx", "query": "q1"},
	})
	if err != nil || !strings.Contains(string(outV1), "(v1)") {
		t.Fatalf("warmup v1 failed: out=%q, err=%v", string(outV1), err)
	}

	// Simulate outage: control plane unreachable, promote v2 once restored, advance clock past 5m TTL.
	spy.offline.Store(true)
	if _, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantID,
		TemplateIdentifier: tmplID,
		Tag:                "@production",
		TargetVersion:      2,
		ExpectedVersion:    1,
		UpdatedBy:          "phase4-recovery",
	}); err != nil {
		t.Fatalf("PromoteTag v2 failed: %v", err)
	}

	// Advance clock by 6 minutes: stale-while-offline triggers 1 failed refresh attempt and enters cooldown.
	sdkClient.Cache().SetClock(func() time.Time { return time.Now().Add(6 * time.Minute) })
	staleOut, err := sdkClient.Render(ctx, "phase4-resilient-rag", "@production", promptsdk.RenderOptions{
		Variables: map[string]string{"context": "ctx", "query": "stale"},
	})
	if err != nil || !strings.Contains(string(staleOut), "(v1)") {
		t.Fatalf("expected stale-while-offline v1 render, got %q, err=%v", string(staleOut), err)
	}

	// Restore control plane and advance clock past retry cooldown so next wave triggers refresh.
	spy.offline.Store(false)
	spy.resolveTagCalls.Store(0)
	sdkClient.Cache().SetClock(func() time.Time { return time.Now().Add(12 * time.Minute) })

	const herdSize = 1000
	startGate := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, herdSize)

	for i := 0; i < herdSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			buf := promptsdk.AcquireBuffer()
			defer promptsdk.ReleaseBuffer(buf)
			rErr := sdkClient.RenderToBuffer(ctx, buf, "phase4-resilient-rag", "@production", promptsdk.RenderOptions{
				Variables: map[string]string{"context": "herd-ctx", "query": "herd-q"},
			})
			if rErr != nil {
				errCh <- rErr
				return
			}
			if !strings.Contains(buf.String(), "(v2)") {
				errCh <- fmt.Errorf("expected refreshed v2 prompt after recovery, got %q", buf.String())
			}
		}()
	}

	// Release all 1,000 goroutines simultaneously.
	close(startGate)
	wg.Wait()
	close(errCh)

	for rErr := range errCh {
		t.Fatalf("thundering herd render error: %v", rErr)
	}

	if got := spy.resolveTagCalls.Load(); got != 1 {
		t.Fatalf("expected singleflight barrier to emit exactly 1 ResolveTag RPC for %d concurrent goroutines, got %d", herdSize, got)
	}
	t.Logf("Singleflight barrier verified: %d concurrent goroutines coalesced into %d gRPC ResolveTag call", herdSize, spy.resolveTagCalls.Load())
}

// TestPhase4PubSubFlappingAndGoroutineCleanup verifies exponential backoff with jitter across
// rapid Redis Pub/Sub disconnects/reconnects, gauge 0/1 flips, and zero goroutine leaks on Close().
func TestPhase4PubSubFlappingAndGoroutineCleanup(t *testing.T) {
	// Stabilize runtime goroutines before capturing baseline.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() { _ = lis.Close() }()

	connCh := make(chan net.Conn, 16)
	go func() {
		for {
			c, aErr := lis.Accept()
			if aErr != nil {
				return
			}
			connCh <- c
		}
	}()

	tenantID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	client, err := promptsdk.NewClient(promptsdk.ClientConfig{
		TenantID:   tenantID,
		RedisAddrs: []string{lis.Addr().String()},
		NodeID:     "phase4-flapping-node",
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Preload a tagged entry so we can also confirm live invalidation after flapping recovers.
	if _, err := client.PreloadTag("tmpl-flap-uuid", "flap-template", "@production", 1, promptsdk.TemplateAST{
		SystemPrompt:   "Flap v1",
		VariableKeys:   []string{"query"},
		DefaultDialect: promptsdk.DialectAnthropicXML,
		Messages:       []promptsdk.MessageBlock{{Role: "user", ContentPattern: "{{query}}"}},
	}, nil); err != nil {
		t.Fatalf("PreloadTag failed: %v", err)
	}

	subscribeReply := fmt.Sprintf("*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:1\r\n", len(redis.DefaultInvalidationChannel), redis.DefaultInvalidationChannel)

	// Cycle 1: Drop connection immediately BEFORE subscribe confirmation (exercises backoff progression).
	var c1 net.Conn
	select {
	case c1 = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for cycle 1 connection")
	}
	_ = c1.Close()

	// Cycle 2 & 3: Confirm subscribe (gauge -> 1), then drop connection (gauge -> 0).
	for cycle := 2; cycle <= 3; cycle++ {
		var c net.Conn
		select {
		case c = <-connCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for cycle %d reconnect", cycle)
		}

		buf := make([]byte, 512)
		if _, rErr := c.Read(buf); rErr != nil {
			t.Fatalf("cycle %d failed reading SUBSCRIBE: %v", cycle, rErr)
		}
		_, _ = c.Write([]byte(subscribeReply))

		deadline := time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) && !client.IsPubSubConnected() {
			time.Sleep(5 * time.Millisecond)
		}
		if !client.IsPubSubConnected() {
			t.Fatalf("cycle %d expected prompt_sdk_pubsub_connected == 1", cycle)
		}
		if !strings.Contains(client.MetricsExposition(), `prompt_sdk_pubsub_connected{node_id="phase4-flapping-node"} 1`) {
			t.Fatalf("expected gauge 1 in MetricsExposition on cycle %d", cycle)
		}

		// Force disconnect to simulate flapping.
		_ = c.Close()

		deadline = time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) && client.IsPubSubConnected() {
			time.Sleep(5 * time.Millisecond)
		}
		if client.IsPubSubConnected() {
			t.Fatalf("cycle %d expected prompt_sdk_pubsub_connected == 0 after disconnect", cycle)
		}
		if !strings.Contains(client.MetricsExposition(), `prompt_sdk_pubsub_connected{node_id="phase4-flapping-node"} 0`) {
			t.Fatalf("expected gauge 0 in MetricsExposition on cycle %d", cycle)
		}
	}

	// Cycle 4: Stable reconnect + live invalidation delivery.
	var c4 net.Conn
	select {
	case c4 = <-connCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for cycle 4 reconnect")
	}
	defer func() { _ = c4.Close() }()

	buf := make([]byte, 512)
	_, _ = c4.Read(buf)
	_, _ = c4.Write([]byte(subscribeReply))

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && !client.IsPubSubConnected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !client.IsPubSubConnected() {
		t.Fatalf("expected cycle 4 to reconnect")
	}

	payload := fmt.Sprintf(`{"event_id":"01PHASE4FLAP00000000000001","tenant_id":"%s","template_id":"tmpl-flap-uuid","tag":"@production","new_version":2,"timestamp":"%s"}`,
		tenantID, time.Now().UTC().Format(time.RFC3339))
	msgFrame := fmt.Sprintf("*3\r\n$7\r\nmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
		len(redis.DefaultInvalidationChannel), redis.DefaultInvalidationChannel, len(payload), payload)
	_, _ = c4.Write([]byte(msgFrame))

	deadline = time.Now().Add(1 * time.Second)
	evicted := false
	for time.Now().Before(deadline) {
		if _, _, ok := client.Cache().GetTag("flap-template", "@production"); !ok {
			evicted = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !evicted {
		t.Fatalf("expected invalidation message after flapping recovery to evict L1_tag")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("client.Close failed: %v", err)
	}

	// Verify no orphan subscriber goroutines remain.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baselineGoroutines+1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baselineGoroutines+1 {
		t.Fatalf("goroutine leak detected after client.Close(): baseline=%d, after=%d", baselineGoroutines, after)
	}
}

// TestPhase4RollbackUnderPartialRedisFailure verifies Runbook 1 & Runbook 2 behavior when Redis
// fails during a pointer rollback: PostgreSQL/store commits atomically, broadcast_emitted is false,
// prompt_registry_pointer_rollbacks_total & error metrics increment, and republish/Purge restores coherence.
func TestPhase4RollbackUnderPartialRedisFailure(t *testing.T) {
	store, l2Cache, invalidator, metrics, promHandler, grpcServer, lis, tenantID, tmplID := setupPhase4ControlPlane(t)
	defer grpcServer.Stop()

	pubHandler := command.NewPublishVersionHandler(store, l2Cache, slog.New(slog.NewJSONHandler(io.Discard, nil)), metrics)
	getHandler := query.NewGetTemplateASTHandler(store, l2Cache)
	listHandler := query.NewListTemplatesHandler(store)

	router := httptransport.NewRouter(httptransport.RouterConfig{
		PublishHandler: pubHandler,
		PromoteHandler: promHandler,
		GetASTHandler:  getHandler,
		ListHandler:    listHandler,
		MutationMetric: metrics,
		Invalidator:    invalidator,
		DefaultTenant:  tenantID,
		WriteRateRPS:   1000,
		WriteRateBurst: 1000,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(router)
	defer ts.Close()

	ctx := context.Background()
	// Promote @production -> v2 while Redis is healthy.
	if _, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantID,
		TemplateIdentifier: tmplID,
		Tag:                "@production",
		TargetVersion:      2,
		ExpectedVersion:    1,
		UpdatedBy:          "release-pipeline",
	}); err != nil {
		t.Fatalf("PromoteTag v2 failed: %v", err)
	}

	// Spin up 2 orchestrator SDK clients subscribed to the invalidation channel and warm them at v2.
	sub1, unsub1 := l2Cache.SubscribeChannel(ctx, redis.DefaultInvalidationChannel)
	defer unsub1()
	sub2, unsub2 := l2Cache.SubscribeChannel(ctx, redis.DefaultInvalidationChannel)
	defer unsub2()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient failed: %v", err)
	}
	defer func() { _ = conn.Close() }()
	grpcClient := promptv1.NewPromptRegistryServiceClient(conn)

	sdk1, err := promptsdk.NewClient(promptsdk.ClientConfig{
		RegistryClient:  grpcClient,
		TenantID:        tenantID,
		InvalidationSub: sub1,
		NodeID:          "node-1",
		Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("sdk1 NewClient failed: %v", err)
	}
	defer func() { _ = sdk1.Close() }()

	sdk2, err := promptsdk.NewClient(promptsdk.ClientConfig{
		RegistryClient:  grpcClient,
		TenantID:        tenantID,
		InvalidationSub: sub2,
		NodeID:          "node-2",
		Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("sdk2 NewClient failed: %v", err)
	}
	defer func() { _ = sdk2.Close() }()

	for _, c := range []*promptsdk.Client{sdk1, sdk2} {
		out, rErr := c.Render(ctx, "phase4-resilient-rag", "@production", promptsdk.RenderOptions{
			Variables: map[string]string{"context": "c", "query": "q"},
		})
		if rErr != nil || !strings.Contains(string(out), "(v2)") {
			t.Fatalf("expected v2 warmup render, got %q, err=%v", string(out), rErr)
		}
	}

	// Inject partial Redis failure before executing Runbook 1 rollback.
	invalidator.SetForceFail(true)

	jwtToken, err := httptransport.IssueHS256JWT(tenantID, httptransport.DefaultJWTIssuer, httptransport.DefaultJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("IssueHS256JWT failed: %v", err)
	}

	doRollbackHTTP := func(expectedVer int, updatedBy string) (int, bool) {
		payload, _ := json.Marshal(map[string]any{
			"target_version":   1,
			"expected_version": expectedVer,
			"updated_by":       updatedBy,
		})
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/templates/phase4-resilient-rag/tags/@production", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+jwtToken)
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatalf("HTTP rollback request failed: %v", doErr)
		}
		defer func() { _ = resp.Body.Close() }()
		bodyBytes, _ := io.ReadAll(resp.Body)
		var out struct {
			TargetVersion    int  `json:"target_version"`
			BroadcastEmitted bool `json:"broadcast_emitted"`
		}
		_ = json.Unmarshal(bodyBytes, &out)
		return resp.StatusCode, out.BroadcastEmitted
	}

	statusCode, broadcastEmitted := doRollbackHTTP(2, "admin-rollback")
	if statusCode != http.StatusOK {
		t.Fatalf("expected 200 OK even during Redis publish failure, got %d", statusCode)
	}
	if broadcastEmitted {
		t.Fatalf("expected broadcast_emitted == false under Redis failure")
	}

	// Verify DB pointer is durably committed at v1 and audit ledger recorded TAG_ROLLED_BACK.
	resolvedVer, err := getHandler.ResolveTag(ctx, tenantID, tmplID, "@production")
	if err != nil || resolvedVer.Version != 1 {
		t.Fatalf("expected DB/L2 resolved pointer at v1, got ver=%+v, err=%v", resolvedVer, err)
	}
	audits, err := store.ListByTemplate(ctx, tenantID, tmplID, 50)
	if err != nil || len(audits) == 0 || audits[len(audits)-1].Action != entity.AuditActionTagRolledBack {
		t.Fatalf("expected final audit action TAG_ROLLED_BACK, got %+v, err=%v", audits, err)
	}

	// Verify Prometheus /metrics exports ElevatedPointerRollbacks and PubSubInvalidationFailure telemetry.
	mResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	mBody, _ := io.ReadAll(mResp.Body)
	_ = mResp.Body.Close()
	mStr := string(mBody)
	if !strings.Contains(mStr, "prompt_registry_pointer_rollbacks_total 1") {
		t.Fatalf("expected prompt_registry_pointer_rollbacks_total 1 in /metrics:\n%s", mStr)
	}
	if !strings.Contains(mStr, `prompt_invalidation_events_published_total{status="error"} 1`) {
		t.Fatalf("expected invalidation publish error metric == 1 in /metrics:\n%s", mStr)
	}

	// Recovery Path A (Runbook 2): local Purge() on sdk2 immediately recovers coherence to v1.
	sdk2.Purge()
	out2, err := sdk2.Render(ctx, "phase4-resilient-rag", "@production", promptsdk.RenderOptions{
		Variables: map[string]string{"context": "c", "query": "after-purge"},
	})
	if err != nil || !strings.Contains(string(out2), "(v1)") {
		t.Fatalf("expected sdk2 to render v1 after Purge(), got %q, err=%v", string(out2), err)
	}

	// Recovery Path B (Runbook 1): Restore Redis and re-issue PUT with expected_version == 1 to emit broadcast.
	invalidator.SetForceFail(false)
	statusCode, broadcastEmitted = doRollbackHTTP(1, "admin-rollback-republish")
	if statusCode != http.StatusOK || !broadcastEmitted {
		t.Fatalf("expected republish 200 OK with broadcast_emitted=true, got status=%d emitted=%v", statusCode, broadcastEmitted)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, _, ok := sdk1.Cache().GetTag("phase4-resilient-rag", "@production"); !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	out1, err := sdk1.Render(ctx, "phase4-resilient-rag", "@production", promptsdk.RenderOptions{
		Variables: map[string]string{"context": "c", "query": "after-republish"},
	})
	if err != nil || !strings.Contains(string(out1), "(v1)") {
		t.Fatalf("expected sdk1 to render v1 after republish broadcast, got %q, err=%v", string(out1), err)
	}
}
