package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// PlanStore is the SQLite durable implementation of service.PlanStore
// (migration 2026080902). Plans are partitioned by namespace.
type PlanStore struct {
	db *sql.DB
}

// NewPlanStore wraps an existing SQLite DB for plan persistence.
func NewPlanStore(d *DB) *PlanStore {
	return &PlanStore{db: d.db}
}

var _ service.PlanStore = (*PlanStore)(nil)

// Create inserts one frozen execution plan.
func (s *PlanStore) Create(ctx context.Context, plan *service.ExecutionPlan) error {
	if plan == nil || plan.ID == "" {
		return errors.New("sqlite: plan id is required")
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
		INSERT INTO execution_plans (
			plan_id, namespace, version, tool_name, arguments, arguments_hash,
			registry_revision, descriptor_policy_revision, authorization_policy_revision,
			authorization_hash, identity_binding_hash, principal_json, context_json,
			idempotency_key, approval_required, approval_kind, prepared_at, expires_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		plan.ID, plan.Principal.Namespace, plan.Version, plan.ToolName,
		string(plan.Arguments), plan.ArgumentsHash,
		plan.RegistryRevision, plan.DescriptorPolicyRevision, plan.AuthorizationPolicyRevision,
		plan.AuthorizationHash, plan.IdentityBindingHash,
		string(principalJSON), string(contextJSON),
		plan.IdempotencyKey, boolInt(plan.Approval.Required), plan.Approval.ApprovalKind,
		plan.PreparedAt.UTC().Format(time.RFC3339Nano), plan.ExpiresAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// Get loads a plan by namespace-scoped id.
func (s *PlanStore) Get(ctx context.Context, scope execution.NamespaceScope, id string) (*service.ExecutionPlan, error) {
	if scope.Namespace == "" {
		return nil, errors.New("sqlite: namespace scope is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT plan_id, version, tool_name, arguments, arguments_hash,
			registry_revision, descriptor_policy_revision, authorization_policy_revision,
			authorization_hash, identity_binding_hash, principal_json, context_json,
			idempotency_key, approval_required, approval_kind, prepared_at, expires_at
		FROM execution_plans WHERE namespace = ? AND plan_id = ?`,
		scope.Namespace, id,
	)
	var p service.ExecutionPlan
	var arguments, principalJSON, contextJSON, approvalKind, preparedAt, expiresAt string
	var approvalRequired int
	var idempotencyKey sql.NullString
	if err := row.Scan(
		&p.ID, &p.Version, &p.ToolName, &arguments, &p.ArgumentsHash,
		&p.RegistryRevision, &p.DescriptorPolicyRevision, &p.AuthorizationPolicyRevision,
		&p.AuthorizationHash, &p.IdentityBindingHash, &principalJSON, &contextJSON,
		&idempotencyKey, &approvalRequired, &approvalKind, &preparedAt, &expiresAt,
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
	p.Approval.Required = approvalRequired != 0
	p.Approval.ApprovalKind = approvalKind
	if t, err := time.Parse(time.RFC3339Nano, preparedAt); err == nil {
		p.PreparedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, expiresAt); err == nil {
		p.ExpiresAt = t
	}
	return &p, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
