package dialects

import (
	"bytes"
)

// Meta Llama-3 canonical special header and turn termination tokens.
const (
	LlamaBeginOfText   = "<|begin_of_text|>"
	LlamaStartHeaderID = "<|start_header_id|>"
	LlamaEndHeaderID   = "<|end_header_id|>"
	LlamaEotID         = "<|eot_id|>"
	LlamaEndOfText     = "<|end_of_text|>"
	LlamaEomID         = "<|eom_id|>"
	LlamaPythonTag     = "<|python_tag|>"
)

// matchReservedLlamaToken checks whether s[idx:] starts with a reserved Meta Llama-3 control token
// and returns the token's byte length (or 0 if no control token starts at idx).
func matchReservedLlamaToken(s string, idx int) int {
	rem := len(s) - idx
	if rem < 10 || s[idx] != '<' || s[idx+1] != '|' {
		return 0
	}
	switch {
	case rem >= 10 && s[idx:idx+10] == LlamaEotID:
		return 10
	case rem >= 10 && s[idx:idx+10] == LlamaEomID:
		return 10
	case rem >= 14 && s[idx:idx+14] == LlamaPythonTag:
		return 14
	case rem >= 15 && s[idx:idx+15] == LlamaEndOfText:
		return 15
	case rem >= 16 && s[idx:idx+16] == LlamaEndHeaderID:
		return 16
	case rem >= 17 && s[idx:idx+17] == LlamaBeginOfText:
		return 17
	case rem >= 19 && s[idx:idx+19] == LlamaStartHeaderID:
		return 19
	default:
		return 0
	}
}

// WriteLlamaSanitized writes s into dst while neutralizing reserved Meta Llama-3 control tokens
// (<|begin_of_text|>, <|start_header_id|>, <|end_header_id|>, <|eot_id|>, <|end_of_text|>, <|eom_id|>, <|python_tag|>)
// with 0 heap allocations, preventing prompt-injection role-spoofing without corrupting XML/HTML characters.
func WriteLlamaSanitized(dst *bytes.Buffer, s string) {
	start := 0
	n := len(s)
	i := 0
	for i < n {
		if s[i] == '<' {
			if tokLen := matchReservedLlamaToken(s, i); tokLen > 0 {
				if i > start {
					dst.WriteString(s[start:i])
				}
				// Neutralize "<|token|>" as "< |token| >" so the control delimiter sequence is disarmed.
				dst.WriteString("< |")
				dst.WriteString(s[i+2 : i+tokLen-2])
				dst.WriteString("| >")
				i += tokLen
				start = i
				continue
			}
		}
		i++
	}
	if start < n {
		dst.WriteString(s[start:])
	}
}

// RenderLlamaCompiled writes the DIALECT_LLAMA_SPECIAL representation of the pre-compiled AST
// directly into dst with 0 heap allocations.
func RenderLlamaCompiled(
	dst *bytes.Buffer,
	systemBlock CompiledBlock,
	exemplars []CompiledBlock,
	messages []CompiledBlock,
	vars map[string]string,
	contextOverride string,
	hasContextOverride bool,
	strictMissing bool,
) (string, bool) {
	// 1. <|begin_of_text|><|start_header_id|>system<|end_header_id|>\n\n{system_prompt}<|eot_id|>
	dst.WriteString("<|begin_of_text|><|start_header_id|>system<|end_header_id|>\n\n")
	if missingKey, failed := writeLlamaSegments(dst, systemBlock.Segments, vars, contextOverride, hasContextOverride, strictMissing); failed {
		return missingKey, true
	}
	dst.WriteString("<|eot_id|>")

	// 2. Optional Few-Shot Exemplars
	for i := 0; i < len(exemplars); i++ {
		ex := exemplars[i]
		dst.WriteString("<|start_header_id|>")
		WriteLlamaSanitized(dst, ex.Role)
		dst.WriteString("<|end_header_id|>\n\n")
		if missingKey, failed := writeLlamaSegments(dst, ex.Segments, vars, contextOverride, hasContextOverride, strictMissing); failed {
			return missingKey, true
		}
		dst.WriteString("<|eot_id|>")
	}

	// 3. Ordered Message Blocks
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		dst.WriteString("<|start_header_id|>")
		WriteLlamaSanitized(dst, msg.Role)
		dst.WriteString("<|end_header_id|>\n\n")
		if missingKey, failed := writeLlamaSegments(dst, msg.Segments, vars, contextOverride, hasContextOverride, strictMissing); failed {
			return missingKey, true
		}
		dst.WriteString("<|eot_id|>")
	}

	// 4. Trailing assistant generation header
	dst.WriteString("<|start_header_id|>assistant<|end_header_id|>\n\n")
	return "", false
}

func writeLlamaSegments(
	dst *bytes.Buffer,
	segments []Segment,
	vars map[string]string,
	contextOverride string,
	hasContextOverride bool,
	strictMissing bool,
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
			dst.WriteString(seg.RawPlaceholder)
			continue
		}

		WriteLlamaSanitized(dst, val)
	}
	return "", false
}
