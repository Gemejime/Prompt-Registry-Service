package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/domain/repository"
)

// PostgresAuditRepository provides read access and transactional write helpers for prompt_audit_ledger.
type PostgresAuditRepository struct {
	pool *pgxpool.Pool
}

var _ repository.AuditRepository = (*PostgresAuditRepository)(nil)

// NewAuditRepository creates a new PostgresAuditRepository.
func NewAuditRepository(pool *pgxpool.Pool) *PostgresAuditRepository {
	return &PostgresAuditRepository{pool: pool}
}

// insertAuditEntryTx writes an immutable compliance record inside an active PostgreSQL transaction (ADR-004).
func insertAuditEntryTx(ctx context.Context, tx pgx.Tx, entry entity.AuditEntry) error {
	snapshotBytes, err := json.Marshal(entry.PayloadSnapshot)
	if err != nil {
		return fmt.Errorf("failed to marshal audit payload snapshot: %w", err)
	}

	var tagPtr *string
	if entry.Tag != "" {
		t := entry.Tag
		tagPtr = &t
	}

	const q = `
		INSERT INTO prompt_audit_ledger (
			tenant_id, template_id, action, version, tag, performed_by, payload_snapshot
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	_, err = tx.Exec(ctx, q,
		entry.TenantID,
		entry.TemplateID,
		string(entry.Action),
		entry.Version,
		tagPtr,
		entry.PerformedBy,
		snapshotBytes,
	)
	if err != nil {
		return fmt.Errorf("failed to insert audit ledger entry: %w", err)
	}
	return nil
}

// ListByTemplate retrieves ordered compliance ledger entries for a template or schema resource within a tenant.
func (r *PostgresAuditRepository) ListByTemplate(ctx context.Context, tenantID, templateID string, limit int) ([]entity.AuditEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	const q = `
		SELECT id, tenant_id::text, template_id::text, action, version, COALESCE(tag, ''), performed_by, payload_snapshot, created_at
		FROM prompt_audit_ledger
		WHERE tenant_id = $1 AND template_id = $2
		ORDER BY id ASC
		LIMIT $3
	`
	rows, err := r.pool.Query(ctx, q, tenantID, templateID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query audit ledger: %w", err)
	}
	defer rows.Close()

	var entries []entity.AuditEntry
	for rows.Next() {
		var e entity.AuditEntry
		var actionStr string
		var snapshotBytes []byte
		if err := rows.Scan(
			&e.ID,
			&e.TenantID,
			&e.TemplateID,
			&actionStr,
			&e.Version,
			&e.Tag,
			&e.PerformedBy,
			&snapshotBytes,
			&e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan audit ledger row: %w", err)
		}
		e.Action = entity.AuditAction(actionStr)
		if len(snapshotBytes) > 0 {
			if err := json.Unmarshal(snapshotBytes, &e.PayloadSnapshot); err != nil {
				return nil, fmt.Errorf("failed to unmarshal audit payload snapshot: %w", err)
			}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit ledger rows iteration error: %w", err)
	}
	return entries, nil
}
