package dialects

import (
	"bytes"
	"encoding/json"
	"strings"
)

const hexDigits = "0123456789abcdef"

// WriteJSONEscapedString writes s into dst with RFC 8259 JSON string escaping (without outer quotes)
// performing 0 heap allocations.
func WriteJSONEscapedString(dst *bytes.Buffer, s string) {
	start := 0
	n := len(s)
	for i := 0; i < n; i++ {
		b := s[i]
		switch b {
		case '"':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString(`\"`)
			start = i + 1
		case '\\':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString(`\\`)
			start = i + 1
		case '\b':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString(`\b`)
			start = i + 1
		case '\f':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString(`\f`)
			start = i + 1
		case '\n':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString(`\n`)
			start = i + 1
		case '\r':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString(`\r`)
			start = i + 1
		case '\t':
			if i > start {
				dst.WriteString(s[start:i])
			}
			dst.WriteString(`\t`)
			start = i + 1
		default:
			if b < 0x20 {
				if i > start {
					dst.WriteString(s[start:i])
				}
				var esc [6]byte
				esc[0] = '\\'
				esc[1] = 'u'
				esc[2] = '0'
				esc[3] = '0'
				esc[4] = hexDigits[b>>4]
				esc[5] = hexDigits[b&0x0F]
				dst.Write(esc[:])
				start = i + 1
			}
		}
	}
	if start < n {
		dst.WriteString(s[start:])
	}
}

// BuildResponseFormatTail pre-serializes the OpenAI/Mistral JSON schema response_format tail
// (starting with `,"response_format":...`) at template compilation time so hot-path rendering
// requires 0 encoding/json allocations.
func BuildResponseFormatTail(schemaID, schemaName string, schemaJSON []byte) string {
	trimmedSchema := bytes.TrimSpace(schemaJSON)
	if len(trimmedSchema) == 0 && strings.TrimSpace(schemaID) == "" && strings.TrimSpace(schemaName) == "" {
		return ""
	}

	name := strings.TrimSpace(schemaName)
	if name == "" {
		name = strings.TrimSpace(schemaID)
	}
	if name == "" {
		name = "output_schema"
	}

	var compactBuf bytes.Buffer
	if len(trimmedSchema) > 0 && json.Valid(trimmedSchema) {
		if err := json.Compact(&compactBuf, trimmedSchema); err != nil {
			compactBuf.Reset()
			compactBuf.Write(trimmedSchema)
		}
	} else {
		compactBuf.WriteString(`{"type":"object"}`)
	}

	var out bytes.Buffer
	out.WriteString(`,"response_format":{"type":"json_schema","json_schema":{"name":"`)
	WriteJSONEscapedString(&out, name)
	out.WriteString(`","strict":true,"schema":`)
	out.Write(compactBuf.Bytes())
	out.WriteString(`}}`)
	return out.String()
}

// RenderStandardJSONCompiled writes the DIALECT_OPENAI_STANDARD JSON payload
// (`{"messages":[...],"response_format":{...}}`) directly into dst with 0 heap allocations.
func RenderStandardJSONCompiled(
	dst *bytes.Buffer,
	systemBlock CompiledBlock,
	exemplars []CompiledBlock,
	messages []CompiledBlock,
	responseFormatTail string,
	vars map[string]string,
	contextOverride string,
	hasContextOverride bool,
	strictMissing bool,
) (string, bool) {
	dst.WriteString(`{"messages":[{"role":"system","content":"`)
	if missingKey, failed := writeJSONSegments(dst, systemBlock.Segments, vars, contextOverride, hasContextOverride, strictMissing); failed {
		return missingKey, true
	}
	dst.WriteString(`"}`)

	for i := 0; i < len(exemplars); i++ {
		ex := exemplars[i]
		dst.WriteString(`,{"role":"`)
		WriteJSONEscapedString(dst, ex.Role)
		dst.WriteString(`","content":"`)
		if missingKey, failed := writeJSONSegments(dst, ex.Segments, vars, contextOverride, hasContextOverride, strictMissing); failed {
			return missingKey, true
		}
		dst.WriteString(`"}`)
	}

	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		dst.WriteString(`,{"role":"`)
		WriteJSONEscapedString(dst, msg.Role)
		dst.WriteString(`","content":"`)
		if missingKey, failed := writeJSONSegments(dst, msg.Segments, vars, contextOverride, hasContextOverride, strictMissing); failed {
			return missingKey, true
		}
		dst.WriteString(`"}`)
	}

	dst.WriteByte(']')
	if responseFormatTail != "" {
		dst.WriteString(responseFormatTail)
	}
	dst.WriteByte('}')
	return "", false
}

func writeJSONSegments(
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
			WriteJSONEscapedString(dst, seg.Text)
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
			WriteJSONEscapedString(dst, seg.RawPlaceholder)
			continue
		}

		WriteJSONEscapedString(dst, val)
	}
	return "", false
}
