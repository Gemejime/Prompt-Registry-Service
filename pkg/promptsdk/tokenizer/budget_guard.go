// Package tokenizer implements the zero-allocation streaming subword tokenizer
// and token budget safety guard for the in-process Data Plane SDK (Invariant 8).
package tokenizer

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// ErrBudgetExceeded is returned when the static template, dialect framing, and non-context
// variables exceed RenderOptions.MaxTokens or leave zero tokens for a non-empty context.
var ErrBudgetExceeded = errors.New("prompt token budget exceeded")

// EscapeMode identifies the dialect-specific escaping applied to runtime variable values.
type EscapeMode uint8

const (
	// EscapeNone performs no character escaping on bound variable values.
	EscapeNone EscapeMode = iota
	// EscapeAnthropicXML escapes &, <, and > into XML entities (&amp;, &lt;, &gt;).
	EscapeAnthropicXML
	// EscapeLlamaSpecial neutralizes reserved Meta Llama-3 control tokens (<|...|>) inside untrusted context.
	EscapeLlamaSpecial
	// EscapeJSONString escapes JSON string characters (", \, control chars) per RFC 8259.
	EscapeJSONString
)

// Tokenizer defines the zero-allocation token counting interface used by BudgetGuard.
type Tokenizer interface {
	// CountTokens returns the subword token count for text without heap allocations.
	CountTokens(text string) int
	// CountBytes returns the subword token count for data without heap allocations.
	CountBytes(data []byte) int
}

// CL100KTokenizer is a deterministic, zero-allocation BPE / TikToken cl100k_base-compatible
// subword tokenizer operating directly over UTF-8 bytes and runes.
type CL100KTokenizer struct{}

var defaultTokenizerInstance = &CL100KTokenizer{}

// NewCL100KTokenizer returns a zero-allocation CL100KTokenizer instance.
func NewCL100KTokenizer() *CL100KTokenizer {
	return defaultTokenizerInstance
}

// DefaultTokenizer returns the default singleton Tokenizer.
func DefaultTokenizer() Tokenizer {
	return defaultTokenizerInstance
}

// charClass categorizes characters into alphanumeric runs, whitespace runs, or punctuation.
type charClass uint8

const (
	classNone charClass = iota
	classAlnum
	classSpace
)

// tokenScannerState tracks the streaming BPE subword state on the stack (0 heap allocations).
type tokenScannerState struct {
	tokens   int
	runClass charClass
	runLen   int
}

// feedASCIIByte updates the token count and run tracking for a single ASCII byte.
func (s *tokenScannerState) feedASCIIByte(b byte) {
	if isAlnumByte(b) {
		if s.runClass == classAlnum && s.runLen < 4 {
			s.runLen++
			return
		}
		s.tokens++
		s.runClass = classAlnum
		s.runLen = 1
		return
	}
	if isSpaceByte(b) {
		if s.runClass == classSpace && s.runLen < 4 {
			s.runLen++
			return
		}
		s.tokens++
		s.runClass = classSpace
		s.runLen = 1
		return
	}
	// Punctuation / delimiter byte: 1 token, resets contiguous alnum/space run.
	s.tokens++
	s.runClass = classNone
	s.runLen = 0
}

// feedUTF8Rune updates the token counter for multi-byte UTF-8 runes.
func (s *tokenScannerState) feedUTF8Rune(r rune, width int) {
	s.runClass = classNone
	s.runLen = 0
	if r == utf8.RuneError && width <= 1 {
		s.tokens++
		return
	}
	if width <= 2 {
		s.tokens++
	} else {
		s.tokens += 2
	}
}

// feedLiteralASCII ingests a fixed ASCII string without intermediate allocations.
func (s *tokenScannerState) feedLiteralASCII(lit string) {
	for i := 0; i < len(lit); i++ {
		s.feedASCIIByte(lit[i])
	}
}

// isAlnumByte reports whether b is an ASCII alphanumeric character or underscore.
func isAlnumByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// isSpaceByte reports whether b is standard ASCII whitespace.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// matchLlamaSpecialToken checks whether text[idx:] starts with a reserved Meta Llama-3 special token
// and returns its byte length (or 0 if not matched).
func matchLlamaSpecialToken(text string, idx int) int {
	rem := len(text) - idx
	if rem < 10 || text[idx] != '<' || text[idx+1] != '|' {
		return 0
	}
	switch {
	case rem >= 10 && text[idx:idx+10] == "<|eot_id|>":
		return 10
	case rem >= 10 && text[idx:idx+10] == "<|eom_id|>":
		return 10
	case rem >= 14 && text[idx:idx+14] == "<|python_tag|>":
		return 14
	case rem >= 15 && text[idx:idx+15] == "<|end_of_text|>":
		return 15
	case rem >= 16 && text[idx:idx+16] == "<|end_header_id|>":
		return 16
	case rem >= 17 && text[idx:idx+17] == "<|begin_of_text|>":
		return 17
	case rem >= 19 && text[idx:idx+19] == "<|start_header_id|>":
		return 19
	default:
		return 0
	}
}

// matchLlamaSpecialTokenBytes checks whether data[idx:] matches a Meta Llama-3 special token byte sequence.
func matchLlamaSpecialTokenBytes(data []byte, idx int) int {
	rem := len(data) - idx
	if rem < 10 || data[idx] != '<' || data[idx+1] != '|' {
		return 0
	}
	switch {
	case rem >= 10 && string(data[idx:idx+10]) == "<|eot_id|>":
		return 10
	case rem >= 10 && string(data[idx:idx+10]) == "<|eom_id|>":
		return 10
	case rem >= 14 && string(data[idx:idx+14]) == "<|python_tag|>":
		return 14
	case rem >= 15 && string(data[idx:idx+15]) == "<|end_of_text|>":
		return 15
	case rem >= 16 && string(data[idx:idx+16]) == "<|end_header_id|>":
		return 16
	case rem >= 17 && string(data[idx:idx+17]) == "<|begin_of_text|>":
		return 17
	case rem >= 19 && string(data[idx:idx+19]) == "<|start_header_id|>":
		return 19
	default:
		return 0
	}
}

// CountTokens counts the BPE subword tokens in text with 0 heap allocations.
func (t *CL100KTokenizer) CountTokens(text string) int {
	var state tokenScannerState
	n := len(text)
	i := 0
	for i < n {
		b := text[i]
		if b < utf8.RuneSelf {
			if b == '<' {
				if specLen := matchLlamaSpecialToken(text, i); specLen > 0 {
					state.tokens++
					state.runClass = classNone
					state.runLen = 0
					i += specLen
					continue
				}
			}
			state.feedASCIIByte(b)
			i++
			continue
		}
		r, w := utf8.DecodeRuneInString(text[i:])
		state.feedUTF8Rune(r, w)
		i += w
	}
	return state.tokens
}

// CountBytes counts the BPE subword tokens in data with 0 heap allocations.
func (t *CL100KTokenizer) CountBytes(data []byte) int {
	var state tokenScannerState
	n := len(data)
	i := 0
	for i < n {
		b := data[i]
		if b < utf8.RuneSelf {
			if b == '<' {
				if specLen := matchLlamaSpecialTokenBytes(data, i); specLen > 0 {
					state.tokens++
					state.runClass = classNone
					state.runLen = 0
					i += specLen
					continue
				}
			}
			state.feedASCIIByte(b)
			i++
			continue
		}
		r, w := utf8.DecodeRune(data[i:])
		state.feedUTF8Rune(r, w)
		i += w
	}
	return state.tokens
}

// BudgetGuard calculates dialect structural framing overhead and truncates runtime context
// variables on valid UTF-8 and token boundaries before injection (Invariant 8).
type BudgetGuard struct {
	tokenizer Tokenizer
	isDefault bool
}

// NewBudgetGuard creates a BudgetGuard using the provided Tokenizer (or DefaultTokenizer if nil).
func NewBudgetGuard(tok Tokenizer) *BudgetGuard {
	if tok == nil {
		tok = DefaultTokenizer()
	}
	_, isDef := tok.(*CL100KTokenizer)
	return &BudgetGuard{
		tokenizer: tok,
		isDefault: isDef,
	}
}

// Tokenizer returns the underlying Tokenizer implementation.
func (g *BudgetGuard) Tokenizer() Tokenizer {
	if g == nil || g.tokenizer == nil {
		return DefaultTokenizer()
	}
	return g.tokenizer
}

// CountTokens returns the token count of text using the guard's Tokenizer.
func (g *BudgetGuard) CountTokens(text string) int {
	if g == nil || g.tokenizer == nil {
		return defaultTokenizerInstance.CountTokens(text)
	}
	return g.tokenizer.CountTokens(text)
}

// CountBytes returns the token count of data using the guard's Tokenizer.
func (g *BudgetGuard) CountBytes(data []byte) int {
	if g == nil || g.tokenizer == nil {
		return defaultTokenizerInstance.CountBytes(data)
	}
	return g.tokenizer.CountBytes(data)
}

// feedEscapedStep advances state by the escaped representation of the next rune or control token at text[i:].
func feedEscapedStep(state *tokenScannerState, text string, i int, mode EscapeMode) int {
	b := text[i]
	if b < utf8.RuneSelf {
		switch mode {
		case EscapeAnthropicXML:
			switch b {
			case '&':
				state.feedLiteralASCII("&amp;")
				return 1
			case '<':
				state.feedLiteralASCII("&lt;")
				return 1
			case '>':
				state.feedLiteralASCII("&gt;")
				return 1
			}
		case EscapeLlamaSpecial:
			if b == '<' {
				if specLen := matchLlamaSpecialToken(text, i); specLen > 0 {
					// Neutralized form: "< |" + inner + "| >"
					state.feedLiteralASCII("< |")
					state.feedLiteralASCII(text[i+2 : i+specLen-2])
					state.feedLiteralASCII("| >")
					return specLen
				}
			}
		case EscapeJSONString:
			switch b {
			case '"':
				state.feedLiteralASCII(`\"`)
				return 1
			case '\\':
				state.feedLiteralASCII(`\\`)
				return 1
			case '\b':
				state.feedLiteralASCII(`\b`)
				return 1
			case '\f':
				state.feedLiteralASCII(`\f`)
				return 1
			case '\n':
				state.feedLiteralASCII(`\n`)
				return 1
			case '\r':
				state.feedLiteralASCII(`\r`)
				return 1
			case '\t':
				state.feedLiteralASCII(`\t`)
				return 1
			default:
				if b < 0x20 {
					state.feedLiteralASCII(`\u0000`)
					return 1
				}
			}
		}
		state.feedASCIIByte(b)
		return 1
	}

	r, w := utf8.DecodeRuneInString(text[i:])
	state.feedUTF8Rune(r, w)
	return w
}

// CountEscapedTokens returns the exact token count of text after applying dialect-specific escaping,
// executing in a single streaming pass with 0 heap allocations.
func (g *BudgetGuard) CountEscapedTokens(text string, mode EscapeMode) int {
	if text == "" {
		return 0
	}
	if g != nil && !g.isDefault && mode == EscapeNone {
		return g.tokenizer.CountTokens(text)
	}
	var state tokenScannerState
	n := len(text)
	i := 0
	for i < n {
		i += feedEscapedStep(&state, text, i, mode)
	}
	if g != nil && !g.isDefault {
		rawCount := g.tokenizer.CountTokens(text)
		if rawCount > state.tokens {
			return rawCount
		}
	}
	return state.tokens
}

// TruncateToTokenBudget returns the longest valid UTF-8 prefix of text whose escaped representation
// consumes at most maxTokens tokens, along with the consumed token count. Performs 0 heap allocations.
func (g *BudgetGuard) TruncateToTokenBudget(text string, maxTokens int, mode EscapeMode) (string, int) {
	if maxTokens <= 0 || text == "" {
		return "", 0
	}

	var state tokenScannerState
	n := len(text)
	i := 0
	lastValidIdx := 0
	lastValidTokens := 0

	for i < n {
		nextState := state
		step := feedEscapedStep(&nextState, text, i, mode)
		if nextState.tokens > maxTokens {
			break
		}
		state = nextState
		i += step
		lastValidIdx = i
		lastValidTokens = state.tokens
	}

	// Ensure UTF-8 rune boundary safety.
	for lastValidIdx > 0 && lastValidIdx < len(text) && !utf8.RuneStart(text[lastValidIdx]) {
		lastValidIdx--
	}

	trimmed := text[:lastValidIdx]
	if g != nil && !g.isDefault && trimmed != "" {
		for trimmed != "" && g.tokenizer.CountTokens(trimmed) > maxTokens {
			_, rWidth := utf8.DecodeLastRuneInString(trimmed)
			if rWidth <= 0 {
				break
			}
			trimmed = trimmed[:len(trimmed)-rWidth]
		}
		return trimmed, g.tokenizer.CountTokens(trimmed)
	}
	return trimmed, lastValidTokens
}

// EnforceBudget validates that non-context overhead fits within maxTokens and truncates contextText
// on valid UTF-8 and token boundaries so the final rendered prompt never breaches maxTokens.
func (g *BudgetGuard) EnforceBudget(
	maxTokens int,
	overheadTokens int,
	contextText string,
	contextOccurrences int,
	mode EscapeMode,
) (string, error) {
	if maxTokens <= 0 {
		return contextText, nil
	}
	if overheadTokens > maxTokens {
		return "", fmt.Errorf("%w: template and dialect framing overhead (%d tokens) exceeds max_tokens (%d)",
			ErrBudgetExceeded, overheadTokens, maxTokens)
	}
	if contextOccurrences <= 0 || contextText == "" {
		return contextText, nil
	}

	remaining := maxTokens - overheadTokens
	perOccurrenceBudget := remaining / contextOccurrences
	if perOccurrenceBudget <= 0 {
		return "", fmt.Errorf("%w: zero token budget remaining for context (overhead=%d, max_tokens=%d)",
			ErrBudgetExceeded, overheadTokens, maxTokens)
	}

	escapedTokens := g.CountEscapedTokens(contextText, mode)
	if escapedTokens <= perOccurrenceBudget {
		return contextText, nil
	}

	trimmed, _ := g.TruncateToTokenBudget(contextText, perOccurrenceBudget, mode)
	if trimmed == "" {
		return "", fmt.Errorf("%w: available context budget (%d tokens) insufficient for first context token",
			ErrBudgetExceeded, perOccurrenceBudget)
	}
	return trimmed, nil
}
