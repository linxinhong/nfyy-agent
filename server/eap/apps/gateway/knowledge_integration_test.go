//go:build integration

// Knowledge vertical slice integration (TASK-012 acceptance): upload ->
// parse -> chunk -> embed -> index -> cited search inside the authorized
// Workspace; cross-Workspace, revoked keys, missing membership and DLP
// failures expose no content; reprocessing is idempotent and version
// replacement removes stale retrieval.
package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/knowledge"
)

func TestKnowledgeSliceIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.Knowledge = newKnowledgeService(t, env)
	// DLP 引擎与生产一致：控制表驱动的 TableScanner。
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.handler = env.srv.Handler()

	// Two employees in different workspaces.
	mcpLogin(t, env, "kn-secops@example.com", "secops")
	secopsKey := seedKey(t, env, mcpUserID(t, env, "kn-secops@example.com"))
	mcpLogin(t, env, "kn-finance@example.com", "finance")
	financeKey := seedKey(t, env, mcpUserID(t, env, "kn-finance@example.com"))
	secopsAuth := map[string]string{"Authorization": "Bearer " + secopsKey, "X-Agent-Workspace": "secops"}
	financeAuth := map[string]string{"Authorization": "Bearer " + financeKey, "X-Agent-Workspace": "finance"}

	// 1. Upload into secops (content mentions an isolated topic).
	doc := `{"title":"隔离验证手册","content":"隔离验证手册：secops 专属流程。核心步骤是校验 workspace membership 并检查 trace id 传播。隔离是平台安全边界。"}`
	rec := env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", secopsKey, doc, secopsAuth)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}

	// 2. Authorized search returns cited results (title/version/seq present).
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", secopsKey,
		`{"query":"隔离 验证 流程","top_k":5}`, secopsAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"title":"隔离验证手册"`, `"version":1`, `"seq":`} {
		if !strings.Contains(body, want) {
			t.Fatalf("citation incomplete, missing %s: %s", want, body)
		}
	}

	// 3. Cross-Workspace: finance finds nothing (filter inside the query).
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", financeKey,
		`{"query":"隔离 验证 流程"}`, financeAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("cross-ws search: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "隔离验证手册") || strings.Contains(rec.Body.String(), "membership") {
		t.Fatalf("cross-workspace content leaked: %s", rec.Body.String())
	}

	// 4. Missing membership: a user with no workspace gets fail-closed.
	mcpLogin(t, env, "kn-orphan@example.com", "secops")
	// 随后移除成员，制造"无有效 Workspace 会话"探针。
	if _, err := env.pool.Exec(ctx, `DELETE FROM workspace_members WHERE user_id = (SELECT id FROM users WHERE email='kn-orphan@example.com')`); err != nil {
		t.Fatalf("remove orphan membership: %v", err)
	}
	orphanKey := seedKey(t, env, mcpUserID(t, env, "kn-orphan@example.com"))
	orphanAuth := map[string]string{"Authorization": "Bearer " + orphanKey, "X-Agent-Workspace": "secops"}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", orphanKey,
		`{"query":"隔离"}`, orphanAuth)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("orphan search: %d, want 403", rec.Code)
	}

	// 5. DLP deny-class content never enters the index.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", secopsKey,
		`{"title":"泄密文档","content":"这里的密钥是 sk-abcdefghij0123456789AB 请保存"}`, secopsAuth)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "dlp_blocked") {
		t.Fatalf("dlp upload block: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", secopsKey,
		`{"query":"sk-abcdefghij0123456789AB"}`, secopsAuth)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("dlp query block: %d %s", rec.Code, rec.Body.String())
	}

	// 5c. Legacy chunk withholding: a chunk stored raw (e.g. ingested before
	// the current rules existed — simulated by rewriting the chunk in place)
	// is withheld on egress by deny-class rules.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", secopsKey,
		`{"title":"遗留库配置","content":"旧库连接信息已下线，见内部文档。"}`, secopsAuth)
	if rec.Code != http.StatusCreated {
		t.Fatalf("legacy upload: %d %s", rec.Code, rec.Body.String())
	}
	var legacyDoc struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &legacyDoc)
	if _, err := env.pool.Exec(ctx,
		`UPDATE knowledge_chunks SET content = '旧库 postgres://legacy:pw@10.9.9.9:5432/legacydb 已下线。'
		 WHERE document_id = $1`, legacyDoc.DocumentID); err != nil {
		t.Fatalf("simulate legacy chunk: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, legacyDoc.DocumentID)
	})
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", secopsKey,
		`{"query":"遗留库配置 下线","top_k":3}`, secopsAuth)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[WITHHELD:DLP]") {
		t.Fatalf("legacy deny-class chunk must be withheld: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "postgres://legacy:pw@10.9.9.9") {
		t.Fatalf("withheld chunk leaked the connection string")
	}

	// 5b. TASK-026 hardening: redact-class upload stores the REDACTED
	// content, retrieval returns the redacted form, and pre-existing
	// (legacy) chunks with deny-class content are withheld on egress.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", secopsKey,
		`{"title":"值班联系人","content":"值班联系人张三，电话 13812345678，A-102。"}`, secopsAuth)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"redactions":1`) {
		t.Fatalf("redact upload: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", secopsKey,
		`{"query":"值班联系人 电话","top_k":3}`, secopsAuth)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "13812345678") {
		t.Fatalf("retrieval must not leak the raw phone: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[REDACTED:phone]") {
		t.Fatalf("retrieval must return the redacted content: %s", rec.Body.String())
	}

	// 清理本段测试文档，避免污染后续检索断言。
	if _, err := env.pool.Exec(ctx,
		`DELETE FROM knowledge_chunks WHERE document_id IN (SELECT id FROM knowledge_documents WHERE title IN ('值班联系人','遗留库配置'));
		 DELETE FROM knowledge_documents WHERE title IN ('值班联系人','遗留库配置');`); err != nil {
		t.Fatalf("cleanup test docs: %v", err)
	}

	// 6. Idempotent reprocessing: same content uploads as a no-op replay.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", secopsKey, doc, secopsAuth)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"idempotent_replay":true`) {
		t.Fatalf("idempotent replay: %d %s", rec.Code, rec.Body.String())
	}

	// 7. Version replacement: new content bumps version and stale chunks are
	// no longer retrievable.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", secopsKey,
		`{"title":"隔离验证手册","content":"隔离验证手册第二版：全新的替换内容与量子隔离流程。"}`, secopsAuth)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"version":2`) {
		t.Fatalf("version bump: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", secopsKey,
		`{"query":"核心步骤 校验 workspace membership 传播"}`, secopsAuth)
	staleBody := rec.Body.String()
	if strings.Contains(staleBody, `"version":1`) || strings.Contains(staleBody, "核心步骤是校验") {
		t.Fatalf("stale version still retrievable: %s", staleBody)
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", secopsKey,
		`{"query":"量子隔离 替换"}`, secopsAuth)
	if !strings.Contains(rec.Body.String(), `"version":2`) {
		t.Fatalf("new version not retrievable: %s", rec.Body.String())
	}

	// 8. Revoked key: upload and search both fail closed.
	revoke := env.do(t, http.MethodDelete, "/enterprise/v1/keys/"+seededKeyID(t, env, secopsKey), loginAs(t, env, "kn-secops@example.com"), "", nil)
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", revoke.Code)
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", secopsKey, `{"query":"隔离"}`, secopsAuth)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key search: %d, want 401", rec.Code)
	}
}

// loginAs returns a fresh control-plane session for key management.
func loginAs(t *testing.T, env *sliceEnv, email string) string {
	t.Helper()
	body := `{"email":"` + email + `","workspaces":["secops"]}`
	rec := env.do(t, http.MethodPost, "/enterprise/v1/auth/dev-login", "", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: %d", email, rec.Code)
	}
	var session struct {
		SessionToken string `json:"session_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &session)
	return session.SessionToken
}

func newKnowledgeService(t *testing.T, env *sliceEnv) *knowledge.Service {
	t.Helper()
	return knowledge.NewService(env.pool, knowledge.HashEmbedder{})
}

// doWithHeaders performs a request with explicit extra headers (no authz
// convenience mapping).
func (e *sliceEnv) doWithHeaders(t *testing.T, method, path, bearer, jsonBody string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(jsonBody))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}
