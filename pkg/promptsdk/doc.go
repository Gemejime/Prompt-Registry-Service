// Package promptsdk provides the embedded in-process Data Plane SDK for
// rag-prompt-registry-service (ADR-001).
//
// It implements a thread-safe, two-tier read-through L1 memory cache, Redis Pub/Sub
// invalidation subscriber, zero-allocation sync.Pool variable interpolation engine,
// deterministic multi-dialect AST transformers (Anthropic XML, Meta Llama-3 special
// headers, OpenAI/Mistral JSON schema mode), and a tokenizer-aware budget safety guard.
package promptsdk

import (
	"context"
	"encoding/json"
	"errors"

	"rag-prompt-registry-service/pkg/promptsdk/tokenizer"
)

// Sentinel errors returned by the in-process SDK compiler and client.
var (
	// ErrMissingVariable is returned when a template placeholder key is absent from RenderOptions.Variables
	// during checked interpolation or when RenderOptions.StrictVariables is true.
	ErrMissingVariable = errors.New("missing template variable")

	// ErrBudgetExceeded is returned when template framing and non-context variables exceed RenderOptions.MaxTokens.
	ErrBudgetExceeded = tokenizer.ErrBudgetExceeded

	// ErrUnsupportedDialect is returned when RenderOptions.TargetDialect (and the template's default dialect) is invalid.
	ErrUnsupportedDialect = errors.New("unsupported target dialect")

	// ErrTemplateNotFound is returned when a requested template, version, or tag pointer cannot be resolved.
	ErrTemplateNotFound = errors.New("template or version not found")
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

// IsValid reports whether d is a supported DialectType.
func (d DialectType) IsValid() bool {
	switch d {
	case DialectAnthropicXML, DialectLlamaSpecial, DialectOpenAIStandard:
		return true
	default:
		return false
	}
}

// MessageBlock represents a role-tagged message pattern within a TemplateAST.
type MessageBlock struct {
	Role           string `json:"role"`
	ContentPattern string `json:"content_pattern"`
}

// TemplateAST represents the structured abstract syntax tree of a prompt template version.
type TemplateAST struct {
	SystemPrompt     string            `json:"system_prompt"`
	Messages         []MessageBlock    `json:"messages"`
	VariableKeys     []string          `json:"variable_keys"`
	DefaultDialect   DialectType       `json:"default_dialect"`
	PersonaOverrides map[string]string `json:"persona_overrides,omitempty"`
	FewShotExemplars []MessageBlock    `json:"few_shot_exemplars,omitempty"`
}

// SchemaContract represents the registered JSON output schema bound to an immutable template version.
type SchemaContract struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	SchemaJSON json.RawMessage `json:"schema_json"`
}

// RenderOptions configures in-process variable interpolation, persona selection, token budgeting, and dialect framing.
type RenderOptions struct {
	TargetDialect   DialectType
	Variables       map[string]string
	MaxTokens       int
	Persona         string
	StrictVariables bool
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
