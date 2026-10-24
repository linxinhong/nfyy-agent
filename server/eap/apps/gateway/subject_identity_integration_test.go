//go:build integration

// Subject-primary identity coverage (TASK-023): a first login with subject
// only JIT-provisions the canonical Principal idempotently; a later
// IdP-asserted email backfills the row; an email that matches an existing
// employee links the incoming subject (directory reconciliation) and the
// partial unique index keeps one row per non-empty email.

package main_test

import (
	"context"
	"testing"

	"enterprise-agent-platform/packages/store"
)

func TestSubjectPrimaryIdentity(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	// 1. Subject-only JIT provisioning (no directory email at all).
	first, err := env.store.UpsertUser(ctx, store.IDClaims{
		Subject: "on_subject_only", DisplayName: "飞书用户",
		OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil {
		t.Fatalf("subject-only provisioning: %v", err)
	}
	if first.Email != "" {
		t.Fatalf("subject-only row must not invent an email: %q", first.Email)
	}

	// 2. Idempotent replay: same subject, same row.
	second, err := env.store.UpsertUser(ctx, store.IDClaims{
		Subject: "on_subject_only", DisplayName: "飞书用户",
		OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil || second.ID != first.ID {
		t.Fatalf("replay must be idempotent: %v %+v vs %+v", err, second, first)
	}
	var count int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE idp_subject = 'on_subject_only'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("exactly one row per subject: %d %v", count, err)
	}

	// 3. A later IdP-asserted email backfills the row (never overwrites a
	// provisioned address with empty).
	backfilled, err := env.store.UpsertUser(ctx, store.IDClaims{
		Subject: "on_subject_only", Email: "feishu-user@example.com",
		DisplayName: "飞书用户", OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil || backfilled.Email != "feishu-user@example.com" {
		t.Fatalf("email backfill: %v %+v", err, backfilled)
	}
	emptied, err := env.store.UpsertUser(ctx, store.IDClaims{
		Subject: "on_subject_only", DisplayName: "飞书用户",
		OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil || emptied.Email != "feishu-user@example.com" {
		t.Fatalf("empty claim must not wipe a provisioned email: %v %+v", err, emptied)
	}

	// 4. Directory reconciliation: an email matching an existing employee
	// row links the incoming subject — one row per human.
	existing, err := env.store.UpsertUser(ctx, store.IDClaims{
		Subject: "dev:li.lei@example.com", Email: "li.lei@example.com",
		DisplayName: "Li Lei", OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil {
		t.Fatalf("provision li.lei: %v", err)
	}
	migrated, err := env.store.UpsertUser(ctx, store.IDClaims{
		Subject: "on_new_idp_subject", Email: "li.lei@example.com",
		DisplayName: "Li Lei", OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil || migrated.ID != existing.ID {
		t.Fatalf("email must link to the existing employee row: %v %+v vs %+v", err, migrated, existing)
	}
	var rows int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE lower(email) = 'li.lei@example.com'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("one row per non-empty email: %d %v", rows, err)
	}
}
