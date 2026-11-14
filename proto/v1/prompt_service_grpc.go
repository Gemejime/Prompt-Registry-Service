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

// JSONCodecName is the gRPC content subtype used for JSON wire transport without overriding the default "proto" codec.
const JSONCodecName = "json"

// init registers the JSON codec under the "json" content subtype.
func init() {
	encoding.RegisterCodec(jsonCodec{})
}

// jsonCodec implements google.golang.org/grpc/encoding.Codec for the "json" content subtype.
type jsonCodec struct{}

// Marshal serializes v into JSON bytes.
func (jsonCodec) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// Unmarshal deserializes JSON data into v.
func (jsonCodec) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// Name returns the codec subtype name ("json").
func (jsonCodec) Name() string {
	return JSONCodecName
}

// DialectTarget enumerates supported model dialect targets.
type DialectTarget int32

const (
	DialectTarget_DIALECT_UNSPECIFIED     DialectTarget = 0
	DialectTarget_DIALECT_ANTHROPIC_XML   DialectTarget = 1
	DialectTarget_DIALECT_LLAMA_SPECIAL   DialectTarget = 2
	DialectTarget_DIALECT_OPENAI_STANDARD DialectTarget = 3
)

// String returns the canonical proto enum name for DialectTarget.
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

// MessageBlock represents a role-tagged message pattern within a TemplateAST.
type MessageBlock struct {
	Role           string `json:"role,omitempty"`
	ContentPattern string `json:"content_pattern,omitempty"`
}

// TemplateAST represents the structured abstract syntax tree of a prompt template.
type TemplateAST struct {
	SystemPrompt     string            `json:"system_prompt,omitempty"`
	Messages         []*MessageBlock   `json:"messages,omitempty"`
	VariableKeys     []string          `json:"variable_keys,omitempty"`
	DefaultDialect   DialectTarget     `json:"default_dialect,omitempty"`
	PersonaOverrides map[string]string `json:"persona_overrides,omitempty"`
	FewShotExemplars []*MessageBlock   `json:"few_shot_exemplars,omitempty"`
}

// CreateTemplateRequest is the request message for CreateTemplate.
type CreateTemplateRequest struct {
	TenantId    string       `json:"tenant_id,omitempty"`
	Name        string       `json:"name,omitempty"`
	RawAst      *TemplateAST `json:"raw_ast,omitempty"`
	SchemaId    string       `json:"schema_id,omitempty"`
	Author      string       `json:"author,omitempty"`
	Description string       `json:"description,omitempty"`
}

// CreateTemplateResponse is the response message for CreateTemplate.
type CreateTemplateResponse struct {
	TemplateId     string `json:"template_id,omitempty"`
	InitialVersion uint32 `json:"initial_version,omitempty"`
}

// PublishVersionRequest is the request message for PublishVersion.
type PublishVersionRequest struct {
	TemplateId string       `json:"template_id,omitempty"`
	RawAst     *TemplateAST `json:"raw_ast,omitempty"`
	SchemaId   string       `json:"schema_id,omitempty"`
	Author     string       `json:"author,omitempty"`
	TenantId   string       `json:"tenant_id,omitempty"`
}

// PublishVersionResponse is the response message for PublishVersion.
type PublishVersionResponse struct {
	TemplateId string `json:"template_id,omitempty"`
	Version    uint32 `json:"version,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// PromoteTagRequest is the request message for PromoteTag.
type PromoteTagRequest struct {
	TemplateId      string `json:"template_id,omitempty"`
	Tag             string `json:"tag,omitempty"`
	TargetVersion   uint32 `json:"target_version,omitempty"`
	UpdatedBy       string `json:"updated_by,omitempty"`
	ExpectedVersion uint32 `json:"expected_version,omitempty"`
	TenantId        string `json:"tenant_id,omitempty"`
}

// PromoteTagResponse is the response message for PromoteTag.
type PromoteTagResponse struct {
	TemplateId       string `json:"template_id,omitempty"`
	Tag              string `json:"tag,omitempty"`
	TargetVersion    uint32 `json:"target_version,omitempty"`
	BroadcastEmitted bool   `json:"broadcast_emitted,omitempty"`
}

// ResolveTagRequest is the request message for ResolveTag.
type ResolveTagRequest struct {
	TemplateId string `json:"template_id,omitempty"`
	Tag        string `json:"tag,omitempty"`
	TenantId   string `json:"tenant_id,omitempty"`
}

// ResolveTagResponse is the response message for ResolveTag.
type ResolveTagResponse struct {
	TemplateId string       `json:"template_id,omitempty"`
	Tag        string       `json:"tag,omitempty"`
	Version    uint32       `json:"version,omitempty"`
	Ast        *TemplateAST `json:"ast,omitempty"`
	SchemaId   string       `json:"schema_id,omitempty"`
}

// GetTemplateVersionRequest is the request message for GetTemplateVersion.
type GetTemplateVersionRequest struct {
	TemplateId string `json:"template_id,omitempty"`
	Version    uint32 `json:"version,omitempty"`
	TenantId   string `json:"tenant_id,omitempty"`
}

// GetTemplateVersionResponse is the response message for GetTemplateVersion.
type GetTemplateVersionResponse struct {
	TemplateId string       `json:"template_id,omitempty"`
	Version    uint32       `json:"version,omitempty"`
	Ast        *TemplateAST `json:"ast,omitempty"`
	SchemaId   string       `json:"schema_id,omitempty"`
	Author     string       `json:"author,omitempty"`
	CreatedAt  string       `json:"created_at,omitempty"`
}

// RegisterSchemaRequest is the request message for RegisterSchema.
type RegisterSchemaRequest struct {
	TenantId   string `json:"tenant_id,omitempty"`
	Name       string `json:"name,omitempty"`
	JsonSchema string `json:"json_schema,omitempty"`
}

// RegisterSchemaResponse is the response message for RegisterSchema.
type RegisterSchemaResponse struct {
	SchemaId string `json:"schema_id,omitempty"`
}

// GetSchemaRequest is the request message for GetSchema.
type GetSchemaRequest struct {
	SchemaId string `json:"schema_id,omitempty"`
	TenantId string `json:"tenant_id,omitempty"`
}

// GetSchemaResponse is the response message for GetSchema.
type GetSchemaResponse struct {
	SchemaId   string `json:"schema_id,omitempty"`
	Name       string `json:"name,omitempty"`
	JsonSchema string `json:"json_schema,omitempty"`
	TenantId   string `json:"tenant_id,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// ListTemplatesRequest is the request message for ListTemplates.
type ListTemplatesRequest struct {
	TenantId string `json:"tenant_id,omitempty"`
	Limit    uint32 `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

// TemplateSummary represents a prompt template aggregate root in list responses.
type TemplateSummary struct {
	Id             string `json:"id,omitempty"`
	TenantId       string `json:"tenant_id,omitempty"`
	Name           string `json:"name,omitempty"`
	Description    string `json:"description,omitempty"`
	CurrentVersion uint32 `json:"current_version,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

// ListTemplatesResponse is the response message for ListTemplates.
type ListTemplatesResponse struct {
	Templates  []*TemplateSummary `json:"templates,omitempty"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// ListSchemasRequest is the request message for ListSchemas.
type ListSchemasRequest struct {
	TenantId string `json:"tenant_id,omitempty"`
	Limit    uint32 `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}

// ListSchemasResponse is the response message for ListSchemas.
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

// promptRegistryServiceClient implements PromptRegistryServiceClient over a grpc.ClientConnInterface.
type promptRegistryServiceClient struct {
	cc grpc.ClientConnInterface
}

// withJSONSubtype prepends the "json" content subtype option for RPC invocations.
func withJSONSubtype(opts []grpc.CallOption) []grpc.CallOption {
	callOpts := make([]grpc.CallOption, 0, len(opts)+1)
	callOpts = append(callOpts, grpc.CallContentSubtype(JSONCodecName))
	callOpts = append(callOpts, opts...)
	return callOpts
}

// NewPromptRegistryServiceClient creates a new PromptRegistryServiceClient bound to cc.
func NewPromptRegistryServiceClient(cc grpc.ClientConnInterface) PromptRegistryServiceClient {
	return &promptRegistryServiceClient{cc}
}

// CreateTemplate invokes the CreateTemplate RPC.
func (c *promptRegistryServiceClient) CreateTemplate(ctx context.Context, in *CreateTemplateRequest, opts ...grpc.CallOption) (*CreateTemplateResponse, error) {
	out := new(CreateTemplateResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/CreateTemplate", in, out, withJSONSubtype(opts)...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PublishVersion invokes the PublishVersion RPC.
func (c *promptRegistryServiceClient) PublishVersion(ctx context.Context, in *PublishVersionRequest, opts ...grpc.CallOption) (*PublishVersionResponse, error) {
	out := new(PublishVersionResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/PublishVersion", in, out, withJSONSubtype(opts)...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetTemplateVersion invokes the GetTemplateVersion RPC.
func (c *promptRegistryServiceClient) GetTemplateVersion(ctx context.Context, in *GetTemplateVersionRequest, opts ...grpc.CallOption) (*GetTemplateVersionResponse, error) {
	out := new(GetTemplateVersionResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/GetTemplateVersion", in, out, withJSONSubtype(opts)...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PromoteTag invokes the PromoteTag RPC.
func (c *promptRegistryServiceClient) PromoteTag(ctx context.Context, in *PromoteTagRequest, opts ...grpc.CallOption) (*PromoteTagResponse, error) {
	out := new(PromoteTagResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/PromoteTag", in, out, withJSONSubtype(opts)...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveTag invokes the ResolveTag RPC.
func (c *promptRegistryServiceClient) ResolveTag(ctx context.Context, in *ResolveTagRequest, opts ...grpc.CallOption) (*ResolveTagResponse, error) {
	out := new(ResolveTagResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/ResolveTag", in, out, withJSONSubtype(opts)...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RegisterSchema invokes the RegisterSchema RPC.
func (c *promptRegistryServiceClient) RegisterSchema(ctx context.Context, in *RegisterSchemaRequest, opts ...grpc.CallOption) (*RegisterSchemaResponse, error) {
	out := new(RegisterSchemaResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/RegisterSchema", in, out, withJSONSubtype(opts)...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetSchema invokes the GetSchema RPC.
func (c *promptRegistryServiceClient) GetSchema(ctx context.Context, in *GetSchemaRequest, opts ...grpc.CallOption) (*GetSchemaResponse, error) {
	out := new(GetSchemaResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/GetSchema", in, out, withJSONSubtype(opts)...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListTemplates invokes the ListTemplates RPC.
func (c *promptRegistryServiceClient) ListTemplates(ctx context.Context, in *ListTemplatesRequest, opts ...grpc.CallOption) (*ListTemplatesResponse, error) {
	out := new(ListTemplatesResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/ListTemplates", in, out, withJSONSubtype(opts)...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListSchemas invokes the ListSchemas RPC.
func (c *promptRegistryServiceClient) ListSchemas(ctx context.Context, in *ListSchemasRequest, opts ...grpc.CallOption) (*ListSchemasResponse, error) {
	out := new(ListSchemasResponse)
	err := c.cc.Invoke(ctx, "/rag.prompt.v1.PromptRegistryService/ListSchemas", in, out, withJSONSubtype(opts)...)
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

// CreateTemplate returns codes.Unimplemented for CreateTemplate.
func (UnimplementedPromptRegistryServiceServer) CreateTemplate(context.Context, *CreateTemplateRequest) (*CreateTemplateResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method CreateTemplate not implemented")
}

// PublishVersion returns codes.Unimplemented for PublishVersion.
func (UnimplementedPromptRegistryServiceServer) PublishVersion(context.Context, *PublishVersionRequest) (*PublishVersionResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method PublishVersion not implemented")
}

// GetTemplateVersion returns codes.Unimplemented for GetTemplateVersion.
func (UnimplementedPromptRegistryServiceServer) GetTemplateVersion(context.Context, *GetTemplateVersionRequest) (*GetTemplateVersionResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetTemplateVersion not implemented")
}

// PromoteTag returns codes.Unimplemented for PromoteTag.
func (UnimplementedPromptRegistryServiceServer) PromoteTag(context.Context, *PromoteTagRequest) (*PromoteTagResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method PromoteTag not implemented")
}

// ResolveTag returns codes.Unimplemented for ResolveTag.
func (UnimplementedPromptRegistryServiceServer) ResolveTag(context.Context, *ResolveTagRequest) (*ResolveTagResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ResolveTag not implemented")
}

// RegisterSchema returns codes.Unimplemented for RegisterSchema.
func (UnimplementedPromptRegistryServiceServer) RegisterSchema(context.Context, *RegisterSchemaRequest) (*RegisterSchemaResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method RegisterSchema not implemented")
}

// GetSchema returns codes.Unimplemented for GetSchema.
func (UnimplementedPromptRegistryServiceServer) GetSchema(context.Context, *GetSchemaRequest) (*GetSchemaResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetSchema not implemented")
}

// ListTemplates returns codes.Unimplemented for ListTemplates.
func (UnimplementedPromptRegistryServiceServer) ListTemplates(context.Context, *ListTemplatesRequest) (*ListTemplatesResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListTemplates not implemented")
}

// ListSchemas returns codes.Unimplemented for ListSchemas.
func (UnimplementedPromptRegistryServiceServer) ListSchemas(context.Context, *ListSchemasRequest) (*ListSchemasResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method ListSchemas not implemented")
}

// RegisterPromptRegistryServiceServer registers srv with the provided grpc.ServiceRegistrar.
func RegisterPromptRegistryServiceServer(s grpc.ServiceRegistrar, srv PromptRegistryServiceServer) {
	s.RegisterService(&PromptRegistryService_ServiceDesc, srv)
}

// _PromptRegistryService_CreateTemplate_Handler dispatches incoming CreateTemplate RPCs.
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

// _PromptRegistryService_PublishVersion_Handler dispatches incoming PublishVersion RPCs.
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

// _PromptRegistryService_GetTemplateVersion_Handler dispatches incoming GetTemplateVersion RPCs.
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

// _PromptRegistryService_PromoteTag_Handler dispatches incoming PromoteTag RPCs.
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

// _PromptRegistryService_ResolveTag_Handler dispatches incoming ResolveTag RPCs.
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

// _PromptRegistryService_RegisterSchema_Handler dispatches incoming RegisterSchema RPCs.
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

// _PromptRegistryService_GetSchema_Handler dispatches incoming GetSchema RPCs.
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

// _PromptRegistryService_ListTemplates_Handler dispatches incoming ListTemplates RPCs.
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

// _PromptRegistryService_ListSchemas_Handler dispatches incoming ListSchemas RPCs.
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

// PromptRegistryService_ServiceDesc is the grpc.ServiceDesc for PromptRegistryService.
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
