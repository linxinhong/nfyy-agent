// Package team implements multi-agent team orchestration.
package team

import (
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// TeamConfig contains everything needed to create a Team.
type TeamConfig struct {
	// ID is the unique team identifier.
	ID string
	// Name is the human-readable team name.
	Name string
	// Model is the coordinator model that delegates to members.
	Model models.Model
	// Members are the agents that make up the team.
	Members []*agent.Agent
	// Instructions describe the team's goals and behavior.
	Instructions string
	// DB is an optional persistent session store.
	DB db.TeamStore
	// SessionState is the initial session state for new sessions.
	SessionState map[string]any
	// UserID is the active user identifier.
	UserID string
	// SessionID is the active session identifier.
	SessionID string
	// MaxIterations caps the number of delegation rounds (coordinator turns
	// that call tools).
	MaxIterations int
	// RetryConfig, FallbackModels, and CircuitBreaker apply to coordinator
	// model calls exactly as they do for an Agent.
	RetryConfig    agent.RetryConfig
	FallbackModels []models.Model
	CircuitBreaker *agent.CircuitBreakerConfig
	// UsageGuard and MaxRunTokens enforce quotas on coordinator calls;
	// members enforce their own.
	UsageGuard   agent.UsageGuard
	MaxRunTokens int
	// DelegationTimeout bounds one delegation to a member. Zero selects 30
	// minutes; the run context still applies.
	DelegationTimeout time.Duration
	// RunLeaseTimeout enables crash detection and cross-replica
	// cancellation when DB implements db.RunLeaseStore, as for an Agent.
	// Zero selects 2 minutes; negative disables leases.
	RunLeaseTimeout time.Duration
}

// New creates a new Team after validating the configuration.
func New(cfg TeamConfig) (*Team, error) {
	if cfg.Model == nil {
		return nil, errors.New("team: Model is required")
	}
	if len(cfg.Members) == 0 {
		return nil, errors.New("team: at least one Member is required")
	}
	if cfg.DelegationTimeout < 0 {
		return nil, errors.New("team: DelegationTimeout must not be negative")
	}
	cfg.setDefaults()
	return &Team{cfg: cfg}, nil
}

func (cfg *TeamConfig) setDefaults() {
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 10
	}
	if cfg.DelegationTimeout == 0 {
		cfg.DelegationTimeout = 30 * time.Minute
	}
}
