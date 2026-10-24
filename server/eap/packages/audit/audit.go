// Package audit defines the EAP security audit event contract.
//
// Reliability rule (non-negotiable): security audit events use a reliable
// path whose delivery failure is observable. audit.Writer implementations
// return an error until the event is durably accepted, and callers use
// RequireWrite so an undeliverable security event fails the request closed.
// Best-effort telemetry (metrics, product analytics) belongs to a different
// pipeline and may be lossy; it must never be wired in place of this one.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"enterprise-agent-platform/packages/identity"
)

// EventType names the security/governance event classes. Names are
// namespaced and versioned with the contracts (v1).
type EventType string

const (
	EventAuthKeyCreated  EventType = "identity.key.created"
	EventAuthKeyRevoked  EventType = "identity.key.revoked"
	EventAuthSucceeded   EventType = "gateway.auth.succeeded"
	EventAuthFailed      EventType = "gateway.auth.failed"
	EventPolicyDenied    EventType = "policy.denied"
	EventDLPHit          EventType = "dlp.hit"
	EventLLMRequest      EventType = "llm.request"
	EventMCPToolExecuted EventType = "mcp.tool.executed"
	EventKnowledgeRead   EventType = "knowledge.read"
	EventTokenExchanged  EventType = "gateway.token.exchanged"
)

// Outcome is the event result class.
type Outcome string

const (
	OutcomeAllowed Outcome = "allowed"
	OutcomeDenied  Outcome = "denied"
	OutcomeError   Outcome = "error"
)

// Event is one audit record. It carries a Principal snapshot, never raw
// credentials. Detail keys are checked by ValidateNoSecrets.
type Event struct {
	ID          string              `json:"id"`
	Type        EventType           `json:"type"`
	Time        time.Time           `json:"time"`
	Outcome     Outcome             `json:"outcome"`
	Actor       *identity.Principal `json:"actor,omitempty"`
	WorkspaceID string              `json:"workspace_id,omitempty"`
	TraceID     string              `json:"trace_id,omitempty"`
	RequestID   string              `json:"request_id,omitempty"`
	Resource    string              `json:"resource,omitempty"`
	Detail      map[string]any      `json:"detail,omitempty"`
}

// NewEvent returns an event with a fresh id and UTC timestamp.
func NewEvent(t EventType, o Outcome) Event {
	return Event{ID: newEventID(), Type: t, Time: time.Now().UTC(), Outcome: o}
}

// Writer is the reliable security-audit path. Implementations must block
// until the event is durably accepted (PostgreSQL-backed writer arrives with
// the identity slice) and return an error otherwise.
type Writer interface {
	Write(ctx context.Context, e Event) error
}

// BatchWriter is implemented by writers that accept an event set in one
// durable round trip (TASK-038). WriteBatch is all-or-nothing: on error the
// caller must treat every event in the set as undelivered.
type BatchWriter interface {
	WriteBatch(ctx context.Context, events []Event) error
}

// RequireWriteBatch delivers a set of security-audit events, batching the
// delivery when the writer supports it and looping over RequireWrite
// otherwise. Fail closed: any validation or delivery failure returns an
// error telling the caller to deny the operation rather than continue
// unaudited.
func RequireWriteBatch(ctx context.Context, w Writer, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	for _, e := range events {
		if err := ValidateNoSecrets(e); err != nil {
			return fmt.Errorf("security audit event %s was not delivered (fail closed): %w", e.Type, err)
		}
	}
	if bw, ok := w.(BatchWriter); ok {
		if err := bw.WriteBatch(ctx, events); err != nil {
			return fmt.Errorf("security audit events were not delivered (fail closed): %w", err)
		}
		return nil
	}
	for _, e := range events {
		if err := RequireWrite(ctx, w, e); err != nil {
			return err
		}
	}
	return nil
}

// RequireWrite delivers a security audit event and fails closed: if the event
// cannot be accepted, the returned error tells the caller to deny the
// operation rather than continue unaudited.
func RequireWrite(ctx context.Context, w Writer, e Event) error {
	if w == nil {
		return fmt.Errorf("security audit event %s was not delivered (fail closed): no writer configured", e.Type)
	}
	if err := ValidateNoSecrets(e); err != nil {
		return fmt.Errorf("security audit event %s was not delivered (fail closed): %w", e.Type, err)
	}
	if err := w.Write(ctx, e); err != nil {
		return fmt.Errorf("security audit event %s was not delivered (fail closed): %w", e.Type, err)
	}
	return nil
}

// secretDetailKeys are Detail key names that must never appear in audit
// events. Matching is exact after lowercasing the key; nested maps are
// inspected recursively.
var secretDetailKeys = map[string]struct{}{
	"api_key": {}, "apikey": {}, "password": {}, "secret": {}, "token": {},
	"access_token": {}, "refresh_token": {}, "id_token": {}, "credential": {},
	"credentials": {}, "authorization": {}, "cookie": {}, "private_key": {},
}

// ValidateNoSecrets rejects events whose Detail carries credential-shaped
// keys. It is a contract guard used before writing and in tests; it cannot
// prove content safety by itself, which is why raw secrets must never be
// placed into Detail in the first place.
func ValidateNoSecrets(e Event) error {
	var found []string
	var walk func(path string, m map[string]any)
	walk = func(path string, m map[string]any) {
		for k, v := range m {
			lk := strings.ToLower(k)
			full := k
			if path != "" {
				full = path + "." + k
			}
			if _, bad := secretDetailKeys[lk]; bad {
				found = append(found, full)
				continue
			}
			if nested, ok := v.(map[string]any); ok {
				walk(full, nested)
			}
		}
	}
	if e.Detail != nil {
		walk("", e.Detail)
	}
	if len(found) > 0 {
		return fmt.Errorf("audit detail contains forbidden secret-shaped keys: %s", strings.Join(found, ", "))
	}
	return nil
}

// MemoryWriter is a synchronous in-memory Writer for tests and local tools.
// Not a production audit store.
type MemoryWriter struct {
	mu     sync.Mutex
	events []Event
	Err    error // when set, Write fails (test hook for the fail-closed path)
}

// Write implements Writer.
func (w *MemoryWriter) Write(_ context.Context, e Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Err != nil {
		return w.Err
	}
	w.events = append(w.events, e)
	return nil
}

// Events returns a copy of the accepted events.
func (w *MemoryWriter) Events() []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Event, len(w.events))
	copy(out, w.events)
	return out
}

func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
