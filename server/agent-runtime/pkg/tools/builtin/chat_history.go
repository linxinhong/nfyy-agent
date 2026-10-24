// Package builtin provides built-in tools available to agents, teams, and workflows.
package builtin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// ChatHistory returns the current session messages as a JSON string.
func ChatHistory(ctx context.Context) (string, error) {
	rc, ok := RunContextFromContext(ctx)
	if !ok || rc.Session == nil {
		return "", errors.New("no session available")
	}
	messages := rc.Session.GetMessages()
	if messages == nil {
		messages = []models.Message{}
	}
	out, err := json.Marshal(messages)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// NewChatHistoryTool returns a Tool wrapping ChatHistory.
func NewChatHistoryTool() (*tools.Function, error) {
	return tools.NewFunction("chat_history", "Retrieve the current conversation history.", ChatHistory)
}
