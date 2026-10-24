package agent

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

const (
	defaultBreakerThreshold = 5
	defaultBreakerCooldown  = 30 * time.Second
)

// modelBreakers tracks consecutive retryable failures per candidate model
// (by position) so an outage of the primary does not cost every request a
// full retry cycle before reaching a fallback. Shared by request copies.
type modelBreakers struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	modelIDs []string

	mu    sync.Mutex
	state []breakerState
}

type breakerState struct {
	failures  int
	openUntil time.Time
	probing   bool
}

// newModelBreakers returns nil (disabled) without fallbacks: skipping the
// only model would just turn an outage into an immediate error.
func newModelBreakers(cfg AgentConfig) *modelBreakers {
	if len(cfg.FallbackModels) == 0 {
		return nil
	}
	threshold, cooldown := defaultBreakerThreshold, defaultBreakerCooldown
	if cfg.CircuitBreaker != nil {
		if cfg.CircuitBreaker.FailureThreshold < 0 {
			return nil
		}
		if cfg.CircuitBreaker.FailureThreshold > 0 {
			threshold = cfg.CircuitBreaker.FailureThreshold
		}
		if cfg.CircuitBreaker.Cooldown > 0 {
			cooldown = cfg.CircuitBreaker.Cooldown
		}
	}
	ids := make([]string, 0, 1+len(cfg.FallbackModels))
	for _, m := range append([]models.Model{cfg.Model}, cfg.FallbackModels...) {
		id := ""
		if m != nil {
			id = m.ID()
		}
		ids = append(ids, id)
	}
	return &modelBreakers{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		modelIDs:  ids,
		state:     make([]breakerState, 1+len(cfg.FallbackModels)),
	}
}

// allow reports whether candidate i may be called. After the cooldown one
// probe is let through; its outcome closes or re-opens the circuit.
func (b *modelBreakers) allow(i int) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &b.state[i]
	if s.failures < b.threshold {
		return true
	}
	if s.probing || b.now().Before(s.openUntil) {
		return false
	}
	s.probing = true
	return true
}

// record updates candidate i with the outcome of a call (after retries).
// Only retryable failures (outages, overload) count; a 400 says nothing
// about the model's health.
func (b *modelBreakers) record(i int, err error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &b.state[i]
	s.probing = false
	if err == nil {
		s.failures = 0
		return
	}
	if !models.IsRetryable(err) {
		return
	}
	s.failures++
	if s.failures >= b.threshold {
		if !b.now().Before(s.openUntil) {
			circuitOpened.Add(context.Background(), 1, attrs(attribute.String("model_id", b.modelIDs[i])))
		}
		s.openUntil = b.now().Add(b.cooldown)
	}
}
