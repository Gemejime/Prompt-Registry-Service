// Command prompt-registry-server runs the RAG Prompt Registry Control Plane daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"rag-prompt-registry-service/internal/application/command"
	"rag-prompt-registry-service/internal/application/query"
	"rag-prompt-registry-service/internal/config"
	"rag-prompt-registry-service/internal/domain/repository"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
	"rag-prompt-registry-service/internal/infrastructure/persistence/postgres"
	grpctransport "rag-prompt-registry-service/internal/transport/grpc"
	httptransport "rag-prompt-registry-service/internal/transport/http"
)

// version is injected at build time via -ldflags="-X main.version=...".
var version = "dev"

// serverReadiness implements httptransport.ReadinessChecker for the /ready endpoint.
type serverReadiness struct {
	pgPool      *pgxpool.Pool
	redisClient *redis.L2CacheClient
}

// CheckPostgres verifies PostgreSQL connection pool reachability when a pool is configured.
func (sr *serverReadiness) CheckPostgres(ctx context.Context) error {
	if sr.pgPool != nil {
		return sr.pgPool.Ping(ctx)
	}
	return nil
}

// CheckRedis verifies Redis cluster reachability when a Redis client is configured.
func (sr *serverReadiness) CheckRedis(ctx context.Context) error {
	if sr.redisClient != nil {
		return sr.redisClient.Ping(ctx)
	}
	return nil
}

// main initializes dependencies, starts the HTTP and gRPC control plane servers, and handles graceful shutdown.
func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		errLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
		errLogger.Error("failed to load environment file",
			slog.String("error_code", "CONFIG_DOTENV_LOAD_FAILED"),
			slog.String("detail", err.Error()),
		)
		os.Exit(1)
	}

	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		httpPort := config.GetEnv("HTTP_PORT", "8080")
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + httpPort + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	rawLogLevel := config.GetEnv("LOG_LEVEL", "INFO")
	logLevel, validLogLevel := config.ParseLogLevel(rawLogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)
	if !validLogLevel {
		logger.Warn("unrecognized LOG_LEVEL value, defaulting to INFO", slog.String("log_level", rawLogLevel))
	}

	httpPort := config.GetEnv("HTTP_PORT", "8080")
	grpcPort := config.GetEnv("GRPC_PORT", "9090")
	openAPIPath := config.GetEnv("OPENAPI_SPEC_PATH", "openapi/openapi.yaml")
	pgDSN := strings.TrimSpace(os.Getenv("POSTGRES_DSN"))
	connExecMode := config.GetEnv("CONN_EXEC_MODE", "simple_protocol")
	allowInMemoryStore := config.GetEnvBool("ALLOW_IN_MEMORY_STORE", false)
	pgMaxConns := int32(config.GetEnvInt("POSTGRES_MAX_CONNS", 25))
	pgMinConns := int32(config.GetEnvInt("POSTGRES_MIN_CONNS", 2))
	autoMigrate := config.GetEnvBool("POSTGRES_AUTO_MIGRATE", true)

	redisAddrs := config.ParseCSV(config.GetEnv("REDIS_CLUSTER_ADDRS", config.GetEnv("REDIS_ADDRS", "")))
	redisDB := config.GetEnvInt("REDIS_DB", 0)
	invalidationChan := config.GetEnv("REDIS_INVALIDATION_CHANNEL", redis.DefaultInvalidationChannel)
	defaultTenant := config.GetEnv("DEFAULT_TENANT_ID", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
	writeRPS := float64(config.GetEnvInt("ADMIN_WRITE_RATE_LIMIT_RPS", 50))
	writeBurst := config.GetEnvInt("ADMIN_WRITE_RATE_LIMIT_BURST", 100)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		promptRepo repository.PromptRepository
		pgPool     *pgxpool.Pool
	)

	if pgDSN != "" {
		if !strings.EqualFold(connExecMode, "simple_protocol") {
			logger.Warn("CONN_EXEC_MODE is not simple_protocol; PgBouncer transaction pooling requires simple_protocol (ADR-003)",
				slog.String("conn_exec_mode", connExecMode),
			)
		}
		pool, err := postgres.NewPool(ctx, postgres.PoolOptions{
			DSN:          pgDSN,
			ConnExecMode: connExecMode,
			MaxConns:     pgMaxConns,
			MinConns:     pgMinConns,
		})
		if err != nil {
			logger.Error("failed to initialize postgres pool", slog.String("error_code", "DB_INIT_FAILED"))
			os.Exit(1)
		}
		pgPool = pool
		defer pgPool.Close()

		if autoMigrate {
			if err := postgres.ApplyMigrations(ctx, pgPool); err != nil {
				logger.Error("failed to apply database migrations", slog.String("error_code", "DB_MIGRATION_FAILED"))
				os.Exit(1)
			}
		}
		promptRepo = postgres.NewPromptRepository(pgPool)
	} else if allowInMemoryStore {
		logger.Warn("using non-durable in-memory store because POSTGRES_DSN is unset and ALLOW_IN_MEMORY_STORE=true; all data will be lost on restart",
			slog.String("storage_mode", "in_memory_non_durable"),
		)
		promptRepo = postgres.NewTransactionalMemoryStore()
	} else {
		logger.Error("POSTGRES_DSN is required unless ALLOW_IN_MEMORY_STORE=true is explicitly set",
			slog.String("error_code", "CONFIG_MISSING_POSTGRES_DSN"),
		)
		os.Exit(1)
	}

	l2Cache := redis.NewClusterClient(redis.ClientOptions{
		Addrs:    redisAddrs,
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       redisDB,
	})
	defer func() { _ = l2Cache.Close() }()

	invalidator := redis.NewInvalidationBroadcaster(l2Cache, invalidationChan)
	mutationMetrics := command.NewMutationMetricsRecorder()

	publishHandler := command.NewPublishVersionHandler(promptRepo, l2Cache, logger, mutationMetrics)
	promoteHandler := command.NewPromoteTagHandler(promptRepo, l2Cache, invalidator, logger, mutationMetrics)
	getASTHandler := query.NewGetTemplateASTHandler(promptRepo, l2Cache)
	listHandler := query.NewListTemplatesHandler(promptRepo)

	httpRouter := httptransport.NewRouter(httptransport.RouterConfig{
		PublishHandler: publishHandler,
		PromoteHandler: promoteHandler,
		GetASTHandler:  getASTHandler,
		ListHandler:    listHandler,
		MutationMetric: mutationMetrics,
		Invalidator:    invalidator,
		Readiness:      &serverReadiness{pgPool: pgPool, redisClient: l2Cache},
		DefaultTenant:  defaultTenant,
		OpenAPIPath:    openAPIPath,
		WriteRateRPS:   writeRPS,
		WriteRateBurst: writeBurst,
		Logger:         logger,
	})

	httpServer := &http.Server{
		Addr:              ":" + httpPort,
		Handler:           httpRouter,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	grpcAdmin := grpctransport.NewPromptAdminServer(
		publishHandler,
		promoteHandler,
		getASTHandler,
		listHandler,
		defaultTenant,
		logger,
	)
	grpcServer := grpctransport.NewGRPCServer(grpcAdmin)

	grpcListener, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		logger.Error("failed to bind gRPC listener", slog.String("port", grpcPort), slog.String("error_code", "GRPC_BIND_FAILED"))
		os.Exit(1)
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("starting control plane HTTP server", slog.String("port", httpPort), slog.String("version", version))
		if serveErr := httpServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server error: %w", serveErr)
		}
	}()

	go func() {
		logger.Info("starting control plane gRPC server", slog.String("port", grpcPort))
		if serveErr := grpcServer.Serve(grpcListener); serveErr != nil {
			errCh <- fmt.Errorf("grpc server error: %w", serveErr)
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections (15s timeout)")
	case fatalErr := <-errCh:
		logger.Error("server listener terminated unexpectedly", slog.String("error_code", "LISTENER_TERMINATED"), slog.String("detail", fatalErr.Error()))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var shutdownWg sync.WaitGroup
	shutdownWg.Add(2)

	go func() {
		defer shutdownWg.Done()
		grpcDone := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(grpcDone)
		}()
		select {
		case <-grpcDone:
		case <-shutdownCtx.Done():
			logger.Warn("grpc server graceful shutdown timed out, forcing stop", slog.String("error_code", "GRPC_SHUTDOWN_TIMEOUT"))
			grpcServer.Stop()
		}
	}()

	go func() {
		defer shutdownWg.Done()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("http server graceful shutdown error", slog.String("error_code", "HTTP_SHUTDOWN_ERROR"))
		}
	}()

	shutdownWg.Wait()
	logger.Info("rag-prompt-registry-service control plane shut down cleanly")
}
