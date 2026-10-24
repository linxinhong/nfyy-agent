// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrBusy reports that an Agent has reached its configured concurrent run limit.
var ErrBusy = errors.New("agent is busy")

// ErrConflictingMemoryConfiguration rejects ambiguous double injection through
// the legacy Manager and the strict per-call memory context provider.
var ErrConflictingMemoryConfiguration = errors.New("agent: legacy and strict memory cannot be configured together")

// Agent is the primary execution unit.
type Agent struct {
	cfg          AgentConfig
	outputSchema *jsonschema.Schema
	runSlots     chan struct{}
	// Middleware slices retained for lazy chain building.
	runMWs    []RunMiddleware
	streamMWs []StreamMiddleware
	// Merged tool middlewares (legacy Hooks + ToolMiddlewares).
	toolMWs []ToolMiddleware
	// toolSet is the validated tool catalog, computed once.
	toolSet *toolSet
	// defaults holds the mutable default identity behind the deprecated
	// SetUserID/SetSessionID; shared by request-scoped copies.
	defaults *runDefaults
	// sessionLocker serializes runs per session; shared by request copies.
	sessionLocker SessionLocker
	// breakers holds per-model circuit state; nil when disabled.
	breakers *modelBreakers
	// calibration scales the default context budget's token estimates by
	// provider-reported usage; nil without a default budget.
	calibration *calibratedCounter
}

// runDefaults guards the default user/session identity so the deprecated
// setters cannot race with runs copying the Agent.
type runDefaults struct {
	mu        sync.RWMutex
	userID    string
	sessionID string
}

// New creates a new Agent.
func New(cfg AgentConfig) (*Agent, error) {
	if cfg.Model == nil {
		return nil, errors.New("agent: Model is required")
	}
	if cfg.ToolTimeout < 0 || cfg.RunTimeout < 0 || cfg.MaxConcurrentRuns < 0 || cfg.MaxToolCalls < 0 || cfg.MaxParallelTools < 0 {
		return nil, errors.New("agent: execution limits must not be negative")
	}
	if cfg.Memory != nil {
		for _, provider := range cfg.ContextProviders {
			if _, ok := provider.(interface{ strictMemoryContextProvider() }); ok {
				return nil, ErrConflictingMemoryConfiguration
			}
		}
	}
	if err := cfg.Stream.Validate(); err != nil {
		return nil, fmt.Errorf("agent: invalid stream config: %w", err)
	}
	if cfg.SessionLockTimeout < 0 || cfg.MaxHistoryRuns < 0 {
		return nil, errors.New("agent: execution limits must not be negative")
	}
	cfg.setDefaults()
	set, err := buildToolSet(cfg)
	if err != nil {
		return nil, err
	}
	calibration := newCalibratedCounter(set.schemas)
	budget, err := defaultContextBudget(cfg, calibration)
	if err != nil {
		return nil, err
	}
	if budget == nil {
		calibration = nil
	} else {
		// Copy so the caller's slice is never appended to in place.
		cfg.ContextProviders = append(append([]ContextProvider(nil), cfg.ContextProviders...), budget)
	}
	// 归一化 typed-nil：调用方常以 nil map/slice 经 any 传入 OutputSchema，
	// 此时 `cfg.OutputSchema == nil` 为假，会让无 schema 的 Agent 误入结构化
	// 解析分支并对 nil schema 做 Validate。这里统一折算为非 nil 接口。
	if isNilOutputSchema(cfg.OutputSchema) {
		cfg.OutputSchema = nil
	}
	compiled, err := compileOutputSchema(cfg.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("agent: compile output schema: %w", err)
	}
	var slots chan struct{}
	if cfg.MaxConcurrentRuns > 0 {
		slots = make(chan struct{}, cfg.MaxConcurrentRuns)
	}
	toolMWs := MergeToolMiddlewares(cfg.Hooks, cfg.ToolMiddlewares)
	locker := cfg.SessionLocker
	if locker == nil {
		locker = NewSessionLocker()
	}
	return &Agent{
		cfg:           cfg,
		outputSchema:  compiled,
		runSlots:      slots,
		runMWs:        cfg.RunMiddlewares,
		streamMWs:     cfg.StreamMiddlewares,
		toolMWs:       toolMWs,
		toolSet:       set,
		defaults:      &runDefaults{userID: cfg.UserID, sessionID: cfg.SessionID},
		sessionLocker: locker,
		breakers:      newModelBreakers(cfg),
		calibration:   calibration,
	}, nil
}

// ID returns the agent identifier.
func (a *Agent) ID() string {
	if a.cfg.ID != "" {
		return a.cfg.ID
	}
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return "agent"
}

// Name returns the agent name.
func (a *Agent) Name() string {
	if a.cfg.Name != "" {
		return a.cfg.Name
	}
	return a.ID()
}

// UsesLegacyMemory reports whether this immutable Agent was assembled with
// the legacy memory Manager. AgentOS uses it to reject ambiguous dual wiring
// with the strict project-scoped memory capability.
func (a *Agent) UsesLegacyMemory() bool {
	return a != nil && a.cfg.Memory != nil
}

// SetUserID updates the default user identifier for subsequent runs.
//
// Deprecated: mutating a shared Agent is error-prone in multi-user hosts;
// pass session.RunRequest.UserID to RunWith/RunStreamWith instead.
func (a *Agent) SetUserID(userID string) {
	if a.defaults == nil {
		a.cfg.UserID = userID
		return
	}
	a.defaults.mu.Lock()
	a.defaults.userID = userID
	a.defaults.mu.Unlock()
}

// SetSessionID updates the default session identifier for subsequent runs.
//
// Deprecated: pass session.RunRequest.SessionID to RunWith/RunStreamWith.
func (a *Agent) SetSessionID(sessionID string) {
	if a.defaults == nil {
		a.cfg.SessionID = sessionID
		return
	}
	a.defaults.mu.Lock()
	a.defaults.sessionID = sessionID
	a.defaults.mu.Unlock()
}

// defaultIdentity returns the current default user and session.
func (a *Agent) defaultIdentity() (userID, sessionID string) {
	if a.defaults == nil {
		return a.cfg.UserID, a.cfg.SessionID
	}
	a.defaults.mu.RLock()
	defer a.defaults.mu.RUnlock()
	return a.defaults.userID, a.defaults.sessionID
}

// forRequest returns a request-scoped copy whose identity is the current
// defaults overridden by non-empty request fields. Runs only ever mutate
// the copy.
func (a *Agent) forRequest(userID, sessionID string) Agent {
	runAgent := *a
	runAgent.cfg.UserID, runAgent.cfg.SessionID = a.defaultIdentity()
	if userID != "" {
		runAgent.cfg.UserID = userID
	}
	if sessionID != "" {
		runAgent.cfg.SessionID = sessionID
	}
	return runAgent
}

// Run executes the agent synchronously and returns a RunOutput.
func (a *Agent) Run(ctx context.Context, input string) (*session.RunOutput, error) {
	return a.RunWith(ctx, session.RunRequest{Input: session.RunInput{Message: input}})
}

// requireNamespaceStore prevents a tenant-scoped context from reaching an
// adapter that cannot carry its trusted namespace through the legacy DB API.
func (a *Agent) requireNamespaceStore(ctx context.Context) error {
	if _, tenant := execution.NamespaceFromContext(ctx); tenant {
		if _, ok := a.cfg.DB.(db.NamespaceScopedStore); !ok {
			return db.ErrNamespaceStoreRequired
		}
	}
	return nil
}

func (a *Agent) acquireAdmission(ctx context.Context) (context.Context, *run.AdmissionPermit, error) {
	return a.cfg.Admission.Acquire(ctx)
}

// RunWith executes one immutable request. Empty request fields use the
// defaults supplied when the Agent was created.
func (a *Agent) RunWith(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
	if err := a.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	if !a.acquireRunSlot() {
		return nil, ErrBusy
	}
	defer a.releaseRunSlot()
	ctx, permit, err := a.acquireAdmission(ctx)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	if request.Metadata == nil {
		request.Metadata, _ = session.RunMetadataFromContext(ctx)
	}
	metadata, err := session.CloneRunMetadata(request.Metadata)
	if err != nil {
		return nil, err
	}
	request.Metadata = metadata
	ctx = session.ContextWithRunMetadata(ctx, metadata)
	runAgent := a.forRequest(request.UserID, request.SessionID)
	if runAgent.cfg.RunTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runAgent.cfg.RunTimeout)
		defer cancel()
	}
	return runAgent.start(ctx, request.Input.Message, request.RunID, request.ParentRunID, request.NodeID, metadata, emitter{})
}

// RunStream executes the agent and streams events through a channel.
func (a *Agent) RunStream(ctx context.Context, input string) (<-chan Event, error) {
	return a.RunStreamWith(ctx, session.RunRequest{Input: session.RunInput{Message: input}})
}

// RunStreamWith streams one immutable request.
func (a *Agent) RunStreamWith(ctx context.Context, request session.RunRequest) (<-chan Event, error) {
	if err := a.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	if !a.acquireRunSlot() {
		return nil, ErrBusy
	}
	ctx, permit, admissionErr := a.acquireAdmission(ctx)
	if admissionErr != nil {
		a.releaseRunSlot()
		return nil, admissionErr
	}
	if request.Metadata == nil {
		request.Metadata, _ = session.RunMetadataFromContext(ctx)
	}
	metadata, err := session.CloneRunMetadata(request.Metadata)
	if err != nil {
		permit.Release()
		a.releaseRunSlot()
		return nil, err
	}
	request.Metadata = metadata
	ctx = session.ContextWithRunMetadata(ctx, metadata)
	runAgent := a.forRequest(request.UserID, request.SessionID)
	streamCtx, cancel := context.WithCancel(ctx)
	writer, ch := newEventStream(streamCtx, cancel, runAgent.cfg.Stream)
	go func() {
		var runErr error
		defer func() { writer.Finish(runErr) }()
		defer permit.Release()
		defer a.releaseRunSlot()
		if runAgent.cfg.RunTimeout > 0 {
			var timeoutCancel context.CancelFunc
			streamCtx, timeoutCancel = context.WithTimeout(streamCtx, runAgent.cfg.RunTimeout)
			defer timeoutCancel()
		}
		_, runErr = runAgent.start(streamCtx, request.Input.Message, request.RunID, request.ParentRunID, request.NodeID, metadata, emitter{w: &writer})
	}()
	return ch, nil
}

func newEventStream(ctx context.Context, cancel context.CancelFunc, cfg streamcfg.Config) (streamtransport.Writer[Event], <-chan Event) {
	return streamtransport.New(ctx, cancel, cfg, agentEventBytes, newAgentStreamFailure)
}

func newAgentStreamFailure(cause error) Event {
	return Event{Type: EventTypeRunError, Err: boundedStreamError(cause)}
}

func agentEventKind(event Event) streamtransport.Kind {
	switch event.Type {
	case EventTypeRunCompleted, EventTypeRunError, EventTypeRunPaused:
		return streamtransport.Terminal
	default:
		return streamtransport.Data
	}
}

func (a *Agent) acquireRunSlot() bool {
	if a.runSlots == nil {
		return true
	}
	select {
	case a.runSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *Agent) releaseRunSlot() {
	if a.runSlots != nil {
		<-a.runSlots
	}
}
