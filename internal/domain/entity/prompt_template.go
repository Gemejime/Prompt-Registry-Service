// Package entity defines core domain entities and validation invariants for the control plane.
package entity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Domain sentinel errors.
var (
	ErrInvalidInput    = errors.New("invalid input")
	ErrNotFound        = errors.New("resource not found")
	ErrConflict        = errors.New("resource conflict")
	ErrVersionConflict = errors.New("tag pointer version conflict")
)

// DialectType represents the target model chat-markup dialect.
type DialectType string

const (
	DialectAnthropicXML   DialectType = "DIALECT_ANTHROPIC_XML"
	DialectLlamaSpecial   DialectType = "DIALECT_LLAMA_SPECIAL"
	DialectOpenAIStandard DialectType = "DIALECT_OPENAI_STANDARD"
)

// IsValid checks whether the dialect is supported.
func (d DialectType) IsValid() bool {
	switch d {
	case DialectAnthropicXML, DialectLlamaSpecial, DialectOpenAIStandard:
		return true
	default:
		return false
	}
}

var (
	uuidRegex     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	varKeyRegex   = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]*$`)
	templateNameR = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,254}$`)
)

// IsValidUUID reports whether s is a canonical 36-character UUID string.
func IsValidUUID(s string) bool {
	return uuidRegex.MatchString(strings.TrimSpace(s))
}

// ValidateResourceName validates a template or schema name.
func ValidateResourceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if !templateNameR.MatchString(name) {
		return fmt.Errorf("%w: invalid resource name %q", ErrInvalidInput, name)
	}
	return nil
}

// MessageBlock represents a structured role message in a prompt template AST.
type MessageBlock struct {
	Role           string `json:"role"`
	ContentPattern string `json:"content_pattern"`
}

// Validate checks role and content pattern invariants.
func (m MessageBlock) Validate() error {
	switch m.Role {
	case "system", "user", "assistant":
	default:
		return fmt.Errorf("%w: unsupported message role %q (must be system, user, or assistant)", ErrInvalidInput, m.Role)
	}
	if strings.TrimSpace(m.ContentPattern) == "" {
		return fmt.Errorf("%w: message content_pattern cannot be empty", ErrInvalidInput)
	}
	return nil
}

// TemplateAST represents the canonical structured prompt syntax tree.
type TemplateAST struct {
	SystemPrompt     string            `json:"system_prompt"`
	Messages         []MessageBlock    `json:"messages"`
	VariableKeys     []string          `json:"variable_keys"`
	DefaultDialect   DialectType       `json:"default_dialect"`
	PersonaOverrides map[string]string `json:"persona_overrides,omitempty"`
	FewShotExemplars []MessageBlock    `json:"few_shot_exemplars,omitempty"`
}

// Validate performs fail-fast structural, dialect, and balanced {{variable}} validation.
func (ast *TemplateAST) Validate() error {
	if ast == nil {
		return fmt.Errorf("%w: raw_ast is required", ErrInvalidInput)
	}
	if strings.TrimSpace(ast.SystemPrompt) == "" {
		return fmt.Errorf("%w: system_prompt cannot be empty", ErrInvalidInput)
	}
	if !ast.DefaultDialect.IsValid() {
		return fmt.Errorf("%w: unsupported default_dialect %q", ErrInvalidInput, ast.DefaultDialect)
	}
	if len(ast.Messages) == 0 {
		return fmt.Errorf("%w: messages must contain at least one message block", ErrInvalidInput)
	}

	declaredVars := make(map[string]struct{}, len(ast.VariableKeys))
	for _, k := range ast.VariableKeys {
		if !varKeyRegex.MatchString(k) {
			return fmt.Errorf("%w: invalid variable key %q", ErrInvalidInput, k)
		}
		declaredVars[k] = struct{}{}
	}

	referencedVars := make(map[string]struct{})
	if err := extractAndValidatePlaceholders(ast.SystemPrompt, declaredVars, referencedVars); err != nil {
		return err
	}

	for i, msg := range ast.Messages {
		if err := msg.Validate(); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
		if err := extractAndValidatePlaceholders(msg.ContentPattern, declaredVars, referencedVars); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}

	for persona, override := range ast.PersonaOverrides {
		if strings.TrimSpace(persona) == "" {
			return fmt.Errorf("%w: persona_overrides key cannot be empty", ErrInvalidInput)
		}
		if strings.TrimSpace(override) == "" {
			return fmt.Errorf("%w: persona_overrides[%q] cannot be empty", ErrInvalidInput, persona)
		}
		if err := extractAndValidatePlaceholders(override, declaredVars, referencedVars); err != nil {
			return fmt.Errorf("persona_overrides[%q]: %w", persona, err)
		}
	}

	for i, ex := range ast.FewShotExemplars {
		if err := ex.Validate(); err != nil {
			return fmt.Errorf("few_shot_exemplars[%d]: %w", i, err)
		}
		if err := extractAndValidatePlaceholders(ex.ContentPattern, declaredVars, referencedVars); err != nil {
			return fmt.Errorf("few_shot_exemplars[%d]: %w", i, err)
		}
	}

	return nil
}

// extractAndValidatePlaceholders checks that all {{...}} markers are balanced,
// syntactically valid variable names, and declared in declaredVars.
func extractAndValidatePlaceholders(text string, declaredVars, referencedVars map[string]struct{}) error {
	idx := 0
	for idx < len(text) {
		openIdx := strings.Index(text[idx:], "{{")
		closeIdx := strings.Index(text[idx:], "}}")

		if closeIdx != -1 && (openIdx == -1 || closeIdx < openIdx) {
			return fmt.Errorf("%w: unbalanced closing '}}' delimiter in template pattern", ErrInvalidInput)
		}
		if openIdx == -1 {
			break
		}

		absOpen := idx + openIdx
		afterOpen := absOpen + 2
		relClose := strings.Index(text[afterOpen:], "}}")
		if relClose == -1 {
			return fmt.Errorf("%w: unclosed '{{' delimiter in template pattern", ErrInvalidInput)
		}

		rawVar := text[afterOpen : afterOpen+relClose]
		if strings.Contains(rawVar, "{{") {
			return fmt.Errorf("%w: nested '{{' delimiter in template pattern", ErrInvalidInput)
		}
		varName := strings.TrimSpace(rawVar)
		if varName == "" || !varKeyRegex.MatchString(varName) {
			return fmt.Errorf("%w: invalid placeholder variable name %q", ErrInvalidInput, rawVar)
		}
		if _, ok := declaredVars[varName]; !ok {
			return fmt.Errorf("%w: placeholder {{%s}} is not declared in variable_keys", ErrInvalidInput, varName)
		}
		referencedVars[varName] = struct{}{}
		idx = afterOpen + relClose + 2
	}
	return nil
}

// canonicalVersionInput ensures deterministic JSON hashing across AST + schema_id.
type canonicalVersionInput struct {
	SystemPrompt     string            `json:"system_prompt"`
	Messages         []MessageBlock    `json:"messages"`
	VariableKeys     []string          `json:"variable_keys"`
	DefaultDialect   DialectType       `json:"default_dialect"`
	PersonaOverrides map[string]string `json:"persona_overrides,omitempty"`
	FewShotExemplars []MessageBlock    `json:"few_shot_exemplars,omitempty"`
	SchemaID         string            `json:"schema_id"`
}

// CanonicalHash computes a deterministic 64-character SHA-256 hex digest over the AST and schema_id.
func (ast *TemplateAST) CanonicalHash(schemaID string) (string, error) {
	if ast == nil {
		return "", fmt.Errorf("%w: nil AST", ErrInvalidInput)
	}
	sortedKeys := make([]string, len(ast.VariableKeys))
	copy(sortedKeys, ast.VariableKeys)
	sort.Strings(sortedKeys)

	canon := canonicalVersionInput{
		SystemPrompt:     ast.SystemPrompt,
		Messages:         ast.Messages,
		VariableKeys:     sortedKeys,
		DefaultDialect:   ast.DefaultDialect,
		PersonaOverrides: ast.PersonaOverrides,
		FewShotExemplars: ast.FewShotExemplars,
		SchemaID:         strings.ToLower(strings.TrimSpace(schemaID)),
	}
	b, err := json.Marshal(canon)
	if err != nil {
		return "", fmt.Errorf("failed to marshal canonical AST: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// TruncatedHash returns the 16-character SHA-256 prefix safe for structured operational logging (Invariant 2).
func (ast *TemplateAST) TruncatedHash(schemaID string) string {
	h, err := ast.CanonicalHash(schemaID)
	if err != nil || len(h) < 16 {
		return "0000000000000000"
	}
	return h[:16]
}

// TruncateHashPrefix safely truncates a full SHA-256 hex digest to 16 chars for logging.
func TruncateHashPrefix(fullHash string) string {
	if len(fullHash) >= 16 {
		return fullHash[:16]
	}
	return fullHash
}

// PromptTemplate is the control plane aggregate root for a prompt template lineage.
type PromptTemplate struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	CurrentVersion int       `json:"current_version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// PromptVersion represents an immutable append-only version (v_1 .. v_n) of a PromptTemplate.
type PromptVersion struct {
	ID         string      `json:"id"`
	TemplateID string      `json:"template_id"`
	Version    int         `json:"version"`
	RawAST     TemplateAST `json:"ast"`
	ASTHash    string      `json:"ast_hash"`
	SchemaID   string      `json:"schema_id"`
	Author     string      `json:"author"`
	CreatedAt  time.Time   `json:"created_at"`
}

// JSONSchema represents a registered output schema contract.
type JSONSchema struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id"`
	Name       string          `json:"name"`
	SchemaJSON json.RawMessage `json:"schema_json"`
	CreatedAt  time.Time       `json:"created_at"`
}

var allowedJSONSchemaTypes = map[string]struct{}{
	"object":  {},
	"array":   {},
	"string":  {},
	"number":  {},
	"integer": {},
	"boolean": {},
	"null":    {},
}

// ValidateJSONSchemaDraft validates that raw is a well-formed JSON Schema object (Draft-07 / 2020-12).
func ValidateJSONSchemaDraft(raw []byte) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, fmt.Errorf("%w: json_schema must be a non-empty JSON object", ErrInvalidInput)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil, fmt.Errorf("%w: json_schema is not a valid JSON object: %v", ErrInvalidInput, err)
	}
	if len(obj) == 0 {
		return nil, fmt.Errorf("%w: json_schema cannot be an empty object", ErrInvalidInput)
	}
	if tVal, hasType := obj["type"]; hasType {
		tStr, ok := tVal.(string)
		if !ok {
			return nil, fmt.Errorf("%w: json_schema 'type' field must be a string", ErrInvalidInput)
		}
		if _, valid := allowedJSONSchemaTypes[tStr]; !valid {
			return nil, fmt.Errorf("%w: unsupported json_schema type %q", ErrInvalidInput, tStr)
		}
	}
	if props, hasProps := obj["properties"]; hasProps {
		if _, ok := props.(map[string]any); !ok {
			return nil, fmt.Errorf("%w: json_schema 'properties' must be an object", ErrInvalidInput)
		}
	}
	canonicalBytes, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to normalize json_schema: %v", ErrInvalidInput, err)
	}
	return json.RawMessage(canonicalBytes), nil
}
