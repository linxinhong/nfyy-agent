// Package in_memory provides an in-memory implementation of the db.DB interface.
package in_memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
	"sync"
	"time"
)

// DB is an in-memory implementation of db.DB useful for tests and local use.
type DB struct {
	mu               sync.RWMutex
	sessions         map[string]*session.AgentSession
	teamSessions     map[string]*session.TeamSession
	workflowSessions map[string]*session.WorkflowSession
	memories         map[string]*memory.UserMemory

	// Secondary indexes map an id to the set of namespaces holding a record
	// with that id, so unscoped lookups are O(1) instead of scanning every
	// key. Indexed per record kind so a shared id across kinds never
	// cross-contaminates the ambiguity check.
	sessionIndex         map[string]map[string]struct{}
	teamSessionIndex     map[string]map[string]struct{}
	workflowSessionIndex map[string]map[string]struct{}
	memoryIndex          map[string]map[string]struct{}
}

// compile-time interface check.
var _ dbpkg.DB = (*DB)(nil)

// nsPart normalizes a namespace for partitioning and indexing. An empty
// namespace maps to the reserved "legacy" partition so pre-namespace records
// stay isolated from managed namespaces.
func nsPart(namespace string) string {
	if namespace == "" {
		return execution.ReservedNamespace
	}
	return namespace
}

// nsKey partitions a record by namespace.
func nsKey(namespace, id string) string {
	return nsPart(namespace) + "\x00" + id
}

// indexAdd records that namespace holds an entry with the given id. It is
// idempotent so create and update paths can call it uniformly.
func indexAdd(index map[string]map[string]struct{}, namespace, id string) {
	ns := nsPart(namespace)
	set, ok := index[id]
	if !ok {
		set = make(map[string]struct{})
		index[id] = set
	}
	set[ns] = struct{}{}
}

// findKey resolves an unscoped id via its secondary index. When several
// namespaces share the same id the lookup is ambiguous and returns
// ErrConflict: callers must scope with a namespace.
func findKey(index map[string]map[string]struct{}, id string) (string, bool, error) {
	namespaces := index[id]
	switch len(namespaces) {
	case 0:
		return "", false, nil
	case 1:
		for ns := range namespaces {
			return nsKey(ns, id), true, nil
		}
	}
	return "", false, dbpkg.ErrConflict
}

// New creates a new in-memory database.
func New() *DB {
	return &DB{
		sessions:             make(map[string]*session.AgentSession),
		teamSessions:         make(map[string]*session.TeamSession),
		workflowSessions:     make(map[string]*session.WorkflowSession),
		memories:             make(map[string]*memory.UserMemory),
		sessionIndex:         make(map[string]map[string]struct{}),
		teamSessionIndex:     make(map[string]map[string]struct{}),
		workflowSessionIndex: make(map[string]map[string]struct{}),
		memoryIndex:          make(map[string]map[string]struct{}),
	}
}

// CreateSession stores a session. Empty SessionID will be assigned a generated id.
func (db *DB) CreateSession(_ context.Context, s *session.AgentSession) error {
	if s == nil {
		return fmt.Errorf("session is nil")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if s.SessionID == "" {
		s.SessionID = generateID()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.CreatedAt
	}
	if s.Revision == 0 {
		s.Revision = 1
	}

	clone, err := cloneSession(s)
	if err != nil {
		return err
	}
	key := nsKey(s.Namespace, s.SessionID)
	db.sessions[key] = clone
	indexAdd(db.sessionIndex, s.Namespace, s.SessionID)
	return nil
}

// ReadSession returns a session by id. Returns nil, nil if not found.
func (db *DB) ReadSession(_ context.Context, sessionID string) (*session.AgentSession, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is empty")
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	key, ok, err := findKey(db.sessionIndex, sessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	s, ok := db.sessions[key]
	if !ok {
		return nil, nil
	}
	return cloneSession(s)
}

// UpdateSession replaces an existing session.
func (db *DB) UpdateSession(_ context.Context, s *session.AgentSession) error {
	if s == nil {
		return fmt.Errorf("session is nil")
	}
	if s.SessionID == "" {
		return fmt.Errorf("session_id is empty")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	existing, ok := db.sessions[nsKey(s.Namespace, s.SessionID)]
	if !ok {
		return dbpkg.ErrNotFound
	}
	if s.Revision == 0 || s.Revision != existing.Revision {
		return dbpkg.ErrConflict
	}

	clone, err := cloneSession(s)
	if err != nil {
		return err
	}
	if clone.UpdatedAt.IsZero() {
		clone.UpdatedAt = time.Now().UTC()
	}
	clone.Runs = existing.Runs
	clone.Revision = existing.Revision + 1
	s.Revision = clone.Revision
	key := nsKey(s.Namespace, s.SessionID)
	db.sessions[key] = clone
	indexAdd(db.sessionIndex, s.Namespace, s.SessionID)
	return nil
}

// DeleteSession removes a session by id.
func (db *DB) DeleteSession(_ context.Context, sessionID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	key, ok, err := findKey(db.sessionIndex, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return dbpkg.ErrNotFound
	}
	delete(db.sessions, key)
	// findKey only returns ok when the id is unambiguous (exactly one
	// namespace), so removing the whole index entry is exact.
	delete(db.sessionIndex, sessionID)
	return nil
}

// ListSessions returns sessions filtered before pagination.
func (db *DB) ListSessions(_ context.Context, query dbpkg.SessionQuery) ([]*session.AgentSession, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var result []*session.AgentSession
	for _, s := range db.sessions {
		if query.Namespace != "" && s.Namespace != query.Namespace {
			continue
		}
		if query.ComponentID != "" && s.AgentID != query.ComponentID {
			continue
		}
		if query.UserID != "" && s.UserID != query.UserID {
			continue
		}
		result = append(result, s)
	}

	// Sort by UpdatedAt descending, then CreatedAt descending for stability.
	for i := 0; i < len(result)-1; i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i].UpdatedAt.Before(result[j].UpdatedAt) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Offset > len(result) {
		return []*session.AgentSession{}, nil
	}
	result = result[query.Offset:]

	if query.Limit > 0 && query.Limit < len(result) {
		result = result[:query.Limit]
	}

	return cloneSessions(result)
}

// CreateTeamSession stores a team session. Empty SessionID will be assigned a generated id.
func applyFields(metadata *map[string]any, state *map[string]any, summary *string, update dbpkg.SessionFieldsUpdate) {
	if update.Metadata != nil {
		*metadata = update.Metadata
	}
	if update.SessionState != nil {
		*state = update.SessionState
	}
	if update.Summary != nil {
		*summary = *update.Summary
	}
}

func (db *DB) UpdateSessionFields(ctx context.Context, u dbpkg.SessionFieldsUpdate) (uint64, error) {
	s, err := db.ReadSession(ctx, u.SessionID)
	if err != nil {
		return 0, err
	}
	if s == nil {
		return 0, dbpkg.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	s.UpdatedAt = time.Now().UTC()
	err = db.UpdateSession(ctx, s)
	return s.Revision, err
}
func (db *DB) UpdateTeamSessionFields(ctx context.Context, u dbpkg.SessionFieldsUpdate) (uint64, error) {
	s, err := db.ReadTeamSession(ctx, u.SessionID)
	if err != nil {
		return 0, err
	}
	if s == nil {
		return 0, dbpkg.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	s.UpdatedAt = time.Now().UTC()
	err = db.UpdateTeamSession(ctx, s)
	return s.Revision, err
}
func (db *DB) UpdateWorkflowSessionFields(ctx context.Context, u dbpkg.SessionFieldsUpdate) (uint64, error) {
	s, err := db.ReadWorkflowSession(ctx, u.SessionID)
	if err != nil {
		return 0, err
	}
	if s == nil {
		return 0, dbpkg.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	s.UpdatedAt = time.Now().UTC()
	err = db.UpdateWorkflowSession(ctx, s)
	return s.Revision, err
}

// GetRun returns a run by id, optionally scoped to the owning user.
func (db *DB) GetRun(_ context.Context, query dbpkg.RunQuery) (*session.RunOutput, error) {
	if query.RunID == "" {
		return nil, fmt.Errorf("run_id is empty")
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	for _, s := range db.sessions {
		if query.Namespace != "" && s.Namespace != query.Namespace {
			continue
		}
		if query.UserID != "" && s.UserID != query.UserID {
			continue
		}
		for _, run := range s.Runs {
			if run.RunID == query.RunID {
				return cloneRunOutput(run)
			}
		}
	}
	for _, s := range db.teamSessions {
		if query.Namespace != "" && s.Namespace != query.Namespace {
			continue
		}
		if query.UserID != "" && s.UserID != query.UserID {
			continue
		}
		for _, run := range s.Runs {
			if run.RunID == query.RunID {
				return cloneRunOutput(run)
			}
		}
	}
	for _, s := range db.workflowSessions {
		if query.Namespace != "" && s.Namespace != query.Namespace {
			continue
		}
		if query.UserID != "" && s.UserID != query.UserID {
			continue
		}
		for _, run := range s.Runs {
			if run.RunID == query.RunID {
				return cloneRunOutput(run)
			}
		}
	}

	return nil, nil
}

// AppendRun adds one run to its existing parent session.
func (db *DB) AppendRun(_ context.Context, run *session.RunOutput) error {
	if run == nil {
		return fmt.Errorf("run is nil")
	}
	if run.SessionID == "" {
		return fmt.Errorf("session_id is empty")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	clone, err := cloneRunOutput(run)
	if err != nil {
		return err
	}
	if clone.RunID == "" {
		clone.RunID = generateID()
		run.RunID = clone.RunID
	}
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = time.Now().UTC()
		run.CreatedAt = clone.CreatedAt
	}
	if clone.UpdatedAt.IsZero() {
		clone.UpdatedAt = clone.CreatedAt
		run.UpdatedAt = clone.UpdatedAt
	}

	updatedAt := time.Now().UTC()
	switch {
	case clone.WorkflowID != "":
		s, ok := db.workflowSessions[nsKey(clone.Namespace, clone.SessionID)]
		if !ok || s.WorkflowID != clone.WorkflowID || s.UserID != clone.UserID || s.Namespace != clone.Namespace {
			return fmt.Errorf("workflow session not found: %s", clone.SessionID)
		}
		s.Runs = append(s.Runs, clone)
		s.UpdatedAt = updatedAt
	case clone.TeamID != "":
		s, ok := db.teamSessions[nsKey(clone.Namespace, clone.SessionID)]
		if !ok || s.TeamID != clone.TeamID || s.UserID != clone.UserID || s.Namespace != clone.Namespace {
			return fmt.Errorf("team session not found: %s", clone.SessionID)
		}
		s.Runs = append(s.Runs, clone)
		s.UpdatedAt = updatedAt
	case clone.AgentID != "":
		s, ok := db.sessions[nsKey(clone.Namespace, clone.SessionID)]
		if !ok || s.AgentID != clone.AgentID || s.UserID != clone.UserID || s.Namespace != clone.Namespace {
			return fmt.Errorf("agent session not found: %s", clone.SessionID)
		}
		s.Runs = append(s.Runs, clone)
		s.UpdatedAt = updatedAt
	default:
		return fmt.Errorf("run has no component id")
	}

	return nil
}

// UpdateRun atomically replaces a run when it is still in expectedStatus.
func (db *DB) UpdateRun(_ context.Context, run *session.RunOutput, expectedStatus session.RunStatus) error {
	if run == nil || run.RunID == "" {
		return fmt.Errorf("run is nil or run_id is empty")
	}
	if !expectedStatus.CanTransitionTo(run.Status) {
		return dbpkg.ErrInvalidTransition
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	clone, err := cloneRunOutput(run)
	if err != nil {
		return err
	}
	if clone.UpdatedAt.IsZero() {
		clone.UpdatedAt = time.Now().UTC()
		run.UpdatedAt = clone.UpdatedAt
	}

	var runs *[]*session.RunOutput
	switch {
	case clone.WorkflowID != "":
		parent, ok := db.workflowSessions[nsKey(clone.Namespace, clone.SessionID)]
		if !ok || parent.WorkflowID != clone.WorkflowID || parent.UserID != clone.UserID {
			return dbpkg.ErrNotFound
		}
		runs = &parent.Runs
		parent.UpdatedAt = clone.UpdatedAt
	case clone.TeamID != "":
		parent, ok := db.teamSessions[nsKey(clone.Namespace, clone.SessionID)]
		if !ok || parent.TeamID != clone.TeamID || parent.UserID != clone.UserID {
			return dbpkg.ErrNotFound
		}
		runs = &parent.Runs
		parent.UpdatedAt = clone.UpdatedAt
	case clone.AgentID != "":
		parent, ok := db.sessions[nsKey(clone.Namespace, clone.SessionID)]
		if !ok || parent.AgentID != clone.AgentID || parent.UserID != clone.UserID {
			return dbpkg.ErrNotFound
		}
		runs = &parent.Runs
		parent.UpdatedAt = clone.UpdatedAt
	default:
		return fmt.Errorf("run has no component id")
	}

	for i, existing := range *runs {
		if existing.RunID != clone.RunID {
			continue
		}
		if existing.Status != expectedStatus {
			return dbpkg.ErrConflict
		}
		(*runs)[i] = clone
		return nil
	}
	return dbpkg.ErrNotFound
}

// CreateMemory stores a memory. Empty MemoryID will be assigned a generated id.
func (db *DB) Ping(_ context.Context) error {
	return nil
}

// Close is a no-op for the in-memory store.
func (db *DB) Close() error {
	return nil
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to a timestamp-based id if the system RNG fails.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func cloneSession(s *session.AgentSession) (*session.AgentSession, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var clone session.AgentSession
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func cloneRunOutput(run *session.RunOutput) (*session.RunOutput, error) {
	data, err := json.Marshal(run)
	if err != nil {
		return nil, err
	}
	var clone session.RunOutput
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	if clone.WorkflowID != "" && clone.Workflow == nil {
		clone.Workflow = &workflowschema.RunData{Legacy: true}
	}
	return &clone, nil
}

func cloneSessions(src []*session.AgentSession) ([]*session.AgentSession, error) {
	out := make([]*session.AgentSession, len(src))
	for i, s := range src {
		clone, err := cloneSession(s)
		if err != nil {
			return nil, err
		}
		out[i] = clone
	}
	return out, nil
}

func cloneMemory(m *memory.UserMemory) (*memory.UserMemory, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var clone memory.UserMemory
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func cloneMemories(src []*memory.UserMemory) ([]*memory.UserMemory, error) {
	out := make([]*memory.UserMemory, len(src))
	for i, m := range src {
		clone, err := cloneMemory(m)
		if err != nil {
			return nil, err
		}
		out[i] = clone
	}
	return out, nil
}

func cloneTeamSession(s *session.TeamSession) (*session.TeamSession, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var clone session.TeamSession
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func cloneTeamSessions(src []*session.TeamSession) ([]*session.TeamSession, error) {
	out := make([]*session.TeamSession, len(src))
	for i, s := range src {
		clone, err := cloneTeamSession(s)
		if err != nil {
			return nil, err
		}
		out[i] = clone
	}
	return out, nil
}

func cloneWorkflowSession(s *session.WorkflowSession) (*session.WorkflowSession, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var clone session.WorkflowSession
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	for _, run := range clone.Runs {
		if run != nil && run.WorkflowID != "" && run.Workflow == nil {
			run.Workflow = &workflowschema.RunData{Legacy: true}
		}
	}
	return &clone, nil
}

func cloneWorkflowSessions(src []*session.WorkflowSession) ([]*session.WorkflowSession, error) {
	out := make([]*session.WorkflowSession, len(src))
	for i, s := range src {
		clone, err := cloneWorkflowSession(s)
		if err != nil {
			return nil, err
		}
		out[i] = clone
	}
	return out, nil
}
