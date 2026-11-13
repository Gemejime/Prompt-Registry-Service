package unit_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rag-prompt-registry-service/internal/domain/entity"
	"rag-prompt-registry-service/internal/infrastructure/cache/redis"
)

// readRESPArray reads a single RESP array command from a bufio.Reader.
func readRESPArray(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("expected RESP array, got %q", line)
	}
	count, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if !strings.HasPrefix(hdr, "$") {
			return nil, fmt.Errorf("expected RESP bulk string header, got %q", hdr)
		}
		n, err := strconv.Atoi(hdr[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:n]))
	}
	return args, nil
}

// startMockRedisServer starts an in-process TCP RESP mock node for testing cluster slot redirects.
func startMockRedisServer(t *testing.T, handler func(conn net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mock redis: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				handler(c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// TestRedisClusterRedirectMOVEDAndASK verifies that L2CacheClient authenticates with AUTH,
// follows -MOVED redirects on writes, and sends ASKING when following -ASK redirects on reads.
func TestRedisClusterRedirectMOVEDAndASK(t *testing.T) {
	const expectedPass = "cluster-test-pass"

	var (
		mu         sync.Mutex
		store      = make(map[string]string)
		askingSeen bool
		shardAuth  bool
		coordAuth  bool
	)

	shardAddr := startMockRedisServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		askingNext := false
		connAuthed := false
		for {
			args, err := readRESPArray(r)
			if err != nil || len(args) == 0 {
				return
			}
			cmd := strings.ToUpper(args[0])
			switch cmd {
			case "AUTH":
				if len(args) == 2 && args[1] == expectedPass {
					connAuthed = true
					mu.Lock()
					shardAuth = true
					mu.Unlock()
					_, _ = conn.Write([]byte("+OK\r\n"))
				} else {
					_, _ = conn.Write([]byte("-ERR invalid password\r\n"))
				}
			case "ASKING":
				if !connAuthed {
					_, _ = conn.Write([]byte("-NOAUTH Authentication required.\r\n"))
					return
				}
				askingNext = true
				mu.Lock()
				askingSeen = true
				mu.Unlock()
				_, _ = conn.Write([]byte("+OK\r\n"))
			case "SET":
				if !connAuthed {
					_, _ = conn.Write([]byte("-NOAUTH Authentication required.\r\n"))
					return
				}
				mu.Lock()
				store[args[1]] = args[2]
				mu.Unlock()
				_, _ = conn.Write([]byte("+OK\r\n"))
			case "GET":
				if !connAuthed {
					_, _ = conn.Write([]byte("-NOAUTH Authentication required.\r\n"))
					return
				}
				if !askingNext {
					_, _ = conn.Write([]byte("-ERR expected ASKING before redirected GET\r\n"))
					return
				}
				askingNext = false
				mu.Lock()
				val, ok := store[args[1]]
				mu.Unlock()
				if !ok {
					_, _ = conn.Write([]byte("$-1\r\n"))
					continue
				}
				_, _ = fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(val), val)
			default:
				_, _ = conn.Write([]byte("+OK\r\n"))
			}
		}
	})

	coordAddr := startMockRedisServer(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		for {
			args, err := readRESPArray(r)
			if err != nil || len(args) == 0 {
				return
			}
			cmd := strings.ToUpper(args[0])
			switch cmd {
			case "AUTH":
				if len(args) == 2 && args[1] == expectedPass {
					mu.Lock()
					coordAuth = true
					mu.Unlock()
					_, _ = conn.Write([]byte("+OK\r\n"))
				} else {
					_, _ = conn.Write([]byte("-ERR invalid password\r\n"))
				}
			case "SET":
				_, _ = fmt.Fprintf(conn, "-MOVED 42 %s\r\n", shardAddr)
			case "GET":
				_, _ = fmt.Fprintf(conn, "-ASK 42 %s\r\n", shardAddr)
			default:
				_, _ = conn.Write([]byte("+OK\r\n"))
			}
		}
	})

	writer := redis.NewClusterClient(redis.ClientOptions{
		Addrs:    []string{coordAddr},
		Password: expectedPass,
	})
	reader := redis.NewClusterClient(redis.ClientOptions{
		Addrs:    []string{coordAddr},
		Password: expectedPass,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ver := &entity.PromptVersion{
		ID:         "ver-101",
		TemplateID: "tpl-cluster",
		Version:    3,
		RawAST: entity.TemplateAST{
			SystemPrompt:   "Answer concisely: {{context}}",
			VariableKeys:   []string{"context"},
			DefaultDialect: entity.DialectOpenAIStandard,
		},
		ASTHash: "abcdef0123456789",
		Author:  "cluster-test",
	}

	if err := writer.SetVersion(ctx, "tenant-a", ver); err != nil {
		t.Fatalf("SetVersion failed through -MOVED redirect: %v", err)
	}

	got, ok := reader.GetVersion(ctx, "tenant-a", ver.TemplateID, ver.Version)
	if !ok {
		t.Fatalf("GetVersion returned miss through -ASK redirect")
	}
	if got.ID != ver.ID || got.Version != ver.Version || got.ASTHash != ver.ASTHash {
		t.Fatalf("decoded PromptVersion mismatch: got %+v, want %+v", got, ver)
	}

	mu.Lock()
	defer mu.Unlock()
	if !coordAuth || !shardAuth {
		t.Fatalf("expected AUTH on both coordinator (%v) and target shard (%v)", coordAuth, shardAuth)
	}
	if !askingSeen {
		t.Fatalf("expected ASKING command before -ASK redirected GET")
	}
}

// TestRedisClusterDeliverablesPresentAndAligned validates that Helm values, ArgoCD manifests,
// Docker Compose config, deployment scripts, workflow secret names, and docs stay aligned.
func TestRedisClusterDeliverablesPresentAndAligned(t *testing.T) {
	root := filepath.Join("..", "..")

	checks := []struct {
		relPath  string
		contains []string
	}{
		{
			relPath: filepath.Join("deployments", "helm", "redis-cluster", "values.yaml"),
			contains: []string{
				`existingSecret: "rag-prompt-registry-service-secret"`,
				`existingSecretPasswordKey: "REDIS_PASSWORD"`,
				"init: true",
				"nodes: 6",
				"podAntiAffinityPreset: soft",
				"cpu: 100m",
				"memory: 256Mi",
				"cpu: 500m",
				"memory: 1Gi",
				"size: 5Gi",
				"port: 9121",
				`prometheus.io/scrape: "true"`,
			},
		},
		{
			relPath: filepath.Join("deployments", "argocd", "redis-cluster-application.yaml"),
			contains: []string{
				"kind: Application",
				"namespace: argocd",
				"https://charts.bitnami.com/bitnami",
				"chart: redis-cluster",
				"https://kubernetes.default.svc",
				"namespace: rag-platform",
				"prune: true",
				"selfHeal: true",
				"retry:",
			},
		},
		{
			relPath: filepath.Join("deployments", "docker", "docker-compose.redis.yml"),
			contains: []string{
				"bitnami/redis:7.2-debian-12",
				`"127.0.0.1:6379:6379"`,
				`REDIS_AOF_ENABLED: "yes"`,
				"redis-cli ping",
			},
		},
		{
			relPath: filepath.Join("scripts", "deploy_redis_cluster.sh"),
			contains: []string{
				"rag-prompt-registry-service",
				"${SERVICE_NAME}-redis",
				"rag-platform",
				"cluster info",
			},
		},
		{
			relPath: filepath.Join("scripts", "deploy_redis_cluster.ps1"),
			contains: []string{
				"rag-prompt-registry-service",
				"$ServiceName-redis",
				"rag-platform",
				"cluster info",
			},
		},
		{
			relPath: filepath.Join("deployments", "redis.md"),
			contains: []string{
				"rag-prompt-registry-service-redis-0.rag-prompt-registry-service-redis-headless.rag-platform.svc.cluster.local:6379",
				"redis-cli --latency",
				"9121",
			},
		},
		{
			relPath: filepath.Join(".github", "workflows", "deploy.yml"),
			contains: []string{
				"K8S_SECRET_NAME: rag-prompt-registry-service-secret",
			},
		},
	}

	for _, tc := range checks {
		raw, err := os.ReadFile(filepath.Join(root, tc.relPath))
		if err != nil {
			t.Fatalf("read %s: %v", tc.relPath, err)
		}
		text := string(raw)
		for _, want := range tc.contains {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing expected content %q", tc.relPath, want)
			}
		}
	}
}
