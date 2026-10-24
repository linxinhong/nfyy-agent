//go:build integration

// Protected skill injection: the provider receives the skill after the
// client's system prompt only for skills active for the caller; copied runs
// are masked in plain and streamed answers; a tool call that copies the
// skill withholds the answer (streamed tool calls are held until checked);
// other inference routes refuse the header; audit names skills, never text.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/skillguard"
	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillshare"
)

const injectedSkill = "# 对赌回购审查\n先看回购触发条件是否约定了明确的业绩指标和考核期间，避免触发条件模糊导致争议。\n"
const copiedSentence = "先看回购触发条件是否约定了明确的业绩指标和考核期间"

func TestProtectedSkillInjection(t *testing.T) {
	var mu sync.Mutex
	var captured []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		captured = append(captured, body)
		mu.Unlock()
		msgs := body["messages"].([]any)
		ask := msgs[len(msgs)-1].(map[string]any)["content"].(string)
		reply := "结论：可以主张回购。"
		args := `{"path":"notes.md"}`
		if strings.Contains(ask, "copy") {
			reply = "要点是" + copiedSentence + "，所以可以主张。"
		}
		if ask == "copyall" {
			reply = "原文：" + injectedSkill + "以上。"
		}
		if ask == "tool" {
			args = `{"path":"skill.md","content":"` + copiedSentence + `"}`
		}
		if stream, _ := body["stream"].(bool); stream {
			frame := func(delta string) { _, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":" + delta + "}]}\n\n")) }
			runes := []rune(reply)
			for i := 0; i < len(runes); i += 6 {
				b, _ := json.Marshal(string(runes[i:min(i+6, len(runes))]))
				frame(`{"content":` + string(b) + `}`)
			}
			if strings.Contains(ask, "tool") {
				b, _ := json.Marshal(args)
				frame(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":` + string(b) + `}}]}`)
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"))
			return
		}
		msg := map[string]any{"role": "assistant", "content": reply}
		if strings.Contains(ask, "tool") {
			msg["tool_calls"] = []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "write_file", "arguments": args}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	}))
	t.Cleanup(upstream.Close)
	env := newToolEnv(t, upstream)
	env.srv.Members = env.store
	env.srv.Content = content.New(env.pool, env.store)
	env.srv.Skills = skillshare.New(env.pool, env.store)
	env.handler = env.srv.Handler()
	key := toolEnvKey(t, env)
	ctx := context.Background()

	archive, digest, err := skillreg.BuildArchive([]skillreg.PackageFile{{Path: skillshare.MarkerFile, Content: "protected\n"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"equity-review", "not-enabled"} {
		if _, err := env.pool.Exec(ctx, `INSERT INTO skill_packages (name, version, workspace_id, digest, payload, status, published_by, visibility, protected_body)
			VALUES ($1, '1.0.0', 'finance', $2, $3, 'released', 'someone-else', 'organization', $4)`, name, digest, archive, injectedSkill); err != nil {
			t.Fatalf("seed skill: %v", err)
		}
	}
	headers := toolAuthz(key)
	if rec := env.do(t, http.MethodPut, "/enterprise/v1/me/skills/equity-review", key, "", headers); rec.Code != http.StatusNoContent {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	withSkills := map[string]string{HeaderSkillsName: "equity-review,not-enabled"}
	for k, v := range headers {
		withSkills[k] = v
	}
	chat := func(ask string, stream bool, hdr map[string]string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"model": "stub-model", "stream": stream, "messages": []any{
			map[string]any{"role": "system", "content": "你是律师助手。"}, map[string]any{"role": "user", "content": ask}}})
		return env.do(t, http.MethodPost, "/v1/chat/completions", key, string(body), hdr)
	}
	last := func() []any {
		mu.Lock()
		defer mu.Unlock()
		return captured[len(captured)-1]["messages"].([]any)
	}

	// Injection: after the client's system prompt, only the active skill.
	if rec := chat("plain", false, withSkills); rec.Code != http.StatusOK {
		t.Fatalf("plain: %d %s", rec.Code, rec.Body.String())
	}
	msgs := last()
	injected, _ := msgs[1].(map[string]any)["content"].(string)
	if len(msgs) != 3 || msgs[0].(map[string]any)["content"] != "你是律师助手。" || !strings.Contains(injected, copiedSentence) ||
		!strings.Contains(injected, `name="equity-review"`) || strings.Contains(injected, `name="not-enabled"`) ||
		!strings.Contains(injected, "技能原文受保护") {
		t.Fatalf("provider messages: %v", msgs)
	}
	if rec := chat("plain", false, headers); rec.Code != http.StatusOK || len(last()) != 2 {
		t.Fatal("no header, no injection")
	}

	// Copied runs are masked; a copying tool call withholds the answer.
	rec := chat("copy", false, withSkills)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "业绩指标") || !strings.Contains(rec.Body.String(), skillguard.Mask) ||
		!strings.Contains(rec.Body.String(), "所以可以主张") {
		t.Fatalf("masked answer: %d %s", rec.Code, rec.Body.String())
	}
	if rec := chat("tool", false, withSkills); rec.Code != http.StatusForbidden {
		t.Fatalf("copying tool call: %d %s", rec.Code, rec.Body.String())
	}
	if rec := chat("copy", false, headers); !strings.Contains(rec.Body.String(), "业绩指标") {
		t.Fatal("without injected skills nothing is masked")
	}

	// Streaming: masked in the window; tool calls held and withheld.
	rec = chat("copy", true, withSkills)
	text := sseText(t, rec.Body.String())
	if strings.Contains(text, "业绩指标") || !strings.Contains(text, skillguard.Mask) || !strings.Contains(text, "所以可以主张") {
		t.Fatalf("streamed answer: %q", text)
	}
	// A copy streamed in small deltas leaves no tail behind the masks.
	rec = chat("copyall", true, withSkills)
	text = sseText(t, rec.Body.String())
	if strings.Contains(text, "考核期间") || strings.Contains(text, "导致争议") || strings.Count(text, skillguard.Mask) != 1 ||
		!strings.HasPrefix(text, "原文：") || !strings.HasSuffix(text, "以上。") {
		t.Fatalf("streamed full copy: %q", text)
	}
	rec = chat("tool", true, withSkills)
	if !strings.Contains(rec.Body.String(), "[withheld:protected-skill]") || strings.Contains(rec.Body.String(), "业绩指标") {
		t.Fatalf("streamed copying tool call: %s", rec.Body.String())
	}
	rec = chat("plain-tool", true, withSkills)
	if !strings.Contains(rec.Body.String(), `"name":"write_file"`) || !strings.Contains(rec.Body.String(), `notes.md`) {
		t.Fatalf("a clean held tool call is released: %s", rec.Body.String())
	}

	// Other inference routes refuse the header.
	if rec := env.do(t, http.MethodPost, "/v1/messages", key, `{"model":"stub-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, withSkills); rec.Code != http.StatusBadRequest {
		t.Fatalf("messages with skills: %d %s", rec.Code, rec.Body.String())
	}

	// Audit names skills and never carries their text.
	var detail string
	if err := env.pool.QueryRow(ctx, `SELECT detail::text FROM audit_events WHERE type = 'skill.injected' ORDER BY time DESC LIMIT 1`).Scan(&detail); err != nil ||
		!strings.Contains(detail, "equity-review@1.0.0") || !strings.Contains(detail, "not-enabled") || strings.Contains(detail, "业绩指标") {
		t.Fatalf("injection audit: %s %v", detail, err)
	}
	var guarded int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE type = 'skill.output_guarded'`).Scan(&guarded); err != nil || guarded < 3 {
		t.Fatalf("output guard audit: %d %v", guarded, err)
	}
}

const HeaderSkillsName = "X-MindLink-Skills"

func sseText(t *testing.T, body string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err == nil && len(chunk.Choices) > 0 {
			b.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return b.String()
}
