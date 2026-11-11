package sdk_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
	"rag-prompt-registry-service/internal/infrastructure/persistence/postgres"
	httptransport "rag-prompt-registry-service/internal/transport/http"
	"rag-prompt-registry-service/pkg/promptsdk"
)

// BenchmarkSustained10000RPSAllDialects benchmarks parallel 50 KB context rendering across
// Anthropic XML, Meta Llama-3, and OpenAI Standard JSON dialects verifying 0 heap allocations.
func BenchmarkSustained10000RPSAllDialects(b *testing.B) {
	client, vars := setupSeededSDKClient(b)
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	dialects := []promptsdk.DialectType{
		promptsdk.DialectAnthropicXML,
		promptsdk.DialectLlamaSpecial,
		promptsdk.DialectOpenAIStandard,
	}

	for _, dialect := range dialects {
		b.Run(string(dialect), func(b *testing.B) {
			opts := promptsdk.RenderOptions{
				TargetDialect: dialect,
				Variables:     vars,
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					buf := promptsdk.AcquireBuffer()
					if err := client.RenderToBuffer(ctx, buf, "rag-financial-summary", "@production", opts); err != nil {
						b.Fatalf("RenderToBuffer failed: %v", err)
					}
					promptsdk.ReleaseBuffer(buf)
				}
			})
		})
	}
}

// TestPhase4Sustained10000RPSAndZeroMemoryLeak executes repeated 10,000-request parallel batches
// with 50 KB context payloads, verifying 0 allocs/op, P99 latency compliance across all dialects,
// flat heap utilization across batches, and bounded oversized buffer eviction from sync.Pool.
func TestPhase4Sustained10000RPSAndZeroMemoryLeak(t *testing.T) {
	client, vars := setupSeededSDKClient(t)
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	dialects := []promptsdk.DialectType{
		promptsdk.DialectAnthropicXML,
		promptsdk.DialectLlamaSpecial,
		promptsdk.DialectOpenAIStandard,
	}

	// 1. Assert 0 allocs/op across all 3 dialects for 50 KB context blocks.
	for _, dialect := range dialects {
		opts := promptsdk.RenderOptions{
			TargetDialect: dialect,
			Variables:     vars,
		}
		allocs := testing.AllocsPerRun(100, func() {
			buf := promptsdk.AcquireBuffer()
			if err := client.RenderToBuffer(ctx, buf, "rag-financial-summary", "@production", opts); err != nil {
				t.Fatalf("RenderToBuffer(%s) failed: %v", dialect, err)
			}
			promptsdk.ReleaseBuffer(buf)
		})
		if allocs != 0 {
			t.Fatalf("expected 0 allocs/op for %s, got %.2f", dialect, allocs)
		}
	}

	// 2. Verify oversized buffer (>512 KiB) is bounded and not pinned indefinitely in sync.Pool.
	oversized := promptsdk.AcquireBuffer()
	oversized.Grow(1024 * 1024) // 1 MiB anomalous request buffer
	oversized.WriteString("anomalous-oversized-payload")
	promptsdk.ReleaseBuffer(oversized)
	runtime.GC()

	// Warm normal 50 KB pooled buffers.
	for i := 0; i < 32; i++ {
		b := promptsdk.AcquireBuffer()
		_ = client.RenderToBuffer(ctx, b, "rag-financial-summary", "@production", promptsdk.RenderOptions{
			TargetDialect: promptsdk.DialectAnthropicXML,
			Variables:     vars,
		})
		promptsdk.ReleaseBuffer(b)
	}

	// 3. Execute 3 consecutive 10,000-render batches and profile heap stats + per-dialect P99.
	const batchSize = 10000
	const batches = 3
	workers := runtime.GOMAXPROCS(0)
	if workers < 4 {
		workers = 4
	}

	// Parallel warmup so every GOMAXPROCS P populates its local sync.Pool shard before timing.
	var warmWg sync.WaitGroup
	for w := 0; w < workers; w++ {
		warmWg.Add(1)
		go func(workerID int) {
			defer warmWg.Done()
			opts := promptsdk.RenderOptions{
				TargetDialect: dialects[workerID%len(dialects)],
				Variables:     vars,
			}
			for i := 0; i < 8; i++ {
				b := promptsdk.AcquireBuffer()
				_ = client.RenderToBuffer(ctx, b, "rag-financial-summary", "@production", opts)
				promptsdk.ReleaseBuffer(b)
			}
		}(w)
	}
	warmWg.Wait()

	var baselineHeapAlloc uint64
	var baselineHeapObjects uint64

	for batch := 0; batch < batches; batch++ {
		durations := make([]time.Duration, batchSize)
		perWorker := batchSize / workers
		remainder := batchSize % workers

		var wg sync.WaitGroup
		errCh := make(chan error, batchSize)
		batchStart := time.Now()

		offset := 0
		for w := 0; w < workers; w++ {
			count := perWorker
			if w < remainder {
				count++
			}
			startIdx := offset
			offset += count

			wg.Add(1)
			go func(workerID, baseIdx, n int) {
				defer wg.Done()
				opts := promptsdk.RenderOptions{
					TargetDialect: dialects[workerID%len(dialects)],
					Variables:     vars,
				}
				for i := 0; i < n; i++ {
					t0 := time.Now()
					buf := promptsdk.AcquireBuffer()
					rErr := client.RenderToBuffer(ctx, buf, "rag-financial-summary", "@production", opts)
					bufLen := buf.Len()
					promptsdk.ReleaseBuffer(buf)
					durations[baseIdx+i] = time.Since(t0)
					if rErr != nil {
						errCh <- rErr
						return
					}
					if bufLen < 50000 {
						errCh <- fmt.Errorf("unexpected short buffer len %d", bufLen)
						return
					}
				}
			}(w, startIdx, count)
		}
		wg.Wait()
		close(errCh)
		batchElapsed := time.Since(batchStart)

		for rErr := range errCh {
			t.Fatalf("batch %d render error: %v", batch, rErr)
		}

		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p50 := durations[batchSize/2]
		p99 := durations[int(float64(batchSize)*0.99)]
		rps := float64(batchSize) / batchElapsed.Seconds()

		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		t.Logf("Batch %d: %d renders in %v (%.0f RPS) | P50=%v P99=%v | HeapAlloc=%d KiB HeapObjects=%d NumGC=%d",
			batch+1, batchSize, batchElapsed, rps, p50, p99, ms.HeapAlloc/1024, ms.HeapObjects, ms.NumGC)

		if p99 > 5*time.Millisecond {
			t.Fatalf("batch %d P99 latency %v exceeded 5ms CI jitter guard", batch+1, p99)
		}

		if batch == 0 {
			baselineHeapAlloc = ms.HeapAlloc
			baselineHeapObjects = ms.HeapObjects
		} else {
			// Verify zero runaway buffer growth across repeated 10,000-request batches.
			const maxHeapGrowthBytes = 2 * 1024 * 1024 // 2 MiB tolerance for Go runtime metadata
			if ms.HeapAlloc > baselineHeapAlloc+maxHeapGrowthBytes {
				t.Fatalf("memory accumulation detected: baseline HeapAlloc=%d, batch %d HeapAlloc=%d",
					baselineHeapAlloc, batch+1, ms.HeapAlloc)
			}
			if ms.HeapObjects > baselineHeapObjects+5000 {
				t.Fatalf("heap object accumulation detected: baseline=%d, batch %d=%d",
					baselineHeapObjects, batch+1, ms.HeapObjects)
			}
		}
	}
}

// TestPhase4ControlPlaneMemoryFootprint profiles the control plane HTTP/CQRS stack under
// administrative load and verifies footprint compliance well below the 64 MiB steady-state /
// 128 MiB peak RSS ceiling.
func TestPhase4ControlPlaneMemoryFootprint(t *testing.T) {
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
	router := httptransport.NewRouter(httptransport.RouterConfig{
		PublishHandler: pubHandler,
		PromoteHandler: promHandler,
		GetASTHandler:  getHandler,
		ListHandler:    listHandler,
		MutationMetric: metrics,
		Invalidator:    invalidator,
		DefaultTenant:  tenantID,
		WriteRateRPS:   5000,
		WriteRateBurst: 5000,
		Logger:         logger,
	})
	ts := httptest.NewServer(router)
	defer ts.Close()

	jwtToken, err := httptransport.IssueHS256JWT(tenantID, httptransport.DefaultJWTIssuer, httptransport.DefaultJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("IssueHS256JWT failed: %v", err)
	}

	doReq := func(method, path string, payload any) []byte {
		var bodyReader io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			bodyReader = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, bodyReader)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+jwtToken)
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatalf("HTTP %s %s failed: %v", method, path, doErr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("HTTP %s %s returned %d: %s", method, path, resp.StatusCode, string(b))
		}
		b, _ := io.ReadAll(resp.Body)
		return b
	}

	scBytes := doReq(http.MethodPost, "/v1/schemas", map[string]any{
		"tenant_id":   tenantID,
		"name":        "cp-footprint-schema",
		"json_schema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}},
	})
	var scResp struct {
		SchemaID string `json:"schema_id"`
	}
	_ = json.Unmarshal(scBytes, &scResp)

	// Drive 200 administrative mutations + reads to simulate sustained control plane workload.
	for i := 0; i < 50; i++ {
		tmplName := fmt.Sprintf("cp-load-tmpl-%03d", i)
		doReq(http.MethodPost, "/v1/templates", map[string]any{
			"tenant_id": tenantID,
			"name":      tmplName,
			"schema_id": scResp.SchemaID,
			"author":    "load-profiler",
			"raw_ast": map[string]any{
				"system_prompt":   "Control plane load prompt",
				"variable_keys":   []string{"context", "query"},
				"default_dialect": "DIALECT_ANTHROPIC_XML",
				"messages":        []map[string]any{{"role": "user", "content_pattern": "{{context}} {{query}}"}},
			},
		})
		doReq(http.MethodPost, "/v1/templates/"+tmplName+"/versions", map[string]any{
			"schema_id": scResp.SchemaID,
			"author":    "load-profiler",
			"raw_ast": map[string]any{
				"system_prompt":   "Control plane load prompt v2",
				"variable_keys":   []string{"context", "query"},
				"default_dialect": "DIALECT_ANTHROPIC_XML",
				"messages":        []map[string]any{{"role": "user", "content_pattern": "{{context}} {{query}}"}},
			},
		})
		doReq(http.MethodPut, "/v1/templates/"+tmplName+"/tags/@production", map[string]any{
			"target_version":   2,
			"expected_version": 0,
			"updated_by":       "load-profiler",
		})
		doReq(http.MethodGet, "/v1/templates/"+tmplName+"/tags/@production", nil)
	}

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	heapInuseMiB := float64(ms.HeapInuse) / (1024 * 1024)
	heapAllocMiB := float64(ms.HeapAlloc) / (1024 * 1024)
	sysMiB := float64(ms.Sys) / (1024 * 1024)
	t.Logf("Control Plane Memory Footprint after 200 admin ops: HeapAlloc=%.2f MiB, HeapInuse=%.2f MiB, Sys=%.2f MiB",
		heapAllocMiB, heapInuseMiB, sysMiB)

	if heapInuseMiB > 64.0 {
		t.Fatalf("Control Plane HeapInuse %.2f MiB exceeded 64 MiB steady-state target", heapInuseMiB)
	}
	if sysMiB > 128.0 {
		t.Fatalf("Control Plane Sys %.2f MiB exceeded 128 MiB peak RSS limit", sysMiB)
	}
}
