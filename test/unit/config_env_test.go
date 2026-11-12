package unit_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/config"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
	"rag-prompt-registry-service/internal/infrastructure/persistence/postgres"
	httptransport "rag-prompt-registry-service/internal/transport/http"
)

// TestLoadDotEnvParsingAndPrecedence verifies .env parsing, 12-factor OS env precedence,
// ENV_FILE overrides, and secured error handling.
func TestLoadDotEnvParsingAndPrecedence(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	content := "\uFEFF# Comment line\r\n" +
		"\r\n" +
		"HTTP_PORT=8181 # inline comment\r\n" +
		"export GRPC_PORT=9191\r\n" +
		"SINGLE_Q='raw\\nvalue' # trailing comment\r\n" +
		"DOUBLE_Q=\"line1\\n\\\"line2\\\"\" # comment\r\n" +
		"EMPTY_VAL=\r\n" +
		"PRESET_KEY=from_dotenv\r\n"

	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write temp .env: %v", err)
	}

	// Ensure keys are isolated per test and restored afterwards.
	for _, k := range []string{"HTTP_PORT", "GRPC_PORT", "SINGLE_Q", "DOUBLE_Q", "EMPTY_VAL"} {
		k := k
		if prev, had := os.LookupEnv(k); had {
			t.Cleanup(func() { _ = os.Setenv(k, prev) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(k)
	}
	t.Setenv("PRESET_KEY", "from_process_env")
	t.Setenv("ENV_FILE", envPath)

	if err := config.LoadDotEnv(); err != nil {
		t.Fatalf("LoadDotEnv failed: %v", err)
	}

	if got := os.Getenv("HTTP_PORT"); got != "8181" {
		t.Fatalf("expected HTTP_PORT=8181, got %q", got)
	}
	if got := os.Getenv("GRPC_PORT"); got != "9191" {
		t.Fatalf("expected GRPC_PORT=9191, got %q", got)
	}
	if got := os.Getenv("SINGLE_Q"); got != `raw\nvalue` {
		t.Fatalf("expected literal single-quoted value, got %q", got)
	}
	if got := os.Getenv("DOUBLE_Q"); got != "line1\n\"line2\"" {
		t.Fatalf("expected unescaped double-quoted value, got %q", got)
	}
	if val, ok := os.LookupEnv("EMPTY_VAL"); !ok || val != "" {
		t.Fatalf("expected EMPTY_VAL to be set to empty string, got ok=%v val=%q", ok, val)
	}
	if got := os.Getenv("PRESET_KEY"); got != "from_process_env" {
		t.Fatalf("expected process env to take precedence over .env, got %q", got)
	}
}

// TestLoadDotEnvMissingAndMalformed verifies missing-file handling and syntax error reporting.
func TestLoadDotEnvMissingAndMalformed(t *testing.T) {
	t.Setenv("ENV_FILE", "")
	missingDefault := filepath.Join(t.TempDir(), "does-not-exist.env")
	if err := config.LoadDotEnv(missingDefault); err != nil {
		t.Fatalf("expected missing default .env path to be ignored, got: %v", err)
	}

	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "explicit-missing.env"))
	if err := config.LoadDotEnv(); err == nil {
		t.Fatalf("expected error when explicit ENV_FILE does not exist")
	}

	badPath := filepath.Join(t.TempDir(), "bad.env")
	secretMarker := "SUPER_SECRET_SHOULD_NOT_APPEAR"
	if err := os.WriteFile(badPath, []byte("BAD_LINE_WITHOUT_EQUALS_"+secretMarker+"\n"), 0o600); err != nil {
		t.Fatalf("failed to write bad.env: %v", err)
	}
	t.Setenv("ENV_FILE", badPath)
	err := config.LoadDotEnv()
	if err == nil {
		t.Fatalf("expected syntax error for malformed .env")
	}
	if strings.Contains(err.Error(), secretMarker) {
		t.Fatalf("malformed .env error leaked line contents: %v", err)
	}
}

// TestParseLogLevelAndEnvHelpers verifies LOG_LEVEL parsing and typed environment helpers.
func TestParseLogLevelAndEnvHelpers(t *testing.T) {
	cases := []struct {
		raw    string
		want   slog.Level
		wantOK bool
	}{
		{"", slog.LevelInfo, true},
		{"INFO", slog.LevelInfo, true},
		{"debug", slog.LevelDebug, true},
		{"WARN", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"Error", slog.LevelError, true},
		{"TRACE", slog.LevelInfo, false},
	}
	for _, tc := range cases {
		got, ok := config.ParseLogLevel(tc.raw)
		if got != tc.want || ok != tc.wantOK {
			t.Fatalf("ParseLogLevel(%q) = (%v, %v), want (%v, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}

	t.Setenv("TEST_INT_ENV", "42")
	if got := config.GetEnvInt("TEST_INT_ENV", 10); got != 42 {
		t.Fatalf("GetEnvInt = %d, want 42", got)
	}
	t.Setenv("TEST_BOOL_ENV", "true")
	if got := config.GetEnvBool("TEST_BOOL_ENV", false); !got {
		t.Fatalf("GetEnvBool = %v, want true", got)
	}
	csv := config.ParseCSV(" redis-1:6379 , , redis-2:6379 ")
	if len(csv) != 2 || csv[0] != "redis-1:6379" || csv[1] != "redis-2:6379" {
		t.Fatalf("unexpected ParseCSV output: %#v", csv)
	}
}

// TestPostgresConnExecModeConfig verifies CONN_EXEC_MODE wiring in BuildPoolConfig.
func TestPostgresConnExecModeConfig(t *testing.T) {
	dsn := "postgres://postgres:postgres@localhost:6432/prompt_registry?sslmode=disable"

	cfgDefault, err := postgres.BuildPoolConfig(postgres.PoolOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("BuildPoolConfig default failed: %v", err)
	}
	if cfgDefault.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Fatalf("expected default QueryExecModeSimpleProtocol, got %v", cfgDefault.ConnConfig.DefaultQueryExecMode)
	}

	cfgExec, err := postgres.BuildPoolConfig(postgres.PoolOptions{
		DSN:          dsn,
		ConnExecMode: "exec",
	})
	if err != nil {
		t.Fatalf("BuildPoolConfig exec failed: %v", err)
	}
	if cfgExec.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Fatalf("expected QueryExecModeExec, got %v", cfgExec.ConnConfig.DefaultQueryExecMode)
	}

	if _, err := postgres.BuildPoolConfig(postgres.PoolOptions{
		DSN:          dsn,
		ConnExecMode: "invalid_mode",
	}); err == nil {
		t.Fatalf("expected error for invalid CONN_EXEC_MODE")
	}
}

// TestDotEnvExampleContainsAllImportantVariables verifies that .env.example defines
// all important environment variables used across the control plane and SDK.
func TestDotEnvExampleContainsAllImportantVariables(t *testing.T) {
	raw, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("failed to read .env.example: %v", err)
	}
	content := string(raw)

	requiredVars := []string{
		"HTTP_PORT=",
		"GRPC_PORT=",
		"OPENAPI_SPEC_PATH=",
		"POSTGRES_DSN=",
		"CONN_EXEC_MODE=",
		"ALLOW_IN_MEMORY_STORE=",
		"POSTGRES_MAX_CONNS=",
		"POSTGRES_MIN_CONNS=",
		"POSTGRES_AUTO_MIGRATE=",
		"REDIS_CLUSTER_ADDRS=",
		"REDIS_ADDRS=",
		"REDIS_PASSWORD=",
		"REDIS_DB=",
		"REDIS_INVALIDATION_CHANNEL=",
		"JWT_HMAC_SECRET=",
		"JWT_ISSUER=",
		"ADMIN_WRITE_RATE_LIMIT_RPS=",
		"ADMIN_WRITE_RATE_LIMIT_BURST=",
		"DEFAULT_TENANT_ID=",
		"LOG_LEVEL=",
	}
	for _, key := range requiredVars {
		if !strings.Contains(content, key) {
			t.Fatalf(".env.example is missing required variable %q", strings.TrimSuffix(key, "="))
		}
	}
}

// TestDotEnvChangeAffectsRuntimeJWTAndConfig verifies that loading a .env file
// immediately affects control plane JWT authentication and configuration behavior.
func TestDotEnvChangeAffectsRuntimeJWTAndConfig(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	customSecret := "custom-dotenv-secret-key-2026"
	customIssuer := "custom-dotenv-issuer"

	envBody := strings.Join([]string{
		"JWT_HMAC_SECRET=" + customSecret,
		"JWT_ISSUER=" + customIssuer,
		"LOG_LEVEL=DEBUG",
	}, "\n") + "\n"
	if err := os.WriteFile(envPath, []byte(envBody), 0o600); err != nil {
		t.Fatalf("failed to write .env: %v", err)
	}

	for _, k := range []string{"JWT_HMAC_SECRET", "JWT_ISSUER", "LOG_LEVEL"} {
		k := k
		if prev, had := os.LookupEnv(k); had {
			t.Cleanup(func() { _ = os.Setenv(k, prev) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(k)
	}
	t.Setenv("ENV_FILE", envPath)

	if err := config.LoadDotEnv(); err != nil {
		t.Fatalf("LoadDotEnv failed: %v", err)
	}

	level, ok := config.ParseLogLevel(config.GetEnv("LOG_LEVEL", "INFO"))
	if !ok || level != slog.LevelDebug {
		t.Fatalf("expected LOG_LEVEL=DEBUG from .env, got level=%v ok=%v", level, ok)
	}

	store := postgres.NewTransactionalMemoryStore()
	l2Cache := redis.NewInMemoryL2Cache()
	invalidator := redis.NewInvalidationBroadcaster(l2Cache, redis.DefaultInvalidationChannel)
	metrics := command.NewMutationMetricsRecorder()
	router := httptransport.NewRouter(httptransport.RouterConfig{
		PublishHandler: command.NewPublishVersionHandler(store, l2Cache, nil, metrics),
		PromoteHandler: command.NewPromoteTagHandler(store, l2Cache, invalidator, nil, metrics),
		GetASTHandler:  query.NewGetTemplateASTHandler(store, l2Cache),
		ListHandler:    query.NewListTemplatesHandler(store),
		MutationMetric: metrics,
		Invalidator:    invalidator,
		DefaultTenant:  "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
		OpenAPIPath:    "../../openapi/openapi.yaml",
		WriteRateRPS:   100,
		WriteRateBurst: 100,
	})

	tenantID := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	defaultTok, err := httptransport.IssueHS256JWT(tenantID, httptransport.DefaultJWTIssuer, httptransport.DefaultJWTSecret, time.Hour)
	if err != nil {
		t.Fatalf("IssueHS256JWT default failed: %v", err)
	}
	reqOld := httptest.NewRequest(http.MethodGet, "/v1/templates", nil)
	reqOld.Header.Set("Authorization", "Bearer "+defaultTok)
	recOld := httptest.NewRecorder()
	router.ServeHTTP(recOld, reqOld)
	if recOld.Code != http.StatusUnauthorized {
		t.Fatalf("expected token signed with default secret to be rejected (401) when .env overrides JWT_HMAC_SECRET, got %d", recOld.Code)
	}

	envTok, err := httptransport.IssueHS256JWT(tenantID, customIssuer, customSecret, time.Hour)
	if err != nil {
		t.Fatalf("IssueHS256JWT custom failed: %v", err)
	}
	reqNew := httptest.NewRequest(http.MethodGet, "/v1/templates", nil)
	reqNew.Header.Set("Authorization", "Bearer "+envTok)
	recNew := httptest.NewRecorder()
	router.ServeHTTP(recNew, reqNew)
	if recNew.Code != http.StatusOK {
		t.Fatalf("expected token signed with .env secret/issuer to succeed (200), got %d: %s", recNew.Code, recNew.Body.String())
	}
}

// TestHelmDynamicConfigMapAndEnvExampleNotice verifies dynamic ConfigMap rendering rules,
// checksum/config pod restart annotations, and .env.example deployment notice.
func TestHelmDynamicConfigMapAndEnvExampleNotice(t *testing.T) {
	cmRaw, err := os.ReadFile("../../deployments/helm/rag-prompt-registry/templates/configmap.yaml")
	if err != nil {
		t.Fatalf("failed to read configmap.yaml: %v", err)
	}
	cm := string(cmRaw)
	for _, marker := range []string{
		"range $key, $val := $env",
		"HTTP_PORT:",
		"GRPC_PORT:",
		`ne $key "secretName"`,
		`ne $val nil`,
	} {
		if !strings.Contains(cm, marker) {
			t.Fatalf("configmap.yaml missing expected dynamic template marker %q", marker)
		}
	}

	for _, tmpl := range []string{
		"../../deployments/helm/rag-prompt-registry/templates/deployment.yaml",
		"../../deployments/helm/rag-prompt-registry/templates/rollout.yaml",
	} {
		raw, err := os.ReadFile(tmpl)
		if err != nil {
			t.Fatalf("failed to read %s: %v", tmpl, err)
		}
		if !strings.Contains(string(raw), `checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}`) {
			t.Fatalf("%s missing checksum/config annotation", tmpl)
		}
	}

	exRaw, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("failed to read .env.example: %v", err)
	}
	if !strings.Contains(string(exRaw), ".env.prod") || !strings.Contains(string(exRaw), "deployments/") {
		t.Fatalf(".env.example header must clarify .env is local-only and .env.prod governs deployments/")
	}
}
