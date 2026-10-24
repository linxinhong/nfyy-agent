//go:build integration

// TASK-055 regression: a mid-stream provider death must NEVER masquerade as
// clean completion, on any of the three streaming protocols.
//
// The race (found by TASK-046, failure-modes report finding 1): when the
// provider dies mid-stream, the router's stream goroutine delivers the
// terminal error on the BUFFERED errCh and then closes BOTH errCh and chunks,
// back-to-back. If the gateway handler reaches its select after both channels
// became ready (it was busy consuming the deltas buffered before the death),
// Go picks between the ready cases at random. The chunks-closed branch used to
// treat the death as clean completion: no terminal SSE error frame, outcome
// audited as allowed, a fabricated zero-usage ledger row (ADR-0027 violated).
//
// The stub below makes that interleaving the NORM: it streams two deltas with
// zero pacing, then aborts the connection. The kernel goroutine buffers both
// deltas instantly, reads the reset, sends the error and closes both channels
// — while the handler is still busy with its per-delta work (DLP scans hit
// PostgreSQL). The handler therefore parks on a select with BOTH cases ready,
// and pre-fix Go's random pick masquerades roughly half the iterations. The
// test iterates each protocol >=50 times and requires the terminal error
// frame + audited failure + zero usage rows on EVERY iteration: after the
// gateway-side drain fix this is deterministic (150/150), pre-fix it flaps.
// A clean-completion control case (well-behaved stub) pins the no-error path:
// normal stop frames, an allowed outcome and exactly one real usage row.
package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sdrIterations is the per-protocol death-race iteration count (card: >=50).
const sdrIterations = 50

// --- SSE frame plumbing (shared by the three protocol flavors) ---

// sdrFrame is one parsed SSE data line with the event name that preceded it
// ("" for the default chat frames, which carry no event: line).
type sdrFrame struct {
	event string
	data  string
}

// sdrReadFrames drains an SSE body into frames. A body that ends without a
// terminal frame (the pre-fix masquerade shape) simply yields fewer frames —
// the callers assert on what MUST be there.
func sdrReadFrames(t *testing.T, body io.Reader) []sdrFrame {
	t.Helper()
	var frames []sdrFrame
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case line == "data: [DONE]":
			frames = append(frames, sdrFrame{data: "[DONE]"})
			event = ""
		case strings.HasPrefix(line, "data: "):
			frames = append(frames, sdrFrame{event: event, data: strings.TrimPrefix(line, "data: ")})
			event = ""
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("stream read: %v", err)
	}
	return frames
}

// sdrPostStream sends one streaming protocol request through the real handler
// chain and returns the parsed frames (status must be 200: the stream headers
// are committed before any mid-stream death can happen).
func sdrPostStream(t *testing.T, ts *httptest.Server, client *http.Client, path, key, body, reqID string) []sdrFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Agent-Workspace", "secops")
	req.Header.Set("X-Request-ID", reqID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream request %s: %v", reqID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %s: %d (headers must already be committed when the upstream dies)", reqID, resp.StatusCode)
	}
	return sdrReadFrames(t, resp.Body)
}

// sdrChunk decodes a chat-shaped data frame.
func sdrChunk(t *testing.T, f sdrFrame) (content string, finish *string) {
	t.Helper()
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(f.data), &chunk); err != nil {
		return "", nil
	}
	if len(chunk.Choices) == 0 {
		return "", nil
	}
	return chunk.Choices[0].Delta.Content, chunk.Choices[0].FinishReason
}

// sdrEventData decodes a Responses/Messages-shaped event frame.
func sdrEventData(t *testing.T, f sdrFrame) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(f.data), &m); err != nil {
		return nil
	}
	return m
}

// sdrEnv bundles what each scenario needs after wiring.
type sdrEnv struct {
	env    *sliceEnv
	ts     *httptest.Server
	client *http.Client
	key    string
}

// sdrSetup wires the production governance chain (same durable path as the
// failure-modes suite) with both stub providers: the death stub and the
// clean-completion control stub.
func sdrSetup(t *testing.T) *sdrEnv {
	t.Helper()
	env := newSliceEnv(t, 1<<30)
	env.srv.OIDCStore = env.store
	fmWireGovernance(t, env)

	// Death stub: two deltas back-to-back, then abort. No pacing on purpose:
	// the router goroutine must buffer both deltas, read the reset, send the
	// error on errCh and close BOTH channels while the handler is still busy
	// with the earlier deltas, so the handler's select sees both cases ready.
	death := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		delta := func(text string) {
			b, _ := json.Marshal(map[string]any{"id": "d1", "object": "chat.completion.chunk", "model": "sdr-model",
				"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": text}}}})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
		delta("race ")
		delta("death")
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(death.Close)

	// Control stub: well-behaved stream — deltas, a real stop frame carrying
	// usage, then [DONE]. The clean-completion path must stay untouched.
	clean := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v map[string]any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
		chunk := func(delta map[string]any, finish string, usage map[string]any) map[string]any {
			choice := map[string]any{"index": 0, "delta": delta}
			if finish != "" {
				choice["finish_reason"] = finish
			}
			frame := map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": "sdr-model",
				"choices": []map[string]any{choice}}
			if usage != nil {
				frame["usage"] = usage
			}
			return frame
		}
		send(chunk(map[string]any{"content": "hello "}, "", nil))
		send(chunk(map[string]any{"content": "world"}, "", nil))
		send(chunk(map[string]any{}, "stop", map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8}))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(clean.Close)

	fmSeedAlias(t, env, "sdr-death", []string{"sdr-death-provider"}, map[string]string{"sdr-death-provider": death.URL})
	fmSeedAlias(t, env, "sdr-clean", []string{"sdr-clean-provider"}, map[string]string{"sdr-clean-provider": clean.URL})

	mcpLogin(t, env, "sdr@example.com", "secops")
	key := seedKey(t, env, mcpUserID(t, env, "sdr@example.com"))

	ts := httptest.NewServer(env.handler)
	t.Cleanup(ts.Close)
	client := &http.Client{Timeout: 60 * time.Second}
	t.Cleanup(func() { client.CloseIdleConnections() })

	return &sdrEnv{env: env, ts: ts, client: client, key: key}
}

// sdrAssertFailureBookkeeping pins the governance contract for a dead stream
// (ADR-0027): the audit set carries the failure, the ledger holds nothing.
func sdrAssertFailureBookkeeping(t *testing.T, e *sdrEnv, reqID string) {
	t.Helper()
	if got := fmCount(t, e.env,
		`SELECT count(*) FROM audit_events WHERE request_id=$1 AND type='llm.request'
		 AND detail->>'error_code'='upstream_unavailable' AND outcome='denied'`, reqID); got != 1 {
		t.Errorf("[%s] mid-stream death must be audited as a failure with the upstream error class, got %d llm.request rows", reqID, got)
	}
	if got := fmCount(t, e.env,
		`SELECT count(*) FROM audit_events WHERE request_id=$1 AND type='policy.decision'`, reqID); got != 1 {
		t.Errorf("[%s] policy decision must be recorded for the failed stream, got %d", reqID, got)
	}
	if got := fmCount(t, e.env, `SELECT count(*) FROM usage_records WHERE request_id=$1`, reqID); got != 0 {
		t.Errorf("[%s] no usage may be fabricated for a stream that never completed, got %d rows", reqID, got)
	}
}

// --- the regression: death must surface as the terminal error frame, 100% ---

func TestStreamDeathRaceMidStreamErrorAlwaysTerminal(t *testing.T) {
	e := sdrSetup(t)

	// sdrDeathCase drives one protocol through one death iteration and
	// reports every way the death masqueraded as clean completion.
	chatCase := func(i int) {
		reqID := fmUnique(fmt.Sprintf("sdr-chat-%02d", i))
		frames := sdrPostStream(t, e.ts, e.client, "/v1/chat/completions", e.key,
			`{"model":"sdr-death","stream":true,"messages":[{"role":"user","content":"hi"}]}`, reqID)
		sawError, sawDone, sawStop := false, false, false
		errorContent := ""
		for _, f := range frames {
			if f.data == "[DONE]" {
				sawDone = true
				continue
			}
			content, finish := sdrChunk(t, f)
			if finish == nil {
				continue
			}
			switch *finish {
			case "error":
				sawError = true
				errorContent = content
			case "stop":
				sawStop = true
			}
		}
		if !sawError {
			t.Errorf("[%s] death masqueraded: no terminal finish_reason:\"error\" frame (clean completion has none)", reqID)
		} else if !strings.Contains(errorContent, "[stream error]") {
			t.Errorf("[%s] terminal frame must carry the EAP stream-error marker, got %q", reqID, errorContent)
		}
		if !sawDone {
			t.Errorf("[%s] death masqueraded: [DONE] missing — the error frame must terminate the stream", reqID)
		}
		if sawStop {
			t.Errorf("[%s] a dead stream must never emit a stop frame", reqID)
		}
		sdrAssertFailureBookkeeping(t, e, reqID)
	}
	responsesCase := func(i int) {
		reqID := fmUnique(fmt.Sprintf("sdr-resp-%02d", i))
		frames := sdrPostStream(t, e.ts, e.client, "/v1/responses", e.key,
			`{"model":"sdr-death","input":"hi","stream":true}`, reqID)
		sawError, sawCompleted := false, false
		for _, f := range frames {
			switch f.event {
			case "error":
				sawError = true
			case "response.completed":
				sawCompleted = true
			}
		}
		if !sawError {
			t.Errorf("[%s] death masqueraded: no terminal error event", reqID)
		}
		if sawCompleted {
			t.Errorf("[%s] death masqueraded: response.completed emitted for a dead stream", reqID)
		}
		sdrAssertFailureBookkeeping(t, e, reqID)
	}
	messagesCase := func(i int) {
		reqID := fmUnique(fmt.Sprintf("sdr-msg-%02d", i))
		frames := sdrPostStream(t, e.ts, e.client, "/v1/messages", e.key,
			`{"model":"sdr-death","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, reqID)
		sawError, sawMessageStop := false, false
		for _, f := range frames {
			switch f.event {
			case "error":
				sawError = true
			case "message_stop":
				sawMessageStop = true
			}
		}
		if !sawError {
			t.Errorf("[%s] death masqueraded: no terminal error event", reqID)
		}
		if sawMessageStop {
			t.Errorf("[%s] death masqueraded: message_stop emitted for a dead stream", reqID)
		}
		sdrAssertFailureBookkeeping(t, e, reqID)
	}

	t.Run("chat completions", func(t *testing.T) {
		for i := 1; i <= sdrIterations; i++ {
			chatCase(i)
		}
	})
	t.Run("responses", func(t *testing.T) {
		for i := 1; i <= sdrIterations; i++ {
			responsesCase(i)
		}
	})
	t.Run("messages", func(t *testing.T) {
		for i := 1; i <= sdrIterations; i++ {
			messagesCase(i)
		}
	})
}

// --- control: the clean-completion path (no error) must stay unchanged ---

func TestStreamDeathRaceCleanCompletionControl(t *testing.T) {
	e := sdrSetup(t)

	t.Run("chat completions", func(t *testing.T) {
		reqID := fmUnique("sdr-ctrl-chat")
		frames := sdrPostStream(t, e.ts, e.client, "/v1/chat/completions", e.key,
			`{"model":"sdr-clean","stream":true,"messages":[{"role":"user","content":"hi"}]}`, reqID)
		sawStop, sawDone, sawError := false, false, false
		for _, f := range frames {
			if f.data == "[DONE]" {
				sawDone = true
				continue
			}
			_, finish := sdrChunk(t, f)
			if finish == nil {
				continue
			}
			switch *finish {
			case "stop":
				sawStop = true
			case "error":
				sawError = true
			}
		}
		if !sawStop || !sawDone || sawError {
			t.Fatalf("clean stream must end with a stop frame + [DONE] and no error frame (stop=%v done=%v error=%v)", sawStop, sawDone, sawError)
		}
		if got := fmCount(t, e.env, `SELECT count(*) FROM usage_records WHERE request_id=$1 AND total_tokens=8`, reqID); got != 1 {
			t.Fatalf("a completed stream must bill exactly the provider-reported usage, got %d rows", got)
		}
		if got := fmCount(t, e.env,
			`SELECT count(*) FROM audit_events WHERE request_id=$1 AND type='llm.request' AND outcome='allowed'`, reqID); got != 1 {
			t.Fatalf("clean completion must stay audited as allowed, got %d", got)
		}
	})

	t.Run("responses", func(t *testing.T) {
		reqID := fmUnique("sdr-ctrl-resp")
		frames := sdrPostStream(t, e.ts, e.client, "/v1/responses", e.key,
			`{"model":"sdr-clean","input":"hi","stream":true}`, reqID)
		sawCompleted, sawError := false, false
		for _, f := range frames {
			switch f.event {
			case "response.completed":
				sawCompleted = true
			case "error":
				sawError = true
			}
		}
		if !sawCompleted || sawError {
			t.Fatalf("clean stream must end with response.completed and no error event (completed=%v error=%v)", sawCompleted, sawError)
		}
		if got := fmCount(t, e.env, `SELECT count(*) FROM usage_records WHERE request_id=$1`, reqID); got != 1 {
			t.Fatalf("a completed stream must bill exactly one usage row, got %d", got)
		}
	})

	t.Run("messages", func(t *testing.T) {
		reqID := fmUnique("sdr-ctrl-msg")
		frames := sdrPostStream(t, e.ts, e.client, "/v1/messages", e.key,
			`{"model":"sdr-clean","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, reqID)
		sawStop, sawError := false, false
		endTurn := false
		for _, f := range frames {
			switch f.event {
			case "message_stop":
				sawStop = true
			case "message_delta":
				if m := sdrEventData(t, f); m != nil {
					if delta, ok := m["delta"].(map[string]any); ok && delta["stop_reason"] == "end_turn" {
						endTurn = true
					}
				}
			case "error":
				sawError = true
			}
		}
		if !sawStop || !endTurn || sawError {
			t.Fatalf("clean stream must end with message_delta(end_turn)+message_stop and no error (stop=%v end_turn=%v error=%v)", sawStop, endTurn, sawError)
		}
		if got := fmCount(t, e.env, `SELECT count(*) FROM usage_records WHERE request_id=$1`, reqID); got != 1 {
			t.Fatalf("a completed stream must bill exactly one usage row, got %d", got)
		}
	})
}
