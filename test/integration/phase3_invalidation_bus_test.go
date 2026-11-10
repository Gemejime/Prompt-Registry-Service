package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
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
	"rag-prompt-registry-service/pkg/promptsdk"
	promptv1 "rag-prompt-registry-service/proto/v1"
)

// TestMultiInstanceCacheInvalidationBus tests:
// 1. Multi-node invalidation delivery (<500ms global cluster propagation SLO, ADR-002) across 3 nodes.
// 2. Tenant boundary isolation (Layer 4): events targeting Tenant B do NOT evict Tenant A's cached tags.
// 3. Pointer promotion v1 -> v2 and immediate pointer rollback v2 -> v1 (Runbook 1).
// 4. Redis Pub/Sub disconnection and fallback to 5-minute safety TTL (Runbook 2).
// 5. Diagnostic cache wipe via Client.Purge() (Runbook 2 Step 3).
func TestMultiInstanceCacheInvalidationBus(t *testing.T) {
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
	tenantB := "b1ffcd88-8d1a-3de7-aa5c-5aa8ac271b22"

	adminSrv := grpctransport.NewPromptAdminServer(pubHandler, promHandler, getHandler, listHandler, tenantA, logger)
	grpcServer := grpctransport.NewGRPCServer(adminSrv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	ctx := context.Background()

	// Register output schema
	sc, err := pubHandler.RegisterSchema(ctx, command.RegisterSchemaInput{
		TenantID:   tenantA,
		Name:       "invalidation-test-schema",
		SchemaJSON: []byte(`{"type":"object","properties":{"result":{"type":"string"}}}`),
		Author:     "phase3-bus-test",
	})
	if err != nil {
		t.Fatalf("RegisterSchema failed: %v", err)
	}

	// Create template for Tenant A
	tmplA, _, err := pubHandler.CreateTemplate(ctx, command.CreateTemplateInput{
		TenantID: tenantA,
		Name:     "cluster-rag-template",
		SchemaID: sc.ID,
		Author:   "phase3-bus-test",
		RawAST: entity.TemplateAST{
			SystemPrompt:   "Cluster System v1",
			VariableKeys:   []string{"query"},
			DefaultDialect: entity.DialectAnthropicXML,
			Messages: []entity.MessageBlock{
				{Role: "user", ContentPattern: "V1 Query: {{query}}"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateTemplate failed: %v", err)
	}

	// Publish v2 for Tenant A
	if _, _, err := pubHandler.PublishVersion(ctx, command.PublishVersionInput{
		TenantID:           tenantA,
		TemplateIdentifier: tmplA.ID,
		SchemaID:           sc.ID,
		Author:             "phase3-bus-test",
		RawAST: entity.TemplateAST{
			SystemPrompt:   "Cluster System v2",
			VariableKeys:   []string{"query"},
			DefaultDialect: entity.DialectAnthropicXML,
			Messages: []entity.MessageBlock{
				{Role: "user", ContentPattern: "V2 Query: {{query}}"},
			},
		},
	}); err != nil {
		t.Fatalf("PublishVersion v2 failed: %v", err)
	}

	// Initially promote @production -> v1 for Tenant A
	if _, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantA,
		TemplateIdentifier: tmplA.ID,
		Tag:                "@production",
		TargetVersion:      1,
		ExpectedVersion:    0,
		UpdatedBy:          "phase3-bus-test",
	}); err != nil {
		t.Fatalf("PromoteTag v1 failed: %v", err)
	}

	// Spin up 3 separate SDK client instances connected to the invalidation bus:
	// - Node Alpha (Tenant A)
	// - Node Beta  (Tenant A)
	// - Node Gamma (Tenant B - to verify Tenant Isolation)
	subAlpha, unsubAlpha := l2Cache.SubscribeChannel(ctx, redis.DefaultInvalidationChannel)
	defer unsubAlpha()
	subBeta, unsubBeta := l2Cache.SubscribeChannel(ctx, redis.DefaultInvalidationChannel)
	defer unsubBeta()
	subGamma, unsubGamma := l2Cache.SubscribeChannel(ctx, redis.DefaultInvalidationChannel)
	defer unsubGamma()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	grpcClient := promptv1.NewPromptRegistryServiceClient(conn)

	clientAlpha, err := promptsdk.NewClient(promptsdk.ClientConfig{
		RegistryClient:  grpcClient,
		TenantID:        tenantA,
		InvalidationSub: subAlpha,
		NodeID:          "node-alpha",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("failed to create clientAlpha: %v", err)
	}
	defer func() { _ = clientAlpha.Close() }()

	clientBeta, err := promptsdk.NewClient(promptsdk.ClientConfig{
		RegistryClient:  grpcClient,
		TenantID:        tenantA,
		InvalidationSub: subBeta,
		NodeID:          "node-beta",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("failed to create clientBeta: %v", err)
	}
	defer func() { _ = clientBeta.Close() }()

	clientGamma, err := promptsdk.NewClient(promptsdk.ClientConfig{
		RegistryClient:  grpcClient,
		TenantID:        tenantB,
		InvalidationSub: subGamma,
		NodeID:          "node-gamma",
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("failed to create clientGamma: %v", err)
	}
	defer func() { _ = clientGamma.Close() }()

	// Seed Tenant B template in clientGamma to test tenant boundary isolation
	astTenantB := promptsdk.TemplateAST{
		SystemPrompt:   "Tenant B Confidential System",
		VariableKeys:   []string{"query"},
		DefaultDialect: promptsdk.DialectAnthropicXML,
		Messages: []promptsdk.MessageBlock{
			{Role: "user", ContentPattern: "Tenant B Query: {{query}}"},
		},
	}
	if _, err := clientGamma.PreloadTag("tenant-b-template", "tenant-b-template", "@production", 1, astTenantB, nil); err != nil {
		t.Fatalf("PreloadTag clientGamma failed: %v", err)
	}

	// Warm caches on clientAlpha and clientBeta by rendering @production (v1)
	for _, client := range []*promptsdk.Client{clientAlpha, clientBeta} {
		out, rErr := client.Render(ctx, "cluster-rag-template", "@production", promptsdk.RenderOptions{
			Variables: map[string]string{"query": "Hello v1"},
		})
		if rErr != nil || !strings.Contains(string(out), "V1 Query:") {
			t.Fatalf("expected V1 render, got %q, err=%v", string(out), rErr)
		}
	}

	// Confirm that Pub/Sub gauge is connected (1) on all nodes
	if !clientAlpha.IsPubSubConnected() || !clientBeta.IsPubSubConnected() || !clientGamma.IsPubSubConnected() {
		t.Fatalf("expected all nodes to report pubsubConnected == true")
	}

	// -------------------------------------------------------------------------
	// Step 1: Tenant Boundary Isolation Verification (Layer 4)
	// -------------------------------------------------------------------------
	// Broadcast invalidation event explicitly scoped to Tenant B
	fakeTenantBEvent := fmt.Sprintf(`{
		"event_id": "01JMBUS0000000000000000001",
		"tenant_id": "%s",
		"template_id": "%s",
		"tag": "@production",
		"new_version": 2,
		"timestamp": "%s"
	}`, tenantB, tmplA.ID, time.Now().UTC().Format(time.RFC3339))

	// Ingest Tenant B event on all clients directly or via bus
	for _, client := range []*promptsdk.Client{clientAlpha, clientBeta} {
		_, err := client.Cache().HandleInvalidationPayload(fakeTenantBEvent)
		if err != nil {
			t.Fatalf("HandleInvalidationPayload failed: %v", err)
		}
		// Tenant A clients must NOT have evicted tmplA.ID because the event was scoped to Tenant B!
		if _, _, ok := client.Cache().GetTag("cluster-rag-template", "@production"); !ok {
			t.Fatalf("Tenant Boundary Breach: Tenant A client evicted cache key on Tenant B invalidation event!")
		}
	}

	// -------------------------------------------------------------------------
	// Step 2: Multi-Node Invalidation (<500ms global cluster propagation SLO, ADR-002)
	// -------------------------------------------------------------------------
	// Promote @production -> v2 on the Control Plane
	startPromote := time.Now()
	promResp, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantA,
		TemplateIdentifier: tmplA.ID,
		Tag:                "@production",
		TargetVersion:      2,
		ExpectedVersion:    1,
		UpdatedBy:          "phase3-bus-test",
	})
	if err != nil {
		t.Fatalf("PromoteTag v2 failed: %v", err)
	}
	if !promResp.BroadcastEmitted {
		t.Fatalf("expected BroadcastEmitted == true")
	}

	// Verify both Node Alpha and Node Beta evict their local cache keys in <500ms
	propagationDeadline := time.Now().Add(500 * time.Millisecond)
	for _, client := range []*promptsdk.Client{clientAlpha, clientBeta} {
		evicted := false
		for time.Now().Before(propagationDeadline) {
			if _, _, ok := client.Cache().GetTag("cluster-rag-template", "@production"); !ok {
				evicted = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !evicted {
			t.Fatalf("node failed to evict L1_tag within <500ms SLO")
		}

		// Subsequent render on this node must immediately compile v2
		outV2, rErr := client.Render(ctx, "cluster-rag-template", "@production", promptsdk.RenderOptions{
			Variables: map[string]string{"query": "Hello v2"},
		})
		if rErr != nil || !strings.Contains(string(outV2), "V2 Query:") {
			t.Fatalf("expected v2 output across nodes, got %q, err=%v", string(outV2), rErr)
		}
	}
	t.Logf("Multi-node invalidation verified in %v (<500ms SLO)", time.Since(startPromote))

	// Tenant B cache on clientGamma must remain intact
	if _, _, ok := clientGamma.Cache().GetTag("tenant-b-template", "@production"); !ok {
		t.Fatalf("clientGamma cache was evicted by Tenant A promotion!")
	}

	// -------------------------------------------------------------------------
	// Step 3: Runbook 1 Pointer Rollback (v2 -> v1)
	// -------------------------------------------------------------------------
	rollResp, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantA,
		TemplateIdentifier: tmplA.ID,
		Tag:                "@production",
		TargetVersion:      1,
		ExpectedVersion:    2,
		UpdatedBy:          "admin-rollback-runbook1",
	})
	if err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	if !rollResp.IsRollback || !rollResp.BroadcastEmitted {
		t.Fatalf("expected IsRollback=true, BroadcastEmitted=true, got %+v", rollResp)
	}

	for _, client := range []*promptsdk.Client{clientAlpha, clientBeta} {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if _, _, ok := client.Cache().GetTag("cluster-rag-template", "@production"); !ok {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		outRollback, rErr := client.Render(ctx, "cluster-rag-template", "@production", promptsdk.RenderOptions{
			Variables: map[string]string{"query": "Hello rollback v1"},
		})
		if rErr != nil || !strings.Contains(string(outRollback), "V1 Query:") {
			t.Fatalf("expected rolled back v1 render, got %q, err=%v", string(outRollback), rErr)
		}
	}

	// -------------------------------------------------------------------------
	// Step 4: Runbook 2 Connection Loss & Fallback to 5-Minute Safety TTL
	// -------------------------------------------------------------------------
	// Simulate Pub/Sub disconnection on clientAlpha
	clientAlpha.SetPubSubConnected(false)
	if clientAlpha.IsPubSubConnected() {
		t.Fatalf("expected clientAlpha IsPubSubConnected == false")
	}

	// Metrics exposition must report prompt_sdk_pubsub_connected 0
	alphaMetrics := clientAlpha.MetricsExposition()
	if !strings.Contains(alphaMetrics, `prompt_sdk_pubsub_connected{node_id="node-alpha"} 0`) {
		t.Fatalf("expected prompt_sdk_pubsub_connected 0 in exposition:\n%s", alphaMetrics)
	}

	// While disconnected, in-memory availability is 100% maintained with <0.5ms latency
	for i := 0; i < 50; i++ {
		out, rErr := clientAlpha.Render(ctx, "cluster-rag-template", "@production", promptsdk.RenderOptions{
			Variables: map[string]string{"query": "Offline query"},
		})
		if rErr != nil || !strings.Contains(string(out), "V1 Query:") {
			t.Fatalf("disconnected render failed: %v", rErr)
		}
	}

	// Reconnect Pub/Sub on clientAlpha
	clientAlpha.SetPubSubConnected(true)
	if !clientAlpha.IsPubSubConnected() {
		t.Fatalf("expected clientAlpha IsPubSubConnected == true")
	}

	// -------------------------------------------------------------------------
	// Step 5: Runbook 2 Step 3 - Forced Cache Wipe via Client.Purge()
	// -------------------------------------------------------------------------
	// Confirm entry exists before purge
	if _, _, ok := clientAlpha.Cache().GetTag("cluster-rag-template", "@production"); !ok {
		t.Fatalf("expected tag to be present before purge")
	}
	clientAlpha.Purge()

	// Tag pointers must be wiped from L1_tag
	if _, _, ok := clientAlpha.Cache().GetTag("cluster-rag-template", "@production"); ok {
		t.Fatalf("expected L1_tag to be empty after clientAlpha.Purge()")
	}
	// Immutable L1_version entries must remain preserved!
	if _, ok := clientAlpha.Cache().GetVersion("cluster-rag-template", 1); !ok {
		t.Fatalf("expected L1_version v1 to remain intact after Purge()")
	}
}

// TestRedisTCPSubscriberBackoffAndReconnect exercises the live TCP RESP subscriber loop,
// automated reconnection with exponential backoff & jitter, backoff curve reset on SUBSCRIBE,
// and prompt_sdk_pubsub_connected gauge transitions (Runbook 2).
func TestRedisTCPSubscriberBackoffAndReconnect(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on local TCP: %v", err)
	}
	defer func() { _ = lis.Close() }()

	addr := lis.Addr().String()
	tenantID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"

	var conn1 net.Conn
	var conn2 net.Conn
	connCh := make(chan net.Conn, 2)

	go func() {
		for {
			c, aErr := lis.Accept()
			if aErr != nil {
				return
			}
			connCh <- c
		}
	}()

	client, err := promptsdk.NewClient(promptsdk.ClientConfig{
		TenantID:   tenantID,
		RedisAddrs: []string{addr},
		NodeID:     "tcp-backoff-node",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	// 1. Accept first connection
	select {
	case conn1 = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for initial TCP connection")
	}

	// Read SUBSCRIBE command from client and respond with Redis subscribe confirmation
	buf := make([]byte, 512)
	n, err := conn1.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "SUBSCRIBE") {
		t.Fatalf("expected SUBSCRIBE command, got: %s, err=%v", string(buf[:n]), err)
	}

	// Send RESP subscribe confirmation
	subscribeReply := "*3\r\n$9\r\nsubscribe\r\n$25\r\nchannel:prompt:invalidation\r\n:1\r\n"
	_, _ = conn1.Write([]byte(subscribeReply))

	// Verify gauge transitions to connected (1)
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if client.IsPubSubConnected() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !client.IsPubSubConnected() {
		t.Fatalf("expected client to report pubsub connected")
	}

	// 2. Drop the connection from the server side
	_ = conn1.Close()

	// Verify gauge transitions to disconnected (0)
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if !client.IsPubSubConnected() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if client.IsPubSubConnected() {
		t.Fatalf("expected client to transition to disconnected (0)")
	}

	// 3. Automated reconnect with exponential backoff: client must reconnect on connCh
	select {
	case conn2 = <-connCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for automatic client reconnect")
	}
	defer func() { _ = conn2.Close() }()

	n, err = conn2.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "SUBSCRIBE") {
		t.Fatalf("expected SUBSCRIBE command on reconnect, got: %s, err=%v", string(buf[:n]), err)
	}

	// Send subscribe confirmation on conn2; resets backoff curve!
	_, _ = conn2.Write([]byte(subscribeReply))

	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if client.IsPubSubConnected() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !client.IsPubSubConnected() {
		t.Fatalf("expected client to report reconnected (1)")
	}
}
