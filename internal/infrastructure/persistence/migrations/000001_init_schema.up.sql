-- 000001_init_schema.up.sql
-- PostgreSQL 16+ Relational Schema for rag-prompt-registry-service
-- Enforces strict multi-tenant isolation, append-only immutability (ADR-001),
-- and transactional internal audit ledger (ADR-004).

-- Aggregate root for prompt templates
CREATE TABLE IF NOT EXISTS prompt_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_template_name UNIQUE (tenant_id, name)
);

-- Registered JSON output validation schemas
CREATE TABLE IF NOT EXISTS json_schemas (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name VARCHAR(255) NOT NULL,
    schema_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_schema UNIQUE (tenant_id, name)
);

-- Append-only immutable prompt versions (v_1 .. v_n)
CREATE TABLE IF NOT EXISTS prompt_versions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES prompt_templates(id) ON DELETE RESTRICT,
    version INT NOT NULL CHECK (version >= 1),
    raw_ast JSONB NOT NULL,
    ast_hash VARCHAR(64) NOT NULL,
    schema_id UUID NOT NULL REFERENCES json_schemas(id) ON DELETE RESTRICT,
    author VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_template_version UNIQUE (template_id, version)
);

-- Mutable semantic tag pointers (@production, @staging, @canary) with tenant isolation defense-in-depth
CREATE TABLE IF NOT EXISTS prompt_tags (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    template_id UUID NOT NULL REFERENCES prompt_templates(id) ON DELETE CASCADE,
    tag VARCHAR(64) NOT NULL,
    target_version INT NOT NULL CHECK (target_version >= 1),
    updated_by VARCHAR(255) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_template_tag UNIQUE (tenant_id, template_id, tag),
    CONSTRAINT fk_prompt_tags_version FOREIGN KEY (template_id, target_version)
        REFERENCES prompt_versions(template_id, version) ON DELETE RESTRICT
);

-- Transactional compliance ledger (ADR-004: Direct write in same ACID transaction, no external CDC)
CREATE TABLE IF NOT EXISTS prompt_audit_ledger (
    id BIGSERIAL PRIMARY KEY,
    tenant_id UUID NOT NULL,
    template_id UUID NOT NULL, -- Resource UUID (template_id or schema_id for SCHEMA_REGISTERED)
    action VARCHAR(64) NOT NULL, -- "TEMPLATE_CREATED", "VERSION_PUBLISHED", "TAG_PROMOTED", "TAG_ROLLED_BACK", "SCHEMA_REGISTERED"
    version INT,
    tag VARCHAR(64),
    performed_by VARCHAR(255) NOT NULL,
    payload_snapshot JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Database-level immutability guard for append-only tables
CREATE OR REPLACE FUNCTION prevent_append_only_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'Table % is strictly append-only; % operations are forbidden', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_prompt_versions_immutable ON prompt_versions;
CREATE TRIGGER trg_prompt_versions_immutable
BEFORE UPDATE OR DELETE ON prompt_versions
FOR EACH ROW EXECUTE FUNCTION prevent_append_only_mutation();

DROP TRIGGER IF EXISTS trg_prompt_audit_ledger_immutable ON prompt_audit_ledger;
CREATE TRIGGER trg_prompt_audit_ledger_immutable
BEFORE UPDATE OR DELETE ON prompt_audit_ledger
FOR EACH ROW EXECUTE FUNCTION prevent_append_only_mutation();

CREATE INDEX IF NOT EXISTS idx_prompt_templates_tenant ON prompt_templates(tenant_id);
CREATE INDEX IF NOT EXISTS idx_prompt_versions_lookup ON prompt_versions(template_id, version);
CREATE INDEX IF NOT EXISTS idx_prompt_versions_hash ON prompt_versions(template_id, ast_hash);
CREATE INDEX IF NOT EXISTS idx_prompt_tags_lookup ON prompt_tags(tenant_id, template_id, tag);
CREATE INDEX IF NOT EXISTS idx_prompt_audit_template ON prompt_audit_ledger(tenant_id, template_id, created_at);
CREATE INDEX IF NOT EXISTS idx_json_schemas_tenant ON json_schemas(tenant_id);

