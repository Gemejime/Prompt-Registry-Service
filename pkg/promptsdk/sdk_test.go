package promptsdk

import (
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
