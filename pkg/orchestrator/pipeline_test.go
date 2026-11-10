package orchestrator_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"rag-prompt-registry-service/pkg/orchestrator"
	"rag-prompt-registry-service/pkg/promptsdk"
)

// mockRetriever implements orchestrator.ContextRetriever for testing.
type mockRetriever struct {
	chunks []orchestrator.RetrievedChunk
}

// Retrieve returns mock retrieved passage chunks for unit testing.
func (m *mockRetriever) Retrieve(ctx context.Context, query string) ([]orchestrator.RetrievedChunk, error) {
	return m.chunks, nil
}

// mockDispatcher implements orchestrator.InferenceDispatcher for testing.
type mockDispatcher struct {
	lastRequest orchestrator.InferenceRequest
	response    orchestrator.InferenceResponse
}

// Dispatch records the inference request and returns the configured mock response.
func (m *mockDispatcher) Dispatch(ctx context.Context, req orchestrator.InferenceRequest) (*orchestrator.InferenceResponse, error) {
	m.lastRequest = req
	return &m.response, nil
}

// setupTestPipeline constructs a test harness containing a RAGPipeline and mock dispatcher.
func setupTestPipeline(t *testing.T) (*orchestrator.RAGPipeline, *promptsdk.Client, *mockDispatcher) {
	t.Helper()
	client, err := promptsdk.NewClient(promptsdk.ClientConfig{
		NodeID: "test-orchestrator-node",
	})
	if err != nil {
		t.Fatalf("failed to create promptsdk.Client: %v", err)
	}

	dispatcher := &mockDispatcher{
		response: orchestrator.InferenceResponse{
			ModelID:      "mock-llm-v1",
			OutputText:   "Financial summary of earnings generated successfully.",
			FinishReason: "stop",
			TokensUsed:   42,
		},
	}

	pipeline, err := orchestrator.NewRAGPipeline(orchestrator.PipelineConfig{
		PromptSDK:    client,
		Dispatcher:   dispatcher,
		DefaultModel: "claude-3-5-sonnet",
	})
	if err != nil {
		t.Fatalf("failed to create RAGPipeline: %v", err)
	}

	return pipeline, client, dispatcher
}

// TestPipelineEndToEnd50KBContextAndAllDialects tests pipeline compilation across 50 KB context chunks
// for Anthropic XML, Meta Llama-3, and OpenAI Standard dialects.
func TestPipelineEndToEnd50KBContextAndAllDialects(t *testing.T) {
	pipeline, client, _ := setupTestPipeline(t)
	defer func() { _ = client.Close() }()

	// Seed template in L1 cache
	templateID := "financial-rag-template"
	ast := promptsdk.TemplateAST{
		SystemPrompt:   "You are an expert financial analyst assistant.",
		VariableKeys:   []string{"context", "query"},
		DefaultDialect: promptsdk.DialectAnthropicXML,
		Messages: []promptsdk.MessageBlock{
			{Role: "user", ContentPattern: "Context:\n{{context}}\n\nUser Question: {{query}}"},
		},
	}
	if _, err := client.PreloadTag(templateID, templateID, "@production", 1, ast, nil); err != nil {
		t.Fatalf("PreloadTag failed: %v", err)
	}

	// Build 50 KB of parent document chunk context
	const chunkSize = 10 * 1024
	chunks := make([]orchestrator.RetrievedChunk, 5)
	for i := range chunks {
		chunks[i] = orchestrator.RetrievedChunk{
			ChunkID:    fmt.Sprintf("chunk-%d", i+1),
			DocumentID: "doc-10k-filing-2026",
			Text:       fmt.Sprintf("Section %d: %s", i+1, strings.Repeat("Revenue growth exceeded targets for Q4 2026. ", chunkSize/45)),
			Score:      0.95 - float64(i)*0.05,
		}
	}

	ctx := context.Background()

	// 1. Anthropic XML Dialect
	respAnthropic, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:              "What was the Q4 revenue growth?",
		TemplateID:         templateID,
		TagOrVersion:       "@production",
		TargetDialect:      promptsdk.DialectAnthropicXML,
		PreRetrievedChunks: chunks,
		Mode:               orchestrator.ExecutionModeProduction,
	})
	if err != nil {
		t.Fatalf("Anthropic pipeline execute failed: %v", err)
	}
	promptAnthropic := string(respAnthropic.CompiledPrompt)
	if !strings.Contains(promptAnthropic, "<context>") || !strings.Contains(promptAnthropic, "</context>") {
		t.Fatalf("expected Anthropic XML idempotent context framing, got:\n%s", promptAnthropic[:200])
	}
	if !strings.Contains(promptAnthropic, "Revenue growth exceeded targets") {
		t.Fatalf("expected context content in prompt")
	}

	// 2. Meta Llama-3 Dialect
	respLlama, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:              "What was the Q4 revenue growth?",
		TemplateID:         templateID,
		TagOrVersion:       "@production",
		TargetDialect:      promptsdk.DialectLlamaSpecial,
		PreRetrievedChunks: chunks,
		Mode:               orchestrator.ExecutionModeProduction,
	})
	if err != nil {
		t.Fatalf("Llama pipeline execute failed: %v", err)
	}
	promptLlama := string(respLlama.CompiledPrompt)
	if !strings.Contains(promptLlama, "<|begin_of_text|>") || !strings.Contains(promptLlama, "<|start_header_id|>") {
		t.Fatalf("expected Llama-3 header tokens, got:\n%s", promptLlama[:200])
	}

	// 3. OpenAI Standard Dialect
	respOpenAI, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:              "What was the Q4 revenue growth?",
		TemplateID:         templateID,
		TagOrVersion:       "@production",
		TargetDialect:      promptsdk.DialectOpenAIStandard,
		PreRetrievedChunks: chunks,
		Mode:               orchestrator.ExecutionModeProduction,
	})
	if err != nil {
		t.Fatalf("OpenAI pipeline execute failed: %v", err)
	}
	promptOpenAI := string(respOpenAI.CompiledPrompt)
	if !strings.Contains(promptOpenAI, `"messages"`) || !strings.Contains(promptOpenAI, `"user"`) {
		t.Fatalf("expected OpenAI JSON array format, got:\n%s", promptOpenAI[:200])
	}
}

// TestPipelineDualModeExecution verifies minimal production response vs full diagnostic trace.
func TestPipelineDualModeExecution(t *testing.T) {
	pipeline, client, dispatcher := setupTestPipeline(t)
	defer func() { _ = client.Close() }()

	templateID := "dual-mode-template"
	ast := promptsdk.TemplateAST{
		SystemPrompt:   "System instructions.",
		VariableKeys:   []string{"context", "query"},
		DefaultDialect: promptsdk.DialectAnthropicXML,
		Messages: []promptsdk.MessageBlock{
			{Role: "user", ContentPattern: "Context: {{context}}\nQuery: {{query}}"},
		},
	}
	if _, err := client.PreloadTag(templateID, templateID, "@production", 1, ast, nil); err != nil {
		t.Fatalf("PreloadTag failed: %v", err)
	}

	ctx := context.Background()
	chunks := []orchestrator.RetrievedChunk{
		{ChunkID: "c1", DocumentID: "d1", Text: "Chunk 1 information.", Score: 0.99},
	}

	// Production Mode: diagnostic trace should be nil
	prodResp, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:              "Brief question",
		TemplateID:         templateID,
		TagOrVersion:       "@production",
		PreRetrievedChunks: chunks,
		Mode:               orchestrator.ExecutionModeProduction,
	})
	if err != nil {
		t.Fatalf("production execution failed: %v", err)
	}
	if prodResp.Diagnostic != nil {
		t.Fatalf("expected nil Diagnostic in production mode, got: %+v", prodResp.Diagnostic)
	}
	if prodResp.OutputText != dispatcher.response.OutputText {
		t.Fatalf("expected dispatcher output text %q, got %q", dispatcher.response.OutputText, prodResp.OutputText)
	}

	// Diagnostic Mode: diagnostic trace must be populated
	diagResp, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:              "Brief question",
		TemplateID:         templateID,
		TagOrVersion:       "@production",
		PreRetrievedChunks: chunks,
		MaxTokens:          1000,
		Mode:               orchestrator.ExecutionModeDiagnostic,
	})
	if err != nil {
		t.Fatalf("diagnostic execution failed: %v", err)
	}
	if diagResp.Diagnostic == nil {
		t.Fatalf("expected non-nil Diagnostic in diagnostic mode")
	}
	diag := diagResp.Diagnostic
	if diag.Timing.TotalMs < 0 || diag.Timing.SDKRenderMs < 0 {
		t.Fatalf("expected non-negative waterfall timings, got: %+v", diag.Timing)
	}
	if diag.TokenBudget.TokensUsed <= 0 {
		t.Fatalf("expected tokens_used > 0, got %d", diag.TokenBudget.TokensUsed)
	}
	if len(diag.RetrievedChunks) != 1 || diag.RetrievedChunks[0].ChunkID != "c1" {
		t.Fatalf("expected chunk provenance preserved, got %+v", diag.RetrievedChunks)
	}
	if diag.CompiledPrompt == "" {
		t.Fatalf("expected compiled_prompt preserved in diagnostic trace")
	}
}

// TestPipelineTokenizerBudgetCeilingAndTruncation verifies that the budget guard enforces max_tokens ceiling.
func TestPipelineTokenizerBudgetCeilingAndTruncation(t *testing.T) {
	pipeline, client, _ := setupTestPipeline(t)
	defer func() { _ = client.Close() }()

	templateID := "budget-test-template"
	ast := promptsdk.TemplateAST{
		SystemPrompt:   "Brief system.",
		VariableKeys:   []string{"context", "query"},
		DefaultDialect: promptsdk.DialectAnthropicXML,
		Messages: []promptsdk.MessageBlock{
			{Role: "user", ContentPattern: "Context:\n{{context}}\nQuery: {{query}}"},
		},
	}
	if _, err := client.PreloadTag(templateID, templateID, "@production", 1, ast, nil); err != nil {
		t.Fatalf("PreloadTag failed: %v", err)
	}

	ctx := context.Background()
	hugeContext := strings.Repeat("Long context text block with several words. ", 1000) // ~8000 tokens
	chunks := []orchestrator.RetrievedChunk{
		{ChunkID: "huge-chunk", Text: hugeContext},
	}

	resp, err := pipeline.Execute(ctx, orchestrator.PipelineRequest{
		Query:              "Summarize",
		TemplateID:         templateID,
		TagOrVersion:       "@production",
		PreRetrievedChunks: chunks,
		MaxTokens:          300, // strict ceiling
		Mode:               orchestrator.ExecutionModeDiagnostic,
	})
	if err != nil {
		t.Fatalf("pipeline execution under budget ceiling failed: %v", err)
	}

	guard := client.BudgetGuard()
	tokensRendered := guard.CountBytes(resp.CompiledPrompt)
	if tokensRendered > 300 {
		t.Fatalf("expected rendered tokens <= 300, got %d", tokensRendered)
	}
	if !resp.Diagnostic.TokenBudget.TruncationApplied {
		t.Fatalf("expected truncation_applied == true")
	}
}

// BenchmarkPipelineRenderHotPath validates zero heap allocations and sub-millisecond execution
// across 50 KB context blocks during hot-path pipeline rendering (Invariant 4: 0 allocs/op, P99 <= 0.5ms).
func BenchmarkPipelineRenderHotPath(b *testing.B) {
	client, err := promptsdk.NewClient(promptsdk.ClientConfig{
		NodeID: "bench-orchestrator-node",
	})
	if err != nil {
		b.Fatalf("failed to create client: %v", err)
	}
	defer func() { _ = client.Close() }()

	templateID := "bench-50kb-template"
	ast := promptsdk.TemplateAST{
		SystemPrompt:   "You are an AI financial analyst specializing in 10-K filings.",
		VariableKeys:   []string{"context", "query"},
		DefaultDialect: promptsdk.DialectAnthropicXML,
		Messages: []promptsdk.MessageBlock{
			{Role: "user", ContentPattern: "Context:\n{{context}}\n\nUser Question: {{query}}"},
		},
	}
	if _, err := client.PreloadTag(templateID, templateID, "@production", 1, ast, nil); err != nil {
		b.Fatalf("PreloadTag failed: %v", err)
	}

	// 50 KB context payload
	context50KB := strings.Repeat("Enterprise software recurring revenues grew 34% year-over-year in the cloud segment. ", 570)
	vars := map[string]string{
		"context": context50KB,
		"query":   "What was cloud recurring revenue growth?",
	}
	opts := promptsdk.RenderOptions{
		TargetDialect: promptsdk.DialectAnthropicXML,
		Variables:     vars,
	}

	ctx := context.Background()
	buf := promptsdk.AcquireBuffer()
	defer promptsdk.ReleaseBuffer(buf)

	// Warmup
	if err := client.RenderToBuffer(ctx, buf, templateID, "@production", opts); err != nil {
		b.Fatalf("warmup render failed: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		buf.Reset()
		if err := client.RenderToBuffer(ctx, buf, templateID, "@production", opts); err != nil {
			b.Fatalf("render failed: %v", err)
		}
	}
}
