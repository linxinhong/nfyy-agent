package in_memory_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory/memorytest"
)

func TestMemoryContract(t *testing.T) {
	memorytest.Contract(t, func(t *testing.T, limits memory.Limits) memorytest.Harness {
		t.Helper()
		var clockMu sync.Mutex
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		store := in_memory.NewMemoryStore().SetClock(clock).SetLimits(limits)
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

func TestMemoryLifecycle(t *testing.T) {
	memorytest.LifecycleContract(t, func(t *testing.T, limits memory.Limits) memorytest.Harness {
		t.Helper()
		var clockMu sync.Mutex
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		store := in_memory.NewMemoryStore().SetClock(clock).SetLimits(limits)
		return memorytest.Harness{Store: store, Advance: func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }, ContentReads: store.ContentReadCount, LifecycleExamined: store.LifecycleExaminedCount, LifecycleAudits: func() []memory.AuditEvent {
			var out []memory.AuditEvent
			for _, audit := range store.Audits() {
				if audit.Action == memory.ActionPurge {
					out = append(out, audit)
				}
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
