package agent_test

import (
	"context"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestAgentLocallyValidatesStructuredOutput(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []any{"accepted", "rejected"}},
		},
		"required":             []any{"status"},
		"additionalProperties": false,
	}

	tests := []struct {
		name    string
		content string
		parsed  any
		wantOK  bool
	}{
		{name: "valid content", content: `{"status":"accepted"}`, wantOK: true},
		{name: "missing required", content: `{}`},
		{name: "wrong enum", content: `{"status":"maybe"}`},
		{name: "extra property", content: `{"status":"accepted","secret":true}`},
		{name: "untrusted parsed", content: `{"status":"accepted"}`, parsed: map[string]any{"status": "forged"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := mock.New()
			model.ResponseData = &models.ModelResponse{Content: tt.content, Parsed: tt.parsed}
			value, err := agent.New(agent.AgentConfig{ID: "validator", Model: model, OutputSchema: schema})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			out, runErr := value.Run(context.Background(), "validate")
			if tt.wantOK {
				if runErr != nil || out.Status != session.RunStatusCompleted || out.Parsed == nil {
					t.Fatalf("Run = %+v, %v", out, runErr)
				}
				return
			}
			if runErr == nil || out.Status != session.RunStatusError || out.Parsed != nil {
				t.Fatalf("Run = %+v, %v", out, runErr)
			}
		})
	}
}

func TestAgentRejectsInvalidOutputSchemaAtCreation(t *testing.T) {
	_, err := agent.New(agent.AgentConfig{Model: mock.New(), OutputSchema: map[string]any{"type": "not-a-json-type"}})
	if err == nil {
		t.Fatal("New accepted invalid schema")
	}
}

func TestAgentRejectsExternalOutputSchemaReference(t *testing.T) {
	_, err := agent.New(agent.AgentConfig{Model: mock.New(), OutputSchema: map[string]any{"$ref": "https://example.com/schema.json"}})
	if err == nil {
		t.Fatal("New accepted external schema reference")
	}
}
