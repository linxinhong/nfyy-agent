// Package workflow implements sequential step-based workflow execution.
package workflow

import (
	"errors"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

// Config contains everything needed to create a Workflow.
type Config struct {
	// ID is the unique workflow identifier.
	ID string
	// Name is the human-readable workflow name.
	Name string
	// Steps are the workflow steps executed in order.
	Steps []Step
	// Definition is the serializable workflow executed by Workflow Core v1.
	Definition Definition
	// Registry resolves stable executor and predicate references.
	Registry *Registry
	// DB is an optional persistent session store.
	DB db.WorkflowStore
	// RunLeaseTimeout enables crash detection and cross-replica
	// cancellation when DB implements db.RunLeaseStore, as for an Agent:
	// runs heartbeat, run.ExpireStaleRuns fails runs of crashed processes,
	// and a run cancelled on another replica stops. Zero selects 2 minutes;
	// negative disables leases. Managed runs use dispatcher leases instead.
	RunLeaseTimeout time.Duration
	// ManagedResume explicitly enables claim-bound Workflow recovery. It is
	// independent from DB because the Workflow session adapter and the
	// ManagedStore may be separate views over the same database. When set,
	// Resume and ResumeEvents load and update their authoritative Run only
	// through this ManagedStore and never fall back to DB writes.
	ManagedResume *runmanager.ManagedConfig
	// SessionState is the initial session state for new sessions.
	SessionState map[string]any
	// UserID is the active user identifier.
	UserID string
	// SessionID is the active session identifier.
	SessionID string
	// Stream bounds buffered items, cumulative output bytes, and stalled-consumer
	// delivery time. Zero fields select finite safety defaults.
	Stream streamcfg.Config
	// Limits explicitly enables finite resource limits for structured workflow
	// definitions. Nil preserves the legacy behavior.
	Limits *Limits
}

// WorkflowConfig is retained as an alias while callers migrate to Config.
type WorkflowConfig = Config

// New creates a new Workflow.
func New(cfg Config) (*Workflow, error) {
	if err := cfg.Stream.Validate(); err != nil {
		return nil, errors.New("workflow: invalid stream config: " + err.Error())
	}
	if cfg.Definition.ID != "" || cfg.Definition.Root.ID != "" {
		if cfg.Registry == nil {
			return nil, errors.New("workflow: Registry is required")
		}
		if cfg.Limits != nil {
			limits, err := cfg.Limits.resolve()
			if err != nil {
				return nil, err
			}
			definition, err := prepareLimitedDefinition(cfg.Definition, limits)
			if err != nil {
				return nil, err
			}
			if err := validateDefinition(definition, cfg.Registry); err != nil {
				return nil, err
			}
			if err := validateMinimumErrorEnvelope(definition, limits); err != nil {
				return nil, err
			}
			cfg.Definition = definition
			cfg.Limits = &limits
		} else if err := validateDefinition(cfg.Definition, cfg.Registry); err != nil {
			return nil, err
		}
		return &Workflow{cfg: cfg, core: true}, nil
	}
	if cfg.Limits != nil {
		return nil, errors.New("workflow: Limits require a structured Definition")
	}
	if len(cfg.Steps) == 0 {
		return nil, errors.New("workflow: Definition or at least one step is required")
	}
	return &Workflow{cfg: cfg}, nil
}

// validateMinimumErrorEnvelope rejects a configuration that could create a
// run but could not durably describe its own terminal payload-limit failure.
func validateMinimumErrorEnvelope(def Definition, limits Limits) error {
	minimum := &session.RunOutput{
		WorkflowID: def.ID, Status: session.RunStatusError, Error: ErrPayloadLimitExceeded.Error(),
		Workflow: &workflowschema.RunData{SchemaVersion: 1, DefinitionVersion: def.Version, Definition: &def, NodeResults: map[string]NodeResult{}},
	}
	if err := validateRunEnvelope(minimum, limits.MaxRunEnvelopeBytes); err != nil {
		return fmt.Errorf("workflow: Limits.MaxRunEnvelopeBytes cannot hold a minimum terminal error snapshot: %w", err)
	}
	return nil
}
