package run_test

import (
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run/runtest"
)

func TestMemoryDispatcherContract(t *testing.T) {
	runtest.DispatcherContract(t, func(clock run.Clock) run.Dispatcher {
		return run.NewMemoryDispatcher().SetClock(clock)
	})
}

func TestMemoryDispatcherManagedStoreContract(t *testing.T) {
	runtest.ManagedStoreContract(t, func(_ *testing.T, clock run.Clock) run.ManagedStore {
		return run.NewMemoryDispatcher().SetClock(clock)
	})
}
