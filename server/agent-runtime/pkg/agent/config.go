// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// AgentConfig contains everything needed to create an Agent.
type AgentConfig struct {
	// ID is the unique agent identifier.
	ID string
	// Name is the human-readable agent name.
	Name string
	// Instructions are the system instructions sent to the model.
	Instructions string
	// Model is the language model used for responses.
	Model models.Model
	// Tools are the tools available to the agent.
	Tools []tools.Tool
	// ToolService is the managed execution entry point (RC-05). When set,
	// tool invocations flow through ToolService.Prepare/Execute (authorization,
	// approval, audit) instead of raw Tool.Call. The trusted principal must be
	// present in the run context (service.ContextWithPrincipal).
	ToolService service.ToolService
	// Memory is the legacy single-SDK memory manager. Callers must bind its DB
	// adapter explicitly with memory.NewManager. Multi-user AgentOS projects use
	// the strict ProjectSpec memory dependency instead; dual wiring is rejected.
	Memory memory.Manager
	// Knowledge is an optional knowledge base for retrieval.
	Knowledge knowledge.Knowledge
	// DB is an optional persistent session store.
	DB db.AgentStore
	// SessionID is the active session identifier.
	SessionID string
	// UserID is the active user identifier.
	UserID string
	// MaxToolCalls is the maximum number of tool-call rounds: model turns
	// that request tools (one round may contain several parallel calls).
	// A run that still requests tools after this many rounds fails with
	// ErrMaxToolRounds. Zero selects the default of 10.
	MaxToolCalls int
	// Temperature is the optional sampling temperature.
	Temperature *float32
	// MaxTokens is the optional maximum tokens per model response.
	MaxTokens *int
	// OutputSchema is an optional schema for structured output.
	OutputSchema any
	// SessionState is the initial session state merged into new sessions.
	SessionState map[string]any
	// ReasoningModel, etc. can be added later.

	// RetryConfig controls the retry policy for model calls.
	RetryConfig RetryConfig
	// FallbackModels are models to try if the primary model fails.
	FallbackModels []models.Model
	// Hooks are optional lifecycle hooks for the run.
	Hooks *Hooks
	// RunMiddlewares wrap each synchronous model call in the run loop.
	// The first middleware in the slice is outermost (runs first).
	// When both Hooks and RunMiddlewares are set, Hooks are adapted as
	// middleware and placed innermost.
	RunMiddlewares []RunMiddleware
	// StreamMiddlewares wrap each streaming model call in the run loop.
	StreamMiddlewares []StreamMiddleware
	// ToolMiddlewares intercept individual tool invocations. When
	// ToolMiddlewares and Hooks coexist, hooks run before middlewares.
	ToolMiddlewares []ToolMiddleware
	// ToolChoice controls how the model selects tools.
	ToolChoice *models.ToolChoice
	// ContextProviders transform the assembled message list (system, history,
	// current input/run) during initial execution and resume assembly, in order.
	// Providers implementing PerModelCallContextProvider are additionally
	// rechecked before later model calls in a tool loop. nil entries are skipped.
	ContextProviders []ContextProvider
	// ToolTimeout is the per-tool-call timeout.
	ToolTimeout time.Duration
	// MaxParallelTools bounds concurrent execution of consecutive read-only
	// tools (tools.WithReadOnly / tools.ReadOnlyAware) within one model
	// round. 0 or 1 executes sequentially. Managed (ToolService) runs are
	// always sequential. Tool middlewares must be concurrency-safe when > 1.
	MaxParallelTools int
	// MaxToolResultBytes caps each tool result passed to the model; longer
	// results are truncated with a marker. Zero selects 256 KiB; negative
	// disables the cap.
	MaxToolResultBytes int
	// ToolErrorFormatter renders tool failures for the model. Nil uses
	// "error: <message>" truncated to 2 KiB. Hosts use it to redact internal
	// details (DSNs, hostnames) before they reach the model and transcript.
	ToolErrorFormatter func(call models.ToolCall, status session.ToolExecutionStatus, err error) string
	// RunTimeout is the maximum duration of one run. Zero preserves the caller's context.
	RunTimeout time.Duration
	// Stream bounds buffered items, cumulative output bytes, and stalled-consumer
	// delivery time. Zero fields select finite safety defaults.
	Stream streamcfg.Config
	// MaxConcurrentRuns limits active runs for this Agent. Zero is unlimited.
	MaxConcurrentRuns int
	// Admission is an optional controller shared by Agents and a Manager
	// assembled for the same runtime; its limits are process-local unless
	// AdmissionConfig.Cluster adds cluster-wide ones. Nil preserves unlimited
	// shared admission; MaxConcurrentRuns remains this Agent's fail-fast
	// local cap.
	Admission *run.AdmissionController

	// SessionLocker serializes runs (and resumes) on the same session so
	// concurrent requests cannot read stale history and interleave their
	// transcripts. Nil uses an in-process locker private to this Agent; share
	// one NewSessionLocker across Agents of a process, or supply a
	// distributed implementation (e.g. Redis or a database lock) when
	// several replicas serve the same sessions.
	SessionLocker SessionLocker
	// SessionLockTimeout bounds the wait for a busy session; the run then
	// fails with ErrSessionBusy. Zero waits as long as the run context allows.
	SessionLockTimeout time.Duration
	// MaxHistoryRuns bounds how many of the session's most recent runs are
	// loaded as history. Stores implementing db.RecentSessionReader load
	// only those runs instead of the whole session on every request. Zero
	// loads every run.
	MaxHistoryRuns int
	// ContextBudget bounds the assembled context. Nil derives a budget from
	// the model's advertised context window (models.ContextLimits), reserving
	// room for the response, unless a CompactionContextProvider is already
	// configured. A policy with MaxInputTokens 0 disables the default.
	ContextBudget *ContextWindowPolicy
	// ContextSummarizer, when set, lets the default context budget
	// summarize old turns instead of only dropping them; see
	// NewModelSummarizer. Ignored when ContextProviders already include a
	// CompactionContextProvider.
	ContextSummarizer Summarizer
	// LegacySystemContext places session state, memories, knowledge, and the
	// current time in the leading system message, as earlier versions did.
	// By default they are sent in a separate system message after the
	// history, so the instructions and conversation form a stable prefix
	// that provider prompt caches (DeepSeek, OpenAI, Anthropic) can reuse.
	LegacySystemContext bool
	// RunLeaseTimeout enables crash detection when DB implements
	// db.RunLeaseStore: runs record the lease, heartbeat every quarter of it,
	// and run.ExpireStaleRuns fails runs whose process died. A heartbeat or
	// round checkpoint that finds the run no longer running (cancelled from
	// another replica through run.Manager.Cancel) cancels the local run.
	// Zero selects 2 minutes; negative disables leases.
	RunLeaseTimeout time.Duration
	// DisableRoundCheckpoints stops persisting the in-progress transcript
	// after each tool round. Checkpoints keep a crashed run's progress
	// inspectable and detect external cancellation between rounds.
	DisableRoundCheckpoints bool
	// CircuitBreaker skips a model after consecutive retryable failures and
	// goes straight to the fallbacks until a cooldown passes. Nil uses 5
	// failures and 30s when FallbackModels are configured.
	CircuitBreaker *CircuitBreakerConfig
	// MaxRunTokens stops a run with ErrQuotaExceeded before a model call
	// once the run has used this many tokens. Zero is unlimited.
	MaxRunTokens int
	// UsageGuard enforces token/cost quotas across runs (per user,
	// namespace, ...); see NewLocalUsageQuota. Nil is unlimited.
	UsageGuard UsageGuard
}

// CircuitBreakerConfig configures per-model circuit breaking.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of consecutive retryable failures that
	// opens the circuit. Negative disables circuit breaking.
	FailureThreshold int
	// Cooldown is how long an open circuit skips the model before one
	// trial request is allowed through.
	Cooldown time.Duration
}

// RetryConfig controls model-call resilience.
type RetryConfig struct {
	// MaxRetries is the maximum number of retries for a model call.
	MaxRetries int
	// RetryDelay is the initial delay between retries.
	RetryDelay time.Duration
	// RetryMultiplier grows the delay between retries.
	RetryMultiplier float64
}

func (cfg *AgentConfig) setDefaults() {
	if cfg.MaxToolCalls == 0 {
		cfg.MaxToolCalls = 10
	}
	if cfg.ToolTimeout == 0 {
		cfg.ToolTimeout = 30 * time.Second
	}
}
