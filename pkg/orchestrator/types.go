package orchestrator

import (
	"context"

	"rag-prompt-registry-service/pkg/promptsdk"
)

// ExecutionMode designates either high-throughput production execution or detailed diagnostic tracing.
type ExecutionMode string

const (
	// ExecutionModeProduction delivers compiled prompt execution and inference with minimal latency overhead.
	ExecutionModeProduction ExecutionMode = "production"
	// ExecutionModeDiagnostic returns waterfall latency timings, token budget breakdowns, and chunk provenance.
	ExecutionModeDiagnostic ExecutionMode = "diagnostic"
)

// RetrievedChunk represents an assembled context chunk or parent document container.
type RetrievedChunk struct {
	ChunkID    string            `json:"chunk_id"`
	DocumentID string            `json:"document_id"`
	Text       string            `json:"text"`
	Score      float64           `json:"score"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// ContextRetriever decouples query-dependent document retrieval from prompt compilation.
type ContextRetriever interface {
	Retrieve(ctx context.Context, query string) ([]RetrievedChunk, error)
}

// InferenceRequest defines the input payload forwarded to downstream model runtimes (Triton/vLLM/LLM Gateway).
type InferenceRequest struct {
	Model       string            `json:"model"`
	Prompt      []byte            `json:"prompt"`
	MaxTokens   int               `json:"max_tokens"`
	Temperature float64           `json:"temperature,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// InferenceResponse captures the downstream model inference result.
type InferenceResponse struct {
	ModelID      string `json:"model_id"`
	OutputText   string `json:"output_text"`
	FinishReason string `json:"finish_reason"`
	TokensUsed   int    `json:"tokens_used"`
}

// InferenceDispatcher isolates network model invocation outside the prompt domain (ADR-001).
type InferenceDispatcher interface {
	Dispatch(ctx context.Context, req InferenceRequest) (*InferenceResponse, error)
}

// PipelineRequest specifies the parameters for a RAG orchestrator pipeline execution.
type PipelineRequest struct {
	Query              string                `json:"query"`
	TemplateID         string                `json:"template_id"`
	TagOrVersion       string                `json:"tag_or_version"`
	TargetDialect      promptsdk.DialectType `json:"target_dialect,omitempty"`
	Variables          map[string]string     `json:"variables,omitempty"`
	Persona            string                `json:"persona,omitempty"`
	MaxTokens          int                   `json:"max_tokens,omitempty"`
	Model              string                `json:"model,omitempty"`
	Mode               ExecutionMode         `json:"mode,omitempty"`
	StrictVariables    bool                  `json:"strict_variables,omitempty"`
	PreRetrievedChunks []RetrievedChunk      `json:"pre_retrieved_chunks,omitempty"`
}

// TimingWaterfall captures stage-by-stage latencies in milliseconds for diagnostic inspection.
type TimingWaterfall struct {
	AssemblyMs  float64 `json:"assembly_ms"`
	SDKRenderMs float64 `json:"sdk_render_ms"`
	InferenceMs float64 `json:"inference_ms"`
	TotalMs     float64 `json:"total_ms"`
}

// TokenBudgetTrace details token usage, dialect framing structural overhead, and truncation status.
type TokenBudgetTrace struct {
	BudgetLimit       int  `json:"budget_limit"`
	TokensUsed        int  `json:"tokens_used"`
	FramingOverhead   int  `json:"framing_overhead"`
	ContextTokens     int  `json:"context_tokens"`
	TruncationApplied bool `json:"truncation_applied"`
}

// DiagnosticTrace aggregates complete diagnostic waterfall metrics and provenance metadata.
type DiagnosticTrace struct {
	Timing          TimingWaterfall  `json:"timing"`
	TokenBudget     TokenBudgetTrace `json:"token_budget"`
	RetrievedChunks []RetrievedChunk `json:"retrieved_chunks"`
	Dialect         string           `json:"dialect"`
	CompiledPrompt  string           `json:"compiled_prompt,omitempty"`
}

// PipelineResponse is the unified result of the 4-stage RAG orchestrator pipeline.
type PipelineResponse struct {
	OutputText        string             `json:"output_text"`
	InferenceResponse *InferenceResponse `json:"inference_response,omitempty"`
	CompiledPrompt    []byte             `json:"compiled_prompt,omitempty"`
	Diagnostic        *DiagnosticTrace   `json:"diagnostic,omitempty"`
}
