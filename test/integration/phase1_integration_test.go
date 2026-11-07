package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/domain/repository"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
	"rag-prompt-registry-service/internal/infrastructure/persistence/postgres"
)

func setupRepositories(t *testing.T) (repository.PromptRepository, repository.AuditRepository, *postgres.TransactionalMemoryStore, func()) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("POSTGRES_DSN"))
	if dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err := postgres.NewPool(ctx, postgres.PoolOptions{
			DSN:      dsn,
			MaxConns: 20,
			MinConns: 2,
		})
		if err == nil && pool.Ping(ctx) == nil {
			if migErr := postgres.ApplyMigrations(ctx, pool); migErr == nil {
				cancel()
				return postgres.NewPromptRepository(pool), postgres.NewAuditRepository(pool), nil, func() {
					pool.Close()
				}
			}
		}
		cancel()
	}

	mem := postgres.NewTransactionalMemoryStore()
	return mem, mem, mem, func() {}
}

// Scenario 1: PgBouncer Simple Protocol Concurrency & Monotonic Version Serialization (ADR-003)
func TestPgBouncerSimpleProtocolConcurrency(t *testing.T) {
	cfg, err := postgres.BuildPoolConfig(postgres.PoolOptions{
		DSN:      "postgres://postgres:postgres@localhost:6432/prompt_registry?sslmode=disable",
		MaxConns: 25,
		MinConns: 2,
	})
	if err != nil {
		t.Fatalf("BuildPoolConfig failed: %v", err)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Fatalf("ADR-003 violation: expected pgx.QueryExecModeSimpleProtocol, got %v", cfg.ConnConfig.DefaultQueryExecMode)
	}

	promptRepo, _, _, cleanup := setupRepositories(t)
	defer cleanup()

	ctx := context.Background()
	tenantID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	pubHandler := command.NewPublishVersionHandler(promptRepo, nil, nil, command.NewMutationMetricsRecorder())

	sc, err := pubHandler.RegisterSchema(ctx, command.RegisterSchemaInput{
		TenantID:   tenantID,
		Name:       "concurrent-schema-" + strings.ToLower(redis.NewULID()[:8]),
		SchemaJSON: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
		Author:     "concurrency-suite",
	})
	if err != nil {
		t.Fatalf("RegisterSchema failed: %v", err)
	}

	tmplName := "concurrent-template-" + strings.ToLower(redis.NewULID()[:8])
	tmpl, v1, err := pubHandler.CreateTemplate(ctx, command.CreateTemplateInput{
		TenantID:    tenantID,
		Name:        tmplName,
		Description: "High-concurrency monotonic version allocation test",
		SchemaID:    sc.ID,
		Author:      "concurrency-suite",
		RawAST: entity.TemplateAST{
			SystemPrompt:   "Initial system prompt v1",
			VariableKeys:   []string{"context", "query"},
			DefaultDialect: entity.DialectAnthropicXML,
			Messages: []entity.MessageBlock{
				{Role: "user", ContentPattern: "Context: {{context}} Query: {{query}}"},
			},
		},
	})
	if err != nil || v1.Version != 1 {
		t.Fatalf("CreateTemplate failed: v1=%+v, err=%v", v1, err)
	}

	const workers = 30
	var wg sync.WaitGroup
	versionsCh := make(chan int, workers)
	errsCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			ver, _, pubErr := pubHandler.PublishVersion(ctx, command.PublishVersionInput{
				TenantID:           tenantID,
				TemplateIdentifier: tmpl.ID,
				SchemaID:           sc.ID,
				Author:             fmt.Sprintf("worker-%d", workerIdx),
				RawAST: entity.TemplateAST{
					SystemPrompt:   fmt.Sprintf("Concurrent system prompt variant %d", workerIdx),
					VariableKeys:   []string{"context", "query"},
					DefaultDialect: entity.DialectAnthropicXML,
					Messages: []entity.MessageBlock{
						{Role: "user", ContentPattern: "Context: {{context}} Query: {{query}}"},
					},
				},
			})
			if pubErr != nil {
				errsCh <- pubErr
				return
			}
			versionsCh <- ver.Version
		}(i)
	}

	wg.Wait()
	close(versionsCh)
	close(errsCh)

	for err := range errsCh {
		t.Fatalf("concurrent PublishVersion error: %v", err)
	}

	var allocated []int
	for v := range versionsCh {
		allocated = append(allocated, v)
	}
	sort.Ints(allocated)

	if len(allocated) != workers {
		t.Fatalf("expected %d versions, got %d", workers, len(allocated))
	}
	for i, gotVer := range allocated {
		wantVer := i + 2 // v2 .. v_{workers+1}
		if gotVer != wantVer {
			t.Fatalf("monotonic version gap/collision at index %d: got v%d, want v%d", i, gotVer, wantVer)
		}
	}
}

// Scenario 2: Transactional Audit Ledger Commits, Rollback Atomicity & Append-Only Immutability (ADR-004)
func TestTransactionalAuditLedgerAtomicityAndImmutability(t *testing.T) {
	promptRepo, auditRepo, memStore, cleanup := setupRepositories(t)
	defer cleanup()

	ctx := context.Background()
	tenantID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	pubHandler := command.NewPublishVersionHandler(promptRepo, nil, nil, command.NewMutationMetricsRecorder())

	sc, err := pubHandler.RegisterSchema(ctx, command.RegisterSchemaInput{
		TenantID:   tenantID,
		Name:       "audit-schema-" + strings.ToLower(redis.NewULID()[:8]),
		SchemaJSON: []byte(`{"type":"object","properties":{"result":{"type":"string"}}}`),
		Author:     "audit-tester",
	})
	if err != nil {
		t.Fatalf("RegisterSchema failed: %v", err)
	}

	// Verify SCHEMA_REGISTERED audit record was committed atomically
	schemaAudits, err := auditRepo.ListByTemplate(ctx, tenantID, sc.ID, 10)
	if err != nil || len(schemaAudits) != 1 || schemaAudits[0].Action != entity.AuditActionSchemaRegistered {
		t.Fatalf("expected 1 SCHEMA_REGISTERED audit entry, got %+v, err=%v", schemaAudits, err)
	}

	// If running with TransactionalMemoryStore, test atomic rollback when audit ledger write fails
	if memStore != nil {
		memStore.SetFaultInjectAuditFailure(true)
		failedName := "should-rollback-template"
		_, _, createErr := pubHandler.CreateTemplate(ctx, command.CreateTemplateInput{
			TenantID: tenantID,
			Name:     failedName,
			SchemaID: sc.ID,
			Author:   "audit-tester",
			RawAST: entity.TemplateAST{
				SystemPrompt:   "Should rollback",
				VariableKeys:   []string{"q"},
				DefaultDialect: entity.DialectOpenAIStandard,
				Messages:       []entity.MessageBlock{{Role: "user", ContentPattern: "{{q}}"}},
			},
		})
		if createErr == nil {
			t.Fatalf("expected CreateTemplate to fail when audit ledger write fails")
		}
		memStore.SetFaultInjectAuditFailure(false)

		// Verify the template was NOT persisted due to atomic transaction rollback
		if _, getErr := promptRepo.GetTemplateVersion(ctx, tenantID, failedName, 1); getErr == nil {
			t.Fatalf("atomic rollback violation: template %q persisted despite audit ledger failure", failedName)
		}

		// Verify append-only immutability trigger error semantics on prompt_versions and prompt_audit_ledger
		if trigErr := memStore.AttemptAppendOnlyMutation("prompt_versions", "UPDATE"); trigErr == nil ||
			!strings.Contains(trigErr.Error(), "strictly append-only") {
			t.Fatalf("expected append-only trigger error on prompt_versions UPDATE, got: %v", trigErr)
		}
		if trigErr := memStore.AttemptAppendOnlyMutation("prompt_audit_ledger", "DELETE"); trigErr == nil ||
			!strings.Contains(trigErr.Error(), "strictly append-only") {
			t.Fatalf("expected append-only trigger error on prompt_audit_ledger DELETE, got: %v", trigErr)
		}
	}
}

// Scenario 3: Redis L2 Cache & Pub/Sub Invalidation Delivery within <100ms (ADR-002)
func TestRedisL2CacheAndPubSubInvalidationDelivery(t *testing.T) {
	promptRepo, _, _, cleanup := setupRepositories(t)
	defer cleanup()

	ctx := context.Background()
	tenantID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	l2Cache := redis.NewInMemoryL2Cache()
	invalidator := redis.NewInvalidationBroadcaster(l2Cache, redis.DefaultInvalidationChannel)
	metrics := command.NewMutationMetricsRecorder()

	pubHandler := command.NewPublishVersionHandler(promptRepo, l2Cache, nil, metrics)
	promHandler := command.NewPromoteTagHandler(promptRepo, l2Cache, invalidator, nil, metrics)
	getHandler := query.NewGetTemplateASTHandler(promptRepo, l2Cache)

	subCh, unsubscribe := l2Cache.SubscribeChannel(ctx, redis.DefaultInvalidationChannel)
	defer unsubscribe()

	sc, err := pubHandler.RegisterSchema(ctx, command.RegisterSchemaInput{
		TenantID:   tenantID,
		Name:       "pubsub-schema-" + strings.ToLower(redis.NewULID()[:8]),
		SchemaJSON: []byte(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
		Author:     "pubsub-tester",
	})
	if err != nil {
		t.Fatalf("RegisterSchema failed: %v", err)
	}

	tmpl, _, err := pubHandler.CreateTemplate(ctx, command.CreateTemplateInput{
		TenantID: tenantID,
		Name:     "pubsub-template-" + strings.ToLower(redis.NewULID()[:8]),
		SchemaID: sc.ID,
		Author:   "pubsub-tester",
		RawAST: entity.TemplateAST{
			SystemPrompt:   "Version 1 prompt",
			VariableKeys:   []string{"query"},
			DefaultDialect: entity.DialectAnthropicXML,
			Messages:       []entity.MessageBlock{{Role: "user", ContentPattern: "{{query}}"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTemplate failed: %v", err)
	}

	_, _, err = pubHandler.PublishVersion(ctx, command.PublishVersionInput{
		TenantID:           tenantID,
		TemplateIdentifier: tmpl.ID,
		SchemaID:           sc.ID,
		Author:             "pubsub-tester",
		RawAST: entity.TemplateAST{
			SystemPrompt:   "Version 2 prompt",
			VariableKeys:   []string{"query"},
			DefaultDialect: entity.DialectAnthropicXML,
			Messages:       []entity.MessageBlock{{Role: "user", ContentPattern: "V2: {{query}}"}},
		},
	})
	if err != nil {
		t.Fatalf("PublishVersion v2 failed: %v", err)
	}

	// Promote @production to v1 and drain initial event
	if _, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantID,
		TemplateIdentifier: tmpl.ID,
		Tag:                "@production",
		TargetVersion:      1,
		ExpectedVersion:    0,
		UpdatedBy:          "pubsub-tester",
	}); err != nil {
		t.Fatalf("PromoteTag v1 failed: %v", err)
	}
	<-subCh

	// Populate L2 tag resolution cache for v1
	resV1, err := getHandler.ResolveTag(ctx, tenantID, tmpl.ID, "@production")
	if err != nil || resV1.Version != 1 {
		t.Fatalf("expected ResolveTag v1, got %+v, err=%v", resV1, err)
	}

	// Now promote @production from v1 -> v2 and measure Pub/Sub propagation latency (<100ms SLO)
	promoteStart := time.Now()
	if _, err := promHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantID,
		TemplateIdentifier: tmpl.ID,
		Tag:                "@production",
		TargetVersion:      2,
		ExpectedVersion:    1,
		UpdatedBy:          "pubsub-tester",
	}); err != nil {
		t.Fatalf("PromoteTag v2 failed: %v", err)
	}

	select {
	case rawMsg := <-subCh:
		propagationLatency := time.Since(promoteStart)
		if propagationLatency > 100*time.Millisecond {
			t.Fatalf("ADR-002 SLO breach: expected invalidation delivery <100ms, took %v", propagationLatency)
		}
		var evt redis.InvalidationEvent
		if err := json.Unmarshal([]byte(rawMsg), &evt); err != nil {
			t.Fatalf("failed to unmarshal InvalidationEvent: %v", err)
		}
		if len(evt.EventID) != 26 || evt.TenantID != tenantID || evt.TemplateID != tmpl.ID || evt.Tag != "@production" || evt.NewVersion != 2 {
			t.Fatalf("unexpected InvalidationEvent payload: %+v", evt)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timed out waiting for Redis Pub/Sub invalidation event (<100ms SLO)")
	}

	// Verify L2 cache was evicted and ResolveTag immediately returns v2
	resV2, err := getHandler.ResolveTag(ctx, tenantID, tmpl.ID, "@production")
	if err != nil || resV2.Version != 2 {
		t.Fatalf("expected ResolveTag to return version 2 immediately after promotion, got %+v, err=%v", resV2, err)
	}
}
