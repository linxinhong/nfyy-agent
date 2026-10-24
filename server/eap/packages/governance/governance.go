// Package governance turns Provider topology, logical-model routing and
// pricing into server-managed, versioned control data (TASK-008, ADR-0008).
//
// Contract rules:
//   - the public model name is a logical alias; provider-native model names
//     and credential references never leave the server;
//   - routing is deterministic: enabled routes ordered by (priority, name),
//     failures advance to the next route — reproducible and auditable;
//   - unknown pricing is explicit: records carry cost_known=false instead of
//     a fabricated zero-cost success;
//   - usage rows are idempotent on (trace_id, request_id, attempt).
package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/errs"
)

// Provider is one configured upstream endpoint.
type Provider struct {
	Name          string
	BaseURL       string
	CredentialRef string
	Enabled       bool
}

// Route is one candidate path from a logical alias to a provider model.
type Route struct {
	Alias    string
	Provider string
	Model    string // provider-native model name
	Priority int
	Weight   int
}

// RoutePlan is the ordered, deterministic route list for one alias.
type RoutePlan struct {
	Alias     string
	Routes    []Route
	Fallbacks []Route // routes after the first (priority order)
}

// Primary returns the first-choice route.
func (p RoutePlan) Primary() (Route, bool) {
	if len(p.Routes) == 0 {
		return Route{}, false
	}
	return p.Routes[0], true
}

// Pricing is the per-million-token price sheet for one (alias, provider).
type Pricing struct {
	Currency    string
	InputPerMT  float64
	OutputPerMT float64
}

// ControlData is the read/write surface over the versioned control tables.
type ControlData struct {
	pool *pgxpool.Pool
}

// NewControlData wraps a pool.
func NewControlData(pool *pgxpool.Pool) *ControlData { return &ControlData{pool: pool} }

// ErrNoRoutes marks an alias without any enabled route (fail closed).
var ErrNoRoutes = errors.New("no enabled route for logical model")

// RoutePlanFor resolves the deterministic plan for one alias.
func (c *ControlData) RoutePlanFor(ctx context.Context, alias string) (RoutePlan, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT r.provider, r.model, r.priority, r.weight
		FROM model_routes r
		JOIN model_aliases a ON a.alias = r.alias
		JOIN providers p ON p.name = r.provider
		WHERE r.alias = $1 AND r.enabled AND a.enabled AND p.enabled
		ORDER BY r.priority, r.provider, r.model`, alias)
	if err != nil {
		return RoutePlan{}, errs.Wrap(err, errs.Internal, "route lookup failed")
	}
	defer rows.Close()
	plan := RoutePlan{Alias: alias}
	for rows.Next() {
		var r Route
		r.Alias = alias
		if err := rows.Scan(&r.Provider, &r.Model, &r.Priority, &r.Weight); err != nil {
			return RoutePlan{}, errs.Wrap(err, errs.Internal, "route scan failed")
		}
		plan.Routes = append(plan.Routes, r)
	}
	if err := rows.Err(); err != nil {
		return RoutePlan{}, errs.Wrap(err, errs.Internal, "route iteration failed")
	}
	if len(plan.Routes) == 0 {
		return RoutePlan{}, errs.New(errs.NotFound, fmt.Sprintf("logical model %q has no enabled route", alias))
	}
	plan.Fallbacks = plan.Routes[1:]
	return plan, nil
}

// PricingFor returns the price sheet for (alias, provider). The second result
// reports whether pricing exists; unknown pricing is not an error here so the
// caller can fail the cost ledger explicitly.
func (c *ControlData) PricingFor(ctx context.Context, alias, provider string) (Pricing, bool, int, error) {
	var (
		p       Pricing
		version int
	)
	err := c.pool.QueryRow(ctx, `
		SELECT m.currency, m.input_per_mt, m.output_per_mt, a.version
		FROM model_pricing m
		JOIN model_aliases a ON a.alias = m.alias
		WHERE m.alias = $1 AND m.provider = $2`, alias, provider).
		Scan(&p.Currency, &p.InputPerMT, &p.OutputPerMT, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pricing{}, false, 0, nil
	}
	if err != nil {
		return Pricing{}, false, 0, errs.Wrap(err, errs.Internal, "pricing lookup failed")
	}
	return p, true, version, nil
}

// UsageRecord is one durable token/cost row.
type UsageRecord struct {
	TraceID          string
	RequestID        string
	Attempt          int
	UserID           string
	WorkspaceID      string
	Alias            string
	Provider         string
	ProviderModel    string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Streaming        bool
	// Pricing snapshot; when Known is false the record stores zero cost with
	// cost_known=false (observable unknown pricing, never fabricated zero).
	Pricing      Pricing
	PricingVer   int
	PricingKnown bool
}

// CostMicros computes the exact cost in micro-units of the pricing currency.
// price_per_mt is per 1,000,000 tokens, so:
//
//	cost   = price_per_mt * tokens / 1_000_000
//	micros = cost * 1_000_000 = price_per_mt * tokens
//
// The product stays in big.Rat space (exact), then rounds half-away-from-zero
// to an integer micro count. Unknown pricing reports known=false.
func (u UsageRecord) CostMicros() (int64, bool) {
	if !u.PricingKnown {
		return 0, false
	}
	in := new(big.Rat).SetFloat64(u.Pricing.InputPerMT)
	out := new(big.Rat).SetFloat64(u.Pricing.OutputPerMT)
	micros := new(big.Rat).Add(
		new(big.Rat).Mul(in, new(big.Rat).SetInt64(int64(u.PromptTokens))),
		new(big.Rat).Mul(out, new(big.Rat).SetInt64(int64(u.CompletionTokens))),
	)
	// Round half away from zero: floor(x+0.5) for positives, which our
	// price sheets always are.
	f, _ := micros.Float64()
	return int64(f + 0.5), true
}

// InsertUsage writes one usage row idempotently: a duplicate
// (trace_id, request_id, attempt) is a no-op, not an error and not a second
// charge.
func (c *ControlData) InsertUsage(ctx context.Context, u UsageRecord) error {
	return insertUsageStmt(ctx, c.pool, u)
}

// execQuerier matches pool and transaction executors.
type execQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// InsertUsageTx runs the usage insert inside a caller-owned transaction
// (TASK-041 governance batch).
func (c *ControlData) InsertUsageTx(ctx context.Context, tx pgx.Tx, u UsageRecord) error {
	return insertUsageStmt(ctx, tx, u)
}

// insertUsageStmt runs the usage insert on any executor so the governance
// batch (TASK-041) can share one transaction with the audit event set.
func insertUsageStmt(ctx context.Context, q execQuerier, u UsageRecord) error {
	sql, args := UsageInsertSQL(u)
	if _, err := q.Exec(ctx, sql, args...); err != nil {
		return errs.Wrap(err, errs.Internal, "usage insert failed")
	}
	return nil
}

// UsageInsertSQL renders the usage ledger insert with bound arguments
// (TASK-043): the store composes it into its single-round-trip governance
// batch, the transactional path keeps using the identical statement.
func UsageInsertSQL(u UsageRecord) (string, []any) {
	micros, known := u.CostMicros()
	return `
		INSERT INTO usage_records
			(trace_id, request_id, attempt, user_id, workspace_id, alias, provider, provider_model,
			 prompt_tokens, completion_tokens, total_tokens, currency, cost_micros,
			 pricing_version, streaming, cost_known)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (trace_id, request_id, attempt) DO NOTHING`,
		[]any{u.TraceID, u.RequestID, u.Attempt, u.UserID, u.WorkspaceID, u.Alias,
			u.Provider, u.ProviderModel, u.PromptTokens, u.CompletionTokens,
			u.TotalTokens, u.Pricing.Currency, micros, u.PricingVer, u.Streaming, known}
}

// MarshalRoutes renders a plan for trace/audit detail (no credentials).
func MarshalRoutes(p RoutePlan) string {
	type routeView struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Priority int    `json:"priority"`
	}
	views := make([]routeView, 0, len(p.Routes))
	for _, r := range p.Routes {
		views = append(views, routeView{Provider: r.Provider, Model: r.Model, Priority: r.Priority})
	}
	b, _ := json.Marshal(views)
	return string(b)
}

// SortRoutes is exported for fixture tests to mirror the SQL ordering.
func SortRoutes(routes []Route) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Priority != routes[j].Priority {
			return routes[i].Priority < routes[j].Priority
		}
		if routes[i].Provider != routes[j].Provider {
			return routes[i].Provider < routes[j].Provider
		}
		return routes[i].Model < routes[j].Model
	})
}
