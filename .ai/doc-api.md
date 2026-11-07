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
| `/ready` | `GET` | `application/json` | Readiness probe verifying PostgreSQL pool and Redis cluster |
| `/metrics` | `GET` | `text/plain` | Prometheus metrics scrape endpoint |

---

## 2. Administrative REST API Endpoints

| Endpoint | Method | Request Payload | Response Codes | Description |
| :--- | :--- | :--- | :--- | :--- |
| `/v1/templates` | `POST` | `CreateTemplateRequest` | `201 Created`, `400 Bad Request` | Initialize template aggregate root and publish initial version $v_1$ |
| `/v1/templates` | `GET` | Cursor query parameters | `200 OK` | Paginated listing of registered prompt templates |
| `/v1/templates/{id}/versions` | `POST` | `PublishVersionRequest` | `201 Created`, `409 Conflict` | Create next immutable version $v_{n+1}$ with AST and schema contract |
| `/v1/templates/{id}/versions/{version}` | `GET` | None | `200 OK`, `404 Not Found` | Retrieve immutable canonical AST representation |
| `/v1/templates/{id}/tags/{tag}` | `PUT` | `PromoteTagRequest` | `200 OK`, `404 Not Found` | Atomically mutate semantic tag pointer (`@production`) & emit Redis Pub/Sub |
| `/v1/templates/{id}/tags/{tag}` | `GET` | None | `200 OK`, `404 Not Found` | Resolve current target version assigned to a tag pointer |
| `/v1/schemas` | `POST` | `RegisterSchemaRequest` | `201 Created`, `400 Bad Request` | Register or update JSON output validation schema contract |
| `/v1/schemas/{id}` | `GET` | None | `200 OK`, `404 Not Found` | Fetch registered JSON output schema definition |

---

## 3. OpenAPI 3.1 Specification Contract (`openapi/openapi.yaml`)

```yaml
openapi: 3.1.0
info:
  title: RAG Prompt Registry Service API
  description: Control Plane Administration, AST Template Lineage, and Output Schema Registry
  version: 1.0.0
servers:
  - url: http://localhost:8080
    description: Local Control Plane Administration Server
paths:
  /healthz:
    get:
      summary: Liveness Probe
      operationId: getLiveness
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
      operationId: getReadiness
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
  /v1/templates:
    post:
      summary: Create Prompt Template
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
    get:
      summary: List Prompt Templates
      operationId: listTemplates
      parameters:
        - name: limit
          in: query
          schema:
            type: integer
            default: 20
        - name: cursor
          in: query
          schema:
            type: string
      responses:
        '200':
          description: List of templates
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ListTemplatesResponse'
  /v1/templates/{id}/versions:
    post:
      summary: Publish Immutable Version
      operationId: publishVersion
      parameters:
        - name: id
          in: path
          required: true
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
  /v1/templates/{id}/tags/{tag}:
    put:
      summary: Promote Semantic Tag Pointer
      operationId: promoteTag
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
        - name: tag
          in: path
          required: true
          schema:
            type: string
            example: "@production"
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                version:
                  type: integer
                  example: 2
              required: [version]
      responses:
        '200':
          description: Tag updated and invalidation broadcast
components:
  schemas:
    CreateTemplateRequest:
      type: object
      required: [tenant_id, name, raw_ast, author]
      properties:
        tenant_id:
          type: string
          format: uuid
        name:
          type: string
          example: "rag-financial-summary"
        raw_ast:
          type: object
          description: Canonical template AST
        author:
          type: string
          example: "system-admin"
    PublishVersionRequest:
      type: object
      required: [raw_ast, author]
      properties:
        raw_ast:
          type: object
        schema_id:
          type: string
          format: uuid
        author:
          type: string
    TemplateResponse:
      type: object
      properties:
        id:
          type: string
        name:
          type: string
        current_version:
          type: integer
    VersionResponse:
      type: object
      properties:
        template_id:
          type: string
        version:
          type: integer
        created_at:
          type: string
          format: date-time
    ListTemplatesResponse:
      type: object
      properties:
        templates:
          type: array
          items:
            $ref: '#/components/schemas/TemplateResponse'
        next_cursor:
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
}

message CreateTemplateRequest {
  string tenant_id = 1;
  string name = 2;
  TemplateAST raw_ast = 3;
  string author = 4;
}

message CreateTemplateResponse {
  string template_id = 1;
  uint32 initial_version = 2;
}

message PublishVersionRequest {
  string template_id = 1;
  TemplateAST raw_ast = 2;
  string schema_id = 3;
  string author = 4;
}

message PublishVersionResponse {
  string template_id = 1;
  uint32 version = 2;
  string created_at = 3;
}

message PromoteTagRequest {
  string template_id = 1;
  string tag = 2; // e.g. "@production", "@staging", "@canary"
  uint32 target_version = 3;
  string updated_by = 4;
}

message PromoteTagResponse {
  string template_id = 1;
  string tag = 2;
  uint32 target_version = 3;
  bool broadcast_emitted = 4;
}

message ResolveTagRequest {
  string template_id = 1;
  string tag = 2;
}

message ResolveTagResponse {
  string template_id = 1;
  string tag = 2;
  uint32 version = 3;
  TemplateAST ast = 4;
}

message GetTemplateVersionRequest {
  string template_id = 1;
  uint32 version = 2;
}

message GetTemplateVersionResponse {
  string template_id = 1;
  uint32 version = 2;
  TemplateAST ast = 3;
  string schema_id = 4;
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
}

message GetSchemaResponse {
  string schema_id = 1;
  string name = 2;
  string json_schema = 3;
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
}

message MessageBlock {
  string role = 1; // "system", "user", "assistant"
  string content_pattern = 2; // e.g. "Context:\n<context>{{context}}</context>\nQuery: {{query}}"
}
```

---

## 5. Relational Persistence Schema (PostgreSQL 16+)

```sql
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Aggregates
CREATE TABLE prompt_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_name UNIQUE (tenant_id, name)
);

-- Append-only immutable versions
CREATE TABLE prompt_versions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES prompt_templates(id) ON DELETE RESTRICT,
    version INT NOT NULL,
    raw_ast JSONB NOT NULL,
    schema_id UUID,
    author VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_template_version UNIQUE (template_id, version)
);

-- Mutable tag pointers (@production, @staging, @canary)
CREATE TABLE prompt_tags (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES prompt_templates(id) ON DELETE CASCADE,
    tag VARCHAR(64) NOT NULL, -- e.g. "@production"
    target_version INT NOT NULL,
    updated_by VARCHAR(255) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_template_tag UNIQUE (template_id, tag)
);

-- Transactional compliance ledger (ADR-004: Direct write, no external CDC)
CREATE TABLE prompt_audit_ledger (
    id BIGSERIAL PRIMARY KEY,
    template_id UUID NOT NULL,
    action VARCHAR(64) NOT NULL, -- "VERSION_PUBLISHED", "TAG_PROMOTED"
    version INT,
    tag VARCHAR(64),
    performed_by VARCHAR(255) NOT NULL,
    payload_snapshot JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
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

CREATE INDEX idx_prompt_versions_lookup ON prompt_versions(template_id, version);
CREATE INDEX idx_prompt_tags_lookup ON prompt_tags(template_id, tag);
CREATE INDEX idx_prompt_audit_template ON prompt_audit_ledger(template_id, created_at);
```

---

## 6. Redis Pub/Sub Invalidation Contract

- **Channel**: `channel:prompt:invalidation`
- **Payload Schema (JSON)**:
```json
{
  "event_id": "01HXYZ1234567890ABCDEF",
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
    DialectAnthropicXML DialectType = "anthropic_xml"
    DialectLlamaSpecial DialectType = "llama_special"
    DialectStandardJSON DialectType = "standard_json"
)

type RenderOptions struct {
    TargetDialect DialectType
    Variables     map[string]string
    MaxTokens     int
}

type PromptSDK interface {
    // Render compiles template AST and binds context variables using sync.Pool
    // Zero allocations on hot path; P99 <= 0.5ms.
    Render(ctx context.Context, templateID string, tagOrVersion string, opts RenderOptions) ([]byte, error)
    
    // Invalidate explicitly forces cache eviction
    Invalidate(templateID string, tag string)
    
    // Close cleanly terminates Redis Pub/Sub listeners
    Close() error
}
```

---

## 8. Prometheus Observability Metrics

| Metric Name | Type | Labels | Description |
| :--- | :--- | :--- | :--- |
| `prompt_sdk_render_duration_seconds` | Histogram | `dialect`, `status` | SDK in-process compilation latency (Target P99 $\le 0.5$ms) |
| `prompt_registry_mutation_duration_seconds` | Histogram | `operation`, `status` | Control plane write latencies |
| `prompt_sdk_cache_hit_ratio` | Gauge | `tier` (L1_version, L1_tag) | Local SDK memory hit ratio (Target $>99.9\%$) |
| `prompt_invalidation_events_published_total` | Counter | `status` | Redis Pub/Sub eviction signals emitted |
| `prompt_sdk_pubsub_connected` | Gauge | `node_id` | Orchestrator Pub/Sub connection status (1=healthy, 0=fallback TTL) |
