// Package promptv1 provides Protobuf v3 message definitions and gRPC service bindings
// for rag.prompt.v1.PromptRegistryService.
package promptv1

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
)

func init() {
	encoding.RegisterCodec(jsonCodec{})
}

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (jsonCodec) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func (jsonCodec) Name() string {
	return "proto"
}

// DialectTarget enumerates supported model dialect targets.
type DialectTarget int32

const (
	DialectTarget_DIALECT_UNSPECIFIED     DialectTarget = 0
	DialectTarget_DIALECT_ANTHROPIC_XML   DialectTarget = 1
	DialectTarget_DIALECT_LLAMA_SPECIAL   DialectTarget = 2
	DialectTarget_DIALECT_OPENAI_STANDARD DialectTarget = 3
)

func (d DialectTarget) String() string {
	switch d {
	case DialectTarget_DIALECT_ANTHROPIC_XML:
		return "DIALECT_ANTHROPIC_XML"
	case DialectTarget_DIALECT_LLAMA_SPECIAL:
		return "DIALECT_LLAMA_SPECIAL"
	case DialectTarget_DIALECT_OPENAI_STANDARD:
		return "DIALECT_OPENAI_STANDARD"
	default:
		return "DIALECT_UNSPECIFIED"
	}
}

// ParseDialectTarget converts a string representation to DialectTarget.
func ParseDialectTarget(s string) DialectTarget {
	switch s {
	case "DIALECT_ANTHROPIC_XML":
		return DialectTarget_DIALECT_ANTHROPIC_XML
	case "DIALECT_LLAMA_SPECIAL":
		return DialectTarget_DIALECT_LLAMA_SPECIAL
	case "DIALECT_OPENAI_STANDARD":
		return DialectTarget_DIALECT_OPENAI_STANDARD
	default:
		return DialectTarget_DIALECT_UNSPECIFIED
	}
}

type MessageBlock struct {
	Role           string `json:"role,omitempty"`
	ContentPattern string `json:"content_pattern,omitempty"`
}

type TemplateAST struct {
	SystemPrompt     string            `json:"system_prompt,omitempty"`
	Messages         []*MessageBlock   `json:"messages,omitempty"`
	VariableKeys     []string          `json:"variable_keys,omitempty"`
	DefaultDialect   DialectTarget     `json:"default_dialect,omitempty"`
	PersonaOverrides map[string]string `json:"persona_overrides,omitempty"`
	FewShotExemplars []*MessageBlock   `json:"few_shot_exemplars,omitempty"`
}

type CreateTemplateRequest struct {
	TenantId    string       `json:"tenant_id,omitempty"`
	Name        string       `json:"name,omitempty"`
	RawAst      *TemplateAST `json:"raw_ast,omitempty"`
	SchemaId    string       `json:"schema_id,omitempty"`
	Author      string       `json:"author,omitempty"`
	Description string       `json:"description,omitempty"`
}

type CreateTemplateResponse struct {
	TemplateId     string `json:"template_id,omitempty"`
	InitialVersion uint32 `json:"initial_version,omitempty"`
}

type PublishVersionRequest struct {
	TemplateId string       `json:"template_id,omitempty"`
	RawAst     *TemplateAST `json:"raw_ast,omitempty"`
	SchemaId   string       `json:"schema_id,omitempty"`
	Author     string       `json:"author,omitempty"`
	TenantId   string       `json:"tenant_id,omitempty"`
}

type PublishVersionResponse struct {
	TemplateId string `json:"template_id,omitempty"`
	Version    uint32 `json:"version,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

type PromoteTagRequest struct {
	TemplateId      string `json:"template_id,omitempty"`
	Tag             string `json:"tag,omitempty"`
	TargetVersion   uint32 `json:"target_version,omitempty"`
	UpdatedBy       string `json:"updated_by,omitempty"`
	ExpectedVersion uint32 `json:"expected_version,omitempty"`
	TenantId        string `json:"tenant_id,omitempty"`
}

type PromoteTagResponse struct {
	TemplateId       string `json:"template_id,omitempty"`
	Tag              string `json:"tag,omitempty"`
	TargetVersion    uint32 `json:"target_version,omitempty"`
	BroadcastEmitted bool   `json:"broadcast_emitted,omitempty"`
}

type ResolveTagRequest struct {
	TemplateId string `json:"template_id,omitempty"`
	Tag        string `json:"tag,omitempty"`
	TenantId   string `json:"tenant_id,omitempty"`
}

type ResolveTagResponse struct {
	TemplateId string       `json:"template_id,omitempty"`
	Tag        string       `json:"tag,omitempty"`
	Version    uint32       `json:"version,omitempty"`
	Ast        *TemplateAST `json:"ast,omitempty"`
}

type GetTemplateVersionRequest struct {
	TemplateId string `json:"template_id,omitempty"`
	Version    uint32 `json:"version,omitempty"`
	TenantId   string `json:"tenant_id,omitempty"`
}

type GetTemplateVersionResponse struct {
	TemplateId string       `json:"template_id,omitempty"`
	Version    uint32       `json:"version,omitempty"`
	Ast        *TemplateAST `json:"ast,omitempty"`
	SchemaId   string       `json:"schema_id,omitempty"`
	Author     string       `json:"author,omitempty"`
	CreatedAt  string       `json:"created_at,omitempty"`
}

type RegisterSchemaRequest struct {
	TenantId   string `json:"tenant_id,omitempty"`
	Name       string `json:"name,omitempty"`
	JsonSchema string `json:"json_schema,omitempty"`
}

type RegisterSchemaResponse struct {
	SchemaId string `json:"schema_id,omitempty"`
}

type GetSchemaRequest struct {
	SchemaId string `json:"schema_id,omitempty"`
	TenantId string `json:"tenant_id,omitempty"`
}

type GetSchemaResponse struct {
	SchemaId   string `json:"schema_id,omitempty"`
	Name       string `json:"name,omitempty"`
	JsonSchema string `json:"json_schema,omitempty"`
	TenantId   string `json:"tenant_id,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

type ListTemplatesRequest struct {
	TenantId string `json:"tenant_id,omitempty"`
	Limit    uint32 `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

type TemplateSummary struct {
	Id             string `json:"id,omitempty"`
	TenantId       string `json:"tenant_id,omitempty"`
	Name           string `json:"name,omitempty"`
	Description    string `json:"description,omitempty"`
	CurrentVersion uint32 `json:"current_version,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

type ListTemplatesResponse struct {
	Templates  []*TemplateSummary `json:"templates,omitempty"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type ListSchemasRequest struct {
	TenantId string `json:"tenant_id,omitempty"`
	Limit    uint32 `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

type ListSchemasResponse struct {
	Schemas    []*GetSchemaResponse `json:"schemas,omitempty"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// PromptRegistryServiceClient is the client API for PromptRegistryService service.
type PromptRegistryServiceClient interface {
	CreateTemplate(ctx context.Context, in *CreateTemplateRequest, opts ...grpc.CallOption) (*CreateTemplateResponse, error)
	PublishVersion(ctx context.Context, in *PublishVersionRequest, opts ...grpc.CallOption) (*PublishVersionResponse, error)
	GetTemplateVersion(ctx context.Context, in *GetTemplateVersionRequest, opts ...grpc.CallOption) (*GetTemplateVersionResponse, error)
	PromoteTag(ctx context.Context, in *PromoteTagRequest, opts ...grpc.CallOption) (*PromoteTagResponse, error)
	ResolveTag(ctx context.Context, in *ResolveTagRequest, opts ...grpc.CallOption) (*ResolveTagResponse, error)
	RegisterSchema(ctx context.Context, in *RegisterSchemaRequest, opts ...grpc.CallOption) (*RegisterSchemaResponse, error)
	GetSchema(ctx context.Context, in *GetSchemaRequest, opts ...grpc.CallOption) (*GetSchemaResponse, error)
	ListTemplates(ctx context.Context, in *ListTemplatesRequest, opts ...grpc.CallOption) (*ListTemplatesResponse, error)
	ListSchemas(ctx context.Context, in *ListSchemasRequest, opts ...grpc.CallOption) (*ListSchemasResponse, error)
}

type promptRegistryServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewPromptRegistryServiceClient(cc grpc.ClientConnInterface) PromptRegistryServiceClient {
	return &promptRegistryServiceClient{cc}
}

func (c *promptRegistryServiceClient) CreateTemplate(ctx context.Context, in *CreateTemplateRequest, opts ...grpc.CallOption) (*CreateTemplateResponse, error) {
	out := new(CreateTemplateResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/CreateTemplate", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *promptRegistryServiceClient) PublishVersion(ctx context.Context, in *PublishVersionRequest, opts ...grpc.CallOption) (*PublishVersionResponse, error) {
	out := new(PublishVersionResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/PublishVersion", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *promptRegistryServiceClient) GetTemplateVersion(ctx context.Context, in *GetTemplateVersionRequest, opts ...grpc.CallOption) (*GetTemplateVersionResponse, error) {
	out := new(GetTemplateVersionResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/GetTemplateVersion", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *promptRegistryServiceClient) PromoteTag(ctx context.Context, in *PromoteTagRequest, opts ...grpc.CallOption) (*PromoteTagResponse, error) {
	out := new(PromoteTagResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/PromoteTag", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *promptRegistryServiceClient) ResolveTag(ctx context.Context, in *ResolveTagRequest, opts ...grpc.CallOption) (*ResolveTagResponse, error) {
	out := new(ResolveTagResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/ResolveTag", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *promptRegistryServiceClient) RegisterSchema(ctx context.Context, in *RegisterSchemaRequest, opts ...grpc.CallOption) (*RegisterSchemaResponse, error) {
	out := new(RegisterSchemaResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/RegisterSchema", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *promptRegistryServiceClient) GetSchema(ctx context.Context, in *GetSchemaRequest, opts ...grpc.CallOption) (*GetSchemaResponse, error) {
	out := new(GetSchemaResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/GetSchema", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *promptRegistryServiceClient) ListTemplates(ctx context.Context, in *ListTemplatesRequest, opts ...grpc.CallOption) (*ListTemplatesResponse, error) {
	out := new(ListTemplatesResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/ListTemplates", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *promptRegistryServiceClient) ListSchemas(ctx context.Context, in *ListSchemasRequest, opts ...grpc.CallOption) (*ListSchemasResponse, error) {
	out := new(ListSchemasResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/ListSchemas", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PromptRegistryServiceServer is the server API for PromptRegistryService service.
type PromptRegistryServiceServer interface {
	CreateTemplate(context.Context, *CreateTemplateRequest) (*CreateTemplateResponse, error)
	PublishVersion(context.Context, *PublishVersionRequest) (*PublishVersionResponse, error)
	GetTemplateVersion(context.Context, *GetTemplateVersionRequest) (*GetTemplateVersionResponse, error)
	PromoteTag(context.Context, *PromoteTagRequest) (*PromoteTagResponse, error)
	ResolveTag(context.Context, *ResolveTagRequest) (*ResolveTagResponse, error)
	RegisterSchema(context.Context, *RegisterSchemaRequest) (*RegisterSchemaResponse, error)
	GetSchema(context.Context, *GetSchemaRequest) (*GetSchemaResponse, error)
	ListTemplates(context.Context, *ListTemplatesRequest) (*ListTemplatesResponse, error)
	ListSchemas(context.Context, *ListSchemasRequest) (*ListSchemasResponse, error)
}

// UnimplementedPromptRegistryServiceServer must be embedded to have forward compatible implementations.
type UnimplementedPromptRegistryServiceServer struct{}

func (UnimplementedPromptRegistryServiceServer) CreateTemplate(context.Context, *CreateTemplateRequest) (*CreateTemplateResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method CreateTemplate not implemented")
}
func (UnimplementedPromptRegistryServiceServer) PublishVersion(context.Context, *PublishVersionRequest) (*PublishVersionResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method PublishVersion not implemented")
}
func (UnimplementedPromptRegistryServiceServer) GetTemplateVersion(context.Context, *GetTemplateVersionRequest) (*GetTemplateVersionResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetTemplateVersion not implemented")
}
func (UnimplementedPromptRegistryServiceServer) PromoteTag(context.Context, *PromoteTagRequest) (*PromoteTagResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method PromoteTag not implemented")
}
func (UnimplementedPromptRegistryServiceServer) ResolveTag(context.Context, *ResolveTagRequest) (*ResolveTagResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ResolveTag not implemented")
}
func (UnimplementedPromptRegistryServiceServer) RegisterSchema(context.Context, *RegisterSchemaRequest) (*RegisterSchemaResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method RegisterSchema not implemented")
}
func (UnimplementedPromptRegistryServiceServer) GetSchema(context.Context, *GetSchemaRequest) (*GetSchemaResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetSchema not implemented")
}
func (UnimplementedPromptRegistryServiceServer) ListTemplates(context.Context, *ListTemplatesRequest) (*ListTemplatesResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListTemplates not implemented")
}
func (UnimplementedPromptRegistryServiceServer) ListSchemas(context.Context, *ListSchemasRequest) (*ListSchemasResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListSchemas not implemented")
}

func RegisterPromptRegistryServiceServer(s grpc.ServiceRegistrar, srv PromptRegistryServiceServer) {
	s.RegisterService(&PromptRegistryService_ServiceDesc, srv)
}

func _PromptRegistryService_CreateTemplate_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(CreateTemplateRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).CreateTemplate(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/CreateTemplate",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).CreateTemplate(ctx, req.(*CreateTemplateRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _PromptRegistryService_PublishVersion_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(PublishVersionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).PublishVersion(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/PublishVersion",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).PublishVersion(ctx, req.(*PublishVersionRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _PromptRegistryService_GetTemplateVersion_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(GetTemplateVersionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).GetTemplateVersion(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/GetTemplateVersion",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).GetTemplateVersion(ctx, req.(*GetTemplateVersionRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _PromptRegistryService_PromoteTag_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(PromoteTagRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).PromoteTag(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/PromoteTag",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).PromoteTag(ctx, req.(*PromoteTagRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _PromptRegistryService_ResolveTag_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ResolveTagRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).ResolveTag(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/ResolveTag",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).ResolveTag(ctx, req.(*ResolveTagRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _PromptRegistryService_RegisterSchema_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(RegisterSchemaRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).RegisterSchema(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/RegisterSchema",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).RegisterSchema(ctx, req.(*RegisterSchemaRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _PromptRegistryService_GetSchema_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(GetSchemaRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).GetSchema(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/GetSchema",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).GetSchema(ctx, req.(*GetSchemaRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _PromptRegistryService_ListTemplates_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ListTemplatesRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).ListTemplates(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/ListTemplates",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).ListTemplates(ctx, req.(*ListTemplatesRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _PromptRegistryService_ListSchemas_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ListSchemasRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(PromptRegistryServiceServer).ListSchemas(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/rag.prompt.v1.PromptRegistryService/ListSchemas",
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(PromptRegistryServiceServer).ListSchemas(ctx, req.(*ListSchemasRequest))
	}
	return interceptor(ctx, in, info, handler)
}

var PromptRegistryService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "rag.prompt.v1.PromptRegistryService",
	HandlerType: (*PromptRegistryServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "CreateTemplate",
			Handler:    _PromptRegistryService_CreateTemplate_Handler,
		},
		{
			MethodName: "PublishVersion",
			Handler:    _PromptRegistryService_PublishVersion_Handler,
		},
		{
			MethodName: "GetTemplateVersion",
			Handler:    _PromptRegistryService_GetTemplateVersion_Handler,
		},
		{
			MethodName: "PromoteTag",
			Handler:    _PromptRegistryService_PromoteTag_Handler,
		},
		{
			MethodName: "ResolveTag",
			Handler:    _PromptRegistryService_ResolveTag_Handler,
		},
		{
			MethodName: "RegisterSchema",
			Handler:    _PromptRegistryService_RegisterSchema_Handler,
		},
		{
			MethodName: "GetSchema",
			Handler:    _PromptRegistryService_GetSchema_Handler,
		},
		{
			MethodName: "ListTemplates",
			Handler:    _PromptRegistryService_ListTemplates_Handler,
		},
		{
			MethodName: "ListSchemas",
			Handler:    _PromptRegistryService_ListSchemas_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "proto/v1/prompt_service.proto",
}
