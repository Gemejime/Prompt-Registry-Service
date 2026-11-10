package promptsdk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"rag-prompt-registry-service/pkg/promptsdk/dialects"
	"rag-prompt-registry-service/pkg/promptsdk/tokenizer"
)

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

// interpolateBuffer implements the shared zero-allocation scanner for InterpolateIntoBuffer and InterpolateIntoBufferChecked.
func interpolateBuffer(dst *bytes.Buffer, pattern string, vars map[string]string, strictMissing bool) error {
	n := len(pattern)
	i := 0
	for i < n {
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

// CompiledTemplate is an immutable, pre-parsed representation of a template version and its
// output schema contract stored in the L1 version cache for zero-allocation hot-path rendering.
type CompiledTemplate struct {
	TemplateID         string
	TemplateName       string
	Version            int
	SchemaID           string
	AST                TemplateAST
	Schema             *SchemaContract
	TruncatedHash      string
	SystemBlock        dialects.CompiledBlock
	PersonaBlocks      map[string]dialects.CompiledBlock
	ExemplarBlocks     []dialects.CompiledBlock
	MessageBlocks      []dialects.CompiledBlock
	ResponseFormatTail string
}

// CompileTemplateAST pre-parses a TemplateAST and optional SchemaContract into an immutable CompiledTemplate.
func CompileTemplateAST(
	templateID string,
	templateName string,
	version int,
	ast TemplateAST,
	schema *SchemaContract,
) (*CompiledTemplate, error) {
	if len(ast.Messages) == 0 && strings.TrimSpace(ast.SystemPrompt) == "" {
		return nil, fmt.Errorf("%w: template AST must contain a system_prompt or messages", ErrTemplateNotFound)
	}

	sysBlock := dialects.CompileBlock("system", ast.SystemPrompt, false)

	var personaBlocks map[string]dialects.CompiledBlock
	if len(ast.PersonaOverrides) > 0 {
		personaBlocks = make(map[string]dialects.CompiledBlock, len(ast.PersonaOverrides))
		for persona, promptText := range ast.PersonaOverrides {
			personaBlocks[persona] = dialects.CompileBlock("system", promptText, false)
		}
	}

	exemplarBlocks := make([]dialects.CompiledBlock, len(ast.FewShotExemplars))
	for i, ex := range ast.FewShotExemplars {
		exemplarBlocks[i] = dialects.CompileBlock(ex.Role, ex.ContentPattern, false)
	}

	msgBlocks := make([]dialects.CompiledBlock, len(ast.Messages))
	for i, msg := range ast.Messages {
		msgBlocks[i] = dialects.CompileBlock(msg.Role, msg.ContentPattern, true)
	}

	var schemaID string
	var responseFormatTail string
	if schema != nil {
		schemaID = schema.ID
		responseFormatTail = dialects.BuildResponseFormatTail(schema.ID, schema.Name, schema.SchemaJSON)
	}

	return &CompiledTemplate{
		TemplateID:         strings.TrimSpace(templateID),
		TemplateName:       strings.TrimSpace(templateName),
		Version:            version,
		SchemaID:           schemaID,
		AST:                ast,
		Schema:             schema,
		TruncatedHash:      computeTruncatedASTHash(ast, schemaID),
		SystemBlock:        sysBlock,
		PersonaBlocks:      personaBlocks,
		ExemplarBlocks:     exemplarBlocks,
		MessageBlocks:      msgBlocks,
		ResponseFormatTail: responseFormatTail,
	}, nil
}

// computeTruncatedASTHash returns the 16-character SHA-256 hex digest of the AST for Secured Logging (Invariant 2).
func computeTruncatedASTHash(ast TemplateAST, schemaID string) string {
	raw, err := json.Marshal(struct {
		AST      TemplateAST `json:"ast"`
		SchemaID string      `json:"schema_id"`
	}{
		AST:      ast,
		SchemaID: schemaID,
	})
	if err != nil {
		return "0000000000000000"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// activeSystemBlock selects PersonaBlocks[persona] when available, falling back to SystemBlock.
func (ct *CompiledTemplate) activeSystemBlock(persona string) dialects.CompiledBlock {
	if persona != "" && len(ct.PersonaBlocks) > 0 {
		if block, ok := ct.PersonaBlocks[persona]; ok {
			return block
		}
	}
	return ct.SystemBlock
}

// countContextOccurrences counts the number of {{context}} variable bindings in the active AST blocks.
func countContextOccurrences(
	sysBlock dialects.CompiledBlock,
	exemplars []dialects.CompiledBlock,
	messages []dialects.CompiledBlock,
) int {
	count := 0
	for i := 0; i < len(sysBlock.Segments); i++ {
		if sysBlock.Segments[i].IsVar && sysBlock.Segments[i].Text == "context" {
			count++
		}
	}
	for i := 0; i < len(exemplars); i++ {
		segs := exemplars[i].Segments
		for j := 0; j < len(segs); j++ {
			if segs[j].IsVar && segs[j].Text == "context" {
				count++
			}
		}
	}
	for i := 0; i < len(messages); i++ {
		segs := messages[i].Segments
		for j := 0; j < len(segs); j++ {
			if segs[j].IsVar && segs[j].Text == "context" {
				count++
			}
		}
	}
	return count
}

// CompileIntoBuffer compiles the pre-parsed CompiledTemplate with opts directly into dst
// performing 0 heap allocations on the hot path (Invariant 4 & Invariant 8).
func CompileIntoBuffer(
	dst *bytes.Buffer,
	compiled *CompiledTemplate,
	opts RenderOptions,
	guard *tokenizer.BudgetGuard,
) error {
	if compiled == nil {
		return fmt.Errorf("%w: nil compiled template", ErrTemplateNotFound)
	}

	targetDialect := opts.TargetDialect
	if targetDialect == "" {
		targetDialect = compiled.AST.DefaultDialect
	}
	if !targetDialect.IsValid() {
		return fmt.Errorf("%w: %q", ErrUnsupportedDialect, targetDialect)
	}

	sysBlock := compiled.activeSystemBlock(opts.Persona)

	// Fast path when no token ceiling is specified (MaxTokens <= 0): single-pass zero-allocation render.
	if opts.MaxTokens <= 0 {
		return renderDialectPass(dst, compiled, sysBlock, targetDialect, opts.Variables, "", false, opts.StrictVariables)
	}

	// Token-budgeted path (Invariant 8): calculate exact dialect framing + non-context variable overhead first.
	if guard == nil {
		guard = tokenizer.NewBudgetGuard(nil)
	}

	ctxOccurrences := countContextOccurrences(sysBlock, compiled.ExemplarBlocks, compiled.MessageBlocks)
	var rawContext string
	var hasContextVar bool
	if opts.Variables != nil {
		rawContext, hasContextVar = opts.Variables["context"]
	}
	if opts.StrictVariables && ctxOccurrences > 0 && !hasContextVar {
		return fmt.Errorf("%w: %q", ErrMissingVariable, "context")
	}

	// Measure exact structural + non-context token overhead by rendering with an empty context override into dst.
	dst.Reset()
	if err := renderDialectPass(dst, compiled, sysBlock, targetDialect, opts.Variables, "", true, opts.StrictVariables); err != nil {
		dst.Reset()
		return err
	}
	overheadTokens := guard.CountBytes(dst.Bytes())

	// If there is no context to inject, verify the non-context render fits within MaxTokens.
	if ctxOccurrences == 0 || rawContext == "" {
		if overheadTokens > opts.MaxTokens {
			dst.Reset()
			return fmt.Errorf("%w: template and dialect framing overhead (%d tokens) exceeds max_tokens (%d)",
				ErrBudgetExceeded, overheadTokens, opts.MaxTokens)
		}
		return nil
	}

	var escMode tokenizer.EscapeMode
	switch targetDialect {
	case DialectAnthropicXML:
		escMode = tokenizer.EscapeAnthropicXML
	case DialectLlamaSpecial:
		escMode = tokenizer.EscapeLlamaSpecial
	case DialectOpenAIStandard:
		escMode = tokenizer.EscapeJSONString
	}

	trimmedContext, err := guard.EnforceBudget(opts.MaxTokens, overheadTokens, rawContext, ctxOccurrences, escMode)
	if err != nil {
		dst.Reset()
		return err
	}

	dst.Reset()
	if err := renderDialectPass(dst, compiled, sysBlock, targetDialect, opts.Variables, trimmedContext, true, opts.StrictVariables); err != nil {
		dst.Reset()
		return err
	}

	// Boundary-fusion verification pass: guarantee CountBytes(dst.Bytes()) <= opts.MaxTokens even on 1-token boundary merges.
	actualTokens := guard.CountBytes(dst.Bytes())
	if actualTokens > opts.MaxTokens {
		perOccurrenceBudget := (opts.MaxTokens - overheadTokens) / ctxOccurrences
		for actualTokens > opts.MaxTokens {
			excess := actualTokens - opts.MaxTokens
			stepDown := excess / ctxOccurrences
			if stepDown < 1 {
				stepDown = 1
			}
			perOccurrenceBudget -= stepDown
			if perOccurrenceBudget <= 0 {
				dst.Reset()
				return fmt.Errorf("%w: context budget exhausted at token boundary (max_tokens=%d)", ErrBudgetExceeded, opts.MaxTokens)
			}
			trimmedContext, _ = guard.TruncateToTokenBudget(rawContext, perOccurrenceBudget, escMode)
			if trimmedContext == "" {
				dst.Reset()
				return fmt.Errorf("%w: context truncated to empty at token boundary (max_tokens=%d)", ErrBudgetExceeded, opts.MaxTokens)
			}
			dst.Reset()
			if err := renderDialectPass(dst, compiled, sysBlock, targetDialect, opts.Variables, trimmedContext, true, opts.StrictVariables); err != nil {
				dst.Reset()
				return err
			}
			actualTokens = guard.CountBytes(dst.Bytes())
		}
	}

	return nil
}

// renderDialectPass dispatches to the target dialect formatter into dst with 0 heap allocations.
func renderDialectPass(
	dst *bytes.Buffer,
	compiled *CompiledTemplate,
	sysBlock dialects.CompiledBlock,
	targetDialect DialectType,
	vars map[string]string,
	contextOverride string,
	hasContextOverride bool,
	strictMissing bool,
) error {
	var missingKey string
	var failed bool

	switch targetDialect {
	case DialectAnthropicXML:
		missingKey, failed = dialects.RenderAnthropicCompiled(
			dst,
			sysBlock,
			compiled.ExemplarBlocks,
			compiled.MessageBlocks,
			vars,
			contextOverride,
			hasContextOverride,
			strictMissing,
		)
	case DialectLlamaSpecial:
		missingKey, failed = dialects.RenderLlamaCompiled(
			dst,
			sysBlock,
			compiled.ExemplarBlocks,
			compiled.MessageBlocks,
			vars,
			contextOverride,
			hasContextOverride,
			strictMissing,
		)
	case DialectOpenAIStandard:
		missingKey, failed = dialects.RenderStandardJSONCompiled(
			dst,
			sysBlock,
			compiled.ExemplarBlocks,
			compiled.MessageBlocks,
			compiled.ResponseFormatTail,
			vars,
			contextOverride,
			hasContextOverride,
			strictMissing,
		)
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedDialect, targetDialect)
	}

	if failed {
		return fmt.Errorf("%w: %q", ErrMissingVariable, missingKey)
	}
	return nil
}

// Compile renders compiled with opts using a pooled buffer and returns an owned byte slice copy.
func Compile(compiled *CompiledTemplate, opts RenderOptions, guard *tokenizer.BudgetGuard) ([]byte, error) {
	buf := AcquireBuffer()
	defer ReleaseBuffer(buf)

	if err := CompileIntoBuffer(buf, compiled, opts, guard); err != nil {
		return nil, err
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

