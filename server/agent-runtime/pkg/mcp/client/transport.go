package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

// ---------------------------------------------------------------------------
// StdioClientTransport — spawns MCP server as subprocess
// ---------------------------------------------------------------------------

// StdioClientTransport starts an MCP server as a subprocess and communicates
// via newline-delimited JSON-RPC over its stdin/stdout.
type StdioClientTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	mu      sync.Mutex
	pending map[string]chan json.RawMessage
	nextID  atomic.Int64

	done    chan struct{}
	started bool
}

// NewStdioClientTransport creates a transport. The command is not started
// until Start is called.
func NewStdioClientTransport(command string, args ...string) *StdioClientTransport {
	return &StdioClientTransport{
		cmd:     exec.Command(command, args...),
		pending: make(map[string]chan json.RawMessage),
		done:    make(chan struct{}),
	}
}

// Start launches the subprocess and begins the response read loop.
func (t *StdioClientTransport) Start(ctx context.Context) error {
	if t.started {
		return fmt.Errorf("stdio transport already started")
	}

	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	t.stdin = stdin
	t.stdout = bufio.NewScanner(stdout)
	t.stdout.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	t.started = true

	// Read responses in background, dispatching to waiting callers.
	go t.readLoop()

	// Clean up on context cancellation.
	go func() {
		select {
		case <-ctx.Done():
			t.Close()
		case <-t.done:
		}
	}()

	return nil
}

func (t *StdioClientTransport) readLoop() {
	defer close(t.done)
	for t.stdout.Scan() {
		line := t.stdout.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse just enough to get the ID.
		var base struct {
			ID any `json:"id"`
		}
		if err := json.Unmarshal(line, &base); err != nil {
			continue
		}

		id := formatID(base.ID)
		if id == "" {
			continue // notification or error, skip
		}

		t.mu.Lock()
		ch, ok := t.pending[id]
		t.mu.Unlock()
		if ok {
			raw := make(json.RawMessage, len(line))
			copy(raw, line)
			select {
			case ch <- raw:
			default:
			}
		}
	}
}

// Call sends a JSON-RPC request to the subprocess and waits for its response.
func (t *StdioClientTransport) Call(ctx context.Context, request any) (json.RawMessage, error) {
	if !t.started {
		return nil, fmt.Errorf("transport not started")
	}

	id := fmt.Sprintf("req-%d", t.nextID.Add(1))

	// Build the request with the assigned ID, preserving caller-provided fields.
	reqJSON, err := marshalWithID(request, id)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Register pending before sending to avoid a race with readLoop.
	ch := make(chan json.RawMessage, 1)
	t.mu.Lock()
	t.pending[id] = ch
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
	}()

	// Write the request line to stdin.
	if _, err := t.stdin.Write(reqJSON); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	if _, err := t.stdin.Write([]byte("\n")); err != nil {
		return nil, fmt.Errorf("write newline: %w", err)
	}

	// Wait for the response.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, fmt.Errorf("transport closed")
	case raw := <-ch:
		return raw, nil
	}
}

// Close terminates the subprocess.
func (t *StdioClientTransport) Close() error {
	if t.stdin != nil {
		t.stdin.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		return t.cmd.Wait()
	}
	return nil
}

// Notify writes a JSON-RPC notification (no id) to the subprocess.
func (t *StdioClientTransport) Notify(ctx context.Context, notification mcp.Notification) error {
	if !t.started {
		return fmt.Errorf("transport not started")
	}
	raw, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	if _, err := t.stdin.Write(raw); err != nil {
		return fmt.Errorf("write notification: %w", err)
	}
	if _, err := t.stdin.Write([]byte("\n")); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTPClientTransport — MCP over Streamable HTTP
// ---------------------------------------------------------------------------

// Size limits for the HTTP transport.
const (
	// defaultMaxResponseBytes is the default total response limit (4 MiB).
	defaultMaxResponseBytes int64 = 4 << 20
	// hardMaxResponseBytes is the configurable ceiling (16 MiB).
	hardMaxResponseBytes int64 = 16 << 20
	// maxErrorBodyBytes caps how much of a non-2xx body is retained (64 KiB).
	maxErrorBodyBytes int64 = 64 << 10
)

// Default HTTP header names managed by the transport itself. HeaderProvider
// must not override these.
var transportReservedHeaders = map[string]struct{}{
	"Host":                 {},
	"Content-Length":       {},
	"Content-Type":         {},
	"Accept":               {},
	"Connection":           {},
	"Transfer-Encoding":    {},
	"MCP-Protocol-Version": {},
	"MCP-Session-Id":       {},
}

// CredentialProvider supplies per-request credentials (e.g. rotating tokens).
// Values are attached to the Authorization header.
type CredentialProvider interface {
	// Credentials returns a credential header value (without the scheme
	// prefix) for the next request, or an error to abort the request.
	Credentials(ctx context.Context) (string, error)
}

// HeaderProvider supplies extra per-request headers. It must not override
// transport-reserved headers; Authorization is managed by CredentialProvider.
type HeaderProvider interface {
	// Headers returns extra headers for the next request.
	Headers(ctx context.Context) (map[string]string, error)
}

// RedirectPolicy controls how the transport follows HTTP redirects.
type RedirectPolicy int

const (
	// RedirectPolicySameOrigin (default) follows redirects only within the
	// same origin; cross-host redirects fail and never carry credentials.
	RedirectPolicySameOrigin RedirectPolicy = iota
	// RedirectPolicyNever refuses all redirects.
	RedirectPolicyNever
)

// HTTPTransportOptions configures NewHTTPClientTransportWithOptions.
type HTTPTransportOptions struct {
	// HTTPClient, when set, is used for requests. RoundTripper is used only
	// when HTTPClient is nil.
	HTTPClient   *http.Client
	RoundTripper http.RoundTripper

	CredentialProvider CredentialProvider
	HeaderProvider     HeaderProvider

	// MaxResponseBytes limits the total response body; default 4 MiB,
	// ceiling 16 MiB. MaxEventBytes limits a single SSE event.
	MaxResponseBytes int64
	MaxEventBytes    int64

	RedirectPolicy RedirectPolicy

	// ProtocolVersion overrides the MCP protocolVersion sent in initialize
	// and the MCP-Protocol-Version header (default mcp.ProtocolVersion).
	// Hosts set this to interop with servers pinned to a newer revision
	// (e.g. a governed gateway whose frozen contract mandates 2025-11-25).
	// Empty = default.
	ProtocolVersion string
}

// HTTPError is a typed non-2xx response error. Body is truncated to
// maxErrorBodyBytes and is intended for trusted callers only; it must not
// be written to logs or traces.
type HTTPError struct {
	StatusCode int
	Status     string
	RetryAfter string
	RequestID  string
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("mcp http: status %d %s", e.StatusCode, e.Status)
}

// ProtocolError reports a malformed or incomplete MCP HTTP exchange (e.g.
// SSE stream ended before a terminal response arrived).
type ProtocolError struct {
	Message string
}

func (e *ProtocolError) Error() string { return "mcp protocol: " + e.Message }

// HTTPClientTransport sends JSON-RPC requests via HTTP POST to an MCP server,
// supporting dynamic credentials, extra headers, MCP session IDs, SSE
// responses, size limits, and redirect safety.
type HTTPClientTransport struct {
	baseURL      string
	endpointHost string

	httpClient         *http.Client
	credentialProvider CredentialProvider
	headerProvider     HeaderProvider
	maxResponseBytes   int64
	maxEventBytes      int64
	redirectPolicy     RedirectPolicy
	opts               HTTPTransportOptions

	mu        sync.Mutex
	sessionID string

	nextID atomic.Int64
	closed atomic.Bool
}

// protocolVersion returns the configured override ("" = default).
func (t *HTTPClientTransport) protocolVersion() string { return t.opts.ProtocolVersion }

// NewHTTPClientTransport creates an HTTP client transport for the given URL
// with default options.
func NewHTTPClientTransport(baseURL string) *HTTPClientTransport {
	t, _ := NewHTTPClientTransportWithOptions(baseURL, HTTPTransportOptions{})
	return t
}

// NewHTTPClientTransportWithOptions creates an HTTP client transport with
// explicit options. The endpoint must be a valid absolute HTTP(S) URL; URL
// userinfo is rejected (credentials must flow through providers).
func NewHTTPClientTransportWithOptions(endpoint string, opts HTTPTransportOptions) (*HTTPClientTransport, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("mcp http: invalid endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("mcp http: endpoint must be http(s), got %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("mcp http: endpoint userinfo is not allowed")
	}
	if opts.MaxResponseBytes < 0 || opts.MaxResponseBytes > hardMaxResponseBytes {
		return nil, fmt.Errorf("mcp http: MaxResponseBytes out of range (0-%d)", hardMaxResponseBytes)
	}
	if opts.MaxEventBytes < 0 || opts.MaxEventBytes > hardMaxResponseBytes {
		return nil, fmt.Errorf("mcp http: MaxEventBytes out of range (0-%d)", hardMaxResponseBytes)
	}
	maxResponse := opts.MaxResponseBytes
	if maxResponse == 0 {
		maxResponse = defaultMaxResponseBytes
	}
	maxEvent := opts.MaxEventBytes
	if maxEvent == 0 {
		maxEvent = defaultMaxResponseBytes
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: opts.RoundTripper}
	}
	redirectPolicy := opts.RedirectPolicy
	httpClient.CheckRedirect = redirectCheck(redirectPolicy, httpClient.CheckRedirect)

	return &HTTPClientTransport{
		baseURL:            strings.TrimRight(endpoint, "/"),
		endpointHost:       u.Host,
		httpClient:         httpClient,
		credentialProvider: opts.CredentialProvider,
		headerProvider:     opts.HeaderProvider,
		maxResponseBytes:   maxResponse,
		maxEventBytes:      maxEvent,
		redirectPolicy:     redirectPolicy,
		opts:               opts,
	}, nil
}

// redirectCheck enforces the redirect policy and strips credentials from
// cross-origin hops (Go already strips Authorization on cross-domain
// redirects; we additionally refuse them outright under the strict policy).
func redirectCheck(policy RedirectPolicy, existing func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if policy == RedirectPolicyNever {
			return errors.New("mcp http: redirects are disabled")
		}
		if len(via) > 0 && via[0].URL.Host != req.URL.Host {
			return fmt.Errorf("mcp http: cross-origin redirect to %q is not allowed", req.URL.Host)
		}
		if existing != nil {
			return existing(req, via)
		}
		return nil
	}
}

// rpcMethodOf extracts the JSON-RPC method name from a request value.
func rpcMethodOf(request any) string {
	switch r := request.(type) {
	case mcp.Request:
		return r.Method
	case *mcp.Request:
		return r.Method
	default:
		var probe struct {
			Method string `json:"method"`
		}
		if b, err := json.Marshal(request); err == nil {
			if json.Unmarshal(b, &probe) == nil {
				return probe.Method
			}
		}
	}
	return ""
}

// buildRequest assembles the next HTTP request with credentials, headers,
// and the MCP session ID.
func (t *HTTPClientTransport) buildRequest(ctx context.Context, method string, body []byte, acceptJSON bool) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.baseURL, reader)
	if err != nil {
		return nil, fmt.Errorf("mcp http: create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if acceptJSON {
		req.Header.Set("Accept", "application/json, text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	protocolVersion := t.opts.ProtocolVersion
	if protocolVersion == "" {
		protocolVersion = mcp.ProtocolVersion
	}
	req.Header.Set("MCP-Protocol-Version", protocolVersion)

	if t.credentialProvider != nil {
		cred, err := t.credentialProvider.Credentials(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp http: credentials: %w", err)
		}
		if cred != "" {
			req.Header.Set("Authorization", cred)
		}
	}
	if t.headerProvider != nil {
		extra, err := t.headerProvider.Headers(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp http: headers: %w", err)
		}
		for key := range extra {
			if _, reserved := transportReservedHeaders[http.CanonicalHeaderKey(key)]; reserved {
				return nil, fmt.Errorf("mcp http: header provider must not override %q", key)
			}
		}
		for key, value := range extra {
			req.Header.Set(key, value)
		}
	}
	t.mu.Lock()
	sessionID := t.sessionID
	t.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("MCP-Session-Id", sessionID)
	}
	return req, nil
}

// Call sends a JSON-RPC request via HTTP POST and returns the response.
func (t *HTTPClientTransport) Call(ctx context.Context, request any) (json.RawMessage, error) {
	if t.closed.Load() {
		return nil, fmt.Errorf("mcp http: transport closed")
	}
	id := fmt.Sprintf("req-%d", t.nextID.Add(1))
	reqJSON, err := marshalWithID(request, id)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	// A fresh initialize means any previously stored session is stale (the
	// server restarted and forgot it); sending it would 404 the handshake.
	// Clear it so the initialize request carries no session header.
	if rpcMethodOf(request) == mcp.MethodInitialize {
		t.mu.Lock()
		t.sessionID = ""
		t.mu.Unlock()
	}
	req, err := t.buildRequest(ctx, http.MethodPost, reqJSON, true)
	if err != nil {
		return nil, err
	}
	raw, err := t.do(ctx, req, id)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// Notify sends a JSON-RPC notification (no id, no response expected). The
// server may reply with 202 Accepted and an empty body.
func (t *HTTPClientTransport) Notify(ctx context.Context, notification mcp.Notification) error {
	if t.closed.Load() {
		return fmt.Errorf("mcp http: transport closed")
	}
	raw, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	req, err := t.buildRequest(ctx, http.MethodPost, raw, true)
	if err != nil {
		return err
	}
	_, err = t.do(ctx, req, "")
	return err
}

// Terminate ends the MCP session with a DELETE carrying the session ID.
func (t *HTTPClientTransport) Terminate(ctx context.Context) error {
	if t.closed.Load() {
		return nil
	}
	t.mu.Lock()
	sessionID := t.sessionID
	t.mu.Unlock()
	req, err := t.buildRequest(ctx, http.MethodDelete, nil, false)
	if err != nil {
		return err
	}
	if sessionID != "" {
		req.Header.Set("MCP-Session-Id", sessionID)
	}
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mcp http: terminate: %w", err)
	}
	defer resp.Body.Close()
	_, _ = drainLimit(resp.Body, t.maxResponseBytes)
	// 2xx/202 with empty body is the success contract.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError(resp, t.maxResponseBytes)
	}
	return nil
}

// do performs one request and decodes the JSON or SSE response. When id is
// non-empty, an SSE stream must terminate with a message whose JSON-RPC id
// matches; otherwise a typed ProtocolError is returned.
func (t *HTTPClientTransport) do(ctx context.Context, req *http.Request, id string) ([]byte, error) {
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp http: %w", err)
	}
	defer resp.Body.Close()

	// Persist a server-assigned session ID.
	if sid := resp.Header.Get("MCP-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpError(resp, t.maxResponseBytes)
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return t.readSSE(resp.Body, id)
	}
	body, err := drainLimit(resp.Body, t.maxResponseBytes)
	if err != nil {
		return nil, err
	}
	// Notifications may legitimately get a 202 with an empty body.
	if len(bytes.TrimSpace(body)) == 0 {
		if id == "" {
			return nil, nil
		}
		return nil, &ProtocolError{Message: "empty response body"}
	}
	if !json.Valid(body) {
		return nil, &ProtocolError{Message: "response body is not valid JSON"}
	}
	return body, nil
}

// readSSE parses an SSE stream by event blocks: only "message" events (or
// events without an explicit type) are considered; other event types are
// ignored. When id is non-empty, the stream must end with a message whose
// JSON-RPC id matches; EOF before that is a ProtocolError.
func (t *HTTPClientTransport) readSSE(body io.Reader, id string) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), int(t.maxEventBytes)+1)

	var eventName string
	var data strings.Builder
	finish := func() ([]byte, bool, error) {
		if eventName != "" && eventName != "message" {
			eventName = ""
			data.Reset()
			return nil, false, nil
		}
		if data.Len() == 0 {
			return nil, false, nil
		}
		raw := []byte(data.String())
		if !json.Valid(raw) {
			return nil, true, &ProtocolError{Message: "SSE message data is not valid JSON"}
		}
		return raw, true, nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			raw, done, err := finish()
			if err != nil {
				return nil, err
			}
			if done && id == "" {
				return raw, nil
			}
			if done {
				var base struct {
					ID any `json:"id"`
				}
				if err := json.Unmarshal(raw, &base); err != nil {
					return nil, &ProtocolError{Message: "SSE message has no JSON-RPC id"}
				}
				if formatID(base.ID) == id {
					return raw, nil
				}
			}
			eventName = ""
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(payload)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mcp http: read SSE: %w", err)
	}
	// Last event may lack a trailing blank line.
	raw, done, err := finish()
	if err != nil {
		return nil, err
	}
	if done && id == "" {
		return raw, nil
	}
	if done {
		var base struct {
			ID any `json:"id"`
		}
		if err := json.Unmarshal(raw, &base); err == nil && formatID(base.ID) == id {
			return raw, nil
		}
	}
	return nil, &ProtocolError{Message: "SSE stream ended without matching response"}
}

func httpError(resp *http.Response, maxBytes int64) error {
	body, _ := drainLimit(resp.Body, maxErrorBodyBytes)
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		RetryAfter: resp.Header.Get("Retry-After"),
		RequestID:  resp.Header.Get("X-Request-Id"),
		Body:       string(body),
	}
}

func drainLimit(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("mcp http: read response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, &ProtocolError{Message: fmt.Sprintf("response exceeds %d bytes", limit)}
	}
	return body, nil
}

// Close shuts down idle connections and marks the transport closed.
func (t *HTTPClientTransport) Close() error {
	if t.closed.Swap(true) {
		return nil
	}
	t.httpClient.CloseIdleConnections()
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// marshalWithID marshals v as JSON and ensures the "id" field is set to id.
// If v already has an id, it is replaced with the caller-provided one.
func marshalWithID(v any, id string) ([]byte, error) {
	// Marshal to map so we can override id.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["id"] = id
	return json.Marshal(m)
}

func formatID(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
