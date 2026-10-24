// Package os provides a minimal AgentOS HTTP runtime for Agno agents.
package os

import (
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
	mcpserver "github.com/linxinhong/xingding-agent-runtime/pkg/mcp/server"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/middleware"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/service"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

// AuthorizationConfig controls JWT and security-key authentication.
type AuthorizationConfig = middleware.AuthorizationConfig

// ProjectInfo identifies the assembled project hosted by AgentOS.
type ProjectInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	SchemaVersion int    `json:"schema_version"`
	SpecHash      string `json:"spec_hash"`
}

// AgentOSConfig is the top-level configuration for AgentOS.
type AgentOSConfig struct {
	Project    *ProjectInfo
	Agents     map[string]*agent.Agent
	Teams      map[string]*team.Team
	Workflows  map[string]*workflow.Workflow
	Models     map[string]models.Model
	Knowledges map[string]knowledge.Knowledge
	DB         db.DB
	// Admission is an optional run controller shared by Agent instances and
	// the AgentOS background Manager. Its limits are process-local unless
	// AdmissionConfig.Cluster adds cluster-wide ones (e.g.
	// postgres.DB.ClusterAdmission).
	Admission     *run.AdmissionController
	Authorization AuthorizationConfig
	SecurityKey   string
	// Memory is the strict project-scoped memory capability. Nil is an explicit
	// disabled mode; AgentOS never derives it from DB.
	Memory *MemoryConfig
	// MCPServers are optional MCP servers embedded into AgentOS. Each server
	// is exposed at /mcp/<name> via Streamable HTTP.
	MCPServers map[string]*mcpserver.Server
	// Streams configures SSE journaling: streamed runs survive a client
	// disconnect for DisconnectGrace and can be resumed through
	// GET /runs/{run_id}/events with Last-Event-ID. Without
	// Streams.Journal the journal is process-local and reconnects need
	// sticky routing by run ID; with a shared one (e.g.
	// postgres.DB.StreamJournal) any replica can serve them.
	Streams service.StreamHubConfig
	// DisableStreamResume binds streamed runs to the request connection as
	// earlier versions did: a disconnect cancels the run immediately.
	DisableStreamResume bool
	// RunTimeout bounds synchronous, streamed, and background runs started
	// over HTTP. Zero selects 120s; reasoning models with long tool loops
	// usually need more.
	RunTimeout time.Duration
	// Maintenance configures background upkeep that Start (and Serve) run
	// on every replica.
	Maintenance MaintenanceConfig
}

// MaintenanceConfig configures AgentOS background upkeep. Each task needs
// a DB implementing the matching capability and is skipped otherwise.
type MaintenanceConfig struct {
	// StaleRunInterval is how often runs of crashed processes are failed
	// (db.RunLeaseStore). Zero selects one minute; negative disables it.
	StaleRunInterval time.Duration
	// SessionRetention deletes sessions idle longer than this
	// (db.SessionRetentionStore). Zero keeps sessions forever.
	SessionRetention time.Duration
	// SessionJanitorInterval is how often retention runs. Zero selects one
	// hour.
	SessionJanitorInterval time.Duration
	// OnError receives background errors; nil drops them.
	OnError func(error)
}

// MemoryConfig is a frozen project mapping shared by HTTP management and Agent
// recall. Namespace and subject are deliberately resolved per request.
type MemoryConfig struct {
	Module         *memory.Module
	ProjectID      string
	CollectionID   string
	Recall         *MemoryRecallConfig
	ProposeEnabled bool
	SourceResolver memory.ProposalSourceResolver
}

type MemoryRecallConfig struct {
	Limit       int
	Kinds       []memory.Kind
	Topics      []string
	FailureMode agent.MemoryRecallFailureMode
}
