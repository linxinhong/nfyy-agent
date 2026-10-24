package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/dbtest"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestStoreContract(t *testing.T) {
	store := newTestDB(t)
	dbtest.RunStoreContract(t, store)
	dbtest.WorkflowResumeContract(t, store)
}

func TestScopedSessionAndRunIsolation(t *testing.T) {
	store := newTestDB(t)
	ctx := context.Background()
	create := func(namespace, id string) *session.AgentSession {
		s := &session.AgentSession{SessionID: id, AgentID: "agent", UserID: "same-user", Namespace: namespace}
		if err := store.CreateSessionScoped(ctx, s); err != nil {
			t.Fatalf("create %s: %v", namespace, err)
		}
		return s
	}
	a, b := create("tenant-a", "session-a"), create("tenant-b", "session-b")
	legacy := &session.AgentSession{SessionID: "legacy-session", AgentID: "agent", UserID: "same-user"}
	if err := store.CreateSession(ctx, legacy); err != nil {
		t.Fatalf("create legacy: %v", err)
	}
	if got, err := store.ReadSessionScoped(ctx, "tenant-a", legacy.SessionID); err != nil || got != nil {
		t.Fatalf("tenant read legacy = %+v, %v", got, err)
	}
	if got, err := store.ReadSession(ctx, legacy.SessionID); err != nil || got == nil || got.Namespace != "legacy" {
		t.Fatalf("legacy read = %+v, %v", got, err)
	}
	if err := store.CreateSessionScoped(ctx, &session.AgentSession{SessionID: a.SessionID, AgentID: "agent", UserID: "same-user", Namespace: "tenant-b"}); err == nil {
		t.Fatal("same global session ID was accepted")
	}
	if got, err := store.ReadSessionScoped(ctx, "tenant-a", a.SessionID); err != nil || got == nil || got.Namespace != "tenant-a" {
		t.Fatalf("collision changed tenant A = %+v, %v", got, err)
	}
	if err := store.CreateTeamSession(ctx, &session.TeamSession{SessionID: a.SessionID, TeamID: "team", UserID: "same-user", Namespace: "tenant-a"}); !errors.Is(err, dbpkg.ErrConflict) {
		t.Fatalf("cross-kind session collision = %v", err)
	}
	if got, err := store.ReadSessionScoped(ctx, "tenant-a", b.SessionID); err != nil || got != nil {
		t.Fatalf("cross read = %+v, %v", got, err)
	}
	listed, err := store.ListSessionsScoped(ctx, dbpkg.SessionQuery{Namespace: "tenant-a"})
	if err != nil || len(listed) != 1 || listed[0].SessionID != a.SessionID {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	if _, err := store.UpdateSessionFieldsScoped(ctx, dbpkg.SessionFieldsUpdate{SessionID: b.SessionID, Namespace: "tenant-a", ExpectedRevision: 1}); !errors.Is(err, dbpkg.ErrNotFound) {
		t.Fatalf("cross update = %v", err)
	}
	if err := store.DeleteSessionScoped(ctx, "tenant-a", b.SessionID); !errors.Is(err, dbpkg.ErrNotFound) {
		t.Fatalf("cross delete = %v", err)
	}
	run := &session.RunOutput{RunID: "run-a", SessionID: a.SessionID, AgentID: "agent", UserID: "same-user", Namespace: "tenant-a", Status: session.RunStatusCompleted}
	if err := store.AppendRun(execution.ContextWithNamespace(ctx, "tenant-a"), run); err != nil {
		t.Fatalf("append run: %v", err)
	}
	if got, err := store.GetRunScoped(ctx, dbpkg.RunQuery{RunID: run.RunID, Namespace: "tenant-b"}); err != nil || got != nil {
		t.Fatalf("cross run = %+v, %v", got, err)
	}
}

func TestScopedTeamAndWorkflowIsolation(t *testing.T) {
	store := newTestDB(t)
	ctx := context.Background()
	for _, tc := range []struct{ kind, namespace, sessionID, component string }{
		{"team", "team-a", "team-session-a", "team"}, {"team", "team-b", "team-session-b", "team"},
		{"workflow", "workflow-a", "workflow-session-a", "workflow"}, {"workflow", "workflow-b", "workflow-session-b", "workflow"},
	} {
		if tc.kind == "team" {
			err := store.CreateTeamSession(ctx, &session.TeamSession{SessionID: tc.sessionID, TeamID: tc.component, UserID: "same-user", Namespace: tc.namespace})
			if err != nil {
				t.Fatalf("create %s: %v", tc.namespace, err)
			}
		} else {
			err := store.CreateWorkflowSession(ctx, &session.WorkflowSession{SessionID: tc.sessionID, WorkflowID: tc.component, UserID: "same-user", Namespace: tc.namespace})
			if err != nil {
				t.Fatalf("create %s: %v", tc.namespace, err)
			}
		}
	}
	teamCtx := execution.ContextWithNamespace(ctx, "team-a")
	if got, err := store.ReadTeamSession(teamCtx, "team-session-b"); err != nil || got != nil {
		t.Fatalf("cross team read = %+v, %v", got, err)
	}
	workflowCtx := execution.ContextWithNamespace(ctx, "workflow-a")
	if got, err := store.ReadWorkflowSession(workflowCtx, "workflow-session-b"); err != nil || got != nil {
		t.Fatalf("cross workflow read = %+v, %v", got, err)
	}
	embeddedRunID := "embedded-cross-kind-run"
	if err := store.CreateTeamSession(teamCtx, &session.TeamSession{
		SessionID: "embedded-team-session", TeamID: "team", UserID: "same-user", Namespace: "team-a",
		Runs: []*session.RunOutput{{RunID: embeddedRunID, TeamID: "team", UserID: "same-user", Namespace: "team-a", Status: session.RunStatusCompleted}},
	}); err != nil {
		t.Fatalf("create team embedded run: %v", err)
	}
	if err := store.CreateSession(teamCtx, &session.AgentSession{
		SessionID: "embedded-agent-session", AgentID: "agent", UserID: "same-user", Namespace: "team-a",
		Runs: []*session.RunOutput{{RunID: embeddedRunID, AgentID: "agent", UserID: "same-user", Namespace: "team-a", Status: session.RunStatusCompleted}},
	}); !errors.Is(err, dbpkg.ErrConflict) {
		t.Fatalf("embedded cross-kind run collision = %v", err)
	}
	teamRun := &session.RunOutput{RunID: "team-run-a", SessionID: "team-session-a", TeamID: "team", UserID: "same-user", Namespace: "team-a", Status: session.RunStatusCompleted, Messages: []models.Message{{Role: models.RoleAssistant, Content: "private"}}}
	if err := store.AppendRun(teamCtx, teamRun); err != nil {
		t.Fatalf("append team run: %v", err)
	}
	if got, err := store.ReadTeamSession(teamCtx, "team-session-a"); err != nil || len(got.Runs) != 1 || len(got.Runs[0].Messages) != 1 {
		t.Fatalf("team run/message round trip = %+v, %v", got, err)
	}
	if got, err := store.GetRunScoped(ctx, dbpkg.RunQuery{RunID: teamRun.RunID, Namespace: "team-b"}); err != nil || got != nil {
		t.Fatalf("cross team run = %+v, %v", got, err)
	}
	if err := store.CreateWorkflowSession(teamCtx, &session.WorkflowSession{SessionID: "workflow-same-namespace", WorkflowID: "workflow", UserID: "same-user", Namespace: "team-a"}); err != nil {
		t.Fatalf("create same-namespace workflow: %v", err)
	}
	if err := store.AppendRun(teamCtx, &session.RunOutput{RunID: teamRun.RunID, SessionID: "workflow-same-namespace", WorkflowID: "workflow", UserID: "same-user", Namespace: "team-a", Status: session.RunStatusCompleted}); !errors.Is(err, dbpkg.ErrConflict) {
		t.Fatalf("cross-kind run collision = %v", err)
	}
	workflowRun := &session.RunOutput{RunID: "workflow-run-a", SessionID: "workflow-session-a", WorkflowID: "workflow", UserID: "same-user", Namespace: "workflow-a", Status: session.RunStatusCompleted, Messages: []models.Message{{Role: models.RoleAssistant, Content: "private"}}}
	if err := store.AppendRun(workflowCtx, workflowRun); err != nil {
		t.Fatalf("append workflow run: %v", err)
	}
	if got, err := store.GetRunScoped(ctx, dbpkg.RunQuery{RunID: workflowRun.RunID, Namespace: "workflow-b"}); err != nil || got != nil {
		t.Fatalf("cross workflow run = %+v, %v", got, err)
	}
	if _, err := store.ReadSessionScoped(execution.ContextWithNamespace(ctx, "team-a"), "team-b", "team-session-b"); !errors.Is(err, dbpkg.ErrNotFound) {
		t.Fatalf("conflicting trusted context = %v", err)
	}
}

func TestRunEnvelopeReadsLegacyToolCallArray(t *testing.T) {
	var run session.RunOutput
	if err := decodeRunEnvelope(`[{"id":"legacy-call","function":{"name":"lookup"}}]`, &run); err != nil {
		t.Fatalf("decodeRunEnvelope: %v", err)
	}
	if len(run.ToolCalls) != 1 || run.ToolCalls[0].ID != "legacy-call" || run.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("tool calls = %+v", run.ToolCalls)
	}
}

func TestRunEnvelopePreservesFrozenApprovalStateAcrossSessionKinds(t *testing.T) {
	store := newTestDB(t)
	ctx := execution.ContextWithNamespace(context.Background(), "approval-ns")
	principal := execution.Principal{Subject: "requester", Namespace: "approval-ns", Issuer: "idp", AuthzEpoch: 3}
	newRun := func(id string) *session.RunOutput {
		return &session.RunOutput{
			RunID: id, UserID: "requester", Namespace: "approval-ns", Principal: principal, Status: session.RunStatusPaused,
			PendingToolCalls: []models.ToolCall{{ID: "call", Function: models.FunctionCall{Name: "approval_tool", Arguments: `{"value":"frozen"}`}}},
			PendingPlans:     map[string]session.PendingPlanRef{"call": {ID: "plan-" + id, Version: "v1", ApprovalRequired: true}},
		}
	}
	assertFrozen := func(label string, got *session.RunOutput) {
		t.Helper()
		if got == nil || got.Namespace != "approval-ns" || got.Principal != principal || got.PendingPlans["call"].ID == "" || got.PendingPlans["call"].Version != "v1" || len(got.PendingToolCalls) != 1 || got.PendingToolCalls[0].Function.Arguments != `{"value":"frozen"}` {
			t.Fatalf("%s frozen envelope = %+v", label, got)
		}
	}
	agentRun := newRun("agent-frozen")
	agentRun.AgentID, agentRun.SessionID = "agent", "agent-session"
	if err := store.CreateSession(ctx, &session.AgentSession{SessionID: agentRun.SessionID, AgentID: agentRun.AgentID, UserID: agentRun.UserID, Namespace: "approval-ns", Runs: []*session.RunOutput{agentRun}}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	gotAgent, _ := store.ReadSession(ctx, agentRun.SessionID)
	assertFrozen("agent", gotAgent.Runs[0])
	teamRun := newRun("team-frozen")
	teamRun.TeamID, teamRun.SessionID = "team", "team-session"
	if err := store.CreateTeamSession(ctx, &session.TeamSession{SessionID: teamRun.SessionID, TeamID: teamRun.TeamID, UserID: teamRun.UserID, Namespace: "approval-ns", Runs: []*session.RunOutput{teamRun}}); err != nil {
		t.Fatalf("create team: %v", err)
	}
	gotTeam, _ := store.ReadTeamSession(ctx, teamRun.SessionID)
	assertFrozen("team", gotTeam.Runs[0])
	workflowRun := newRun("workflow-frozen")
	workflowRun.WorkflowID, workflowRun.SessionID = "workflow", "workflow-session"
	if err := store.CreateWorkflowSession(ctx, &session.WorkflowSession{SessionID: workflowRun.SessionID, WorkflowID: workflowRun.WorkflowID, UserID: workflowRun.UserID, Namespace: "approval-ns", Runs: []*session.RunOutput{workflowRun}}); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	gotWorkflow, _ := store.ReadWorkflowSession(ctx, workflowRun.SessionID)
	assertFrozen("workflow", gotWorkflow.Runs[0])
}

func TestDB_UpdateSessionUsesRevisionCAS(t *testing.T) {
	store := newTestDB(t)
	ctx := context.Background()
	s := &session.AgentSession{SessionID: "cas-session", AgentID: "agent-1", UserID: "user-1"}
	if err := store.CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first, _ := store.ReadSession(ctx, s.SessionID)
	stale, _ := store.ReadSession(ctx, s.SessionID)
	first.SessionState = map[string]any{"winner": true}
	if err := store.UpdateSession(ctx, first); err != nil {
		t.Fatalf("first UpdateSession: %v", err)
	}
	if first.Revision != 2 {
		t.Fatalf("revision = %d, want 2", first.Revision)
	}
	stale.SessionState = map[string]any{"winner": false}
	if err := store.UpdateSession(ctx, stale); !errors.Is(err, dbpkg.ErrConflict) {
		t.Fatalf("stale UpdateSession error = %v, want ErrConflict", err)
	}
	stored, _ := store.ReadSession(ctx, s.SessionID)
	if stored.SessionState["winner"] != true || len(stored.Runs) != 0 {
		t.Fatalf("stored session = %+v", stored)
	}
}

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")
	db, err := New(dsn)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(dsn)
	})
	return db
}

func TestDB_PingAndClose(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestDB_SessionCRUD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Round(time.Second)
	s := &session.AgentSession{
		SessionID:    "sess-1",
		AgentID:      "agent-1",
		UserID:       "user-1",
		Metadata:     map[string]any{"key": "value", "number": float64(42)},
		SessionState: map[string]any{"theme": "dark"},
		Summary:      "test summary",
		Runs: []*session.RunOutput{
			{
				RunID:     "run-1",
				AgentID:   "agent-1",
				UserID:    "user-1",
				Content:   "hello",
				Status:    session.RunStatusCompleted,
				CreatedAt: now,
				UpdatedAt: now,
				Input:     session.RunInput{Message: "hi"},
				Messages: []models.Message{
					{Role: models.RoleUser, Content: "hi"},
					{Role: models.RoleAssistant, Content: "hello"},
				},
				ToolCalls: []models.ToolCall{
					{ID: "call-1", Function: models.FunctionCall{Name: "fn", Arguments: `{}`}},
				},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := db.CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	read, err := db.ReadSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ReadSession failed: %v", err)
	}
	if read == nil {
		t.Fatal("ReadSession returned nil")
	}
	if read.SessionID != "sess-1" || read.AgentID != "agent-1" {
		t.Fatalf("unexpected session: %+v", read)
	}
	if len(read.Runs) != 1 || len(read.Runs[0].Messages) != 2 {
		t.Fatalf("unexpected runs/messages: %+v", read.Runs)
	}
	if len(read.Runs[0].ToolCalls) != 1 {
		t.Fatalf("unexpected tool_calls: %+v", read.Runs[0].ToolCalls)
	}
	if read.Metadata["key"] != "value" {
		t.Fatalf("unexpected metadata: %+v", read.Metadata)
	}
	if read.SessionState["theme"] != "dark" {
		t.Fatalf("unexpected session state: %+v", read.SessionState)
	}

	// Update.
	s.Summary = "updated summary"
	s.SessionState = map[string]any{"theme": "light"}
	s.Runs[0].Messages = append(s.Runs[0].Messages, models.Message{Role: models.RoleUser, Content: "bye"})
	if err := db.UpdateSession(ctx, s); err != nil {
		t.Fatalf("UpdateSession failed: %v", err)
	}

	updated, err := db.ReadSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ReadSession after update failed: %v", err)
	}
	if updated.Summary != "updated summary" {
		t.Fatalf("summary not updated: %s", updated.Summary)
	}
	if len(updated.Runs[0].Messages) != 2 {
		t.Fatalf("immutable run messages changed: %d", len(updated.Runs[0].Messages))
	}
	if updated.SessionState["theme"] != "light" {
		t.Fatalf("session state not updated: %+v", updated.SessionState)
	}

	// List.
	list, err := db.ListSessions(ctx, dbpkg.SessionQuery{ComponentID: "agent-1", Limit: 10})
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 session, got %d", len(list))
	}
	allSessions, err := db.ListSessions(ctx, dbpkg.SessionQuery{})
	if err != nil || len(allSessions) != 1 {
		t.Fatalf("list all sessions = %d, %v", len(allSessions), err)
	}

	// Delete.
	if err := db.DeleteSession(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	deleted, err := db.ReadSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ReadSession after delete failed: %v", err)
	}
	if deleted != nil {
		t.Fatalf("expected nil after delete, got %+v", deleted)
	}
}

func TestDB_TeamAndWorkflowSessionStateRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	teamSession := &session.TeamSession{
		SessionID: "team-session", TeamID: "team-1", SessionState: map[string]any{"stage": "draft"}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.CreateTeamSession(ctx, teamSession); err != nil {
		t.Fatalf("create team session: %v", err)
	}
	teamRead, err := db.ReadTeamSession(ctx, teamSession.SessionID)
	if err != nil {
		t.Fatalf("read team session: %v", err)
	}
	if teamRead.SessionState["stage"] != "draft" {
		t.Fatalf("unexpected team session state: %+v", teamRead.SessionState)
	}
	teamRead.SessionState = map[string]any{"stage": "complete"}
	if err := db.UpdateTeamSession(ctx, teamRead); err != nil {
		t.Fatalf("update team session: %v", err)
	}
	teamRead, err = db.ReadTeamSession(ctx, teamSession.SessionID)
	if err != nil || teamRead.SessionState["stage"] != "complete" {
		t.Fatalf("team session state update failed: %+v, %v", teamRead, err)
	}

	workflowSession := &session.WorkflowSession{
		SessionID: "workflow-session", WorkflowID: "workflow-1", SessionState: map[string]any{"step": float64(1)}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.CreateWorkflowSession(ctx, workflowSession); err != nil {
		t.Fatalf("create workflow session: %v", err)
	}
	workflowRead, err := db.ReadWorkflowSession(ctx, workflowSession.SessionID)
	if err != nil {
		t.Fatalf("read workflow session: %v", err)
	}
	if workflowRead.SessionState["step"] != float64(1) {
		t.Fatalf("unexpected workflow session state: %+v", workflowRead.SessionState)
	}
	allTeamSessions, err := db.ListTeamSessions(ctx, dbpkg.SessionQuery{})
	if err != nil || len(allTeamSessions) != 1 {
		t.Fatalf("list all team sessions = %d, %v", len(allTeamSessions), err)
	}
	allWorkflowSessions, err := db.ListWorkflowSessions(ctx, dbpkg.SessionQuery{})
	if err != nil || len(allWorkflowSessions) != 1 {
		t.Fatalf("list all workflow sessions = %d, %v", len(allWorkflowSessions), err)
	}
	workflowRead.SessionState = map[string]any{"step": float64(2)}
	if err := db.UpdateWorkflowSession(ctx, workflowRead); err != nil {
		t.Fatalf("update workflow session: %v", err)
	}
	workflowRead, err = db.ReadWorkflowSession(ctx, workflowSession.SessionID)
	if err != nil || workflowRead.SessionState["step"] != float64(2) {
		t.Fatalf("workflow session state update failed: %+v, %v", workflowRead, err)
	}
}

func TestDB_UpgradesLegacySessionTables(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE sessions (session_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, user_id TEXT, metadata TEXT, summary TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE team_sessions (session_id TEXT PRIMARY KEY, team_id TEXT NOT NULL, user_id TEXT, members TEXT, metadata TEXT, summary TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE workflow_sessions (session_id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, user_id TEXT, metadata TEXT, summary TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
	} {
		if _, err := legacy.Exec(statement); err != nil {
			_ = legacy.Close()
			t.Fatalf("create legacy table: %v", err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := legacy.Exec(`INSERT INTO sessions (session_id, agent_id, user_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, "legacy-session", "legacy-agent", "legacy-user", now, now); err != nil {
		_ = legacy.Close()
		t.Fatalf("insert legacy session: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	upgraded, err := New(dsn)
	if err != nil {
		t.Fatalf("upgrade legacy db: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	for _, table := range []string{"sessions", "team_sessions", "workflow_sessions"} {
		rows, err := upgraded.db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		foundState, foundRevision := false, false
		for rows.Next() {
			var cid int
			var name, columnType string
			var notNull, primaryKey int
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			foundState = foundState || name == "session_state"
			foundRevision = foundRevision || name == "revision"
		}
		rows.Close()
		if !foundState || !foundRevision {
			t.Fatalf("%s missing upgraded columns: state=%v revision=%v", table, foundState, foundRevision)
		}
	}
	legacySession, err := upgraded.ReadSession(context.Background(), "legacy-session")
	if err != nil || legacySession == nil || legacySession.SessionState != nil {
		t.Fatalf("legacy session was not preserved: %+v, %v", legacySession, err)
	}
}

func TestDB_ListSessionsPagination(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		s := &session.AgentSession{
			SessionID: "",
			AgentID:   "agent-paginate",
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Hour),
		}
		if err := db.CreateSession(ctx, s); err != nil {
			t.Fatalf("CreateSession failed: %v", err)
		}
	}

	list, err := db.ListSessions(ctx, dbpkg.SessionQuery{ComponentID: "agent-paginate", Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(list))
	}
}

func TestDB_AppendRunPreservesExistingRuns(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Round(time.Microsecond)
	if err := db.CreateSession(ctx, &session.AgentSession{
		SessionID: "append-session", AgentID: "agent-1", UserID: "user-1", CreatedAt: now, UpdatedAt: now,
		Runs: []*session.RunOutput{{
			RunID: "first-run", SessionID: "append-session", AgentID: "agent-1", UserID: "user-1",
			Status: session.RunStatusCompleted, CreatedAt: now, UpdatedAt: now,
		}},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := db.AppendRun(ctx, &session.RunOutput{
		RunID: "second-run", SessionID: "append-session", AgentID: "agent-1", UserID: "user-1",
		Status: session.RunStatusCompleted, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}

	stored, err := db.ReadSession(ctx, "append-session")
	if err != nil || stored == nil {
		t.Fatalf("ReadSession = %+v, %v", stored, err)
	}
	if len(stored.Runs) != 2 || stored.Runs[0].RunID != "first-run" || stored.Runs[1].RunID != "second-run" {
		t.Fatalf("runs = %+v, want both runs in order", stored.Runs)
	}
}

func TestDB_AppendRunRejectsDifferentSessionUser(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Round(time.Microsecond)
	if err := db.CreateSession(ctx, &session.AgentSession{
		SessionID: "alice-session", AgentID: "agent-1", UserID: "alice", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := db.AppendRun(ctx, &session.RunOutput{
		RunID: "bob-run", SessionID: "alice-session", AgentID: "agent-1", UserID: "bob",
		Status: session.RunStatusCompleted, CreatedAt: now, UpdatedAt: now,
	}); err == nil {
		t.Fatal("AppendRun accepted a run for a different user")
	}
}

func TestDB_MemoryCRUD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	m := &memory.UserMemory{
		MemoryID:  "mem-1",
		Memory:    "I love Go",
		Topics:    []string{"golang", "programming"},
		UserID:    "user-1",
		AgentID:   "agent-1",
		Input:     "Tell me about Go",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	if err := db.CreateMemory(ctx, m); err != nil {
		t.Fatalf("CreateMemory failed: %v", err)
	}

	memories, err := db.GetMemories(ctx, "user-1", "agent-1", 10)
	if err != nil {
		t.Fatalf("GetMemories failed: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(memories))
	}
	if memories[0].Memory != "I love Go" || memories[0].MemoryID != "mem-1" {
		t.Fatalf("unexpected memory: %+v", memories[0])
	}
	if len(memories[0].Topics) != 2 {
		t.Fatalf("unexpected topics: %+v", memories[0].Topics)
	}

	if err := db.DeleteMemory(ctx, "mem-1"); err != nil {
		t.Fatalf("DeleteMemory failed: %v", err)
	}

	memories, err = db.GetMemories(ctx, "user-1", "agent-1", 10)
	if err != nil {
		t.Fatalf("GetMemories after delete failed: %v", err)
	}
	if len(memories) != 0 {
		t.Fatalf("expected 0 memories after delete, got %d", len(memories))
	}
}

func TestDB_GetMemoriesLimitAndFilter(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for i := 1; i <= 4; i++ {
		m := &memory.UserMemory{
			MemoryID:  "",
			Memory:    "memory",
			UserID:    "user-limit",
			AgentID:   "agent-limit",
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Minute),
		}
		if err := db.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory failed: %v", err)
		}
	}

	memories, err := db.GetMemories(ctx, "user-limit", "agent-limit", 2)
	if err != nil {
		t.Fatalf("GetMemories failed: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(memories))
	}

	// Most recent first.
	if memories[0].CreatedAt.Before(memories[1].CreatedAt) {
		t.Fatal("memories are not sorted by created_at desc")
	}

	other, err := db.GetMemories(ctx, "other-user", "agent-limit", 10)
	if err != nil {
		t.Fatalf("GetMemories failed: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("expected 0 memories for other user, got %d", len(other))
	}
}
