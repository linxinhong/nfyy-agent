package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// ApprovalStore is the SQLite durable implementation of service.ApprovalStore
// (migration 2026080903). Decisions are partitioned by namespace and unique
// per (namespace, plan_id).
type ApprovalStore struct {
	db *sql.DB
}

// NewApprovalStore wraps an existing SQLite DB for grant persistence.
func NewApprovalStore(d *DB) *ApprovalStore {
	return &ApprovalStore{db: d.db}
}

var _ service.ApprovalStore = (*ApprovalStore)(nil)

// Resolve records a decision. Same decision + nonce is idempotent; a
// different decision for the same plan conflicts.
func (s *ApprovalStore) Resolve(ctx context.Context, scope execution.NamespaceScope, grant *service.ApprovalGrant) (bool, error) {
	if grant == nil || grant.PlanID == "" {
		return false, errors.New("sqlite: grant plan id is required")
	}
	if grant.GrantID == "" {
		grant.GrantID = "grant-" + grant.PlanID
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO approval_grants (
			grant_id, namespace, plan_id, plan_version, tool_name, arguments_hash,
			registry_revision, descriptor_policy_revision, authorization_policy_revision,
			requester, approver, decision, nonce, issued_at, expires_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(namespace, plan_id) DO NOTHING`,
		grant.GrantID, scope.Namespace, grant.PlanID, grant.PlanVersion, grant.ToolName,
		grant.ArgumentsHash, grant.RegistryRevision, grant.DescriptorPolicyRevision,
		grant.AuthorizationPolicyRevision, grant.Requester, grant.Approver,
		grant.Decision, grant.Nonce,
		grant.IssuedAt.UTC().Format(time.RFC3339Nano), grant.ExpiresAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil // fresh insert
	}
	// The row already existed: decide idempotent vs conflict by comparing
	// the stored decision and nonce.
	existing, err := s.Get(ctx, scope, grant.PlanID)
	if err != nil {
		return false, err
	}
	if existing == nil {
		return true, nil
	}
	if existing.Decision == grant.Decision && existing.Nonce == grant.Nonce {
		return false, nil // idempotent
	}
	return false, service.ErrApprovalConflict
}

// Get loads the recorded decision for a plan.
func (s *ApprovalStore) Get(ctx context.Context, scope execution.NamespaceScope, planID string) (*service.ApprovalGrant, error) {
	if scope.Namespace == "" {
		return nil, errors.New("sqlite: namespace scope is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT grant_id, plan_version, tool_name, arguments_hash,
			registry_revision, descriptor_policy_revision, authorization_policy_revision,
			requester, approver, decision, nonce, issued_at, expires_at
		FROM approval_grants WHERE namespace = ? AND plan_id = ?`,
		scope.Namespace, planID,
	)
	var g service.ApprovalGrant
	var issuedAt, expiresAt string
	g.Namespace = scope.Namespace
	g.PlanID = planID
	if err := row.Scan(
		&g.GrantID, &g.PlanVersion, &g.ToolName, &g.ArgumentsHash,
		&g.RegistryRevision, &g.DescriptorPolicyRevision, &g.AuthorizationPolicyRevision,
		&g.Requester, &g.Approver, &g.Decision, &g.Nonce, &issuedAt, &expiresAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, issuedAt); err == nil {
		g.IssuedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, expiresAt); err == nil {
		g.ExpiresAt = t
	}
	return &g, nil
}
