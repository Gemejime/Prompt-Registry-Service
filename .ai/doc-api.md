# Microservice Documented API ("Doc API")

> **MANDATORY INVARIANT**: This document specifies both the internal system contracts and the **HTTP Doc API & OpenAPI Endpoint** exposed by the microservice.
> **API ACCESSIBILITY REQUIREMENT**: This documentation specification MUST be accessible directly via HTTP endpoints hosted on the registry control plane server (port `8080`):
>
> - `GET /docs` — Interactive OpenAPI 3.1 Documentation via **Swagger UI**.
> - `GET /redoc` — Interactive OpenAPI 3.1 Documentation via **ReDoc**.
> - `GET /openapi.yaml` & `GET /api/docs` — Machine-readable **OpenAPI 3.1 Specification**.
>
> **CONTINUOUS DOC API UPDATE INVARIANT**: This file and the corresponding OpenAPI specification schemas (`openapi/openapi.yaml`) MUST be updated on **every code change or addition**, regardless of how small the modification is.

---

## 1. Documentation & Probe API Endpoints (OpenAPI 3.1)

The control plane process (`rag-prompt-registry-service`) runs an internal HTTP router (default port `8080`) serving operational probes, OpenAPI contracts, and administrative endpoints:

| Endpoint | Method | Response Format | Description |
| :--- | :--- | :--- | :--- |
| `/docs` | `GET` | `text/html` | Interactive Swagger UI powered by OpenAPI 3.1 |
| `/redoc` | `GET` | `text/html` | Interactive ReDoc UI documentation |
| `/api/docs` | `GET` | `application/yaml` | Raw OpenAPI 3.1 YAML specification |
| `/openapi.yaml` | `GET` | `application/yaml` | Standard machine-readable OpenAPI 3.1 specification |
| `/healthz` | `GET` | `application/json` | Liveness probe returning `{"status": "healthy"}` |
| `/ready` | `GET` | `application/json` | Readiness probe verifying PostgreSQL pool and Redis cluster (Note: when `POSTGRES_DSN` is absent and `ALLOW_IN_MEMORY_STORE=true`, `CheckPostgres` succeeds against the non-durable in-memory store; a `200 OK` ready response does not confirm PostgreSQL is configured or that storage is durable) |
| `/metrics` | `GET` | `text/plain` | Prometheus metrics scrape endpoint |

---

## 2. Administrative REST API Endpoints

| Endpoint | Method | Request Payload | Response Codes | Description |
| :--- | :--- | :--- | :--- | :--- |
| `/v1/templates` | `POST` | `CreateTemplateRequest` | `201 Created`, `400 Bad Request`, `404 Not Found`, `409 Conflict` | Initialize template aggregate root and publish initial version $v_1$ |
| `/v1/templates` | `GET` | Cursor query parameters (`limit`, `cursor`) | `200 OK` | Paginated listing of registered prompt templates |
| `/v1/templates/{id}/versions` | `POST` | `PublishVersionRequest` | `201 Created`, `400 Bad Request`, `404 Not Found` | Create next immutable version $v_{n+1}$ with AST and schema contract |
| `/v1/templates/{id}/versions/{version}` | `GET` | None | `200 OK`, `404 Not Found` | Retrieve immutable canonical AST representation |
| `/v1/templates/{id}/tags/{tag}` | `PUT` | `PromoteTagRequest` | `200 OK`, `400 Bad Request`, `404 Not Found`, `409 Conflict` | Atomically mutate semantic tag pointer (`@production`) & emit Redis Pub/Sub |
| `/v1/templates/{id}/tags/{tag}` | `GET` | None | `200 OK`, `404 Not Found` | Resolve current target version and AST assigned to a tag pointer |
| `/v1/schemas` | `POST` | `RegisterSchemaRequest` | `201 Created`, `400 Bad Request`, `409 Conflict` | Register a new JSON output validation schema contract |
| `/v1/schemas` | `GET` | Cursor query parameters (`limit`, `cursor`) | `200 OK` | Paginated listing of registered JSON output schemas |
| `/v1/schemas/{id}` | `GET` | None | `200 OK`, `404 Not Found` | Fetch registered JSON output schema definition |

---

## 3. OpenAPI 3.1 Specification Contract (`openapi/openapi.yaml`)

```yaml
openapi: 3.1.0
info:
  title: RAG Prompt Registry Service API
  description: Control Plane Administration, AST Template Lineage, and Output Schema Registry for rag-prompt-registry-service
  version: 1.0.0
servers:
  - url: http://localhost:8080
    description: Local Control Plane Administration Server
security:
  - bearerAuth: []
paths:
  /healthz:
    get:
      summary: Liveness Probe
      operationId: getLiveness
      security: []
      responses:
        '200':
          description: Server process is responsive
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:
                    type: string
                    example: healthy
                required: [status]
  /ready:
    get:
      summary: Readiness Probe
      description: Verifies PostgreSQL pool and Redis cluster reachability. When POSTGRES_DSN is absent (with ALLOW_IN_MEMORY_STORE=true), CheckPostgres reports success without verifying PostgreSQL; a 200 OK response does not confirm PostgreSQL is configured or that storage is durable.
      operationId: getReadiness
      security: []
      responses:
        '200':
          description: Server is ready to handle traffic
          content:
            application/json:
              schema:
                type: object
                properties:
                  status:
                    type: string
                    example: ready
                  postgres:
                    type: string
                    example: connected
                  redis:
                    type: string
                    example: reachable
                required: [status, postgres, redis]
        '503':
          description: Database or cache dependency unreachable
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
  /metrics:
    get:
      summary: Prometheus Metrics
      operationId: getMetrics
      security: []
      responses:
        '200':
          description: Prometheus exposition metrics
          content:
            text/plain:
              schema:
                type: string
  /openapi.yaml:
    get:
      summary: OpenAPI 3.1 Specification in YAML
      operationId: getOpenAPISpec
      security: []
      responses:
        '200':
          description: Raw OpenAPI YAML file
          content:
            application/yaml:
              schema:
                type: string
  /api/docs:
    get:
      summary: Alias for OpenAPI 3.1 Specification
      operationId: getAPIDocs
      security: []
      responses:
        '200':
          description: OpenAPI Specification
          content:
            application/yaml:
              schema:
                type: string

  /v1/templates:
    post:
      summary: Create Prompt Template
      description: Initialize template aggregate root and publish initial immutable version v1.
      operationId: createTemplate
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CreateTemplateRequest'
      responses:
        '201':
          description: Template successfully created
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/TemplateResponse'
        '400':
          description: Invalid input payload or AST schema
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '404':
          description: Referenced output schema not found
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '409':
          description: Template name conflict within tenant
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
    get:
      summary: List Prompt Templates
      description: Paginated listing of registered prompt templates within tenant.
      operationId: listTemplates
      parameters:
        - name: limit
          in: query
          schema:
            type: integer
            default: 20
            maximum: 100
        - name: cursor
          in: query
          schema:
            type: string
      responses:
        '200':
          description: Paginated list of templates
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ListTemplatesResponse'

  /v1/templates/{id}/versions:
    post:
      summary: Publish Immutable Version
      description: Create next immutable version v_{n+1} with AST and registered JSON schema contract.
      operationId: publishVersion
      parameters:
        - name: id
          in: path
          required: true
          description: Template UUID or unique name
          schema:
            type: string
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/PublishVersionRequest'
      responses:
        '201':
          description: Immutable version published
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/VersionResponse'
        '400':
          description: Invalid AST or schema ID
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '404':
          description: Template or referenced output schema not found
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'

  /v1/templates/{id}/versions/{version}:
    get:
      summary: Retrieve Specific Template Version
      description: Fetch immutable canonical AST representation and schema contract for a specific version.
      operationId: getTemplateVersion
      parameters:
        - name: id
          in: path
          required: true
          description: Template UUID or unique name
          schema:
            type: string
        - name: version
          in: path
          required: true
          description: Version number (e.g. 1, 2)
          schema:
            type: integer
      responses:
        '200':
          description: Canonical AST definition and schema association
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/VersionDetailResponse'
        '404':
          description: Template or version not found
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'

  /v1/templates/{id}/tags/{tag}:
    put:
      summary: Promote Semantic Tag Pointer
      description: Atomically mutate semantic tag pointer (@production, @staging, @canary) and broadcast invalidation to Redis Pub/Sub.
      operationId: promoteTag
      parameters:
        - name: id
          in: path
          required: true
          description: Template UUID or unique name
          schema:
            type: string
        - name: tag
          in: path
          required: true
          description: Semantic tag name (e.g. @production)
          schema:
            type: string
            example: "@production"
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/PromoteTagRequest'
      responses:
        '200':
          description: Tag updated and Redis invalidation event broadcast
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/PromoteTagResponse'
        '400':
          description: Invalid target version
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '404':
          description: Template or target version not found
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '409':
          description: Tag pointer version conflict (current pointer version does not match expected_version)
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
    get:
      summary: Resolve Semantic Tag Pointer
      description: Resolve the current target version and AST assigned to a tag pointer.
      operationId: resolveTag
      parameters:
        - name: id
          in: path
          required: true
          description: Template UUID or unique name
          schema:
            type: string
        - name: tag
          in: path
          required: true
          description: Semantic tag name (e.g. @production)
          schema:
            type: string
            example: "@production"
      responses:
        '200':
          description: Resolved target version and canonical AST
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ResolveTagResponse'
        '404':
          description: Template or tag pointer not found
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'

  /v1/schemas:
    post:
      summary: Register JSON Output Schema
      description: Register a new JSON schema contract for validating downstream structured LLM outputs.
      operationId: registerSchema
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/RegisterSchemaRequest'
      responses:
        '201':
          description: Schema registered successfully
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/RegisterSchemaResponse'
        '400':
          description: Invalid JSON Schema draft specification
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
        '409':
          description: Schema name conflict within tenant
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'
    get:
      summary: List JSON Output Schemas
      description: Paginated listing of registered output schemas.
      operationId: listSchemas
      parameters:
        - name: limit
          in: query
          schema:
            type: integer
            default: 20
            maximum: 100
        - name: cursor
          in: query
          schema:
            type: string
      responses:
        '200':
          description: Paginated list of registered schemas
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ListSchemasResponse'

  /v1/schemas/{id}:
    get:
      summary: Get JSON Output Schema
      description: Retrieve the full JSON schema definition by ID.
      operationId: getSchema
      parameters:
        - name: id
          in: path
          required: true
          description: Schema UUID
          schema:
            type: string
            format: uuid
      responses:
        '200':
          description: Registered schema contract details
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/SchemaResponse'
        '404':
          description: Schema not found
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ErrorResponse'

components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT
      description: Bearer JWT token with tenant_id and administrative role claims (prompt:write, prompt:publish, tag:promote).
  schemas:
    CreateTemplateRequest:
      type: object
      required: [tenant_id, name, raw_ast, schema_id, author]
      properties:
        tenant_id:
          type: string
          format: uuid
          example: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
        name:
          type: string
          example: "rag-financial-summary"
        description:
          type: string
          example: "Financial report synthesis template with contextual citations"
        raw_ast:
          $ref: '#/components/schemas/TemplateAST'
        schema_id:
          type: string
          format: uuid
          description: Registered JSON schema UUID governing structured output contract (must resolve to an existing schema)
        author:
          type: string
          example: "system-admin"

    PublishVersionRequest:
      type: object
      required: [raw_ast, schema_id, author]
      properties:
        raw_ast:
          $ref: '#/components/schemas/TemplateAST'
        schema_id:
          type: string
          format: uuid
          description: Registered JSON schema UUID governing structured output contract (must resolve to an existing schema)
        author:
          type: string
          example: "system-admin"

    PromoteTagRequest:
      type: object
      required: [target_version, expected_version, updated_by]
      properties:
        target_version:
          type: integer
          example: 2
        expected_version:
          type: integer
          minimum: 0
          description: Expected current version of the tag pointer for atomic optimistic locking (0 if the tag is not yet assigned)
          example: 1
        updated_by:
          type: string
          example: "deploy-pipeline"

    PromoteTagResponse:
      type: object
      required: [template_id, tag, target_version, broadcast_emitted]
      properties:
        template_id:
          type: string
          format: uuid
        tag:
          type: string
          example: "@production"
        target_version:
          type: integer
          example: 2
        broadcast_emitted:
          type: boolean
          example: true

    ResolveTagResponse:
      type: object
      required: [template_id, tag, version, ast, schema_id]
      properties:
        template_id:
          type: string
          format: uuid
        tag:
          type: string
          example: "@production"
        version:
          type: integer
          example: 2
        ast:
          $ref: '#/components/schemas/TemplateAST'
        schema_id:
          type: string
          format: uuid

    TemplateResponse:
      type: object
      required: [id, tenant_id, name, current_version, created_at, updated_at]
      properties:
        id:
          type: string
          format: uuid
        tenant_id:
          type: string
          format: uuid
        name:
          type: string
          example: "rag-financial-summary"
        description:
          type: string
        current_version:
          type: integer
          example: 1
        created_at:
          type: string
          format: date-time
        updated_at:
          type: string
          format: date-time

    VersionResponse:
      type: object
      required: [template_id, version, created_at]
      properties:
        template_id:
          type: string
          format: uuid
        version:
          type: integer
          example: 2
        created_at:
          type: string
          format: date-time

    VersionDetailResponse:
      type: object
      required: [template_id, version, ast, schema_id, author, created_at]
      properties:
        template_id:
          type: string
          format: uuid
        version:
          type: integer
          example: 2
        ast:
          $ref: '#/components/schemas/TemplateAST'
        schema_id:
          type: string
          format: uuid
        author:
          type: string
          example: "system-admin"
        created_at:
          type: string
          format: date-time

    ListTemplatesResponse:
      type: object
      required: [templates]
      properties:
        templates:
          type: array
          maxItems: 100
          items:
            $ref: '#/components/schemas/TemplateResponse'
        next_cursor:
          type: string

    TemplateAST:
      type: object
      required: [system_prompt, messages, variable_keys, default_dialect]
      properties:
        system_prompt:
          type: string
          example: "You are a specialized enterprise financial analyst assistant."
        messages:
          type: array
          items:
            $ref: '#/components/schemas/MessageBlock'
        variable_keys:
          type: array
          items:
            type: string
          example: ["context", "query"]
        default_dialect:
          type: string
          enum: [DIALECT_ANTHROPIC_XML, DIALECT_LLAMA_SPECIAL, DIALECT_OPENAI_STANDARD]
          example: "DIALECT_ANTHROPIC_XML"
        persona_overrides:
          type: object
          description: Optional role/persona-specific system prompt overrides keyed by persona identifier
          additionalProperties:
            type: string
          example:
            executive: "You are a concise executive briefing assistant."
        few_shot_exemplars:
          type: array
          description: Optional ordered few-shot exemplar message blocks
          items:
            $ref: '#/components/schemas/MessageBlock'

    MessageBlock:
      type: object
      required: [role, content_pattern]
      properties:
        role:
          type: string
          enum: [system, user, assistant]
          example: "user"
        content_pattern:
          type: string
          example: "Context:\n<context>{{context}}</context>\n\nQuery: {{query}}"

    RegisterSchemaRequest:
      type: object
      required: [tenant_id, name, json_schema]
      properties:
        tenant_id:
          type: string
          format: uuid
          example: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
        name:
          type: string
          example: "financial-metrics-output"
        json_schema:
          type: object
          description: Draft-07 / 2020-12 valid JSON schema definition

    RegisterSchemaResponse:
      type: object
      required: [schema_id]
      properties:
        schema_id:
          type: string
          format: uuid

    SchemaResponse:
      type: object
      required: [id, tenant_id, name, schema_json, created_at]
      properties:
        id:
          type: string
          format: uuid
        tenant_id:
          type: string
          format: uuid
        name:
          type: string
        schema_json:
          type: object
        created_at:
          type: string
          format: date-time

    ListSchemasResponse:
      type: object
      required: [schemas]
      properties:
        schemas:
          type: array
          maxItems: 100
          items:
            $ref: '#/components/schemas/SchemaResponse'
        next_cursor:
          type: string

    ErrorResponse:
      type: object
      required: [code, message]
      properties:
        code:
          type: string
          example: "BAD_REQUEST"
        message:
          type: string
          example: "Invalid template specification or syntax error"
        details:
          type: array
          items:
            type: string
```

---

## 4. gRPC Management Contracts (`proto/v1/`)

### `proto/v1/prompt_service.proto`
```protobuf
syntax = "proto3";

package rag.prompt.v1;

option go_package = "rag-prompt-registry-service/proto/v1;promptv1";

import "proto/v1/types.proto";

service PromptRegistryService {
  rpc CreateTemplate (CreateTemplateRequest) returns (CreateTemplateResponse);
  rpc PublishVersion (PublishVersionRequest) returns (PublishVersionResponse);
  rpc GetTemplateVersion (GetTemplateVersionRequest) returns (GetTemplateVersionResponse);
  rpc PromoteTag (PromoteTagRequest) returns (PromoteTagResponse);
  rpc ResolveTag (ResolveTagRequest) returns (ResolveTagResponse);
  rpc RegisterSchema (RegisterSchemaRequest) returns (RegisterSchemaResponse);
  rpc GetSchema (GetSchemaRequest) returns (GetSchemaResponse);
  rpc ListTemplates (ListTemplatesRequest) returns (ListTemplatesResponse);
  rpc ListSchemas (ListSchemasRequest) returns (ListSchemasResponse);
}

message CreateTemplateRequest {
  string tenant_id = 1;
  string name = 2;
  TemplateAST raw_ast = 3;
  string schema_id = 4; // Required: must resolve to a registered JSON schema in the same tenant
  string author = 5;
  string description = 6;
}

message CreateTemplateResponse {
  string template_id = 1;
  uint32 initial_version = 2;
}

message PublishVersionRequest {
  string template_id = 1; // Template UUID or unique name within tenant
  TemplateAST raw_ast = 2;
  string schema_id = 3; // Required: must resolve to a registered JSON schema in the same tenant
  string author = 4;
  string tenant_id = 5; // Validated against caller JWT / x-tenant-id metadata
}

message PublishVersionResponse {
  string template_id = 1;
  uint32 version = 2;
  string created_at = 3;
}

message PromoteTagRequest {
  string template_id = 1; // Template UUID or unique name within tenant
  string tag = 2; // e.g. "@production", "@staging", "@canary"
  uint32 target_version = 3;
  string updated_by = 4;
  uint32 expected_version = 5; // Required optimistic lock precondition (0 if unassigned); returns ABORTED on mismatch
  string tenant_id = 6; // Validated against caller JWT / x-tenant-id metadata
}

message PromoteTagResponse {
  string template_id = 1;
  string tag = 2;
  uint32 target_version = 3;
  bool broadcast_emitted = 4;
}

message ResolveTagRequest {
  string template_id = 1; // Template UUID or unique name within tenant
  string tag = 2;
  string tenant_id = 3;
}

message ResolveTagResponse {
  string template_id = 1;
  string tag = 2;
  uint32 version = 3;
  TemplateAST ast = 4;
  string schema_id = 5;
}

message GetTemplateVersionRequest {
  string template_id = 1; // Template UUID or unique name within tenant
  uint32 version = 2;
  string tenant_id = 3;
}

message GetTemplateVersionResponse {
  string template_id = 1;
  uint32 version = 2;
  TemplateAST ast = 3;
  string schema_id = 4;
  string author = 5;
  string created_at = 6;
}

message RegisterSchemaRequest {
  string tenant_id = 1;
  string name = 2;
  string json_schema = 3;
}

message RegisterSchemaResponse {
  string schema_id = 1;
}

message GetSchemaRequest {
  string schema_id = 1;
  string tenant_id = 2;
}

message GetSchemaResponse {
  string schema_id = 1;
  string name = 2;
  string json_schema = 3;
  string tenant_id = 4;
  string created_at = 5;
}

message ListTemplatesRequest {
  string tenant_id = 1;
  uint32 limit = 2;
  string cursor = 3;
}

message TemplateSummary {
  string id = 1;
  string tenant_id = 2;
  string name = 3;
  string description = 4;
  uint32 current_version = 5;
  string created_at = 6;
  string updated_at = 7;
}

message ListTemplatesResponse {
  repeated TemplateSummary templates = 1;
  string next_cursor = 2;
}

message ListSchemasRequest {
  string tenant_id = 1;
  uint32 limit = 2;
  string cursor = 3;
}

message ListSchemasResponse {
  repeated GetSchemaResponse schemas = 1;
  string next_cursor = 2;
}
```

### `proto/v1/types.proto`
```protobuf
syntax = "proto3";

package rag.prompt.v1;

option go_package = "rag-prompt-registry-service/proto/v1;promptv1";

enum DialectTarget {
  DIALECT_UNSPECIFIED = 0;
  DIALECT_ANTHROPIC_XML = 1;
  DIALECT_LLAMA_SPECIAL = 2;
  DIALECT_OPENAI_STANDARD = 3;
}

message TemplateAST {
  string system_prompt = 1;
  repeated MessageBlock messages = 2;
  repeated string variable_keys = 3;
  DialectTarget default_dialect = 4;
  map<string, string> persona_overrides = 5;
  repeated MessageBlock few_shot_exemplars = 6;
}

message MessageBlock {
  string role = 1; // "system", "user", "assistant"
  string content_pattern = 2; // e.g. "Context:\n<context>{{context}}</context>\nQuery: {{query}}"
}
```

---

## 5. Relational Persistence Schema (PostgreSQL 16+)

```sql
-- Aggregates root
CREATE TABLE prompt_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_template_name UNIQUE (tenant_id, name)
);

-- Registered JSON schemas
CREATE TABLE json_schemas (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name VARCHAR(255) NOT NULL,
    schema_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_schema UNIQUE (tenant_id, name)
);

-- Append-only immutable versions
CREATE TABLE prompt_versions (
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

-- Mutable tag pointers (@production, @staging, @canary) with tenant isolation defense-in-depth
CREATE TABLE prompt_tags (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    template_id UUID NOT NULL REFERENCES prompt_templates(id) ON DELETE CASCADE,
    tag VARCHAR(64) NOT NULL, -- e.g. "@production"
    target_version INT NOT NULL CHECK (target_version >= 1),
    updated_by VARCHAR(255) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_template_tag UNIQUE (tenant_id, template_id, tag),
    CONSTRAINT fk_prompt_tags_version FOREIGN KEY (template_id, target_version)
        REFERENCES prompt_versions(template_id, version) ON DELETE RESTRICT
);

-- Transactional compliance ledger (ADR-004: Direct write, no external CDC)
CREATE TABLE prompt_audit_ledger (
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

CREATE TRIGGER trg_prompt_versions_immutable
BEFORE UPDATE OR DELETE ON prompt_versions
FOR EACH ROW EXECUTE FUNCTION prevent_append_only_mutation();

CREATE TRIGGER trg_prompt_audit_ledger_immutable
BEFORE UPDATE OR DELETE ON prompt_audit_ledger
FOR EACH ROW EXECUTE FUNCTION prevent_append_only_mutation();

CREATE INDEX idx_prompt_templates_tenant ON prompt_templates(tenant_id);
CREATE INDEX idx_prompt_versions_lookup ON prompt_versions(template_id, version);
CREATE INDEX idx_prompt_versions_hash ON prompt_versions(template_id, ast_hash);
CREATE INDEX idx_prompt_tags_lookup ON prompt_tags(tenant_id, template_id, tag);
CREATE INDEX idx_prompt_audit_template ON prompt_audit_ledger(tenant_id, template_id, created_at);
CREATE INDEX idx_json_schemas_tenant ON json_schemas(tenant_id);
```

---

## 6. Redis Pub/Sub Invalidation Contract

- **Channel**: `channel:prompt:invalidation`
- **Payload Schema (JSON)**:
```json
{
  "event_id": "01HXYZ1234567890ABCDEF",
  "tenant_id": "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
  "template_id": "7f8b9a2c-1234-5678-90ab-cdef12345678",
  "tag": "@production",
  "new_version": 2,
  "timestamp": "2026-11-07T08:50:00Z"
}
```
- **Subscriber Semantics**:
  - `pkg/promptsdk/cache.go` maintains active Redis Pub/Sub subscription.
  - Upon receiving payload: immediately evicts `template:{id}:tag:{tag}` from in-memory cache.
  - Sub-millisecond local eviction; global synchronization $<500$ms across all orchestrator nodes.

---

## 7. Embedded Go SDK Contract (`pkg/promptsdk`)

```go
package promptsdk

import (
    "context"
)

type DialectType string

const (
    // DialectType constants map 1:1 to the REST OpenAPI and Protobuf DialectTarget enum values.
    DialectAnthropicXML   DialectType = "DIALECT_ANTHROPIC_XML"
    DialectLlamaSpecial   DialectType = "DIALECT_LLAMA_SPECIAL"
    DialectOpenAIStandard DialectType = "DIALECT_OPENAI_STANDARD"
    // DialectStandardJSON is an explicit alias for DialectOpenAIStandard ("DIALECT_OPENAI_STANDARD").
    DialectStandardJSON   DialectType = DialectOpenAIStandard
)

type RenderOptions struct {
    TargetDialect   DialectType
    Variables       map[string]string
    MaxTokens       int
    Persona         string
    StrictVariables bool
}

type PromptSDK interface {
    // Render compiles template AST and binds context variables using sync.Pool
    // Zero allocations on hot path; P99 <= 0.5ms.
    Render(ctx context.Context, templateID string, tagOrVersion string, opts RenderOptions) ([]byte, error)
    
    // RenderToBuffer renders directly into the provided bytes.Buffer with 0 heap allocations on cache hits.
    RenderToBuffer(ctx context.Context, dst *bytes.Buffer, templateID string, tagOrVersion string, opts RenderOptions) error

    // Invalidate explicitly forces cache eviction
    Invalidate(templateID string, tag string)
    
    // Purge forces an immediate local wipe of all mutable tag cache pointers in L1_tag (Runbook 2)
    Purge()

    // Close cleanly terminates Redis Pub/Sub listeners
    Close() error
}

// Phase 2 In-Process Engine & Pooled Buffer Helpers:
// - AcquireBuffer() *bytes.Buffer
// - ReleaseBuffer(buf *bytes.Buffer)
// - InterpolateIntoBuffer(dst *bytes.Buffer, pattern string, vars map[string]string)
// - InterpolateIntoBufferChecked(dst *bytes.Buffer, pattern string, vars map[string]string) error
// - CompileTemplateAST(templateID, templateName string, version int, ast TemplateAST, schema *SchemaContract) (*CompiledTemplate, error)
// - CompileIntoBuffer(dst *bytes.Buffer, compiled *CompiledTemplate, opts RenderOptions, guard *tokenizer.BudgetGuard) error
// - NewClient(cfg ClientConfig) (*Client, error)
// - (*Client).RenderToBuffer(ctx context.Context, dst *bytes.Buffer, templateID string, tagOrVersion string, opts RenderOptions) error
// - (*Client).PreloadVersion(templateID, templateName string, version int, ast TemplateAST, schema *SchemaContract) (*CompiledTemplate, error)
// - (*Client).PreloadTag(templateID, templateName, tag string, version int, ast TemplateAST, schema *SchemaContract) (*CompiledTemplate, error)
// - (*Client).Purge()

// Phase 3 Orchestrator Data Plane Pipeline (pkg/orchestrator):
//
// type ExecutionMode string // "production" | "diagnostic"
//
// type RetrievedChunk struct {
//     ChunkID    string            `json:"chunk_id"`
//     DocumentID string            `json:"document_id"`
//     Text       string            `json:"text"`
//     Score      float64           `json:"score"`
//     Metadata   map[string]string `json:"metadata,omitempty"`
// }
//
// type ContextRetriever interface {
//     Retrieve(ctx context.Context, query string) ([]RetrievedChunk, error)
// }
//
// type InferenceRequest struct {
//     Model       string            `json:"model"`
//     Prompt      []byte            `json:"prompt"`
//     MaxTokens   int               `json:"max_tokens"`
//     Temperature float64           `json:"temperature,omitempty"`
//     Stop        []string          `json:"stop,omitempty"`
//     Metadata    map[string]string `json:"metadata,omitempty"`
// }
//
// type InferenceResponse struct {
//     ModelID      string `json:"model_id"`
//     OutputText   string `json:"output_text"`
//     FinishReason string `json:"finish_reason"`
//     TokensUsed   int    `json:"tokens_used"`
// }
//
// type InferenceDispatcher interface {
//     Dispatch(ctx context.Context, req InferenceRequest) (*InferenceResponse, error)
// }
//
// type PipelineRequest struct {
//     Query              string                `json:"query"`
//     TemplateID         string                `json:"template_id"`
//     TagOrVersion       string                `json:"tag_or_version"`
//     TargetDialect      promptsdk.DialectType `json:"target_dialect,omitempty"`
//     Variables          map[string]string     `json:"variables,omitempty"`
//     Persona            string                `json:"persona,omitempty"`
//     MaxTokens          int                   `json:"max_tokens,omitempty"`
//     Model              string                `json:"model,omitempty"`
//     Mode               ExecutionMode         `json:"mode,omitempty"`
//     StrictVariables    bool                  `json:"strict_variables,omitempty"`
//     PreRetrievedChunks []RetrievedChunk      `json:"pre_retrieved_chunks,omitempty"`
// }
//
// type TimingWaterfall struct {
//     AssemblyMs  float64 `json:"assembly_ms"`
//     SDKRenderMs float64 `json:"sdk_render_ms"`
//     InferenceMs float64 `json:"inference_ms"`
//     TotalMs     float64 `json:"total_ms"`
// }
//
// type TokenBudgetTrace struct {
//     BudgetLimit       int  `json:"budget_limit"`
//     TokensUsed        int  `json:"tokens_used"`
//     FramingOverhead   int  `json:"framing_overhead"`
//     ContextTokens     int  `json:"context_tokens"`
//     TruncationApplied bool `json:"truncation_applied"`
// }
//
// type DiagnosticTrace struct {
//     Timing          TimingWaterfall  `json:"timing"`
//     TokenBudget     TokenBudgetTrace `json:"token_budget"`
//     RetrievedChunks []RetrievedChunk `json:"retrieved_chunks"`
//     Dialect         string           `json:"dialect"`
//     CompiledPrompt  string           `json:"compiled_prompt,omitempty"`
// }
//
// type PipelineResponse struct {
//     OutputText        string             `json:"output_text"`
//     InferenceResponse *InferenceResponse `json:"inference_response,omitempty"`
//     CompiledPrompt    []byte             `json:"compiled_prompt,omitempty"`
//     Diagnostic        *DiagnosticTrace   `json:"diagnostic,omitempty"`
// }
//
// type RAGPipeline struct { ... }
// func NewRAGPipeline(cfg PipelineConfig) (*RAGPipeline, error)
// func (*RAGPipeline).Execute(ctx context.Context, req PipelineRequest) (*PipelineResponse, error)
```

---

## 8. Prometheus Observability Metrics

| Metric Name | Type | Labels | Description |
| :--- | :--- | :--- | :--- |
| `prompt_sdk_render_duration_seconds` | Histogram | `dialect`, `status` | SDK in-process compilation latency (Target P99 $\le 0.5$ms) |
| `prompt_registry_mutation_duration_seconds` | Histogram | `operation`, `status` | Control plane write latencies |
| `prompt_registry_pointer_rollbacks_total` | Counter | _none_ | Semantic tag pointer rollbacks executed (`is_rollback == true`) |
| `prompt_sdk_cache_hit_ratio` | Gauge | `tier` (L1_version, L1_tag) | Local SDK memory hit ratio (Target $>99.9\%$) |
| `prompt_invalidation_events_published_total` | Counter | `status` | Redis Pub/Sub eviction signals emitted |
| `prompt_sdk_pubsub_connected` | Gauge | `node_id` | Orchestrator Pub/Sub connection status (1=healthy, 0=fallback TTL) |
| `rag_orchestrator_pipeline_duration_seconds` | Histogram | `mode`, `dialect` | End-to-end RAG orchestrator pipeline execution latency |
| `rag_orchestrator_context_bytes` | Histogram | _none_ | Assembled document chunk context payload size in bytes |
| `rag_orchestrator_requests_total` | Counter | `status` | Total RAG pipeline requests executed |

---

## 9. Deployment, Containerization & Prometheus Alerting Specifications

### Distroless Container (`deployments/docker/Dockerfile`)
- **Builder Stage**: `golang:1.26-alpine` compiling `./cmd/prompt-registry-server` with `CGO_ENABLED=0`, `-trimpath`, `-ldflags="-s -w -X main.version=${VERSION}"`.
- **Runtime Stage**: `gcr.io/distroless/static-debian12:nonroot`, `WORKDIR /app`, `USER 65532:65532`, `readOnlyRootFilesystem: true` compatible.
- **Packaged Static Assets**: Copies `openapi/openapi.yaml` to `/app/openapi/openapi.yaml` for `/docs`, `/redoc`, `/api/docs`, and `/openapi.yaml`.
- **Exposed Ports**: `8080` (HTTP REST, Doc API, `/healthz`, `/ready`, `/metrics`) and `9090` (gRPC Management API).
- **Container Healthcheck**: Invokes `/app/prompt-registry-server -healthcheck` targeting `GET /healthz`.

### Production Helm Chart (`deployments/helm/rag-prompt-registry/`)
- **Chart**: `rag-prompt-registry-service` (`apiVersion: v2`).
- **Control Plane Sizing**: `replicas: 2`, `cpu: {requests: 100m, limits: 500m}`, `memory: {requests: 64Mi, limits: 128Mi}`, `PodDisruptionBudget` `minAvailable: 1`.
- **Data Plane Reference Sizing**: `cpu: {requests: 1000m, limits: 4000m}`, `memory: {requests: 1Gi, limits: 2Gi}`.
- **Hardening & Storage**: `runAsNonRoot: true`, `runAsUser: 65532`, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: ["ALL"]`, `seccompProfile: RuntimeDefault`, `/tmp` `emptyDir` (`16Mi`), configurable `podAntiAffinity` (`soft` default / `hard` toggle).
- **Networking & Persistence Env**: `HTTP_PORT=8080`, `GRPC_PORT=9090`, `OPENAPI_SPEC_PATH=openapi/openapi.yaml`, `CONN_EXEC_MODE=simple_protocol` (ADR-003 PgBouncer compatibility), `REDIS_INVALIDATION_CHANNEL=channel:prompt:invalidation` (ADR-002), `JWT_ISSUER=rag-prompt-registry-auth`.
- **Probes**: Liveness `GET /healthz` and Readiness `GET /ready` (`initialDelaySeconds: 5`, `periodSeconds: 10`).

### Runtime Environment Variables (`.env.example` & `internal/config/env.go`)
At startup (`cmd/prompt-registry-server/main.go`), `config.LoadDotEnv(".env")` loads `.env` (or the path specified by `ENV_FILE`) without overwriting variables already set in the process environment:

| Variable | Default | Description |
| :--- | :--- | :--- |
| `HTTP_PORT` | `8080` | Control plane HTTP REST, OpenAPI Doc API, probe, and `/metrics` port |
| `GRPC_PORT` | `9090` | Control plane gRPC management server port |
| `OPENAPI_SPEC_PATH` | `openapi/openapi.yaml` | Filesystem path to the OpenAPI 3.1 YAML specification |
| `POSTGRES_DSN` | _empty_ | PostgreSQL 16+ / PgBouncer connection URI |
| `CONN_EXEC_MODE` | `simple_protocol` | `pgx` query execution mode (`simple_protocol`, `exec`, `cache_statement`, `cache_describe`, `describe_exec`; ADR-003 requires `simple_protocol`) |
| `ALLOW_IN_MEMORY_STORE` | `false` | Explicit opt-in for non-durable in-memory storage when `POSTGRES_DSN` is unset |
| `POSTGRES_MAX_CONNS` | `25` | Maximum PostgreSQL pool connections |
| `POSTGRES_MIN_CONNS` | `2` | Minimum PostgreSQL pool connections |
| `POSTGRES_AUTO_MIGRATE` | `true` | Apply embedded SQL migrations on startup |
| `REDIS_CLUSTER_ADDRS` | _empty_ | Primary comma-separated Redis 7.2 Cluster headless seed endpoints (e.g. `rag-prompt-registry-service-redis-{0,1,2}.rag-prompt-registry-service-redis-headless.rag-platform.svc.cluster.local:6379`) |
| `REDIS_ADDRS` | `localhost:6379` | Fallback comma-separated Redis 7.2 Cluster / Standalone addresses for local Docker Compose (`REDIS_CLUSTER_ADDRS` takes precedence) |
| `REDIS_PASSWORD` | _empty_ | Redis authentication password (injected via Kubernetes Secret `rag-prompt-registry-service-secret`) |
| `REDIS_DB` | `0` | Redis database index |
| `REDIS_INVALIDATION_CHANNEL` | `channel:prompt:invalidation` | Redis Pub/Sub invalidation channel (ADR-002) |
| `JWT_HMAC_SECRET` | _dev default_ | HS256 signing/verification secret for control plane and SDK Bearer JWTs |
| `JWT_ISSUER` | `rag-prompt-registry-auth` | Expected `iss` claim for Bearer JWTs |
| `ADMIN_WRITE_RATE_LIMIT_RPS` | `50` | Token-bucket refill rate (requests/sec) for administrative writes |
| `ADMIN_WRITE_RATE_LIMIT_BURST` | `100` | Token-bucket burst capacity for administrative writes |
| `DEFAULT_TENANT_ID` | `a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11` | Default tenant UUID fallback |
| `LOG_LEVEL` | `INFO` | Structured JSON `slog` level (`DEBUG`, `INFO`, `WARN`, `ERROR`) |

### Deployment Environment Synchronization (`.env.prod` -> Helm `values.yaml`)
- **Single Source of Truth**: `.env` is reserved strictly for local development. `.env.prod` (gitignored) is the single source of truth for Helm deployments.
- **Dynamic ConfigMap (`deployments/helm/rag-prompt-registry/templates/configmap.yaml`)**: Dynamically iterates over `.Values.env`, preserves `HTTP_PORT` (`8080`) and `GRPC_PORT` (`9090`) fallbacks, and ignores `secretName` and `nil` values.
- **Rolling Restarts (`deployment.yaml` & `rollout.yaml`)**: Pod templates include `checksum/config` computed over `configmap.yaml` so any ConfigMap update triggers an automatic rolling restart.
- **Sync & Pre-Commit Drift Gate (`scripts/sync_env_to_deployments.py`, `.git/hooks/pre-commit`)**:
  - `python scripts/sync_env_to_deployments.py --sync` updates `deployments/helm/rag-prompt-registry/values.yaml` (`.Values.env`) from `.env.prod` while excluding sensitive keys (`PASSWORD`, `SECRET`, `TOKEN`, `KEY`, `DSN`, `CREDENTIAL`, `PRIVATE`) and preserving `secretName` and all other Helm blocks.
  - `python scripts/sync_env_to_deployments.py --check-only` verifies zero drift (exits `1` on mismatch), enforced locally by `.git/hooks/pre-commit`.
- **CI/CD Secret Injection (`.github/workflows/deploy.yml` & `deployments/SECRET_MANAGEMENT.md`)**:
  - Sensitive credentials (`POSTGRES_DSN`, `JWT_HMAC_SECRET`, `REDIS_PASSWORD`) are injected out-of-band into Kubernetes Secret `rag-prompt-registry-service-secret` from GitHub Environment Secrets (`staging` / `production`) via `--from-env-file` without storing credentials in `values.yaml` or Helm release history, passing `--set env.secretName` and `--set secrets.existingSecret` to Helm.

### Self-Hosted Bitnami Redis 7.2 Cluster (`deployments/helm/redis-cluster/`, `deployments/redis.md`)
- **Topology**: Dedicated 6-node `bitnami/redis-cluster` (v11.x, Redis `7.2.5-debian-12-r0`, 3 primary shards + 3 replicas with `cluster.init: true`) deployed as Helm release `rag-prompt-registry-service-redis` in namespace `rag-platform`.
- **Shared Secret & Persistence**: Consumes `REDIS_PASSWORD` from `rag-prompt-registry-service-secret`, enforces `100m`/`256Mi` requests and `500m`/`1Gi` limits, `5Gi` AOF/RDB persistence, soft pod anti-affinity, and Prometheus metrics exporter on port `9121` (`prometheus.io/scrape: "true"`).
- **Cluster Slot Redirects (`internal/infrastructure/cache/redis/cluster_client.go`)**: RESP client handles `-MOVED` and `-ASK` (`ASKING`) slot redirections across headless StatefulSet pods (`rag-prompt-registry-service-redis-{0,1,2}.rag-prompt-registry-service-redis-headless.rag-platform.svc.cluster.local:6379`).
- **GitOps & Local Dev**: Managed declaratively via `deployments/argocd/redis-cluster-application.yaml`, one-click deployed via `scripts/deploy_redis_cluster.{sh,ps1}`, and mirrored locally via `deployments/docker/docker-compose.redis.yml` (`bitnami/redis:7.2-debian-12`).

### Prometheus Alerting Matrix (`templates/prometheusrule.yaml`)
| Alert Name | PromQL Expression | Window (`for`) | Severity | Trigger Condition |
| :--- | :--- | :--- | :--- | :--- |
| `SDKHighRenderLatency` | `histogram_quantile(0.99, sum(rate(prompt_sdk_render_duration_seconds_bucket[5m])) by (le)) > 0.002` | `5m` | `critical` | SDK in-process P99 render latency exceeds 2ms |
| `PubSubInvalidationFailure` | `rate(prompt_invalidation_events_published_total{status="error"}[5m]) > 0` | `1m` | `critical` | Registry fails to publish invalidation events to Redis Pub/Sub |
| `ElevatedPointerRollbacks` | `increase(prompt_registry_pointer_rollbacks_total[15m]) > 0` | `0m` | `warning` | Semantic tag pointer rollback executed within 15m window |

### GitOps ArgoCD & Argo Rollouts (`deployments/gitops/argocd-application.yaml`)
- **ArgoCD Application**: Automated sync (`prune: true`, `selfHeal: true`) with exponential retry backoff targeting `deployments/helm/rag-prompt-registry`.
- **Canary Cutover (`canary.enabled: true`)**: Renders `Rollout` + `AnalysisTemplate` with progressive traffic steps (`10%` -> 2m pause + analysis -> `50%` -> 5m pause + analysis -> `100%`), gating on `rate(prompt_invalidation_events_published_total{status="error"}[2m]) == 0`, `rate(prompt_registry_mutation_duration_seconds_count{status="error"}[2m]) == 0`, and zero `ElevatedPointerRollbacks` spikes.

---

## 10. Runbook 3: Progressive Canary Rollout & Automated Abort via Argo Rollouts

1. **Enable Canary Mode**: Set `canary.enabled=true` and update `image.tag` in Helm values (`deployments/helm/rag-prompt-registry/values.yaml`) or ArgoCD parameter overrides.
2. **Monitor Rollout Progression**:
   - Inspect live rollout state and canary weights:
     `kubectl argo rollouts get rollout rag-prompt-registry-service -n rag-platform --watch`
   - Verify step progression (`10%` canary weight -> 2m pause & Prometheus analysis -> `50%` canary weight -> 5m pause & analysis -> `100%` promotion).
3. **Evaluate Canary Telemetry**:
   - Confirm Prometheus `AnalysisRun` queries pass with `0` errors on `prompt_invalidation_events_published_total{status="error"}` and `prompt_registry_mutation_duration_seconds_count{status="error"}`, and zero spikes on `prompt_registry_pointer_rollbacks_total`.
   - If any `AnalysisTemplate` metric fails or `SDKHighRenderLatency` / `PubSubInvalidationFailure` fires, Argo Rollouts automatically aborts and reverts traffic to the stable ReplicaSet without human intervention.
4. **Manual Emergency Abort**:
   - To immediately halt a canary and restore 100% traffic to the stable ReplicaSet:
     `kubectl argo rollouts abort rag-prompt-registry-service -n rag-platform`
   - If a prompt template pointer (`@production`) also needs reverting, execute **Runbook 1** (`PUT /v1/templates/{id}/tags/@production`).
