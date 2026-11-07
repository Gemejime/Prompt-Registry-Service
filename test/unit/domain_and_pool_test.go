package unit_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/infrastructure/persistence/migrations"
	"rag-prompt-registry-service/internal/infrastructure/persistence/postgres"
)

func TestTemplateASTValidationAndCanonicalHash(t *testing.T) {
	validAST := entity.TemplateAST{
		SystemPrompt: "You are a principal financial RAG synthesis assistant.",
		Messages: []entity.MessageBlock{
			{
				Role:           "user",
				ContentPattern: "Context:\n<context>{{context}}</context>\n\nQuery: {{query}}",
			},
		},
		VariableKeys:   []string{"context", "query"},
		DefaultDialect: entity.DialectAnthropicXML,
		PersonaOverrides: map[string]string{
			"executive": "Provide a concise board-level summary for {{query}}.",
		},
		FewShotExemplars: []entity.MessageBlock{
			{
				Role:           "assistant",
				ContentPattern: "Summary based on {{context}}.",
			},
		},
	}

	if err := validAST.Validate(); err != nil {
		t.Fatalf("expected valid AST to pass validation, got: %v", err)
	}

	schemaID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	hash1, err := validAST.CanonicalHash(schemaID)
	if err != nil {
		t.Fatalf("unexpected CanonicalHash error: %v", err)
	}
	if len(hash1) != 64 {
		t.Fatalf("expected 64-char SHA-256 hex digest, got len=%d (%s)", len(hash1), hash1)
	}

	// Reordering VariableKeys must produce the exact same canonical hash.
	reorderedAST := validAST
	reorderedAST.VariableKeys = []string{"query", "context"}
	hash2, err := reorderedAST.CanonicalHash(schemaID)
	if err != nil {
		t.Fatalf("unexpected CanonicalHash error: %v", err)
	}
	if hash1 != hash2 {
		t.Fatalf("expected canonical hash to be order-independent for variable_keys: %s != %s", hash1, hash2)
	}

	trunc := validAST.TruncatedHash(schemaID)
	if len(trunc) != 16 || trunc != hash1[:16] {
		t.Fatalf("expected 16-char truncated hash prefix %q, got %q", hash1[:16], trunc)
	}

	// Unbalanced placeholder delimiter test
	unbalancedAST := validAST
	unbalancedAST.Messages = []entity.MessageBlock{
		{Role: "user", ContentPattern: "Broken {{context"},
	}
	if err := unbalancedAST.Validate(); !errors.Is(err, entity.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unclosed '{{', got: %v", err)
	}

	// Undeclared placeholder variable test
	undeclaredAST := validAST
	undeclaredAST.Messages = []entity.MessageBlock{
		{Role: "user", ContentPattern: "Using {{undeclared_var}}"},
	}
	if err := undeclaredAST.Validate(); !errors.Is(err, entity.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for undeclared variable, got: %v", err)
	}

	// Invalid dialect test
	badDialectAST := validAST
	badDialectAST.DefaultDialect = "DIALECT_UNKNOWN"
	if err := badDialectAST.Validate(); !errors.Is(err, entity.ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for unsupported dialect, got: %v", err)
	}
}

func TestJSONSchemaDraftValidation(t *testing.T) {
	rawValid := []byte(`{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"required": ["summary", "citations"],
		"properties": {
			"summary": {"type": "string"},
			"citations": {"type": "array"}
		}
	}`)
	normalized, err := entity.ValidateJSONSchemaDraft(rawValid)
	if err != nil {
		t.Fatalf("expected valid JSON schema draft to pass, got: %v", err)
	}
	if len(normalized) == 0 {
		t.Fatalf("expected non-empty normalized JSON schema")
	}

	invalidCases := [][]byte{
		[]byte(``),
		[]byte(`{}`),
		[]byte(`not-json`),
		[]byte(`{"type": "invalid_type"}`),
		[]byte(`{"type": "object", "properties": "not-an-object"}`),
	}
	for i, tc := range invalidCases {
		if _, err := entity.ValidateJSONSchemaDraft(tc); !errors.Is(err, entity.ErrInvalidInput) {
			t.Fatalf("case %d: expected ErrInvalidInput for %q, got %v", i, string(tc), err)
		}
	}
}

func TestTagValidationAndOptimisticLocking(t *testing.T) {
	for _, validTag := range []string{"@production", "@staging", "@canary", "@release-2026.11"} {
		if err := entity.ValidateTagName(validTag); err != nil {
			t.Fatalf("expected tag %q to be valid, got: %v", validTag, err)
		}
	}
	for _, invalidTag := range []string{"production", "@", "", "@bad space"} {
		if err := entity.ValidateTagName(invalidTag); !errors.Is(err, entity.ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for tag %q, got: %v", invalidTag, err)
		}
	}

	// First assignment: current=0, expected=0, target=1 -> not a rollback
	isRollback, err := entity.ValidateTagPromotion(0, 0, 1)
	if err != nil || isRollback {
		t.Fatalf("expected initial tag promotion to succeed with isRollback=false, got isRollback=%v, err=%v", isRollback, err)
	}

	// Forward promotion: current=1, expected=1, target=2 -> not a rollback
	isRollback, err = entity.ValidateTagPromotion(1, 1, 2)
	if err != nil || isRollback {
		t.Fatalf("expected forward tag promotion to succeed with isRollback=false, got isRollback=%v, err=%v", isRollback, err)
	}

	// Rollback promotion: current=2, expected=2, target=1 -> isRollback=true
	isRollback, err = entity.ValidateTagPromotion(2, 2, 1)
	if err != nil || !isRollback {
		t.Fatalf("expected rollback promotion to succeed with isRollback=true, got isRollback=%v, err=%v", isRollback, err)
	}

	// Optimistic lock mismatch: current=2, expected=1 -> ErrVersionConflict
	_, err = entity.ValidateTagPromotion(2, 1, 3)
	if !errors.Is(err, entity.ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict on expected_version mismatch, got: %v", err)
	}
}

func TestPgBouncerSimpleProtocolPoolConfig(t *testing.T) {
	dsn := "postgres://registry_user:super_secret_pw_123@localhost:6432/prompt_registry?sslmode=disable"
	cfg, err := postgres.BuildPoolConfig(postgres.PoolOptions{
		DSN:      dsn,
		MaxConns: 20,
		MinConns: 4,
	})
	if err != nil {
		t.Fatalf("BuildPoolConfig failed: %v", err)
	}

	// ADR-003 / Invariant 6: Verify ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeSimpleProtocol
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Fatalf("expected DefaultQueryExecMode=%v (QueryExecModeSimpleProtocol), got %v",
			pgx.QueryExecModeSimpleProtocol, cfg.ConnConfig.DefaultQueryExecMode)
	}
	if cfg.MaxConns != 20 || cfg.MinConns != 4 {
		t.Fatalf("unexpected pool bounds: MaxConns=%d, MinConns=%d", cfg.MaxConns, cfg.MinConns)
	}

	// Verify DSN credential sanitization (Invariant 2)
	sanitized := postgres.SanitizeDSNError(errors.New("dial error on "+dsn), dsn)
	if strings.Contains(sanitized, "super_secret_pw_123") {
		t.Fatalf("SanitizeDSNError leaked password in error string: %s", sanitized)
	}
}

func TestEmbeddedSQLMigrations(t *testing.T) {
	upSQL, err := migrations.InitSchemaUp()
	if err != nil {
		t.Fatalf("InitSchemaUp failed: %v", err)
	}
	requiredMarkers := []string{
		"CREATE TABLE IF NOT EXISTS prompt_templates",
		"CREATE TABLE IF NOT EXISTS json_schemas",
		"CREATE TABLE IF NOT EXISTS prompt_versions",
		"ast_hash VARCHAR(64) NOT NULL",
		"CREATE TABLE IF NOT EXISTS prompt_tags",
		"fk_prompt_tags_version",
		"CREATE TABLE IF NOT EXISTS prompt_audit_ledger",
		"prevent_append_only_mutation()",
		"trg_prompt_versions_immutable",
		"trg_prompt_audit_ledger_immutable",
	}
	for _, marker := range requiredMarkers {
		if !strings.Contains(upSQL, marker) {
			t.Fatalf("expected 000001_init_schema.up.sql to contain %q", marker)
		}
	}

	downSQL, err := migrations.InitSchemaDown()
	if err != nil {
		t.Fatalf("InitSchemaDown failed: %v", err)
	}
	if !strings.Contains(downSQL, "DROP TABLE IF EXISTS prompt_audit_ledger") {
		t.Fatalf("expected 000001_init_schema.down.sql to drop prompt_audit_ledger")
	}
}
