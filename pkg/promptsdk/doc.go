// Package promptsdk provides the embedded in-process Data Plane SDK for
// rag-prompt-registry-service (ADR-001).
//
// In Phase 1, this package exposes the normative SDK contract from .ai/doc-api.md Section 7
// along with the global sync.Pool byte buffer allocator for zero-allocation interpolation.
package promptsdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrMissingVariable is returned by InterpolateIntoBufferChecked when a placeholder key is absent from vars.
var ErrMissingVariable = errors.New("missing template variable")

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
// Adjacent braces such as {{{x}}} treat outer braces as literals surrounding the innermost {{x}} placeholder
// (producing "{<val>}"). When a key is absent from vars, the raw {{key}} token is preserved in dst rather than
// silently omitted; callers that require an error on missing keys should use InterpolateIntoBufferChecked.
func InterpolateIntoBuffer(dst *bytes.Buffer, pattern string, vars map[string]string) {
	_ = interpolateBuffer(dst, pattern, vars, false)
}

// InterpolateIntoBufferChecked binds {{variable}} placeholders from pattern into dst and returns
// an error wrapping ErrMissingVariable if any placeholder key is not present in vars.
func InterpolateIntoBufferChecked(dst *bytes.Buffer, pattern string, vars map[string]string) error {
	return interpolateBuffer(dst, pattern, vars, true)
}

// interpolateBuffer implements the shared scanner for InterpolateIntoBuffer and InterpolateIntoBufferChecked.
func interpolateBuffer(dst *bytes.Buffer, pattern string, vars map[string]string, strictMissing bool) error {
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

		// Scan for '}}' starting after the initial '{{'
		closeIdx := -1
		for j := openIdx + 2; j+1 < n; j++ {
			if pattern[j] == '}' && pattern[j+1] == '}' {
				closeIdx = j
				break
			}
		}
		if closeIdx == -1 {
			dst.WriteString(pattern[i:])
			break
		}

		// Advance openIdx across any adjacent or nested '{{' before closeIdx so {{{x}}} binds the innermost {{x}}.
		for j := openIdx + 1; j+1 < closeIdx; j++ {
			if pattern[j] == '{' && pattern[j+1] == '{' {
				openIdx = j
			}
		}

		if openIdx > i {
			dst.WriteString(pattern[i:openIdx])
		}
		afterOpen := openIdx + 2
		key := pattern[afterOpen:closeIdx]
		if val, ok := vars[key]; ok {
			dst.WriteString(val)
		} else {
			dst.WriteString(pattern[openIdx : closeIdx+2])
			if strictMissing {
				return fmt.Errorf("%w: %q", ErrMissingVariable, key)
			}
		}
		i = closeIdx + 2
	}
	return nil
}
