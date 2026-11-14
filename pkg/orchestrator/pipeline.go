package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"rag-prompt-registry-service/pkg/promptsdk"
	"rag-prompt-registry-service/pkg/promptsdk/tokenizer"
)

// PipelineConfig configures the RAG search orchestrator pipeline.
type PipelineConfig struct {
	// PromptSDK is the embedded in-process Data Plane PromptSDK.
	PromptSDK promptsdk.PromptSDK
	// Retriever optionally fetches parent document chunks for incoming queries.
	Retriever ContextRetriever
	// Dispatcher forwards formatted prompts to downstream inference engines.
	Dispatcher InferenceDispatcher
	// DefaultModel is the fallback model identifier (e.g. "claude-3-5-sonnet", "llama-3-70b").
	DefaultModel string
	// DefaultDialect is the fallback model dialect for prompt formatting.
	DefaultDialect promptsdk.DialectType
	// Tokenizer provides budget guard calculations (defaults to CL100KTokenizer).
	Tokenizer tokenizer.Tokenizer
	// Metrics tracks pipeline Prometheus counters and latency histograms.
	Metrics *PipelineMetrics
	// Logger is the structured logger (defaults to JSON on stdout, enforcing Invariant 2 redaction).
	Logger *slog.Logger
}

// RAGPipeline coordinates the 4-stage Data Plane execution pipeline (ADR-001):
// Stage 1: Context Assembly -> Stage 2: In-Process Compilation -> Stage 3: Budget Guarding -> Stage 4: Inference Forwarding.
type RAGPipeline struct {
	cfg         PipelineConfig
	promptSDK   promptsdk.PromptSDK
	retriever   ContextRetriever
	dispatcher  InferenceDispatcher
	budgetGuard *tokenizer.BudgetGuard
	metrics     *PipelineMetrics
	logger      *slog.Logger
}

// NewRAGPipeline creates an initialized RAGPipeline instance.
func NewRAGPipeline(cfg PipelineConfig) (*RAGPipeline, error) {
	if cfg.PromptSDK == nil {
		return nil, fmt.Errorf("PromptSDK is required for RAGPipeline")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parsePipelineLogLevel(os.Getenv("LOG_LEVEL"))}))
	}
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = NewPipelineMetrics()
	}

	return &RAGPipeline{
		cfg:         cfg,
		promptSDK:   cfg.PromptSDK,
		retriever:   cfg.Retriever,
		dispatcher:  cfg.Dispatcher,
		budgetGuard: tokenizer.NewBudgetGuard(cfg.Tokenizer),
		metrics:     metrics,
		logger:      logger,
	}, nil
}

// parsePipelineLogLevel converts a LOG_LEVEL environment value to slog.Level, defaulting to INFO.
func parsePipelineLogLevel(raw string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// PromptSDK returns the underlying embedded PromptSDK.
func (p *RAGPipeline) PromptSDK() promptsdk.PromptSDK {
	return p.promptSDK
}

// Metrics returns the pipeline's Prometheus metrics collector.
func (p *RAGPipeline) Metrics() *PipelineMetrics {
	return p.metrics
}

// MetricsHandler returns an HTTP handler exposing both the embedded PromptSDK metrics
// (including prompt_sdk_render_duration_seconds_bucket) and orchestrator pipeline metrics.
func (p *RAGPipeline) MetricsHandler() http.Handler {
	type sdkExposer interface {
		MetricsExposition() string
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if exposer, ok := p.promptSDK.(sdkExposer); ok {
			_, _ = io.WriteString(w, exposer.MetricsExposition())
		}
		if p.metrics != nil {
			_, _ = io.WriteString(w, p.metrics.MetricsExposition())
		}
	})
}

// Execute runs the complete 4-stage RAG pipeline.
func (p *RAGPipeline) Execute(ctx context.Context, req PipelineRequest) (*PipelineResponse, error) {
	startTotal := time.Now()
	mode := req.Mode
	if mode == "" {
		mode = ExecutionModeProduction
	}

	effDialect := req.TargetDialect
	if effDialect == "" {
		effDialect = p.cfg.DefaultDialect
	}
	if effDialect == "" {
		effDialect = promptsdk.DialectAnthropicXML
	}

	// -------------------------------------------------------------
	// Stage 1: Context Assembly
	// -------------------------------------------------------------
	startAssembly := time.Now()
	chunks := req.PreRetrievedChunks
	if len(chunks) == 0 && p.retriever != nil && strings.TrimSpace(req.Query) != "" {
		retrieved, err := p.retriever.Retrieve(ctx, req.Query)
		if err != nil {
			p.metrics.ObservePipelineDuration(mode, string(effDialect), time.Since(startTotal).Seconds(), false)
			return nil, fmt.Errorf("context retrieval failed: %w", err)
		}
		chunks = retrieved
	}

	assembledContext := assembleChunks(chunks)
	contextBytes := len(assembledContext)
	p.metrics.ObserveContextBytes(contextBytes)
	assemblyDur := time.Since(startAssembly)

	// Bind conversational variables
	renderVars := make(map[string]string, len(req.Variables)+2)
	for k, v := range req.Variables {
		renderVars[k] = v
	}
	if _, ok := renderVars["query"]; !ok && req.Query != "" {
		renderVars["query"] = req.Query
	}
	if _, ok := renderVars["context"]; !ok && assembledContext != "" {
		renderVars["context"] = assembledContext
	}

	// -------------------------------------------------------------
	// Stage 2 & 3: In-Process Compilation & Tokenizer Budget Guard
	// -------------------------------------------------------------
	startRender := time.Now()
	buf := promptsdk.AcquireBuffer()
	defer promptsdk.ReleaseBuffer(buf)

	renderOpts := promptsdk.RenderOptions{
		TargetDialect:   effDialect,
		Variables:       renderVars,
		Persona:         req.Persona,
		MaxTokens:       req.MaxTokens,
		StrictVariables: req.StrictVariables,
	}

	if err := p.promptSDK.RenderToBuffer(ctx, buf, req.TemplateID, req.TagOrVersion, renderOpts); err != nil {
		p.metrics.ObservePipelineDuration(mode, string(effDialect), time.Since(startTotal).Seconds(), false)
		return nil, fmt.Errorf("prompt rendering failed: %w", err)
	}
	renderDur := time.Since(startRender)

	// Copy rendered prompt for downstream forwarding and caller retention
	compiledBytes := make([]byte, buf.Len())
	copy(compiledBytes, buf.Bytes())

	// -------------------------------------------------------------
	// Stage 4: Inference Forwarding
	// -------------------------------------------------------------
	startInference := time.Now()
	var infResp *InferenceResponse
	var inferenceDur time.Duration

	if p.dispatcher != nil {
		model := req.Model
		if model == "" {
			model = p.cfg.DefaultModel
		}
		resp, err := p.dispatcher.Dispatch(ctx, InferenceRequest{
			Model:     model,
			Prompt:    compiledBytes,
			MaxTokens: req.MaxTokens,
		})
		if err != nil {
			p.metrics.ObservePipelineDuration(mode, string(effDialect), time.Since(startTotal).Seconds(), false)
			return nil, fmt.Errorf("inference dispatch failed: %w", err)
		}
		infResp = resp
		inferenceDur = time.Since(startInference)
	}

	totalDur := time.Since(startTotal)
	p.metrics.ObservePipelineDuration(mode, string(effDialect), totalDur.Seconds(), true)

	// Output text resolution
	outputText := ""
	if infResp != nil {
		outputText = infResp.OutputText
	} else {
		outputText = string(compiledBytes)
	}

	resp := &PipelineResponse{
		OutputText:        outputText,
		InferenceResponse: infResp,
		CompiledPrompt:    compiledBytes,
	}

	// Diagnostic Mode: Waterfall timings and token budget breakdown
	if mode == ExecutionModeDiagnostic {
		totalTokens := p.budgetGuard.CountBytes(compiledBytes)
		ctxTokens := p.budgetGuard.CountTokens(assembledContext)
		framingOverhead := totalTokens - ctxTokens
		if framingOverhead < 0 {
			framingOverhead = 0
		}
		truncationApplied := req.MaxTokens > 0 && totalTokens >= req.MaxTokens

		resp.Diagnostic = &DiagnosticTrace{
			Timing: TimingWaterfall{
				AssemblyMs:  float64(assemblyDur.Nanoseconds()) / 1e6,
				SDKRenderMs: float64(renderDur.Nanoseconds()) / 1e6,
				InferenceMs: float64(inferenceDur.Nanoseconds()) / 1e6,
				TotalMs:     float64(totalDur.Nanoseconds()) / 1e6,
			},
			TokenBudget: TokenBudgetTrace{
				BudgetLimit:       req.MaxTokens,
				TokensUsed:        totalTokens,
				FramingOverhead:   framingOverhead,
				ContextTokens:     ctxTokens,
				TruncationApplied: truncationApplied,
			},
			RetrievedChunks: chunks,
			Dialect:         string(effDialect),
			CompiledPrompt:  string(compiledBytes),
		}
	}

	// Invariant 2: Secured Logging (operational metadata only, strictly redacting raw context & prompts)
	p.logger.InfoContext(ctx, "rag orchestrator pipeline executed",
		slog.String("template_id", req.TemplateID),
		slog.String("tag_or_version", req.TagOrVersion),
		slog.String("mode", string(mode)),
		slog.String("dialect", string(effDialect)),
		slog.Int("context_bytes", contextBytes),
		slog.Int("chunks_count", len(chunks)),
		slog.Float64("assembly_ms", float64(assemblyDur.Microseconds())/1000.0),
		slog.Float64("render_ms", float64(renderDur.Microseconds())/1000.0),
		slog.Float64("total_ms", float64(totalDur.Microseconds())/1000.0),
	)

	return resp, nil
}

// assembleChunks concatenates retrieved chunk contents into a clean context block.
func assembleChunks(chunks []RetrievedChunk) string {
	if len(chunks) == 0 {
		return ""
	}
	if len(chunks) == 1 {
		return chunks[0].Text
	}
	var sb strings.Builder
	for i, chunk := range chunks {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(chunk.Text)
	}
	return sb.String()
}
