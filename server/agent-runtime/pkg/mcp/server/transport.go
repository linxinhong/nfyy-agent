package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
)

// ---------------------------------------------------------------------------
// StdioTransport — MCP over stdin/stdout
// ---------------------------------------------------------------------------

// StdioTransport reads JSON-RPC messages from stdin and writes responses to
// stdout. This is the transport used by Claude Desktop and similar tools.
type StdioTransport struct {
	reader *bufio.Scanner
	writer io.Writer
	mu     sync.Mutex // guards writer
}

// NewStdioTransport creates a transport reading from os.Stdin and writing to
// os.Stdout.
func NewStdioTransport() *StdioTransport {
	return NewStdioTransportRW(os.Stdin, os.Stdout)
}

// NewStdioTransportRW creates a transport with custom reader/writer (useful
// for testing with pipes).
func NewStdioTransportRW(r io.Reader, w io.Writer) *StdioTransport {
	scanner := bufio.NewScanner(r)
	// MCP messages can be large; set a generous buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	return &StdioTransport{
		reader: scanner,
		writer: w,
	}
}

func (t *StdioTransport) Start(ctx context.Context) error {
	// Stdio is always "started" — stdin/stdout are already open.
	return nil
}

func (t *StdioTransport) Send(ctx context.Context, msg any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	enc := json.NewEncoder(t.writer)
	enc.SetEscapeHTML(false)
	return enc.Encode(msg)
}

func (t *StdioTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	for t.reader.Scan() {
		line := t.reader.Bytes()
		if len(line) == 0 {
			continue
		}
		// Copy the data before returning — the scanner reuses its buffer.
		raw := make(json.RawMessage, len(line))
		copy(raw, line)
		return raw, nil
	}
	if err := t.reader.Err(); err != nil {
		return nil, fmt.Errorf("stdio read: %w", err)
	}
	return nil, io.EOF
}

func (t *StdioTransport) Close() error {
	// We don't close stdin/stdout — the process owns them.
	return nil
}

// ---------------------------------------------------------------------------
// StreamableHTTPTransport — MCP over HTTP + SSE
// ---------------------------------------------------------------------------

// HTTPTransport implements the MCP Streamable HTTP transport. Clients POST
// JSON-RPC requests and receive responses either synchronously (same POST
// response) or via an SSE stream established with GET.
type HTTPTransport struct {
	addr   string
	server *http.Server
	ln     net.Listener

	// sseClients maps session IDs to their outgoing event channels.
	sseClients map[string]chan []byte
	mu         sync.RWMutex
}

// NewHTTPTransport creates an HTTP transport that listens on addr (e.g. ":8080").
func NewHTTPTransport(addr string) *HTTPTransport {
	return &HTTPTransport{
		addr:       addr,
		sseClients: make(map[string]chan []byte),
	}
}

// Handler returns an http.Handler that serves MCP requests. Use this to embed
// the transport into an existing HTTP server instead of calling Start.
func (t *HTTPTransport) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", t.handleMCP)
	return mux
}

func (t *HTTPTransport) Start(ctx context.Context) error {
	t.server = &http.Server{
		Addr:    t.addr,
		Handler: t.Handler(),
	}
	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return fmt.Errorf("mcp http: listen: %w", err)
	}
	t.ln = ln
	go func() {
		if err := t.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			observability.Event(ctx, slog.LevelError, "mcp.server.serve_error", observability.Err(err))
		}
	}()
	go func() {
		<-ctx.Done()
		t.server.Shutdown(context.Background())
	}()
	return nil
}

func (t *HTTPTransport) Send(ctx context.Context, msg any) error {
	// HTTP transport sends responses inline (same POST response), not via Send.
	// This method exists for the Transport interface but the real sending happens
	// in handleMCP's POST handler.
	return nil
}

func (t *HTTPTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	// Messages are received via HTTP POST and dispatched by ServeHTTP→handleMCP.
	// This method exists for interface compliance but is not called directly
	// when using ServeHTTPWithServer.
	<-ctx.Done()
	return nil, ctx.Err()
}

func (t *HTTPTransport) Close() error {
	if t.server != nil {
		return t.server.Close()
	}
	return nil
}

// Addr returns the address the transport is listening on.
func (t *HTTPTransport) Addr() string {
	if t.ln != nil {
		return t.ln.Addr().String()
	}
	return t.addr
}

func (t *HTTPTransport) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		t.handleSSE(w, r)
	case http.MethodPost:
		t.handleRPC(w, r)
	case http.MethodDelete:
		// MCP session teardown.
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (t *HTTPTransport) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	// Use a simple session identifier; a production implementation would use
	// the MCP session ID from headers.
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = r.Header.Get("Mcp-Session-Id")
	}
	if sessionID == "" {
		// Auto-generate a session for simplicity — the first message sets it.
		fmt.Fprintf(w, "event: endpoint\ndata: /\n\n")
		flusher.Flush()
		return
	}

	ch := make(chan []byte, 64)
	t.mu.Lock()
	t.sseClients[sessionID] = ch
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.sseClients, sessionID)
		t.mu.Unlock()
	}()

	// Keep the connection open and forward events.
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (t *HTTPTransport) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req mcp.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, nil, mcp.ErrParse, "parse error: "+err.Error())
		return
	}
	r.Body.Close()

	// This method is called by ServeHTTPWithServer which handles dispatch.
	// For standalone use (without a Server), return an error.
	writeHTTPError(w, req.ID, mcp.ErrInternal, "HTTP transport requires ServeHTTPWithServer")
}

func writeHTTPError(w http.ResponseWriter, id any, code int, message string) {
	resp := mcp.NewErrorResponse(id, code, message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors are HTTP 200.
	json.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// ServeHTTPWithServer wires a Server into an HTTP transport for embedded use.
// ---------------------------------------------------------------------------

// ServeHTTPWithServer returns an http.Handler that dispatches incoming MCP
// requests to srv. This is the preferred way to embed an MCP server into an
// existing HTTP server (e.g., AgentOS).
func ServeHTTPWithServer(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleSSEEmbedded(w, r, srv)
		case http.MethodPost:
			handleRPCEmbedded(w, r, srv)
		case http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func handleSSEEmbedded(w http.ResponseWriter, r *http.Request, srv *Server) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprintf(w, "event: endpoint\ndata: /\n\n")
	flusher.Flush()

	// Keep connection alive for server->client notifications.
	<-r.Context().Done()
}

func handleRPCEmbedded(w http.ResponseWriter, r *http.Request, srv *Server) {
	var req mcp.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, nil, mcp.ErrParse, "parse error: "+err.Error())
		return
	}
	r.Body.Close()

	if req.ID == nil {
		// Notification — process and return empty 202.
		srv.processMessage(r.Context(), nil, mustMarshal(req))
		w.WriteHeader(http.StatusAccepted)
		return
	}

	handler, ok := srv.handlers[req.Method]
	if !ok {
		writeHTTPError(w, req.ID, mcp.ErrMethodNotFound, "method not found: "+req.Method)
		return
	}

	result, err := handler(r.Context(), req.Params)
	if err != nil {
		writeHTTPError(w, req.ID, mcp.ErrInternal, err.Error())
		return
	}

	resp := mcp.NewSuccessResponse(req.ID, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
