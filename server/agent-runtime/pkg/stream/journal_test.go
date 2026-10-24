package stream_test

import (
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/stream"
	"github.com/linxinhong/xingding-agent-runtime/pkg/stream/journaltest"
)

func TestMemoryJournalContract(t *testing.T) {
	journaltest.Run(t, stream.NewMemoryJournal(), "memory")
}
