// Package config provides 12-factor environment variable and .env file loading utilities.
package config

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// envPair represents a parsed key-value pair from a .env file.
type envPair struct {
	key   string
	value string
}

// LoadDotEnv loads environment variables from a .env file into the current process environment.
// If ENV_FILE is set in the environment, that exact file path is loaded and any missing or
// unreadable file returns an error. Otherwise, the first existing path in paths (defaulting to
// ".env") is loaded; if none exist, LoadDotEnv returns nil. Variables already present in the
// process environment are preserved and never overwritten (12-factor precedence).
func LoadDotEnv(paths ...string) error {
	if override := strings.TrimSpace(os.Getenv("ENV_FILE")); override != "" {
		return loadEnvFile(override)
	}
	if len(paths) == 0 {
		paths = []string{".env"}
	}
	for _, p := range paths {
		clean := filepath.Clean(strings.TrimSpace(p))
		if clean == "" || clean == "." {
			continue
		}
		err := loadEnvFile(clean)
		if err == nil {
			return nil
		}
		if os.IsNotExist(err) {
			continue
		}
		return err
	}
	return nil
}

// loadEnvFile opens, parses, and applies environment entries from filePath without
// overwriting already-defined process environment variables.
func loadEnvFile(filePath string) error {
	clean := filepath.Clean(filePath)
	f, err := os.Open(clean) // #nosec G304 -- explicitly loads operator-configured .env / ENV_FILE path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	pairs, err := parseDotEnv(f)
	if err != nil {
		return fmt.Errorf("failed to parse %s: %w", clean, err)
	}
	for _, kv := range pairs {
		if _, exists := os.LookupEnv(kv.key); !exists {
			if setErr := os.Setenv(kv.key, kv.value); setErr != nil {
				return fmt.Errorf("failed to set environment variable %s: %w", kv.key, setErr)
			}
		}
	}
	return nil
}

// parseDotEnv parses .env formatted lines from r into ordered key-value pairs.
// Error messages intentionally omit parsed values to uphold Secured Logging redaction invariants.
func parseDotEnv(r io.Reader) ([]envPair, error) {
	scanner := bufio.NewScanner(r)
	var pairs []envPair
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if lineNum == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "export ") || strings.HasPrefix(trimmed, "export\t") {
			trimmed = strings.TrimSpace(trimmed[len("export"):])
		}

		eqIdx := strings.IndexByte(trimmed, '=')
		if eqIdx <= 0 {
			return nil, fmt.Errorf("invalid syntax at line %d", lineNum)
		}

		key := strings.TrimSpace(trimmed[:eqIdx])
		if key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("invalid variable name at line %d", lineNum)
		}

		rawVal := strings.TrimLeft(trimmed[eqIdx+1:], " \t")
		val, err := parseEnvValue(rawVal, lineNum)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, envPair{key: key, value: val})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read error: %w", err)
	}
	return pairs, nil
}

// parseEnvValue extracts a single-quoted, double-quoted, or unquoted .env value.
func parseEnvValue(raw string, lineNum int) (string, error) {
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", nil
	}

	if raw[0] == '\'' {
		endIdx := strings.IndexByte(raw[1:], '\'')
		if endIdx < 0 {
			return "", fmt.Errorf("unterminated single-quoted value at line %d", lineNum)
		}
		rest := strings.TrimSpace(raw[endIdx+2:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("unexpected trailing characters at line %d", lineNum)
		}
		return raw[1 : endIdx+1], nil
	}

	if raw[0] == '"' {
		var sb strings.Builder
		escaped := false
		closedAt := -1
		for i := 1; i < len(raw); i++ {
			ch := raw[i]
			if escaped {
				switch ch {
				case 'n':
					sb.WriteByte('\n')
				case 'r':
					sb.WriteByte('\r')
				case 't':
					sb.WriteByte('\t')
				case '"', '\\':
					sb.WriteByte(ch)
				default:
					sb.WriteByte('\\')
					sb.WriteByte(ch)
				}
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				closedAt = i
				break
			}
			sb.WriteByte(ch)
		}
		if closedAt < 0 {
			return "", fmt.Errorf("unterminated double-quoted value at line %d", lineNum)
		}
		rest := strings.TrimSpace(raw[closedAt+1:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("unexpected trailing characters at line %d", lineNum)
		}
		return sb.String(), nil
	}

	// Unquoted value: strip inline comment only when preceded by whitespace.
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i > 0 && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			return strings.TrimSpace(raw[:i]), nil
		}
	}
	return strings.TrimSpace(raw), nil
}

// ParseLogLevel converts a LOG_LEVEL string into a slog.Level.
// Supported values (case-insensitive): DEBUG, INFO, WARN, WARNING, ERROR.
// Returns (slog.LevelInfo, false) if raw is non-empty and unrecognized.
func ParseLogLevel(raw string) (slog.Level, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", "INFO":
		return slog.LevelInfo, true
	case "DEBUG":
		return slog.LevelDebug, true
	case "WARN", "WARNING":
		return slog.LevelWarn, true
	case "ERROR":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// GetEnv returns the trimmed environment variable value for key or fallback if unset/empty.
func GetEnv(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}

// GetEnvInt parses an integer environment variable for key or returns fallback if unset/invalid.
func GetEnvInt(key string, fallback int) int {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil {
			return parsed
		}
	}
	return fallback
}

// GetEnvBool parses a boolean environment variable for key or returns fallback if unset/invalid.
func GetEnvBool(key string, fallback bool) bool {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		if parsed, err := strconv.ParseBool(val); err == nil {
			return parsed
		}
	}
	return fallback
}

// ParseCSV splits a comma-separated environment string into trimmed, non-empty elements.
func ParseCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
