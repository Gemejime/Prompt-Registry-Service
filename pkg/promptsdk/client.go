package promptsdk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"rag-prompt-registry-service/pkg/promptsdk/tokenizer"
	promptv1 "rag-prompt-registry-service/proto/v1"
)

const (
	defaultRPCTimeout = 2 * time.Second
	defaultJWTSecret  = "rag-prompt-registry-default-jwt-hmac-secret"
	defaultJWTIssuer  = "rag-prompt-registry-auth"
)

// ClientConfig configures the embedded Data Plane PromptSDK Client.
type ClientConfig struct {
	// RegistryAddr is the optional control plane gRPC address (e.g. "localhost:50051").
	RegistryAddr string
	// GRPCConn is an optional pre-established gRPC client connection.
	GRPCConn grpc.ClientConnInterface
	// RegistryClient is an optional pre-constructed PromptRegistryServiceClient.
	RegistryClient promptv1.PromptRegistryServiceClient
	// TenantID is the UUID of the tenant owning the templates.
	TenantID string
	// AuthToken is an optional pre-signed Bearer JWT for authenticating control plane RPCs.
	AuthToken string
	// JWTSecret is an optional HMAC-SHA256 key used to mint service JWTs when AuthToken is empty.
	JWTSecret string
	// JWTIssuer is the JWT issuer claim (defaults to "rag-prompt-registry-auth").
	JWTIssuer string
	// RedisAddrs is an optional list of Redis 7.2 TCP addresses for Pub/Sub invalidation subscription.
	RedisAddrs []string
	// RedisPassword is the optional Redis authentication credential.
	RedisPassword string
	// RedisDB is the optional Redis database index.
	RedisDB int
	// InvalidationChannel is the Redis Pub/Sub channel name (defaults to "channel:prompt:invalidation").
	InvalidationChannel string
	// InvalidationSub is an optional Go channel delivering invalidation JSON payloads in-process.
	InvalidationSub <-chan string
	// TagTTL is the safety fallback TTL for mutable tag pointers in L1_tag (defaults to 5m).
	TagTTL time.Duration
	// OfflineRetryCooldown is the cooldown before re-attempting control plane refresh during outages (defaults to 5s).
	OfflineRetryCooldown time.Duration
	// RPCTimeout bounds individual control plane gRPC calls (defaults to 2s).
	RPCTimeout time.Duration
	// Tokenizer is an optional custom Tokenizer for the budget safety guard (defaults to CL100KTokenizer).
	Tokenizer tokenizer.Tokenizer
	// NodeID identifies this orchestrator instance in Prometheus metrics (defaults to hostname).
	NodeID string
	// Logger is the structured slog.Logger (defaults to JSON handler on os.Stdout).
	Logger *slog.Logger
}

// sfCall represents an in-flight coalesced control plane lookup.
type sfCall struct {
	wg  sync.WaitGroup
	val *CompiledTemplate
	err error
}

// schemaFlightCall represents an in-flight coalesced schema lookup.
type schemaFlightCall struct {
	wg  sync.WaitGroup
	val *SchemaContract
	err error
}

// dialectMetricCounters tracks zero-allocation atomic render counts and durations per dialect.
type dialectMetricCounters struct {
	successCount atomic.Uint64
	successNanos atomic.Uint64
	errorCount   atomic.Uint64
	errorNanos   atomic.Uint64
}

// SDKMetricsSnapshot captures point-in-time Prometheus metrics for the embedded SDK.
type SDKMetricsSnapshot struct {
	CacheStats       CacheStats
	PubSubConnected  bool
	NodeID           string
	AnthropicSuccess uint64
	AnthropicErrors  uint64
	AnthropicSumSec  float64
	LlamaSuccess     uint64
	LlamaErrors      uint64
	LlamaSumSec      float64
	OpenAISuccess    uint64
	OpenAIErrors     uint64
	OpenAISumSec     float64
}

// Client implements the PromptSDK interface with two-tier L1 caching, singleflight request
// coalescing, background Redis Pub/Sub invalidation, and zero-allocation rendering.
type Client struct {
	cfg           ClientConfig
	cache         *L1Cache
	budgetGuard   *tokenizer.BudgetGuard
	grpcClient    promptv1.PromptRegistryServiceClient
	ownedConn     *grpc.ClientConn
	logger        *slog.Logger
	nodeID        string
	authHeaderVal string

	sfMu           sync.Mutex
	inFlight       map[string]*sfCall
	schemaInFlight map[string]*schemaFlightCall

	pubsubConnected atomic.Bool
	closed          atomic.Bool
	stopCh          chan struct{}
	wg              sync.WaitGroup

	connMu     sync.Mutex
	activeConn net.Conn

	anthropicMetrics dialectMetricCounters
	llamaMetrics     dialectMetricCounters
	openaiMetrics    dialectMetricCounters
}

var _ PromptSDK = (*Client)(nil)

// NewClient initializes a new embedded PromptSDK Client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.TagTTL <= 0 {
		cfg.TagTTL = DefaultTagFallbackTTL
	}
	if cfg.OfflineRetryCooldown <= 0 {
		cfg.OfflineRetryCooldown = DefaultOfflineRetryCooldown
	}
	if cfg.RPCTimeout <= 0 {
		cfg.RPCTimeout = defaultRPCTimeout
	}
	if strings.TrimSpace(cfg.InvalidationChannel) == "" {
		cfg.InvalidationChannel = DefaultInvalidationChannel
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	nodeID := strings.TrimSpace(cfg.NodeID)
	if nodeID == "" {
		if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
			nodeID = strings.TrimSpace(h)
		} else {
			nodeID = "orchestrator-local"
		}
	}

	cache := NewL1Cache(cfg.TagTTL)
	cache.SetOfflineRetryCooldown(cfg.OfflineRetryCooldown)

	var ownedConn *grpc.ClientConn
	grpcClient := cfg.RegistryClient
	if grpcClient == nil {
		if cfg.GRPCConn != nil {
			grpcClient = promptv1.NewPromptRegistryServiceClient(cfg.GRPCConn)
		} else if strings.TrimSpace(cfg.RegistryAddr) != "" {
			conn, err := grpc.NewClient(
				strings.TrimSpace(cfg.RegistryAddr),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize registry gRPC client: %w", err)
			}
			ownedConn = conn
			grpcClient = promptv1.NewPromptRegistryServiceClient(conn)
		}
	}

	authToken := strings.TrimSpace(cfg.AuthToken)
	if authToken == "" && strings.TrimSpace(cfg.TenantID) != "" {
		authToken = mintServiceJWT(cfg.TenantID, cfg.JWTIssuer, cfg.JWTSecret)
	}
	var authHeaderVal string
	if authToken != "" {
		if strings.HasPrefix(strings.ToLower(authToken), "bearer ") {
			authHeaderVal = authToken
		} else {
			authHeaderVal = "Bearer " + authToken
		}
	}

	c := &Client{
		cfg:            cfg,
		cache:          cache,
		budgetGuard:    tokenizer.NewBudgetGuard(cfg.Tokenizer),
		grpcClient:     grpcClient,
		ownedConn:      ownedConn,
		logger:         logger,
		nodeID:         nodeID,
		authHeaderVal:  authHeaderVal,
		inFlight:       make(map[string]*sfCall),
		schemaInFlight: make(map[string]*schemaFlightCall),
		stopCh:         make(chan struct{}),
	}

	if cfg.InvalidationSub != nil {
		c.pubsubConnected.Store(true)
		c.wg.Add(1)
		go c.runChannelSubscriber(cfg.InvalidationSub)
	} else if len(cfg.RedisAddrs) > 0 {
		c.wg.Add(1)
		go c.runRedisTCPSubscriber()
	}

	return c, nil
}

// mintServiceJWT signs a 24-hour HS256 service token for control plane read RPCs.
func mintServiceJWT(tenantID, issuer, secret string) string {
	if strings.TrimSpace(secret) == "" {
		secret = strings.TrimSpace(os.Getenv("JWT_HMAC_SECRET"))
	}
	if secret == "" {
		secret = defaultJWTSecret
	}
	if strings.TrimSpace(issuer) == "" {
		issuer = strings.TrimSpace(os.Getenv("JWT_ISSUER"))
	}
	if issuer == "" {
		issuer = defaultJWTIssuer
	}
	now := time.Now().UTC().Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claimsBytes, err := json.Marshal(map[string]any{
		"tenant_id": strings.TrimSpace(tenantID),
		"iss":       issuer,
		"iat":       now,
		"exp":       now + 86400,
	})
	if err != nil {
		return ""
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claimsBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(unsigned))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return unsigned + "." + sig
}

// Cache returns the underlying two-tier L1Cache.
func (c *Client) Cache() *L1Cache {
	return c.cache
}

// BudgetGuard returns the SDK's Tokenizer BudgetGuard.
func (c *Client) BudgetGuard() *tokenizer.BudgetGuard {
	return c.budgetGuard
}

// IsPubSubConnected reports whether the Redis Pub/Sub invalidation listener is currently connected.
func (c *Client) IsPubSubConnected() bool {
	return c.pubsubConnected.Load()
}

// SetPubSubConnected updates the Pub/Sub connection status gauge (Runbook 2).
func (c *Client) SetPubSubConnected(connected bool) {
	c.pubsubConnected.Store(connected)
}

// PreloadVersion compiles and seeds an immutable template version directly into L1_version.
func (c *Client) PreloadVersion(
	templateID string,
	templateName string,
	version int,
	ast TemplateAST,
	schema *SchemaContract,
) (*CompiledTemplate, error) {
	compiled, err := CompileTemplateAST(templateID, templateName, version, ast, schema)
	if err != nil {
		return nil, err
	}
	c.cache.PutVersion(compiled)
	return compiled, nil
}

// PreloadTag compiles and seeds both the immutable template version in L1_version and the mutable
// tag pointer in L1_tag.
func (c *Client) PreloadTag(
	templateID string,
	templateName string,
	tag string,
	version int,
	ast TemplateAST,
	schema *SchemaContract,
) (*CompiledTemplate, error) {
	compiled, err := CompileTemplateAST(templateID, templateName, version, ast, schema)
	if err != nil {
		return nil, err
	}
	idKey := templateID
	if idKey == "" {
		idKey = templateName
	}
	c.cache.PutTag(idKey, tag, compiled)
	return compiled, nil
}

// Render compiles the resolved template AST and binds runtime variables using sync.Pool.
// It returns a freshly copied byte slice safe for caller retention after the pooled buffer is released.
func (c *Client) Render(
	ctx context.Context,
	templateID string,
	tagOrVersion string,
	opts RenderOptions,
) ([]byte, error) {
	buf := AcquireBuffer()
	defer ReleaseBuffer(buf)

	if err := c.RenderToBuffer(ctx, buf, templateID, tagOrVersion, opts); err != nil {
		return nil, err
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

// RenderToBuffer resolves the requested template version or tag from L1 cache (fetching from the
// control plane on cache miss) and compiles the target dialect directly into dst with 0 heap allocations
// on cache hits (Invariant 4: 0 allocs/op, P99 <= 0.5ms).
func (c *Client) RenderToBuffer(
	ctx context.Context,
	dst *bytes.Buffer,
	templateID string,
	tagOrVersion string,
	opts RenderOptions,
) error {
	start := time.Now()
	if tagOrVersion == "" {
		tagOrVersion = "@production"
	}

	compiled, err := c.resolveCompiledTemplate(ctx, templateID, tagOrVersion)
	if err != nil {
		c.recordRenderMetric(opts.TargetDialect, false, time.Since(start))
		return err
	}

	effDialect := opts.TargetDialect
	if effDialect == "" {
		effDialect = compiled.AST.DefaultDialect
	}

	if err := CompileIntoBuffer(dst, compiled, opts, c.budgetGuard); err != nil {
		c.recordRenderMetric(effDialect, false, time.Since(start))
		return err
	}

	c.recordRenderMetric(effDialect, true, time.Since(start))
	return nil
}

// resolveCompiledTemplate resolves (templateID, tagOrVersion) from L1 cache with 0 heap allocations on hits.
func (c *Client) resolveCompiledTemplate(ctx context.Context, templateID, tagOrVersion string) (*CompiledTemplate, error) {
	if tagOrVersion != "" && tagOrVersion[0] == '@' {
		compiled, needsRefresh, ok := c.cache.GetTag(templateID, tagOrVersion)
		if ok && !needsRefresh {
			return compiled, nil
		}
		if ok && needsRefresh {
			refreshed, err := c.fetchAndCacheTag(ctx, templateID, tagOrVersion, true)
			if err != nil {
				// Resilient Data Plane: serve last-known-good cached version during control plane outage.
				c.cache.MarkTagRefreshFailed(templateID, tagOrVersion)
				return compiled, nil
			}
			return refreshed, nil
		}
		return c.fetchAndCacheTag(ctx, templateID, tagOrVersion, false)
	}

	verNum, ok := parseVersionNumber(tagOrVersion)
	if !ok {
		return nil, fmt.Errorf("%w: invalid tag or version identifier %q", ErrTemplateNotFound, tagOrVersion)
	}
	if compiled, found := c.cache.GetVersion(templateID, verNum); found {
		return compiled, nil
	}
	return c.fetchAndCacheVersion(ctx, templateID, verNum)
}

// parseVersionNumber parses "1", "v1", or "V1" into a positive integer without heap allocations.
func parseVersionNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	if s[0] == 'v' || s[0] == 'V' {
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
		if n > 1_000_000_000 {
			return 0, false
		}
	}
	return n, n >= 1
}

// fetchAndCacheTag coalesces concurrent L1 tag misses using singleflight and populates L1_tag + L1_version.
func (c *Client) fetchAndCacheTag(ctx context.Context, templateID, tag string, forceRefresh bool) (*CompiledTemplate, error) {
	sfKey := "tag|" + templateID + "|" + tag

	c.sfMu.Lock()
	if !forceRefresh {
		if compiled, needsRefresh, ok := c.cache.GetTag(templateID, tag); ok && !needsRefresh {
			c.sfMu.Unlock()
			return compiled, nil
		}
	}
	if call, exists := c.inFlight[sfKey]; exists {
		c.sfMu.Unlock()
		call.wg.Wait()
		return call.val, call.err
	}
	call := &sfCall{}
	call.wg.Add(1)
	c.inFlight[sfKey] = call
	c.sfMu.Unlock()

	defer func() {
		call.wg.Done()
		c.sfMu.Lock()
		delete(c.inFlight, sfKey)
		c.sfMu.Unlock()
	}()

	if c.grpcClient == nil {
		call.err = fmt.Errorf("%w: template %q tag %q not in L1 cache and control plane client is not configured",
			ErrTemplateNotFound, templateID, tag)
		return nil, call.err
	}

	rpcCtx, cancel := c.outgoingRPCContext(ctx)
	defer cancel()

	start := time.Now()
	res, err := c.grpcClient.ResolveTag(rpcCtx, &promptv1.ResolveTagRequest{
		TemplateId: templateID,
		Tag:        tag,
		TenantId:   c.cfg.TenantID,
	})
	if err != nil {
		call.err = fmt.Errorf("%w: control plane ResolveTag failed: %v", ErrTemplateNotFound, err)
		return nil, call.err
	}

	verNum := int(res.Version)
	canonicalID := strings.TrimSpace(res.TemplateId)
	if canonicalID == "" {
		canonicalID = templateID
	}

	var templateName string
	if templateID != canonicalID {
		templateName = templateID
	}

	// Check if the resolved immutable version is already cached in L1_version (e.g. pointer rollback v2 -> v1).
	if existingVer, found := c.cache.GetVersion(canonicalID, verNum); found {
		c.cache.PutTag(templateID, tag, existingVer)
		call.val = existingVer
		return existingVer, nil
	}

	var schemaContract *SchemaContract
	schemaID := strings.TrimSpace(res.SchemaId)
	if schemaID == "" {
		// Fallback for older control plane responses that omit schema_id on ResolveTagResponse.
		if verRes, verErr := c.grpcClient.GetTemplateVersion(rpcCtx, &promptv1.GetTemplateVersionRequest{
			TemplateId: canonicalID,
			Version:    res.Version,
			TenantId:   c.cfg.TenantID,
		}); verErr == nil && verRes != nil {
			schemaID = strings.TrimSpace(verRes.SchemaId)
		}
	}
	if schemaID != "" {
		sc, scErr := c.fetchSchemaCoalesced(rpcCtx, schemaID)
		if scErr == nil {
			schemaContract = sc
		}
	}

	ast := protoASTToSDK(res.Ast)
	compiled, err := CompileTemplateAST(canonicalID, templateName, verNum, ast, schemaContract)
	if err != nil {
		call.err = err
		return nil, err
	}

	c.cache.PutTag(templateID, tag, compiled)
	c.logger.InfoContext(ctx, "sdk tag pointer resolved and cached",
		slog.String("tenant_id", c.cfg.TenantID),
		slog.String("template_id", canonicalID),
		slog.String("tag", tag),
		slog.Int("version", verNum),
		slog.String("template_hash", compiled.TruncatedHash),
		slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000.0),
	)

	call.val = compiled
	return compiled, nil
}

// fetchAndCacheVersion coalesces concurrent L1 version misses using singleflight and populates L1_version.
func (c *Client) fetchAndCacheVersion(ctx context.Context, templateID string, version int) (*CompiledTemplate, error) {
	sfKey := "ver|" + templateID + "|" + strconv.Itoa(version)

	c.sfMu.Lock()
	if compiled, ok := c.cache.GetVersion(templateID, version); ok {
		c.sfMu.Unlock()
		return compiled, nil
	}
	if call, exists := c.inFlight[sfKey]; exists {
		c.sfMu.Unlock()
		call.wg.Wait()
		return call.val, call.err
	}
	call := &sfCall{}
	call.wg.Add(1)
	c.inFlight[sfKey] = call
	c.sfMu.Unlock()

	defer func() {
		call.wg.Done()
		c.sfMu.Lock()
		delete(c.inFlight, sfKey)
		c.sfMu.Unlock()
	}()

	if c.grpcClient == nil {
		call.err = fmt.Errorf("%w: template %q version %d not in L1 cache and control plane client is not configured",
			ErrTemplateNotFound, templateID, version)
		return nil, call.err
	}

	rpcCtx, cancel := c.outgoingRPCContext(ctx)
	defer cancel()

	start := time.Now()
	res, err := c.grpcClient.GetTemplateVersion(rpcCtx, &promptv1.GetTemplateVersionRequest{
		TemplateId: templateID,
		Version:    uint32(version),
		TenantId:   c.cfg.TenantID,
	})
	if err != nil {
		call.err = fmt.Errorf("%w: control plane GetTemplateVersion failed: %v", ErrTemplateNotFound, err)
		return nil, call.err
	}

	canonicalID := strings.TrimSpace(res.TemplateId)
	if canonicalID == "" {
		canonicalID = templateID
	}
	var templateName string
	if templateID != canonicalID {
		templateName = templateID
	}

	var schemaContract *SchemaContract
	if schemaID := strings.TrimSpace(res.SchemaId); schemaID != "" {
		if sc, scErr := c.fetchSchemaCoalesced(rpcCtx, schemaID); scErr == nil {
			schemaContract = sc
		}
	}

	ast := protoASTToSDK(res.Ast)
	compiled, err := CompileTemplateAST(canonicalID, templateName, int(res.Version), ast, schemaContract)
	if err != nil {
		call.err = err
		return nil, err
	}

	c.cache.PutVersion(compiled)
	c.logger.InfoContext(ctx, "sdk immutable template version cached",
		slog.String("tenant_id", c.cfg.TenantID),
		slog.String("template_id", canonicalID),
		slog.Int("version", int(res.Version)),
		slog.String("template_hash", compiled.TruncatedHash),
		slog.Float64("latency_ms", float64(time.Since(start).Microseconds())/1000.0),
	)

	call.val = compiled
	return compiled, nil
}

// fetchSchemaCoalesced fetches a JSON output schema contract from L1 cache or the control plane via singleflight.
func (c *Client) fetchSchemaCoalesced(ctx context.Context, schemaID string) (*SchemaContract, error) {
	if sc, ok := c.cache.GetSchema(schemaID); ok {
		return sc, nil
	}

	c.sfMu.Lock()
	if sc, ok := c.cache.GetSchema(schemaID); ok {
		c.sfMu.Unlock()
		return sc, nil
	}
	if call, exists := c.schemaInFlight[schemaID]; exists {
		c.sfMu.Unlock()
		call.wg.Wait()
		return call.val, call.err
	}
	call := &schemaFlightCall{}
	call.wg.Add(1)
	c.schemaInFlight[schemaID] = call
	c.sfMu.Unlock()

	defer func() {
		call.wg.Done()
		c.sfMu.Lock()
		delete(c.schemaInFlight, schemaID)
		c.sfMu.Unlock()
	}()

	res, err := c.grpcClient.GetSchema(ctx, &promptv1.GetSchemaRequest{
		SchemaId: schemaID,
		TenantId: c.cfg.TenantID,
	})
	if err != nil {
		call.err = err
		return nil, err
	}
	sc := &SchemaContract{
		ID:         res.SchemaId,
		Name:       res.Name,
		SchemaJSON: json.RawMessage(res.JsonSchema),
	}
	c.cache.PutSchema(sc)
	call.val = sc
	return sc, nil
}

// outgoingRPCContext attaches the timeout and tenant authentication metadata to control plane gRPC calls.
func (c *Client) outgoingRPCContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, c.cfg.RPCTimeout)
	if c.authHeaderVal != "" || c.cfg.TenantID != "" {
		md := metadata.New(nil)
		if c.authHeaderVal != "" {
			md.Set("authorization", c.authHeaderVal)
		}
		if c.cfg.TenantID != "" {
			md.Set("x-tenant-id", c.cfg.TenantID)
		}
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	return ctx, cancel
}

func protoASTToSDK(pb *promptv1.TemplateAST) TemplateAST {
	if pb == nil {
		return TemplateAST{}
	}
	msgs := make([]MessageBlock, 0, len(pb.Messages))
	for _, m := range pb.Messages {
		if m == nil {
			continue
		}
		msgs = append(msgs, MessageBlock{
			Role:           m.Role,
			ContentPattern: m.ContentPattern,
		})
	}
	exemplars := make([]MessageBlock, 0, len(pb.FewShotExemplars))
	for _, ex := range pb.FewShotExemplars {
		if ex == nil {
			continue
		}
		exemplars = append(exemplars, MessageBlock{
			Role:           ex.Role,
			ContentPattern: ex.ContentPattern,
		})
	}
	var overrides map[string]string
	if len(pb.PersonaOverrides) > 0 {
		overrides = make(map[string]string, len(pb.PersonaOverrides))
		for k, v := range pb.PersonaOverrides {
			overrides[k] = v
		}
	}
	return TemplateAST{
		SystemPrompt:     pb.SystemPrompt,
		Messages:         msgs,
		VariableKeys:     append([]string(nil), pb.VariableKeys...),
		DefaultDialect:   DialectType(pb.DefaultDialect.String()),
		PersonaOverrides: overrides,
		FewShotExemplars: exemplars,
	}
}

// Invalidate explicitly forces cache eviction for (templateID, tag) in L1_tag.
func (c *Client) Invalidate(templateID string, tag string) {
	c.cache.Invalidate(templateID, tag)
}

// Close cleanly terminates Redis Pub/Sub listeners and closes any owned gRPC connection.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.stopCh)
	c.pubsubConnected.Store(false)

	c.connMu.Lock()
	if c.activeConn != nil {
		_ = c.activeConn.Close()
		c.activeConn = nil
	}
	c.connMu.Unlock()

	c.wg.Wait()
	if c.ownedConn != nil {
		return c.ownedConn.Close()
	}
	return nil
}

// runChannelSubscriber listens for invalidation events on an injected Go channel.
func (c *Client) runChannelSubscriber(sub <-chan string) {
	defer c.wg.Done()
	for {
		select {
		case <-c.stopCh:
			c.pubsubConnected.Store(false)
			return
		case payload, ok := <-sub:
			if !ok {
				c.pubsubConnected.Store(false)
				return
			}
			if evt, err := c.cache.HandleInvalidationPayload(payload); err == nil {
				c.logger.Info("sdk cache tag invalidated via pubsub",
					slog.String("event_id", evt.EventID),
					slog.String("tenant_id", evt.TenantID),
					slog.String("template_id", evt.TemplateID),
					slog.String("tag", evt.Tag),
					slog.Int("new_version", evt.NewVersion),
				)
			}
		}
	}
}

// runRedisTCPSubscriber maintains a resilient Redis RESP Pub/Sub subscription to channel:prompt:invalidation.
func (c *Client) runRedisTCPSubscriber() {
	defer c.wg.Done()
	for {
		select {
		case <-c.stopCh:
			c.pubsubConnected.Store(false)
			return
		default:
		}

		err := c.subscribeRedisOnce()
		c.pubsubConnected.Store(false)
		if err != nil && !c.closed.Load() {
			c.logger.Warn("sdk redis pubsub listener disconnected; falling back to 5m safety TTL",
				slog.String("node_id", c.nodeID),
				slog.String("channel", c.cfg.InvalidationChannel),
			)
		}

		select {
		case <-c.stopCh:
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (c *Client) subscribeRedisOnce() error {
	var conn net.Conn
	var dialErr error
	dialer := net.Dialer{Timeout: 1500 * time.Millisecond}
	for _, rawAddr := range c.cfg.RedisAddrs {
		addr := strings.TrimSpace(rawAddr)
		if addr == "" {
			continue
		}
		conn, dialErr = dialer.Dial("tcp", addr)
		if dialErr == nil {
			break
		}
	}
	if conn == nil {
		if dialErr != nil {
			return dialErr
		}
		return fmt.Errorf("no reachable redis addresses")
	}

	c.connMu.Lock()
	if c.closed.Load() {
		c.connMu.Unlock()
		_ = conn.Close()
		return nil
	}
	c.activeConn = conn
	c.connMu.Unlock()

	defer func() {
		c.connMu.Lock()
		if c.activeConn == conn {
			c.activeConn = nil
		}
		c.connMu.Unlock()
		_ = conn.Close()
	}()

	reader := bufio.NewReader(conn)
	if c.cfg.RedisPassword != "" {
		if err := writeRESPCommand(conn, "AUTH", c.cfg.RedisPassword); err != nil {
			return err
		}
		if _, err := readRESPValue(reader); err != nil {
			return err
		}
	}
	if c.cfg.RedisDB > 0 {
		if err := writeRESPCommand(conn, "SELECT", strconv.Itoa(c.cfg.RedisDB)); err != nil {
			return err
		}
		if _, err := readRESPValue(reader); err != nil {
			return err
		}
	}

	if err := writeRESPCommand(conn, "SUBSCRIBE", c.cfg.InvalidationChannel); err != nil {
		return err
	}

	for {
		val, err := readRESPValue(reader)
		if err != nil {
			return err
		}
		arr, ok := val.([]string)
		if !ok || len(arr) < 3 {
			continue
		}
		switch strings.ToLower(arr[0]) {
		case "subscribe":
			c.pubsubConnected.Store(true)
		case "message":
			payload := arr[2]
			if evt, pErr := c.cache.HandleInvalidationPayload(payload); pErr == nil {
				c.logger.Info("sdk cache tag invalidated via redis pubsub",
					slog.String("event_id", evt.EventID),
					slog.String("tenant_id", evt.TenantID),
					slog.String("template_id", evt.TemplateID),
					slog.String("tag", evt.Tag),
					slog.Int("new_version", evt.NewVersion),
				)
			}
		}
	}
}

func writeRESPCommand(w io.Writer, args ...string) error {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*%d\r\n", len(args)))
	for _, a := range args {
		sb.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(a), a))
	}
	_, err := w.Write([]byte(sb.String()))
	return err
}

func readRESPValue(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) == 0 {
		return "", nil
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case ':':
		return line[1:], nil
	case '-':
		return nil, fmt.Errorf("redis error: %s", line[1:])
	case '$':
		length, err := strconv.Atoi(line[1:])
		if err != nil || length < 0 {
			return "", nil
		}
		buf := make([]byte, length+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:length]), nil
	case '*':
		count, err := strconv.Atoi(line[1:])
		if err != nil || count <= 0 {
			return []string(nil), nil
		}
		items := make([]string, 0, count)
		for i := 0; i < count; i++ {
			elem, err := readRESPValue(r)
			if err != nil {
				return nil, err
			}
			if s, ok := elem.(string); ok {
				items = append(items, s)
			} else {
				items = append(items, fmt.Sprintf("%v", elem))
			}
		}
		return items, nil
	default:
		return line, nil
	}
}

// recordRenderMetric updates the atomic duration and count counters with 0 heap allocations.
func (c *Client) recordRenderMetric(dialect DialectType, success bool, d time.Duration) {
	nanos := uint64(d.Nanoseconds())
	var target *dialectMetricCounters
	switch dialect {
	case DialectLlamaSpecial:
		target = &c.llamaMetrics
	case DialectOpenAIStandard:
		target = &c.openaiMetrics
	default:
		target = &c.anthropicMetrics
	}
	if success {
		target.successCount.Add(1)
		target.successNanos.Add(nanos)
	} else {
		target.errorCount.Add(1)
		target.errorNanos.Add(nanos)
	}
}

// MetricsSnapshot returns a structured snapshot of SDK render, cache hit ratio, and Pub/Sub metrics.
func (c *Client) MetricsSnapshot() SDKMetricsSnapshot {
	return SDKMetricsSnapshot{
		CacheStats:       c.cache.Stats(),
		PubSubConnected:  c.pubsubConnected.Load(),
		NodeID:           c.nodeID,
		AnthropicSuccess: c.anthropicMetrics.successCount.Load(),
		AnthropicErrors:  c.anthropicMetrics.errorCount.Load(),
		AnthropicSumSec:  float64(c.anthropicMetrics.successNanos.Load()+c.anthropicMetrics.errorNanos.Load()) / 1e9,
		LlamaSuccess:     c.llamaMetrics.successCount.Load(),
		LlamaErrors:      c.llamaMetrics.errorCount.Load(),
		LlamaSumSec:      float64(c.llamaMetrics.successNanos.Load()+c.llamaMetrics.errorNanos.Load()) / 1e9,
		OpenAISuccess:    c.openaiMetrics.successCount.Load(),
		OpenAIErrors:     c.openaiMetrics.errorCount.Load(),
		OpenAISumSec:     float64(c.openaiMetrics.successNanos.Load()+c.openaiMetrics.errorNanos.Load()) / 1e9,
	}
}

// MetricsExposition formats the SDK Prometheus metrics (`prompt_sdk_render_duration_seconds`,
// `prompt_sdk_cache_hit_ratio`, `prompt_sdk_pubsub_connected`) per .ai/doc-api.md Section 8.
func (c *Client) MetricsExposition() string {
	snap := c.MetricsSnapshot()
	pubsubGauge := 0
	if snap.PubSubConnected {
		pubsubGauge = 1
	}

	var sb strings.Builder
	sb.WriteString("# HELP prompt_sdk_render_duration_seconds SDK in-process compilation latency\n")
	sb.WriteString("# TYPE prompt_sdk_render_duration_seconds histogram\n")
	writeDialectMetricLines(&sb, string(DialectAnthropicXML), &c.anthropicMetrics)
	writeDialectMetricLines(&sb, string(DialectLlamaSpecial), &c.llamaMetrics)
	writeDialectMetricLines(&sb, string(DialectOpenAIStandard), &c.openaiMetrics)

	sb.WriteString("# HELP prompt_sdk_cache_hit_ratio Local SDK memory hit ratio\n")
	sb.WriteString("# TYPE prompt_sdk_cache_hit_ratio gauge\n")
	sb.WriteString(fmt.Sprintf("prompt_sdk_cache_hit_ratio{tier=\"L1_version\"} %.6f\n", snap.CacheStats.VersionHitRatio))
	sb.WriteString(fmt.Sprintf("prompt_sdk_cache_hit_ratio{tier=\"L1_tag\"} %.6f\n", snap.CacheStats.TagHitRatio))

	sb.WriteString("# HELP prompt_sdk_pubsub_connected Orchestrator Pub/Sub connection status (1=healthy, 0=fallback TTL)\n")
	sb.WriteString("# TYPE prompt_sdk_pubsub_connected gauge\n")
	sb.WriteString(fmt.Sprintf("prompt_sdk_pubsub_connected{node_id=%q} %d\n", snap.NodeID, pubsubGauge))
	return sb.String()
}

func writeDialectMetricLines(sb *strings.Builder, dialect string, m *dialectMetricCounters) {
	succCnt := m.successCount.Load()
	succSum := float64(m.successNanos.Load()) / 1e9
	errCnt := m.errorCount.Load()
	errSum := float64(m.errorNanos.Load()) / 1e9

	sb.WriteString(fmt.Sprintf("prompt_sdk_render_duration_seconds_count{dialect=%q,status=\"success\"} %d\n", dialect, succCnt))
	sb.WriteString(fmt.Sprintf("prompt_sdk_render_duration_seconds_sum{dialect=%q,status=\"success\"} %.6f\n", dialect, succSum))
	sb.WriteString(fmt.Sprintf("prompt_sdk_render_duration_seconds_count{dialect=%q,status=\"error\"} %d\n", dialect, errCnt))
	sb.WriteString(fmt.Sprintf("prompt_sdk_render_duration_seconds_sum{dialect=%q,status=\"error\"} %.6f\n", dialect, errSum))
}
