package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// ErrQuotaExceeded reports that a run was stopped by a token or cost quota
// (AgentConfig.MaxRunTokens or a UsageGuard).
var ErrQuotaExceeded = errors.New("agent: usage quota exceeded")

// UsageScope identifies who a model call is billed to.
type UsageScope struct {
	Namespace string
	UserID    string
	SessionID string
	AgentID   string
	RunID     string
	// ModelID is the model that served the call (a fallback when the
	// primary failed). Empty in Admit.
	ModelID string
}

// UsageRecord is the usage of one model call. CostUSD is set when the
// serving model implements models.CostEstimator.
type UsageRecord struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	ReasoningTokens   int
	TotalTokens       int
	CostUSD           float64
}

// UsageGuard enforces quotas across runs. Admit runs before every model
// call, so a runaway tool loop stops at its next call; returning an error
// wrapping ErrQuotaExceeded fails the run. Record runs after every call
// that reported usage, including failed streams. Implementations must be
// safe for concurrent use; a distributed implementation (e.g. Redis
// counters) shares quotas across replicas.
type UsageGuard interface {
	Admit(ctx context.Context, scope UsageScope) error
	Record(ctx context.Context, scope UsageScope, usage UsageRecord)
}

type servedModelKey struct{}

// servedModel records which candidate served the current model call.
type servedModel struct{ model models.Model }

func noteServedModel(ctx context.Context, m models.Model) {
	if served, ok := ctx.Value(servedModelKey{}).(*servedModel); ok {
		served.model = m
	}
}

func (a *Agent) usageScope(ctx context.Context, st *runState) UsageScope {
	namespace := st.output.Namespace
	if namespace == "" {
		namespace, _ = execution.NamespaceFromContext(ctx)
	}
	return UsageScope{
		Namespace: namespace,
		UserID:    st.output.UserID,
		SessionID: st.output.SessionID,
		AgentID:   st.output.AgentID,
		RunID:     st.output.RunID,
	}
}

// admitModelCall enforces the per-run token budget and the UsageGuard.
func (a *Agent) admitModelCall(ctx context.Context, st *runState) error {
	if limit := a.cfg.MaxRunTokens; limit > 0 && st.output.Metrics.TotalTokens >= limit {
		quotaRejected.Add(ctx, 1, attrs(attribute.String("reason", "run_tokens")))
		return fmt.Errorf("%w: run used %d of %d tokens", ErrQuotaExceeded, st.output.Metrics.TotalTokens, limit)
	}
	if a.cfg.UsageGuard == nil {
		return nil
	}
	err := a.cfg.UsageGuard.Admit(ctx, a.usageScope(ctx, st))
	if err != nil {
		quotaRejected.Add(ctx, 1, attrs(attribute.String("reason", "guard")))
	}
	return err
}

// recordModelUsage reports one call's usage to the UsageGuard.
func (a *Agent) recordModelUsage(ctx context.Context, st *runState, served models.Model, usage *models.MessageMetrics) {
	if usage == nil {
		return
	}
	if served == nil {
		served = a.cfg.Model
	}
	recordModelUsage(ctx, served, usage)
	if a.cfg.UsageGuard == nil {
		return
	}
	scope := a.usageScope(ctx, st)
	scope.ModelID = served.ID()
	record := UsageRecord{
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		ReasoningTokens:   usage.ReasoningTokens,
		TotalTokens:       usage.TotalTokens,
	}
	if record.TotalTokens == 0 {
		record.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if estimator, ok := served.(models.CostEstimator); ok {
		record.CostUSD = estimator.EstimateCost(usage)
	}
	// Recording must not be skipped because the run was just cancelled.
	a.cfg.UsageGuard.Record(context.WithoutCancel(ctx), scope, record)
}

// LocalQuotaConfig configures NewLocalUsageQuota. Zero limits are
// unlimited.
type LocalQuotaConfig struct {
	// Window is the fixed accounting window. Default 24h.
	Window time.Duration
	// TokensPerUser and CostPerUserUSD limit each (namespace, user).
	TokensPerUser  int
	CostPerUserUSD float64
	// TokensPerNamespace and CostPerNamespaceUSD limit each namespace.
	TokensPerNamespace  int
	CostPerNamespaceUSD float64
}

// NewLocalUsageQuota returns a process-local UsageGuard with fixed-window
// token and cost limits per user and per namespace. Usage is counted after
// each call, so one call may overshoot a limit; the next call is refused.
// Replicas each count separately; use a shared implementation across them.
func NewLocalUsageQuota(cfg LocalQuotaConfig) UsageGuard {
	if cfg.Window <= 0 {
		cfg.Window = 24 * time.Hour
	}
	return &localUsageQuota{cfg: cfg, now: time.Now, usage: make(map[string]*quotaBucket)}
}

type localUsageQuota struct {
	cfg LocalQuotaConfig
	now func() time.Time

	mu    sync.Mutex
	usage map[string]*quotaBucket
}

type quotaBucket struct {
	windowStart time.Time
	tokens      int
	cost        float64
}

func (q *localUsageQuota) bucket(key string, now time.Time) *quotaBucket {
	b := q.usage[key]
	if b == nil || now.Sub(b.windowStart) >= q.cfg.Window {
		b = &quotaBucket{windowStart: now}
		q.usage[key] = b
	}
	return b
}

func (q *localUsageQuota) Admit(_ context.Context, scope UsageScope) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	user := q.bucket("u\x00"+scope.Namespace+"\x00"+scope.UserID, now)
	if (q.cfg.TokensPerUser > 0 && user.tokens >= q.cfg.TokensPerUser) || (q.cfg.CostPerUserUSD > 0 && user.cost >= q.cfg.CostPerUserUSD) {
		return fmt.Errorf("%w: user quota for this window is used up", ErrQuotaExceeded)
	}
	ns := q.bucket("n\x00"+scope.Namespace, now)
	if (q.cfg.TokensPerNamespace > 0 && ns.tokens >= q.cfg.TokensPerNamespace) || (q.cfg.CostPerNamespaceUSD > 0 && ns.cost >= q.cfg.CostPerNamespaceUSD) {
		return fmt.Errorf("%w: namespace quota for this window is used up", ErrQuotaExceeded)
	}
	return nil
}

func (q *localUsageQuota) Record(_ context.Context, scope UsageScope, usage UsageRecord) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	for _, key := range []string{"u\x00" + scope.Namespace + "\x00" + scope.UserID, "n\x00" + scope.Namespace} {
		b := q.bucket(key, now)
		b.tokens += usage.TotalTokens
		b.cost += usage.CostUSD
	}
	// Drop buckets of past windows so idle users do not accumulate.
	if len(q.usage) > 10000 {
		for key, b := range q.usage {
			if now.Sub(b.windowStart) >= q.cfg.Window {
				delete(q.usage, key)
			}
		}
	}
}
