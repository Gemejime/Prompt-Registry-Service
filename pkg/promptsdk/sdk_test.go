package promptsdk

import (
	"strings"
	"testing"
)

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
}

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
