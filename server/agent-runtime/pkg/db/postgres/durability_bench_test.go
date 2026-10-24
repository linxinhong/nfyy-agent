package postgres

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/stream"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// toolLoopModel calls the tool once, then answers.
type toolLoopModel struct{}

func (toolLoopModel) Response(_ context.Context, messages []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	if messages[len(messages)-1].Role == models.RoleTool {
		return &models.ModelResponse{Content: "done", FinishReason: models.FinishReasonStop}, nil
	}
	return &models.ModelResponse{ToolCalls: []models.ToolCall{{ID: "c", Type: "function",
		Function: models.FunctionCall{Name: "bench_tool", Arguments: `{}`}}}, FinishReason: models.FinishReasonToolCalls}, nil
}
func (toolLoopModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, fmt.Errorf("not streamed")
}
func (toolLoopModel) ID() string       { return "tool-loop" }
func (toolLoopModel) Provider() string { return "bench" }

type benchTool struct{}

func (benchTool) Name() string        { return "bench_tool" }
func (benchTool) Description() string { return "d" }
func (benchTool) Schema() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": "bench_tool", "parameters": map[string]any{"type": "object"}}}
}
func (benchTool) Call(context.Context, map[string]any) (any, error) { return "ok", nil }

// BenchmarkAgentDurabilityPostgres measures a persisted run with one tool
// round against PostgreSQL with the durability features off and on.
func BenchmarkAgentDurabilityPostgres(b *testing.B) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		b.Skip("POSTGRES_TEST_DSN is not set")
	}
	store, err := New(context.Background(), dsn, Options{AutoMigrate: true})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	for _, durable := range []bool{false, true} {
		b.Run(fmt.Sprintf("durable=%v", durable), func(b *testing.B) {
			// Start every measurement from the same table sizes.
			if _, err := store.db.ExecContext(context.Background(), `DELETE FROM agent_runtime_sessions WHERE component_id = 'pg-durability-bench'`); err != nil {
				b.Fatal(err)
			}
			cfg := agent.AgentConfig{ID: "pg-durability-bench", Model: toolLoopModel{}, DB: store, UserID: "bench", Tools: []tools.Tool{benchTool{}}}
			if !durable {
				cfg.DisableRoundCheckpoints = true
				cfg.RunLeaseTimeout = -1
			}
			value, err := agent.New(cfg)
			if err != nil {
				b.Fatal(err)
			}
			var seq atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					req := session.RunRequest{Input: session.RunInput{Message: fmt.Sprintf("q-%d", seq.Add(1))}}
					if _, err := value.RunWith(context.Background(), req); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// BenchmarkStreamJournalFlush measures one stream hub flush: a batch of
// token frames appended to the shared journal.
func BenchmarkStreamJournalFlush(b *testing.B) {
	if os.Getenv("POSTGRES_TEST_DSN") == "" {
		b.Skip("POSTGRES_TEST_DSN is not set")
	}
	journal := testDB(b).StreamJournal()
	owner := stream.Owner{Subject: "bench", Isolated: true}
	runID := "bench-" + uuid.NewString()
	ctx := context.Background()
	seq := uint64(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := make([]stream.Frame, 10)
		for k := range batch {
			seq++
			batch[k] = stream.Frame{Seq: seq, Event: "message", Data: []byte(`{"content":"token"}`)}
		}
		if err := journal.Append(ctx, runID, owner, batch, false); err != nil {
			b.Fatal(err)
		}
	}
}
