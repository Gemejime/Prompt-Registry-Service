// Package migrations embeds version-controlled PostgreSQL DDL scripts.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
)

// FS holds the embedded SQL migration files.
//
//go:embed *.sql
var FS embed.FS

// InitSchemaUp returns the contents of 000001_init_schema.up.sql.
func InitSchemaUp() (string, error) {
	b, err := fs.ReadFile(FS, "000001_init_schema.up.sql")
	if err != nil {
		return "", fmt.Errorf("failed to read 000001_init_schema.up.sql: %w", err)
	}
	return string(b), nil
}

// InitSchemaDown returns the contents of 000001_init_schema.down.sql.
func InitSchemaDown() (string, error) {
	b, err := fs.ReadFile(FS, "000001_init_schema.down.sql")
	if err != nil {
		return "", fmt.Errorf("failed to read 000001_init_schema.down.sql: %w", err)
	}
	return string(b), nil
}
