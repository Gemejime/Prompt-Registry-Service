// Package promptsdk provides the embedded in-process Data Plane SDK for
// rag-prompt-registry-service (ADR-001).
//
// In Phase 1, this package exposes the normative SDK contract from .ai/doc-api.md Section 7
// along with the global sync.Pool byte buffer allocator for zero-allocation interpolation.
package promptsdk

import (
	"bytes"
	"context"
	"sync"
)

// DialectType represents the target model dialect for AST adaptation.
type DialectType string

const (
	// DialectType constants map 1:1 to the REST OpenAPI and Protobuf DialectTarget enum values.
	DialectAnthropicXML   DialectType = "DIALECT_ANTHROPIC_XML"
	DialectLlamaSpecial   DialectType = "DIALECT_LLAMA_SPECIAL"
	DialectOpenAIStandard DialectType = "DIALECT_OPENAI_STANDARD"
	// DialectStandardJSON is an explicit alias for DialectOpenAIStandard ("DIALECT_OPENAI_STANDARD").
	DialectStandardJSON DialectType = DialectOpenAIStandard
)

// RenderOptions configures in-process variable interpolation and dialect framing.
type RenderOptions struct {
	TargetDialect DialectType
	Variables     map[string]string
	MaxTokens     int
}

// PromptSDK defines the embedded Data Plane SDK contract (.ai/doc-api.md Section 7).
type PromptSDK interface {
	// Render compiles template AST and binds context variables using sync.Pool.
	// Zero allocations on hot path; P99 <= 0.5ms.
	Render(ctx context.Context, templateID string, tagOrVersion string, opts RenderOptions) ([]byte, error)

	// Invalidate explicitly forces cache eviction.
	Invalidate(templateID string, tag string)

	// Close cleanly terminates Redis Pub/Sub listeners.
	Close() error
}

// renderBufferPool holds reusable 64 KiB byte buffers to achieve 0 heap allocations
// across 50 KB context blocks (Invariant 4).
var renderBufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 64*1024))
	},
}

// AcquireBuffer retrieves a reset *bytes.Buffer from the global sync.Pool.
func AcquireBuffer() *bytes.Buffer {
	buf := renderBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// ReleaseBuffer resets and returns a *bytes.Buffer to the global sync.Pool.
func ReleaseBuffer(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	buf.Reset()
	renderBufferPool.Put(buf)
}

// InterpolateIntoBuffer binds {{variable}} placeholders from pattern into dst without heap allocations.
func InterpolateIntoBuffer(dst *bytes.Buffer, pattern string, vars map[string]string) {
	n := len(pattern)
	i := 0
	for i < n {
		// Scan for '{{'
		openIdx := -1
		for j := i; j+1 < n; j++ {
			if pattern[j] == '{' && pattern[j+1] == '{' {
				openIdx = j
				break
			}
		}
		if openIdx == -1 {
			dst.WriteString(pattern[i:])
			break
		}
		if openIdx > i {
			dst.WriteString(pattern[i:openIdx])
		}
		afterOpen := openIdx + 2
		closeIdx := -1
		for j := afterOpen; j+1 < n; j++ {
			if pattern[j] == '}' && pattern[j+1] == '}' {
				closeIdx = j
				break
			}
		}
		if closeIdx == -1 {
			dst.WriteString(pattern[openIdx:])
			break
		}
		key := pattern[afterOpen:closeIdx]
		if val, ok := vars[key]; ok {
			dst.WriteString(val)
		}
		i = closeIdx + 2
	}
}
