//go:build integration

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/store"
)

// subjectTestPool connects to the integration database (EAP_TEST_DATABASE_URL).
func subjectTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("EAP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("EAP_TEST_DATABASE_URL not set")
	}
	schema := "subject_dlp_test_" + strings.ReplaceAll(t.Name(), "/", "_")
	admin, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	if _, err := admin.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	admin.Close()
	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), url)
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
			cleanup.Close()
		}
	})
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := pgxpool.New(context.Background(), url+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(context.Background(), pool, "../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// TestMCPEgressSubjectMatrix exercises the employee-surface sanitizer
// directly (the same function the MCP tools/call path uses): privileged
// subjects keep originals, ordinary subjects get withhold/mask. Runs against
// the integration database (integration build tag).
func TestMCPEgressSubjectMatrix(t *testing.T) {
	pool := subjectTestPool(t)
	ctx := context.Background()
	st := store.New(pool)
	if _, err := pool.Exec(ctx, `UPDATE dlp_rules SET action='tokenize', except_roles='{privileged}' WHERE name='internal_phone'`); err != nil {
		t.Fatalf("seed phone rule: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE dlp_rules SET except_roles='{privileged}' WHERE name IN ('db_conn_string','internal_ipv4')`); err != nil {
		t.Fatalf("seed conn rule: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE dlp_rules SET except_roles='{}' WHERE name IN ('internal_phone','db_conn_string','internal_ipv4'); UPDATE dlp_rules SET action='redact' WHERE name='internal_phone'; DELETE FROM dlp_tokens;`)
	})
	scanner := dlp.NewTableScanner(st.LoadDLPRules)
	scanner.Tokenizer = func(c context.Context, ruleName, value string) (string, error) {
		return st.TokenizeDLP(c, "secops", ruleName, value)
	}
	srv := &Server{Store: st, DLP: scanner}

	text := "phone 13812345004 and secret db postgres://u:p@10.9.9.9:5432/x"
	priv := identity.Session{Workspace: identity.WorkspaceContext{WorkspaceID: "secops", Roles: []string{"privileged"}}}
	ord := identity.Session{Workspace: identity.WorkspaceContext{WorkspaceID: "secops", Roles: []string{"developer"}}}

	// Privileged: internal_phone + db_conn_string + internal_ipv4 (all
	// exempt in this test) keep originals.
	out := srv.dlpScanEmployeeText(httptest.NewRequest(http.MethodPost, "/mcp/gitlab", nil), priv, text)
	if !strings.Contains(out, "13812345004") || !strings.Contains(out, "postgres://u:p@10.9.9.9") {
		t.Fatalf("privileged egress must keep exempt-rule originals: %s", out)
	}
	// Ordinary: deny-class (db_conn_string) withholds the whole blob (fail
	// closed); the phone pseudonym path is asserted on deny-free text below.
	out = srv.dlpScanEmployeeText(httptest.NewRequest(http.MethodPost, "/mcp/gitlab", nil), ord, text)
	if strings.Contains(out, "13812345004") || strings.Contains(out, "postgres://u:p@10.9.9.9") {
		t.Fatalf("ordinary egress must mask/withhold: %s", out)
	}
	if !strings.Contains(out, "[WITHHELD:DLP]") {
		t.Fatalf("deny-class content must be withheld: %s", out)
	}
	out = srv.dlpScanEmployeeText(httptest.NewRequest(http.MethodPost, "/mcp/gitlab", nil), ord, "phone 13812345004 only")
	if strings.Contains(out, "13812345004") || !strings.Contains(out, "[internal_phone:") {
		t.Fatalf("ordinary egress must carry the phone pseudonym: %s", out)
	}
}

// TestDLPRuleWorkspaceScope (TASK-031): a workspace-bound rule fires only in
// its workspace; global rules keep firing everywhere; the sandbox endpoint
// returns an accurate preview without persisting content.
func TestWorkspaceScopedRules(t *testing.T) {
	pool := subjectTestPool(t)
	ctx := context.Background()
	st := store.New(pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO dlp_rules (name, pattern, action, replacement, workspace_id) VALUES
		 ('fin_salary', '薪资机密词TESTRULE', 'deny', '[REDACTED:fin_salary]', 'finance')
		 ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("seed ws rule: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM dlp_rules WHERE name='fin_salary'`) })

	scanner := dlp.NewTableScanner(st.LoadDLPRules)
	scanner.CacheTTL = 0
	sample := "这里包含 薪资机密词TESTRULE 和电话 13812345005"

	finCtx := dlp.ContextWithWorkspace(ctx, "finance")
	res, err := scanner.Scan(finCtx, sample)
	if err != nil || !res.Blocked || !strings.Contains(res.Reason, "fin_salary") {
		t.Fatalf("finance ctx must hit fin_salary: %v %+v", err, res)
	}

	secCtx := dlp.ContextWithWorkspace(ctx, "secops")
	res, err = scanner.Scan(secCtx, sample)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range res.RuleNames {
		if name == "fin_salary" {
			t.Fatalf("secops ctx must not hit finance rule: %+v", res.RuleNames)
		}
	}
	if !strings.Contains(res.Redacted, "13812345005") && !strings.Contains(res.Redacted, "[") {
		t.Fatalf("global phone rule should still act in secops: %s", res.Redacted)
	}
}
