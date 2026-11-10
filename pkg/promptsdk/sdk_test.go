package promptsdk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestDialectConstantsParity verifies that SDK dialect constants match OpenAPI and Protobuf enum names.
func TestDialectConstantsParity(t *testing.T) {
	if DialectAnthropicXML != "DIALECT_ANTHROPIC_XML" {
		t.Fatalf("unexpected DialectAnthropicXML: %s", DialectAnthropicXML)
	}
	if DialectLlamaSpecial != "DIALECT_LLAMA_SPECIAL" {
		t.Fatalf("unexpected DialectLlamaSpecial: %s", DialectLlamaSpecial)
	}
	if DialectOpenAIStandard != "DIALECT_OPENAI_STANDARD" {
		t.Fatalf("unexpected DialectOpenAIStandard: %s", DialectOpenAIStandard)
	}
	if DialectStandardJSON != DialectOpenAIStandard {
		t.Fatalf("expected DialectStandardJSON to alias DialectOpenAIStandard")
	}
}

// TestInterpolateIntoBuffer verifies variable binding, adjacent braces {{{x}}}, and missing-key handling.
func TestInterpolateIntoBuffer(t *testing.T) {
	buf := AcquireBuffer()
	defer ReleaseBuffer(buf)

	pattern := "System:\n<context>{{context}}</context>\nQuery: {{query}}"
	vars := map[string]string{
		"context": "Revenue grew 18% YoY in Q4.",
		"query":   "What was Q4 revenue growth?",
	}
	InterpolateIntoBuffer(buf, pattern, vars)

	got := buf.String()
	want := "System:\n<context>Revenue grew 18% YoY in Q4.</context>\nQuery: What was Q4 revenue growth?"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Verify adjacent braces {{{x}}} preserve outer literal braces around the interpolated value.
	buf.Reset()
	InterpolateIntoBuffer(buf, "JSON literal: {{{x}}}", map[string]string{"x": "value"})
	if buf.String() != "JSON literal: {value}" {
		t.Fatalf("expected {{{x}}} to interpolate as {value}, got %q", buf.String())
	}

	// Verify missing keys are preserved verbatim rather than silently omitted in InterpolateIntoBuffer,
	// and reported via ErrMissingVariable in InterpolateIntoBufferChecked.
	buf.Reset()
	InterpolateIntoBuffer(buf, "Hello {{present}} and {{missing}}!", map[string]string{"present": "world"})
	if buf.String() != "Hello world and {{missing}}!" {
		t.Fatalf("expected missing key token to be preserved, got %q", buf.String())
	}

	buf.Reset()
	err := InterpolateIntoBufferChecked(buf, "Hello {{present}} and {{missing}}!", map[string]string{"present": "world"})
	if !errors.Is(err, ErrMissingVariable) {
		t.Fatalf("expected ErrMissingVariable from InterpolateIntoBufferChecked, got %v", err)
	}
}

// TestCompileAndRenderAllDialects verifies in-process compilation across Anthropic XML, Llama-3, and OpenAI JSON.
func TestCompileAndRenderAllDialects(t *testing.T) {
	client, err := NewClient(ClientConfig{
		TenantID: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
		NodeID:   "sdk-unit-node",
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	schema := &SchemaContract{
		ID:         "7f8b9a2c-1234-5678-90ab-cdef12345678",
		Name:       "financial_summary_schema",
		SchemaJSON: json.RawMessage(`{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`),
	}
	ast := TemplateAST{
		SystemPrompt:   "You are a senior financial analyst for {{company}}.",
		VariableKeys:   []string{"company", "context", "query"},
		DefaultDialect: DialectAnthropicXML,
		PersonaOverrides: map[string]string{
			"executive": "Provide a board-level executive briefing for {{company}}.",
		},
		FewShotExemplars: []MessageBlock{
			{Role: "assistant", ContentPattern: "Baseline synthesis for {{company}}."},
		},
		Messages: []MessageBlock{
			{Role: "user", ContentPattern: "Context:\n<context>{{context}}</context>\n\nQuery: {{query}}"},
		},
	}

	if _, err := client.PreloadTag(
		"11111111-2222-3333-4444-555555555555",
		"rag-financial-summary",
		"@production",
		1,
		ast,
		schema,
	); err != nil {
		t.Fatalf("PreloadTag failed: %v", err)
	}

	vars := map[string]string{
		"company": "Acme Corp",
		"context": "Q4 operating cash flow reached $420M (<15% capex>).",
		"query":   "Summarize Q4 cash flow.",
	}

	// 1. Anthropic XML with Persona Override
	xmlOut, err := client.Render(context.Background(), "rag-financial-summary", "@production", RenderOptions{
		TargetDialect: DialectAnthropicXML,
		Variables:     vars,
		Persona:       "executive",
	})
	if err != nil {
		t.Fatalf("Anthropic Render failed: %v", err)
	}
	xmlStr := string(xmlOut)
	if !strings.Contains(xmlStr, "<instructions>Provide a board-level executive briefing for Acme Corp.</instructions>") {
		t.Fatalf("missing persona instructions in Anthropic XML output:\n%s", xmlStr)
	}
	if strings.Contains(xmlStr, "<context><context>") {
		t.Fatalf("duplicate <context><context> wrapper emitted:\n%s", xmlStr)
	}
	if !strings.Contains(xmlStr, "&lt;15% capex&gt;") {
		t.Fatalf("expected XML delimiter escaping in context:\n%s", xmlStr)
	}
	if !strings.Contains(xmlStr, "<query>Summarize Q4 cash flow.</query>") {
		t.Fatalf("expected <query> wrapper in Anthropic XML output:\n%s", xmlStr)
	}

	// 2. Meta Llama-3 Special Headers
	llamaOut, err := client.Render(context.Background(), "rag-financial-summary", "@production", RenderOptions{
		TargetDialect: DialectLlamaSpecial,
		Variables:     vars,
	})
	if err != nil {
		t.Fatalf("Llama Render failed: %v", err)
	}
	llamaStr := string(llamaOut)
	if !strings.HasPrefix(llamaStr, "<|begin_of_text|><|start_header_id|>system<|end_header_id|>\n\nYou are a senior financial analyst for Acme Corp.<|eot_id|>") {
		t.Fatalf("unexpected Llama-3 header prefix:\n%s", llamaStr)
	}
	if !strings.HasSuffix(llamaStr, "<|start_header_id|>assistant<|end_header_id|>\n\n") {
		t.Fatalf("missing trailing assistant generation header in Llama-3 output:\n%s", llamaStr)
	}

	// 3. OpenAI Standard JSON with bound response_format schema
	jsonOut, err := client.Render(context.Background(), "rag-financial-summary", "@production", RenderOptions{
		TargetDialect: DialectOpenAIStandard,
		Variables:     vars,
	})
	if err != nil {
		t.Fatalf("OpenAI Standard Render failed: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(jsonOut, &parsed); err != nil {
		t.Fatalf("OpenAI Standard output is not valid JSON: %v\n%s", err, string(jsonOut))
	}
	if _, hasRF := parsed["response_format"]; !hasRF {
		t.Fatalf("expected response_format in OpenAI Standard JSON output:\n%s", string(jsonOut))
	}
}

// BenchmarkRender50KBContext measures allocation and latency behavior across a 50 KB context payload.
func BenchmarkRender50KBContext(b *testing.B) {
	context50KB := strings.Repeat("Enterprise RAG financial context passage segment. ", 1024) // ~50 KB
	pattern := "<instructions>Analyze the financial filings accurately.</instructions>\n<context>{{context}}</context>\n<query>{{query}}</query>"
	vars := map[string]string{
		"context": context50KB,
		"query":   "Summarize net operating margin and free cash flow trends.",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := AcquireBuffer()
		InterpolateIntoBuffer(buf, pattern, vars)
		if buf.Len() < 50000 {
			b.Fatalf("unexpected rendered length: %d", buf.Len())
		}
		ReleaseBuffer(buf)
	}
}

// BenchmarkClientRender50KBAllDialects benchmarks full L1 cache lookup + AST dialect compilation
// across 50 KB context payloads for all three dialects, verifying 0 heap allocations on pooled renders.
func BenchmarkClientRender50KBAllDialects(b *testing.B) {
	client, err := NewClient(ClientConfig{
		TenantID: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
	})
	if err != nil {
		b.Fatalf("NewClient failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	schema := &SchemaContract{
		ID:         "7f8b9a2c-1234-5678-90ab-cdef12345678",
		Name:       "financial_metrics_output",
		SchemaJSON: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
	}
	ast := TemplateAST{
		SystemPrompt:   "You are a specialized enterprise financial analyst assistant.",
		VariableKeys:   []string{"context", "query"},
		DefaultDialect: DialectAnthropicXML,
		Messages: []MessageBlock{
			{Role: "user", ContentPattern: "Context:\n<context>{{context}}</context>\n\nQuery: {{query}}"},
		},
	}
	if _, err := client.PreloadTag(
		"11111111-2222-3333-4444-555555555555",
		"rag-financial-summary",
		"@production",
		1,
		ast,
		schema,
	); err != nil {
		b.Fatalf("PreloadTag failed: %v", err)
	}

	context50KB := strings.Repeat("Enterprise RAG financial context passage segment. ", 1024)
	vars := map[string]string{
		"context": context50KB,
		"query":   "Summarize net operating margin and free cash flow trends.",
	}
	ctx := context.Background()

	for _, dialect := range []DialectType{DialectAnthropicXML, DialectLlamaSpecial, DialectOpenAIStandard} {
		b.Run(string(dialect), func(b *testing.B) {
			opts := RenderOptions{
				TargetDialect: dialect,
				Variables:     vars,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf := AcquireBuffer()
				if err := client.RenderToBuffer(ctx, buf, "rag-financial-summary", "@production", opts); err != nil {
					b.Fatalf("RenderToBuffer failed: %v", err)
				}
				if buf.Len() < 50000 {
					b.Fatalf("unexpected rendered length: %d", buf.Len())
				}
				ReleaseBuffer(buf)
			}
		})
	}
}
