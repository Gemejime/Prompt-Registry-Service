-- 000001_init_schema.down.sql
-- Teardown migration for rag-prompt-registry-service relational schema

DROP TRIGGER IF EXISTS trg_prompt_audit_ledger_immutable ON prompt_audit_ledger;
DROP TRIGGER IF EXISTS trg_prompt_versions_immutable ON prompt_versions;
DROP FUNCTION IF EXISTS prevent_append_only_mutation();

DROP TABLE IF EXISTS prompt_audit_ledger;
DROP TABLE IF EXISTS prompt_tags;
DROP TABLE IF EXISTS prompt_versions;
DROP TABLE IF EXISTS json_schemas;
DROP TABLE IF EXISTS prompt_templates;

