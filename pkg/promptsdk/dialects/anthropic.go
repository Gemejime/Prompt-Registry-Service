// Package dialects implements deterministic, zero-allocation multi-dialect AST
// transformers for Anthropic XML, Meta Llama-3 special headers, and OpenAI/Mistral
// standard JSON schema envelopes.
package dialects

import (
	"bytes"
	"strings"
)

// Segment represents a pre-parsed literal span or {{variable}} placeholder within a template pattern.
type Segment struct {
	Text           string
	IsVar          bool
	RawPlaceholder string
	AutoWrapXMLTag string
}

// CompiledBlock holds the pre-parsed segments and structural metadata for a single role message or system prompt.
type CompiledBlock struct {
	Role                 string
	Pattern              string
	Segments             []Segment
	HasInstructionsOuter bool
	HasContextXMLTag     bool
	HasQueryXMLTag       bool
}

// CompileBlock pre-parses pattern into a flat slice of Segments and detects existing XML wrapper tags.
func CompileBlock(role, pattern string, autoDetectXMLTags bool) CompiledBlock {
	trimmed := strings.TrimSpace(pattern)
	hasInst := strings.HasPrefix(trimmed, "<instructions>") && strings.HasSuffix(trimmed, "</instructions>")
	hasCtx := strings.Contains(pattern, "<context>") && strings.Contains(pattern, "</context>")
	hasQuery := strings.Contains(pattern, "<query>") && strings.Contains(pattern, "</query>")

	segs := ParseSegments(pattern, autoDetectXMLTags)
	return CompiledBlock{
		Role:                 role,
		Pattern:              pattern,
		Segments:             segs,
		HasInstructionsOuter: hasInst,
		HasContextXMLTag:     hasCtx,
		HasQueryXMLTag:       hasQuery,
	}
}

// ParseSegments splits pattern into literal and {{variable}} segments using the innermost brace rule.
// When autoDetectXMLTags is true, {{context}} and {{query}} placeholders not already enclosed in
// <context>...</context> or <query>...</query> within pattern are marked for idempotent XML wrapping.
func ParseSegments(pattern string, autoDetectXMLTags bool) []Segment {
	var segments []Segment
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
			segments = append(segments, Segment{Text: pattern[i:], IsVar: false})
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
			segments = append(segments, Segment{Text: pattern[i:], IsVar: false})
			break
		}

		// Advance openIdx across adjacent or nested '{{' before closeIdx so {{{x}}} binds innermost {{x}}.
		for j := openIdx + 1; j+1 < closeIdx; j++ {
			if pattern[j] == '{' && pattern[j+1] == '{' {
				openIdx = j
			}
		}

		if openIdx > i {
			segments = append(segments, Segment{Text: pattern[i:openIdx], IsVar: false})
		}

		rawKey := strings.TrimSpace(pattern[openIdx+2 : closeIdx])
		rawPlaceholder := pattern[openIdx : closeIdx+2]

		var autoTag string
		if autoDetectXMLTags && (rawKey == "context" || rawKey == "query") {
			if !isEnclosedInXMLTag(pattern, openIdx, closeIdx+2, rawKey) {
				autoTag = rawKey
			}
		}

		segments = append(segments, Segment{
			Text:           rawKey,
			IsVar:          true,
			RawPlaceholder: rawPlaceholder,
			AutoWrapXMLTag: autoTag,
		})
		i = closeIdx + 2
	}
	return segments
}

// isEnclosedInXMLTag checks whether pattern[openIdx:closeEnd] is already wrapped inside <tag>...</tag>.
func isEnclosedInXMLTag(pattern string, openIdx, closeEnd int, tag string) bool {
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	before := pattern[:openIdx]
	after := pattern[closeEnd:]

	lastOpen := strings.LastIndex(before, openTag)
	if lastOpen == -1 {
		return false
	}
	if strings.LastIndex(before, closeTag) > lastOpen {
		return false
	}

	firstClose := strings.Index(after, closeTag)
	if firstClose == -1 {
		return false
	}
	nextOpen := strings.Index(after, openTag)
	if nextOpen != -1 && nextOpen < firstClose {
		return false
	}
	return true
}

// WriteXMLEscaped writes s into dst while escaping XML delimiters (&, <, >) with 0 heap allocations.
// This prevents untrusted runtime context values from breaking out of <context>, <query>, or <instructions> tags.
func WriteXMLEscaped(dst *bytes.Buffer, s string) {
	start := 0
	n := len(s)
	for i := 0; i < n; {
		for i+8 <= n {
			w := uint64(s[i]) | uint64(s[i+1])<<8 | uint64(s[i+2])<<16 | uint64(s[i+3])<<24 |
				uint64(s[i+4])<<32 | uint64(s[i+5])<<40 | uint64(s[i+6])<<48 | uint64(s[i+7])<<56
			xAmp := w ^ 0x2626262626262626
			xLt := w ^ 0x3c3c3c3c3c3c3c3c
			xGt := w ^ 0x3e3e3e3e3e3e3e3e
			if (((xAmp-0x0101010101010101)&^xAmp |
				(xLt-0x0101010101010101)&^xLt |
				(xGt-0x0101010101010101)&^xGt) & 0x8080808080808080) == 0 {
				i += 8
				continue
			}
			break
		}
		if i >= n {
			break
		}
		switch s[i] {
		case '&':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString("&amp;")
			start = i + 1
		case '<':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString("&lt;")
			start = i + 1
		case '>':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString("&gt;")
			start = i + 1
		}
		i++
	}
	if start < n {
		dst.WriteString(s[start:])
	}
}

// RenderAnthropicCompiled writes the DIALECT_ANTHROPIC_XML representation of the pre-compiled AST
// directly into dst with 0 heap allocations.
func RenderAnthropicCompiled(
	dst *bytes.Buffer,
	systemBlock CompiledBlock,
	exemplars []CompiledBlock,
	messages []CompiledBlock,
	vars map[string]string,
	contextOverride string,
	hasContextOverride bool,
	strictMissing bool,
) (string, bool) {
	// 1. System Prompt / Active Persona wrapped in <instructions>...</instructions>
	if !systemBlock.HasInstructionsOuter {
		dst.WriteString("<instructions>")
	}
	if missingKey, failed := writeAnthropicSegments(dst, systemBlock.Segments, vars, contextOverride, hasContextOverride, strictMissing, false); failed {
		return missingKey, true
	}
	if !systemBlock.HasInstructionsOuter {
		dst.WriteString("</instructions>")
	}

	// 2. Optional Few-Shot Exemplars wrapped in <examples>...</examples>
	if len(exemplars) > 0 {
		dst.WriteString("\n<examples>\n")
		for i := 0; i < len(exemplars); i++ {
			ex := exemplars[i]
			dst.WriteString("<example role=\"")
			WriteXMLEscaped(dst, ex.Role)
			dst.WriteString("\">")
			if missingKey, failed := writeAnthropicSegments(dst, ex.Segments, vars, contextOverride, hasContextOverride, strictMissing, false); failed {
				return missingKey, true
			}
			dst.WriteString("</example>\n")
		}
		dst.WriteString("</examples>")
	}

	// 3. Ordered Message Blocks with idempotent <context> and <query> XML framing
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		dst.WriteByte('\n')
		if msg.Role == "system" && !msg.HasInstructionsOuter {
			dst.WriteString("<instructions>")
			if missingKey, failed := writeAnthropicSegments(dst, msg.Segments, vars, contextOverride, hasContextOverride, strictMissing, true); failed {
				return missingKey, true
			}
			dst.WriteString("</instructions>")
			continue
		}
		if missingKey, failed := writeAnthropicSegments(dst, msg.Segments, vars, contextOverride, hasContextOverride, strictMissing, true); failed {
			return missingKey, true
		}
	}

	return "", false
}

// writeAnthropicSegments renders compiled template segments into the destination buffer with Anthropic XML conventions.
func writeAnthropicSegments(
	dst *bytes.Buffer,
	segments []Segment,
	vars map[string]string,
	contextOverride string,
	hasContextOverride bool,
	strictMissing bool,
	enableAutoXMLWrap bool,
) (string, bool) {
	for i := 0; i < len(segments); i++ {
		seg := segments[i]
		if !seg.IsVar {
			dst.WriteString(seg.Text)
			continue
		}

		var val string
		var ok bool
		if seg.Text == "context" && hasContextOverride {
			val = contextOverride
			ok = true
		} else if vars != nil {
			val, ok = vars[seg.Text]
		}

		if !ok {
			if strictMissing {
				return seg.Text, true
			}
			if enableAutoXMLWrap && seg.AutoWrapXMLTag != "" {
				dst.WriteByte('<')
				dst.WriteString(seg.AutoWrapXMLTag)
				dst.WriteByte('>')
				dst.WriteString(seg.RawPlaceholder)
				dst.WriteString("</")
				dst.WriteString(seg.AutoWrapXMLTag)
				dst.WriteByte('>')
			} else {
				dst.WriteString(seg.RawPlaceholder)
			}
			continue
		}

		if enableAutoXMLWrap && seg.AutoWrapXMLTag != "" {
			dst.WriteByte('<')
			dst.WriteString(seg.AutoWrapXMLTag)
			dst.WriteByte('>')
			WriteXMLEscaped(dst, val)
			dst.WriteString("</")
			dst.WriteString(seg.AutoWrapXMLTag)
			dst.WriteByte('>')
		} else {
			WriteXMLEscaped(dst, val)
		}
	}
	return "", false
}
