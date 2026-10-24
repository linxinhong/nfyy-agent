// Package os provides the AgentOS HTTP server and route handlers.
package os

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	mcpserver "github.com/linxinhong/xingding-agent-runtime/pkg/mcp/server"
	memorycore "github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/middleware"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/agents"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/knowledge"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/runs"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/sessions"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/teams"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/workflows"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/service"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
)

// AgentOS is the minimal AgentOS HTTP server.
type AgentOS struct {
	cfg     AgentOSConfig
	mux     *http.ServeMux
	handler http.Handler

	runs     *runmanager.Manager
	streams  *service.StreamHub
	draining atomic.Bool

	mu              sync.Mutex
	server          *http.Server
	stopMaintenance context.CancelFunc
	maintenance     sync.WaitGroup
}

// maxRequestBodyBytes caps HTTP request bodies (1 MiB). Larger bodies are
// rejected with 413 before routing (P1-5: DoS guard).
const maxRequestBodyBytes = 1 << 20

// New creates an AgentOS from cfg and registers all routes.
func New(cfg AgentOSConfig) (*AgentOS, error) {
	if cfg.Memory != nil {
		for _, configuredAgent := range cfg.Agents {
			if configuredAgent != nil && configuredAgent.UsesLegacyMemory() {
				return nil, errors.Join(agent.ErrConflictingMemoryConfiguration, errors.New("AgentOS strict memory cannot be combined with an Agent legacy memory manager"))
			}
		}
	}
	if cfg.Project != nil {
		project := *cfg.Project
		cfg.Project = &project
	}
	if cfg.Memory != nil {
		memoryConfig := *cfg.Memory
		if cfg.Memory.Recall != nil {
			recall := *cfg.Memory.Recall
			recall.Kinds = append([]memorycore.Kind(nil), recall.Kinds...)
			recall.Topics = append([]string(nil), recall.Topics...)
			memoryConfig.Recall = &recall
		}
		cfg.Memory = &memoryConfig
	}
	os := &AgentOS{
		cfg: cfg,
		mux: http.NewServeMux(),
	}

	authCfg := cfg.Authorization
	authCfg.SecurityKey = cfg.SecurityKey
	auth := middleware.AuthMiddleware(authCfg)
	runTimeout := cfg.RunTimeout
	if runTimeout <= 0 {
		runTimeout = 120 * time.Second
	}
	runManager := runmanager.New(cfg.DB, runTimeout).SetAdmission(cfg.Admission)
	var streams *service.StreamHub
	if !cfg.DisableStreamResume {
		streams = service.NewStreamHub(cfg.Streams)
	}
	os.runs, os.streams = runManager, streams

	// Base routes.
	os.registerBaseRoutes(auth)

	// Agent routes under /agents.
	agentsHandler := agents.NewRouterWithConfig(agents.Config{Agents: cfg.Agents, DB: cfg.DB, Runs: runManager, RequirePrincipal: cfg.Memory != nil, Streams: streams, RunTimeout: runTimeout})
	os.mux.Handle("/agents/", auth(stripPrefix("/agents", agentsHandler)))
	os.mux.Handle("/agents", auth(stripPrefix("/agents", agentsHandler)))

	// Team routes under /teams.
	teamsHandler := teams.NewRouterWithConfig(teams.Config{Teams: cfg.Teams, DB: cfg.DB, Runs: runManager, RequirePrincipal: cfg.Memory != nil, Streams: streams, RunTimeout: runTimeout})
	os.mux.Handle("/teams/", auth(stripPrefix("/teams", teamsHandler)))
	os.mux.Handle("/teams", auth(stripPrefix("/teams", teamsHandler)))

	// Workflow routes under /workflows.
	workflowsHandler := workflows.NewRouterWithConfig(workflows.Config{Workflows: cfg.Workflows, DB: cfg.DB, Runs: runManager, RequirePrincipal: cfg.Memory != nil, Streams: streams, RunTimeout: runTimeout})
	os.mux.Handle("/workflows/", auth(stripPrefix("/workflows", workflowsHandler)))
	os.mux.Handle("/workflows", auth(stripPrefix("/workflows", workflowsHandler)))

	// Session routes under /sessions.
	sessionsHandler := sessions.NewRouter(cfg.DB)
	os.mux.Handle("/sessions/", auth(stripPrefix("/sessions", sessionsHandler)))
	os.mux.Handle("/sessions", auth(stripPrefix("/sessions", sessionsHandler)))

	// Run lookup route under /runs.
	runsHandler := runs.NewRouterWithStreams(cfg.DB, runManager, streams)
	os.mux.Handle("/runs/", auth(stripPrefix("/runs", runsHandler)))
	os.mux.Handle("/runs", auth(stripPrefix("/runs", runsHandler)))

	// Memory routes under /memories.
	memoryConfig := memory.Config{}
	if cfg.Memory != nil {
		memoryConfig = memory.Config{Module: cfg.Memory.Module, ProjectID: cfg.Memory.ProjectID, CollectionID: cfg.Memory.CollectionID, ProposeEnabled: cfg.Memory.ProposeEnabled, SourceResolver: cfg.Memory.SourceResolver}
	}
	memoryHandler := memory.NewRouter(memoryConfig)
	os.mux.Handle("/memories/", auth(stripPrefix("/memories", memoryHandler)))
	os.mux.Handle("/memories", auth(stripPrefix("/memories", memoryHandler)))

	// Knowledge routes under /knowledge.
	knowledgeHandler := knowledge.NewRouter(cfg.Knowledges)
	os.mux.Handle("/knowledge/", auth(stripPrefix("/knowledge", knowledgeHandler)))
	os.mux.Handle("/knowledge", auth(stripPrefix("/knowledge", knowledgeHandler)))

	// MCP server routes under /mcp.
	if len(cfg.MCPServers) > 0 {
		for name, srv := range cfg.MCPServers {
			mcpHandler := mcpserver.ServeHTTPWithServer(srv)
			prefix := "/mcp/" + name
			os.mux.Handle(prefix+"/", auth(stripPrefix(prefix, mcpHandler)))
			os.mux.Handle(prefix, auth(stripPrefix(prefix, mcpHandler)))
		}
	}

	// Reject oversized request bodies before routing (P1-5 DoS guard).
	os.handler = limitRequestBody(os.mux)

	return os, nil
}

// limitRequestBody wraps a handler so request bodies larger than
// maxRequestBodyBytes fail with 413.
func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// stripPrefix strips the given prefix and guarantees the remaining path starts
// with "/" so the inner mux routes work for both /prefix and /prefix/.
func stripPrefix(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch p {
		case prefix, prefix + "/":
			r.URL.Path = "/"
		default:
			r.URL.Path = p[len(prefix):]
		}
		h.ServeHTTP(w, r)
	})
}

// Serve starts background maintenance and the HTTP server on addr. It
// returns http.ErrServerClosed after Shutdown.
func (os *AgentOS) Serve(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return os.ServeListener(listener)
}

// ServeListener is Serve on an existing listener.
func (os *AgentOS) ServeListener(listener net.Listener) error {
	os.Start(context.Background())
	server := &http.Server{Handler: os.handler, ReadHeaderTimeout: 30 * time.Second}
	os.mu.Lock()
	os.server = server
	os.mu.Unlock()
	return server.Serve(listener)
}

// Start launches background maintenance (see MaintenanceConfig) until
// Shutdown or ctx ends. It is idempotent; hosts that mount Handler in their
// own server call it themselves.
func (os *AgentOS) Start(ctx context.Context) {
	os.mu.Lock()
	defer os.mu.Unlock()
	if os.stopMaintenance != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	os.stopMaintenance = cancel
	m := os.cfg.Maintenance
	if os.streams != nil {
		os.maintenance.Add(1)
		go func() {
			defer os.maintenance.Done()
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				if _, err := os.streams.DeleteExpired(ctx); err != nil && m.OnError != nil && ctx.Err() == nil {
					m.OnError(err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if os.cfg.DB == nil {
		return
	}
	if m.StaleRunInterval >= 0 {
		os.maintenance.Add(1)
		go func() {
			defer os.maintenance.Done()
			runmanager.RunStaleRunReaper(ctx, os.cfg.DB, m.StaleRunInterval, m.OnError)
		}()
	}
	if m.SessionRetention > 0 {
		os.maintenance.Add(1)
		go func() {
			defer os.maintenance.Done()
			runmanager.RunSessionJanitor(ctx, os.cfg.DB, m.SessionRetention, m.SessionJanitorInterval, m.OnError)
		}()
	}
}

// Shutdown drains the instance: /health turns 503 so load balancers stop
// routing here, the HTTP server (when started by Serve) stops accepting
// connections and waits for in-flight requests, streamed and background
// runs get until ctx's deadline to finish and are cancelled after it, and
// background maintenance stops.
func (os *AgentOS) Shutdown(ctx context.Context) error {
	os.draining.Store(true)
	os.mu.Lock()
	server := os.server
	os.mu.Unlock()
	var err error
	if server != nil {
		err = server.Shutdown(ctx)
	}
	if os.streams != nil {
		os.streams.Shutdown(ctx)
	}
	if os.runs != nil {
		os.runs.Shutdown(ctx)
	}
	os.mu.Lock()
	if os.stopMaintenance != nil {
		os.stopMaintenance()
	}
	os.mu.Unlock()
	os.maintenance.Wait()
	return err
}

// Handler returns the configured http.Handler.
func (os *AgentOS) Handler() http.Handler {
	return os.handler
}
