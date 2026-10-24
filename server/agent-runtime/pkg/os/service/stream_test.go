package service

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// 思考过程与工具调用事件必须原样透传到 SSE，供前端折叠展示；
// 此前 StreamEvents 只转发 run/message/completed，思考内容对客户端不可见。
func TestStreamEventsForwardsReasoningAndTools(t *testing.T) {
	events := make(chan agent.Event)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		StreamEvents(recorder, events)
		close(done)
	}()

	events <- agent.Event{Type: agent.EventTypeRunStarted}
	events <- agent.Event{Type: agent.EventTypeReasoning, Content: "先想一下"}
	events <- agent.Event{Type: agent.EventTypeToolCall, ToolCall: &models.ToolCall{ID: "t1", Function: models.FunctionCall{Name: "get_weather", Arguments: "{\"city\":\"广州\"}"}}}
	events <- agent.Event{Type: agent.EventTypeToolResult, ToolResult: "Cloudy 7~13°C"}
	events <- agent.Event{Type: agent.EventTypeMessage, Content: "答案"}
	events <- agent.Event{Type: agent.EventTypeRunCompleted, RunOutput: nil}
	close(events)
	<-done

	body := recorder.Body.String()
	for _, want := range []string{
		"event: run_started",
		"event: reasoning",
		`"content":"先想一下"`,
		"event: tool_call",
		`"name":"get_weather"`,
		`"arguments":"{\"city\":\"广州\"}"`,
		"event: tool_result",
		`"result":"Cloudy 7~13°C"`,
		"event: message",
		"event: run_completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE body missing %q\nbody:\n%s", want, body)
		}
	}

	// 每个事件帧以 "event:" 行开头，帧间以空行分隔，保证客户端逐帧解析
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if !strings.Contains(frame, "event: ") {
			t.Errorf("malformed frame: %q", frame)
		}
	}
}
