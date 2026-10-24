package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

const (
	// maxErrorBodyBytes bounds how much of a non-2xx body is read.
	maxErrorBodyBytes = 64 << 10
	// maxErrorMessageRunes bounds the provider message kept in errors.
	maxErrorMessageRunes = 512
	// maxSSELineBytes bounds one SSE line; large tool-call argument chunks
	// exceed bufio.Scanner's 64 KiB default.
	maxSSELineBytes = 16 << 20
)

// newHTTPError builds a typed ProviderError from a non-2xx response. Only
// the message field of a recognized JSON error envelope is kept (truncated);
// unrecognized bodies (HTML gateway pages, echoes) are dropped in favor of
// the status text so upstream content does not leak into run errors.
func newHTTPError(provider string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	code, message := parseErrorEnvelope(body)
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	return &models.ProviderError{
		Provider:   provider,
		StatusCode: resp.StatusCode,
		Code:       code,
		Message:    truncateRunes(message, maxErrorMessageRunes),
		RetryAfter: parseRetryAfter(resp.Header, time.Now()),
	}
}

// parseErrorEnvelope understands the OpenAI/DeepSeek shape
// {"error":{"message","type","code"}} and the Anthropic shape
// {"type":"error","error":{"type","message"}}.
func parseErrorEnvelope(body []byte) (code, message string) {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || len(env.Error) == 0 {
		return "", ""
	}
	var detail struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	if json.Unmarshal(env.Error, &detail) != nil {
		var plain string
		if json.Unmarshal(env.Error, &plain) == nil {
			return "", plain
		}
		return "", ""
	}
	code = detail.Type
	if s, ok := detail.Code.(string); ok && s != "" {
		code = s
	}
	return code, detail.Message
}

// parseRetryAfter reads retry-after-ms (OpenAI) or Retry-After as seconds
// or an HTTP date. Advised delays are capped at five minutes.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	const maxAdvised = 5 * time.Minute
	clamp := func(d time.Duration) time.Duration {
		if d < 0 {
			return 0
		}
		if d > maxAdvised {
			return maxAdvised
		}
		return d
	}
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil {
			return clamp(time.Duration(ms * float64(time.Millisecond)))
		}
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		return clamp(time.Duration(secs * float64(time.Second)))
	}
	if at, err := http.ParseTime(v); err == nil {
		return clamp(at.Sub(now))
	}
	return 0
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// newSSEScanner returns a line scanner sized for large SSE data lines.
func newSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxSSELineBytes)
	return scanner
}

// sseData extracts the payload of a "data:" line; ok is false for comments,
// keep-alives (DeepSeek sends ": keep-alive" while queued), and other fields.
func sseData(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

// streamSender delivers events unless the consumer's context is done, so a
// consumer that stops reading never strands the producer goroutine (and its
// HTTP connection).
type streamSender struct {
	ctx context.Context
	ch  chan<- models.StreamEvent
}

func (s streamSender) send(ev models.StreamEvent) bool {
	select {
	case s.ch <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// doStream sends a streaming request. Unlike unary calls, a stream has no
// total deadline (DeepSeek-R1 reasoning routinely streams for minutes);
// instead the request fails if headers or the next chunk do not arrive
// within idle. A caller-supplied HTTPClient keeps its own policy.
func doStream(ctx context.Context, client *http.Client, idle time.Duration, build func(context.Context) (*http.Request, error)) (*http.Response, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	req, err := build(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	body := &idleBody{idle: idle, cancel: cancel}
	body.timer = time.AfterFunc(idle, body.expire)
	resp, err := client.Do(req)
	if err != nil {
		body.stop()
		if body.expired.Load() {
			return nil, fmt.Errorf("%w: no response headers within %s", models.ErrStreamInterrupted, idle)
		}
		return nil, err
	}
	body.ReadCloser = resp.Body
	body.timer.Reset(idle)
	resp.Body = body
	return resp, nil
}

// idleBody cancels the request when no bytes arrive for idle.
type idleBody struct {
	io.ReadCloser
	idle    time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	expired atomic.Bool
}

func (b *idleBody) expire() {
	b.expired.Store(true)
	b.cancel()
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.expired.Load() {
		b.timer.Reset(b.idle)
	}
	if err != nil && b.expired.Load() {
		return n, fmt.Errorf("stream idle for %s", b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.stop()
	return b.ReadCloser.Close()
}

func (b *idleBody) stop() {
	b.timer.Stop()
	b.cancel()
}
