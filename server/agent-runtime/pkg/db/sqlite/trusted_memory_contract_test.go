package sqlite

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory/memorytest"
)

func TestMemorySQLiteContract(t *testing.T) {
	memorytest.Contract(t, func(t *testing.T, limits memory.Limits) memorytest.Harness {
		t.Helper()
		db, err := New(filepath.Join(t.TempDir(), "memory.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		var clockMu sync.Mutex
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}
		store := NewMemoryStore(db).SetClock(clock).SetLimits(limits)
		return memorytest.Harness{
			Store:               store,
			Advance:             func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() },
			ContentReads:        store.ContentReadCount,
			ReceiptContentBytes: store.ReceiptContentBytes,
			FailAudit: func(fail bool) {
				if fail {
					store.SetAuditFailure(func(memory.AuditEvent) error { return errors.New("injected") })
				} else {
					store.SetAuditFailure(nil)
				}
			},
		}
	})
}

func TestMemorySQLiteLifecycle(t *testing.T) {
	memorytest.LifecycleContract(t, func(t *testing.T, limits memory.Limits) memorytest.Harness {
		t.Helper()
		db, err := New(filepath.Join(t.TempDir(), "memory-lifecycle.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		var clockMu sync.Mutex
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		store := NewMemoryStore(db).SetClock(clock).SetLimits(limits)
		return memorytest.Harness{Store: store, Advance: func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }, ContentReads: store.ContentReadCount, LifecycleAudits: func() []memory.AuditEvent {
			rows, err := db.db.Query(`SELECT operation_id,namespace,project_id,subject_id,collection_id,action,actor,issuer,memory_id,revision,generation,occurred_at_ns FROM agent_runtime_memory_audits WHERE action='purge' ORDER BY audit_id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []memory.AuditEvent
			for rows.Next() {
				var audit memory.AuditEvent
				var occurred int64
				if err := rows.Scan(&audit.OperationID, &audit.Subject.Namespace, &audit.Subject.ProjectID, &audit.Subject.SubjectID, &audit.Scope.CollectionID, &audit.Action, &audit.Actor, &audit.Issuer, &audit.MemoryID, &audit.Revision, &audit.Generation, &occurred); err != nil {
					t.Fatal(err)
				}
				audit.Scope.Namespace, audit.Scope.ProjectID, audit.Scope.SubjectID = audit.Subject.Namespace, audit.Subject.ProjectID, audit.Subject.SubjectID
				audit.OccurredAt = fromNS(occurred)
				out = append(out, audit)
			}
			return out
		}, ReceiptContentBytes: store.ReceiptContentBytes, FailAudit: func(fail bool) {
			if fail {
				store.SetAuditFailure(func(memory.AuditEvent) error { return errors.New("injected") })
			} else {
				store.SetAuditFailure(nil)
			}
		}}
	})
}
