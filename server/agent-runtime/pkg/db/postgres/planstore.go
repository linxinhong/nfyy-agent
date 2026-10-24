package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// PlanStore is the PostgreSQL durable implementation of service.PlanStore
// (migration 2026080902). Plans are partitioned by namespace.
type PlanStore struct {
	db *sql.DB
}

// NewPlanStore wraps an existing PostgreSQL DB for plan persistence.
func NewPlanStore(d *DB) *PlanStore {
	return &PlanStore{db: d.db}
}

var _ service.PlanStore = (*PlanStore)(nil)

// Create inserts one frozen execution plan.
func (s *PlanStore) Create(ctx context.Context, plan *service.ExecutionPlan) error {
	if plan == nil || plan.ID == "" {
		return errors.New("postgres: plan id is required")
	}
	principalJSON, err := json.Marshal(plan.Principal)
	if err != nil {
		return err
	}
	contextJSON, err := json.Marshal(plan.Context)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_runtime_execution_plans (
			plan_id, namespace, version, tool_name, arguments, arguments_hash,
			registry_revision, descriptor_policy_revision, authorization_policy_revision,
			authorization_hash, identity_binding_hash, principal_json, context_json,
			idempotency_key, approval_required, approval_kind, prepared_at, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT (namespace, plan_id) DO NOTHING`,
		plan.ID, plan.Principal.Namespace, plan.Version, plan.ToolName,
		string(plan.Arguments), plan.ArgumentsHash,
		plan.RegistryRevision, plan.DescriptorPolicyRevision, plan.AuthorizationPolicyRevision,
		plan.AuthorizationHash, plan.IdentityBindingHash,
		string(principalJSON), string(contextJSON),
		nullString(plan.IdempotencyKey), plan.Approval.Required, nullString(plan.Approval.ApprovalKind),
		plan.PreparedAt.UTC(), plan.ExpiresAt.UTC(),
	)
	return err
}

// Get loads a plan by namespace-scoped id.
func (s *PlanStore) Get(ctx context.Context, scope execution.NamespaceScope, id string) (*service.ExecutionPlan, error) {
	if scope.Namespace == "" {
		return nil, errors.New("postgres: namespace scope is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT plan_id, version, tool_name, arguments, arguments_hash,
			registry_revision, descriptor_policy_revision, authorization_policy_revision,
			authorization_hash, identity_binding_hash, principal_json, context_json,
			idempotency_key, approval_required, approval_kind, prepared_at, expires_at
		FROM agent_runtime_execution_plans WHERE namespace = $1 AND plan_id = $2`,
		scope.Namespace, id,
	)
	var p service.ExecutionPlan
	var arguments, principalJSON, contextJSON string
	var idempotencyKey, approvalKind sql.NullString
	var preparedAt, expiresAt time.Time
	if err := row.Scan(
		&p.ID, &p.Version, &p.ToolName, &arguments, &p.ArgumentsHash,
		&p.RegistryRevision, &p.DescriptorPolicyRevision, &p.AuthorizationPolicyRevision,
		&p.AuthorizationHash, &p.IdentityBindingHash, &principalJSON, &contextJSON,
		&idempotencyKey, &p.Approval.Required, &approvalKind, &preparedAt, &expiresAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrPlanNotFound
		}
		return nil, err
	}
	p.Arguments = json.RawMessage(arguments)
	if err := json.Unmarshal([]byte(principalJSON), &p.Principal); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(contextJSON), &p.Context); err != nil {
		return nil, err
	}
	p.IdempotencyKey = idempotencyKey.String
	p.Approval.ApprovalKind = approvalKind.String
	p.PreparedAt = preparedAt.UTC()
	p.ExpiresAt = expiresAt.UTC()
	return &p, nil
}

// nullString returns a NULL-capable string value for nullable TEXT columns.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
