// Package grpc implements the low-volume Control Plane gRPC management server.
package grpc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
	promptv1 "rag-prompt-registry-service/proto/v1"
)

// MaxPayloadBytes enforces a 1 MiB ceiling on administrative gRPC messages to protect the <=128 MiB RSS footprint.
const MaxPayloadBytes = 1 << 20

// DefaultJWTSecret and DefaultJWTIssuer provide the default HMAC-SHA256 signing parameters when not overridden via environment.
const (
	DefaultJWTSecret = "rag-prompt-registry-default-jwt-hmac-secret"
	DefaultJWTIssuer = "rag-prompt-registry-auth"
)

// grpcTenantKeyType is the context key type for storing the verified caller tenant ID.
type grpcTenantKeyType struct{}

var grpcTenantContextKey = grpcTenantKeyType{}

// grpcTenantMismatchKeyType is the context key type for recording metadata tenant mismatch errors.
type grpcTenantMismatchKeyType struct{}

var grpcTenantMismatchContextKey = grpcTenantMismatchKeyType{}

// IssueHS256JWT signs a bearer JWT token with HMAC-SHA256 for the given tenant, issuer, secret, and TTL.
func IssueHS256JWT(tenantID, issuer, secret string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(secret) == "" {
		secret = DefaultJWTSecret
	}
	if strings.TrimSpace(issuer) == "" {
		issuer = DefaultJWTIssuer
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	headerJSON := `{"alg":"HS256","typ":"JWT"}`
	now := time.Now().UTC().Unix()
	claims := map[string]any{
		"tenant_id": strings.TrimSpace(tenantID),
		"iss":       strings.TrimSpace(issuer),
		"iat":       now,
		"exp":       now + int64(ttl.Seconds()),
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(headerJSON)) + "." +
		base64.RawURLEncoding.EncodeToString(claimsBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(unsigned))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return unsigned + "." + sig, nil
}

// PromptAdminServer implements promptv1.PromptRegistryServiceServer.
type PromptAdminServer struct {
	promptv1.UnimplementedPromptRegistryServiceServer
	publishHandler *command.PublishVersionHandler
	promoteHandler *command.PromoteTagHandler
	getASTHandler  *query.GetTemplateASTHandler
	listHandler    *query.ListTemplatesHandler
	defaultTenant  string
	logger         *slog.Logger
}

// NewPromptAdminServer creates a new PromptAdminServer.
func NewPromptAdminServer(
	publishHandler *command.PublishVersionHandler,
	promoteHandler *command.PromoteTagHandler,
	getASTHandler *query.GetTemplateASTHandler,
	listHandler *query.ListTemplatesHandler,
	defaultTenant string,
	logger *slog.Logger,
) *PromptAdminServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &PromptAdminServer{
		publishHandler: publishHandler,
		promoteHandler: promoteHandler,
		getASTHandler:  getASTHandler,
		listHandler:    listHandler,
		defaultTenant:  defaultTenant,
		logger:         logger,
	}
}

// NewGRPCServer constructs a hardened *grpc.Server with a 1 MiB message size guard and unary interceptors.
func NewGRPCServer(adminSrv *PromptAdminServer) *grpc.Server {
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(MaxPayloadBytes),
		grpc.MaxSendMsgSize(MaxPayloadBytes),
		grpc.ChainUnaryInterceptor(
			adminSrv.recoveryInterceptor(),
			adminSrv.tracingAndTenantInterceptor(),
		),
	)
	promptv1.RegisterPromptRegistryServiceServer(srv, adminSrv)
	return srv
}

// recoveryInterceptor catches panics in unary gRPC handlers, logs a sanitized error, and returns codes.Internal.
func (s *PromptAdminServer) recoveryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				s.logger.ErrorContext(ctx, "recovered panic in gRPC unary handler",
					slog.String("trace_id", command.TraceIDFromContext(ctx)),
					slog.String("method", info.FullMethod),
					slog.String("error_code", "INTERNAL_PANIC"),
				)
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}

// tracingAndTenantInterceptor attaches a correlated trace_id and verifies the caller's Bearer JWT tenant claim.
func (s *PromptAdminServer) tracingAndTenantInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		traceID := redis.NewULID()
		var callerTenant string
		var mdTenant string

		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get("x-trace-id"); len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
				traceID = strings.TrimSpace(vals[0])
			} else if vals := md.Get("x-request-id"); len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
				traceID = strings.TrimSpace(vals[0])
			}
			if vals := md.Get("x-tenant-id"); len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
				mdTenant = strings.TrimSpace(vals[0])
			}
			if auths := md.Get("authorization"); len(auths) > 0 {
				callerTenant = extractTenantFromBearer(auths[0])
			}
		}

		ctx = command.WithTraceID(ctx, traceID)
		if callerTenant != "" {
			ctx = context.WithValue(ctx, grpcTenantContextKey, callerTenant)
			if mdTenant != "" && !strings.EqualFold(mdTenant, callerTenant) {
				ctx = context.WithValue(ctx, grpcTenantMismatchContextKey, true)
			}
		}

		resp, err := handler(ctx, req)
		code := status.Code(err)
		s.logger.InfoContext(ctx, "grpc request completed",
			slog.String("trace_id", traceID),
			slog.String("tenant_id", callerTenant),
			slog.String("method", info.FullMethod),
			slog.String("grpc_code", code.String()),
			slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000.0),
		)
		return resp, err
	}
}

// extractTenantFromBearer verifies the Bearer JWT's HS256 signature, issuer, and expiry before returning tenant_id.
func extractTenantFromBearer(authHeader string) string {
	parts := strings.SplitN(strings.TrimSpace(authHeader), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	token := strings.TrimSpace(parts[1])
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return ""
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return ""
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerBytes, &hdr); err != nil || hdr.Alg != "HS256" {
		return ""
	}

	secret := strings.TrimSpace(os.Getenv("JWT_HMAC_SECRET"))
	if secret == "" {
		secret = DefaultJWTSecret
	}
	expectedIssuer := strings.TrimSpace(os.Getenv("JWT_ISSUER"))
	if expectedIssuer == "" {
		expectedIssuer = DefaultJWTIssuer
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(segments[0] + "." + segments[1]))
	if !hmac.Equal(sigBytes, mac.Sum(nil)) {
		return ""
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return ""
	}
	var claims struct {
		TenantID string  `json:"tenant_id"`
		Iss      string  `json:"iss"`
		Exp      float64 `json:"exp"`
		Nbf      float64 `json:"nbf"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return ""
	}
	if strings.TrimSpace(claims.Iss) != expectedIssuer {
		return ""
	}
	now := time.Now().UTC().Unix()
	if claims.Exp <= 0 || now >= int64(claims.Exp) {
		return ""
	}
	if claims.Nbf > 0 && now < int64(claims.Nbf) {
		return ""
	}
	tid := strings.TrimSpace(claims.TenantID)
	if !entity.IsValidUUID(tid) {
		return ""
	}
	return tid
}

// resolveTenant validates that the caller presented a verified JWT tenant claim and that any request-supplied tenant matches it.
func (s *PromptAdminServer) resolveTenant(ctx context.Context, reqTenant string) (string, error) {
	reqTenant = strings.TrimSpace(reqTenant)
	metaTenant, _ := ctx.Value(grpcTenantContextKey).(string)
	metaTenant = strings.TrimSpace(metaTenant)

	if metaTenant == "" {
		return "", status.Error(codes.Unauthenticated, "valid bearer token with verified tenant_id claim is required")
	}
	if mismatch, _ := ctx.Value(grpcTenantMismatchContextKey).(bool); mismatch {
		return "", status.Error(codes.PermissionDenied, "cross-tenant request forbidden: x-tenant-id metadata does not match authenticated tenant")
	}
	if reqTenant != "" && !strings.EqualFold(metaTenant, reqTenant) {
		return "", status.Error(codes.PermissionDenied, "cross-tenant request forbidden: request tenant_id does not match caller tenant")
	}
	return metaTenant, nil
}

// mapDomainErrorToGRPC maps domain sentinel errors to their canonical gRPC status codes.
func mapDomainErrorToGRPC(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, entity.ErrInvalidInput):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, entity.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, entity.ErrVersionConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, entity.ErrConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	default:
		return status.Error(codes.Internal, "internal server error")
	}
}

// protoToEntityAST converts a Protobuf TemplateAST message into a domain entity.TemplateAST.
func protoToEntityAST(pb *promptv1.TemplateAST) (entity.TemplateAST, error) {
	if pb == nil {
		return entity.TemplateAST{}, fmt.Errorf("%w: raw_ast is required", entity.ErrInvalidInput)
	}
	msgs := make([]entity.MessageBlock, 0, len(pb.Messages))
	for _, m := range pb.Messages {
		if m == nil {
			continue
		}
		msgs = append(msgs, entity.MessageBlock{
			Role:           m.Role,
			ContentPattern: m.ContentPattern,
		})
	}
	exemplars := make([]entity.MessageBlock, 0, len(pb.FewShotExemplars))
	for _, ex := range pb.FewShotExemplars {
		if ex == nil {
			continue
		}
		exemplars = append(exemplars, entity.MessageBlock{
			Role:           ex.Role,
			ContentPattern: ex.ContentPattern,
		})
	}
	return entity.TemplateAST{
		SystemPrompt:     pb.SystemPrompt,
		Messages:         msgs,
		VariableKeys:     pb.VariableKeys,
		DefaultDialect:   entity.DialectType(pb.DefaultDialect.String()),
		PersonaOverrides: pb.PersonaOverrides,
		FewShotExemplars: exemplars,
	}, nil
}

// entityToProtoAST converts a domain entity.TemplateAST into a Protobuf TemplateAST message.
func entityToProtoAST(ast entity.TemplateAST) *promptv1.TemplateAST {
	msgs := make([]*promptv1.MessageBlock, 0, len(ast.Messages))
	for _, m := range ast.Messages {
		msgs = append(msgs, &promptv1.MessageBlock{
			Role:           m.Role,
			ContentPattern: m.ContentPattern,
		})
	}
	exemplars := make([]*promptv1.MessageBlock, 0, len(ast.FewShotExemplars))
	for _, ex := range ast.FewShotExemplars {
		exemplars = append(exemplars, &promptv1.MessageBlock{
			Role:           ex.Role,
			ContentPattern: ex.ContentPattern,
		})
	}
	return &promptv1.TemplateAST{
		SystemPrompt:     ast.SystemPrompt,
		Messages:         msgs,
		VariableKeys:     ast.VariableKeys,
		DefaultDialect:   promptv1.ParseDialectTarget(string(ast.DefaultDialect)),
		PersonaOverrides: ast.PersonaOverrides,
		FewShotExemplars: exemplars,
	}
}

// CreateTemplate handles the CreateTemplate RPC.
func (s *PromptAdminServer) CreateTemplate(ctx context.Context, req *promptv1.CreateTemplateRequest) (*promptv1.CreateTemplateResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	ast, err := protoToEntityAST(req.RawAst)
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	tmpl, ver, err := s.publishHandler.CreateTemplate(ctx, command.CreateTemplateInput{
		TenantID:    tenantID,
		Name:        req.Name,
		Description: req.Description,
		RawAST:      ast,
		SchemaID:    req.SchemaId,
		Author:      req.Author,
	})
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	return &promptv1.CreateTemplateResponse{
		TemplateId:     tmpl.ID,
		InitialVersion: uint32(ver.Version),
	}, nil
}

// PublishVersion handles the PublishVersion RPC.
func (s *PromptAdminServer) PublishVersion(ctx context.Context, req *promptv1.PublishVersionRequest) (*promptv1.PublishVersionResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	ast, err := protoToEntityAST(req.RawAst)
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	ver, _, err := s.publishHandler.PublishVersion(ctx, command.PublishVersionInput{
		TenantID:           tenantID,
		TemplateIdentifier: req.TemplateId,
		RawAST:             ast,
		SchemaID:           req.SchemaId,
		Author:             req.Author,
	})
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	return &promptv1.PublishVersionResponse{
		TemplateId: ver.TemplateID,
		Version:    uint32(ver.Version),
		CreatedAt:  ver.CreatedAt.UTC().Format(time.RFC3339),
	}, nil
}

// GetTemplateVersion handles the GetTemplateVersion RPC.
func (s *PromptAdminServer) GetTemplateVersion(ctx context.Context, req *promptv1.GetTemplateVersionRequest) (*promptv1.GetTemplateVersionResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	ver, err := s.getASTHandler.GetTemplateVersion(ctx, tenantID, req.TemplateId, int(req.Version))
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	return &promptv1.GetTemplateVersionResponse{
		TemplateId: ver.TemplateID,
		Version:    uint32(ver.Version),
		Ast:        entityToProtoAST(ver.RawAST),
		SchemaId:   ver.SchemaID,
		Author:     ver.Author,
		CreatedAt:  ver.CreatedAt.UTC().Format(time.RFC3339),
	}, nil
}

// PromoteTag handles the PromoteTag RPC.
func (s *PromptAdminServer) PromoteTag(ctx context.Context, req *promptv1.PromoteTagRequest) (*promptv1.PromoteTagResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	out, err := s.promoteHandler.PromoteTag(ctx, command.PromoteTagInput{
		TenantID:           tenantID,
		TemplateIdentifier: req.TemplateId,
		Tag:                req.Tag,
		TargetVersion:      int(req.TargetVersion),
		ExpectedVersion:    int(req.ExpectedVersion),
		UpdatedBy:          req.UpdatedBy,
	})
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	return &promptv1.PromoteTagResponse{
		TemplateId:       out.TemplateID,
		Tag:              out.Tag,
		TargetVersion:    uint32(out.TargetVersion),
		BroadcastEmitted: out.BroadcastEmitted,
	}, nil
}

// ResolveTag handles the ResolveTag RPC.
func (s *PromptAdminServer) ResolveTag(ctx context.Context, req *promptv1.ResolveTagRequest) (*promptv1.ResolveTagResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	ver, err := s.getASTHandler.ResolveTag(ctx, tenantID, req.TemplateId, req.Tag)
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	return &promptv1.ResolveTagResponse{
		TemplateId: ver.TemplateID,
		Tag:        req.Tag,
		Version:    uint32(ver.Version),
		Ast:        entityToProtoAST(ver.RawAST),
	}, nil
}

// RegisterSchema handles the RegisterSchema RPC.
func (s *PromptAdminServer) RegisterSchema(ctx context.Context, req *promptv1.RegisterSchemaRequest) (*promptv1.RegisterSchemaResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	sc, err := s.publishHandler.RegisterSchema(ctx, command.RegisterSchemaInput{
		TenantID:   tenantID,
		Name:       req.Name,
		SchemaJSON: []byte(req.JsonSchema),
		Author:     "grpc-admin",
	})
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	return &promptv1.RegisterSchemaResponse{
		SchemaId: sc.ID,
	}, nil
}

// GetSchema handles the GetSchema RPC.
func (s *PromptAdminServer) GetSchema(ctx context.Context, req *promptv1.GetSchemaRequest) (*promptv1.GetSchemaResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	sc, err := s.getASTHandler.GetSchema(ctx, tenantID, req.SchemaId)
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	return &promptv1.GetSchemaResponse{
		SchemaId:   sc.ID,
		Name:       sc.Name,
		JsonSchema: string(sc.SchemaJSON),
		TenantId:   sc.TenantID,
		CreatedAt:  sc.CreatedAt.UTC().Format(time.RFC3339),
	}, nil
}

// ListTemplates handles the ListTemplates RPC.
func (s *PromptAdminServer) ListTemplates(ctx context.Context, req *promptv1.ListTemplatesRequest) (*promptv1.ListTemplatesResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	list, nextCursor, err := s.listHandler.ListTemplates(ctx, tenantID, int(req.Limit), req.Cursor)
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	summaries := make([]*promptv1.TemplateSummary, 0, len(list))
	for _, t := range list {
		summaries = append(summaries, &promptv1.TemplateSummary{
			Id:             t.ID,
			TenantId:       t.TenantID,
			Name:           t.Name,
			Description:    t.Description,
			CurrentVersion: uint32(t.CurrentVersion),
			CreatedAt:      t.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:      t.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return &promptv1.ListTemplatesResponse{
		Templates:  summaries,
		NextCursor: nextCursor,
	}, nil
}

// ListSchemas handles the ListSchemas RPC.
func (s *PromptAdminServer) ListSchemas(ctx context.Context, req *promptv1.ListSchemasRequest) (*promptv1.ListSchemasResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	tenantID, err := s.resolveTenant(ctx, req.TenantId)
	if err != nil {
		return nil, err
	}
	list, nextCursor, err := s.listHandler.ListSchemas(ctx, tenantID, int(req.Limit), req.Cursor)
	if err != nil {
		return nil, mapDomainErrorToGRPC(err)
	}
	items := make([]*promptv1.GetSchemaResponse, 0, len(list))
	for _, sc := range list {
		items = append(items, &promptv1.GetSchemaResponse{
			SchemaId:   sc.ID,
			Name:       sc.Name,
			JsonSchema: string(sc.SchemaJSON),
			TenantId:   sc.TenantID,
			CreatedAt:  sc.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return &promptv1.ListSchemasResponse{
		Schemas:    items,
		NextCursor: nextCursor,
	}, nil
}
