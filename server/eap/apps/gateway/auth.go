package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"enterprise-agent-platform/packages/apikey"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"
	"enterprise-agent-platform/packages/tracing"
)

// authenticated is the result of credential resolution: who is calling, with
// what kind of credential, and (for API keys) which stored key row.
type authenticated struct {
	Principal identity.Principal
	Key       *store.APIKey // nil for control-plane sessions
	// Memberships are the live membership rows prefetched by the fused auth
	// query (TASK-043) for the header and binding workspaces; nil when the
	// fallback auth path ran (the resolver then reads live rows itself).
	Memberships map[string]identity.Membership
}

// bearerToken extracts the value after "Bearer" (case-insensitive).
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, value, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

// authenticate resolves any accepted credential type:
//   - agk_* employee API keys (enterprise surface: me, token exchange)
//   - internal session JWTs with aud=agent-gateway (control plane: key management)
//
// Every denial emits a gateway.auth.failed audit event through the reliable
// writer; write failures are logged loudly (the denial itself still stands).
func (s *Server) authenticate(r *http.Request) (authenticated, error) {
	raw, ok := bearerToken(r)
	if !ok {
		return authenticated{}, errs.New(errs.Unauthenticated, "missing bearer credential")
	}

	if strings.HasPrefix(raw, apikey.PublicPrefix) {
		return s.authenticateAPIKey(r, raw)
	}
	return s.authenticateSession(r, raw)
}

func (s *Server) authenticateAPIKey(r *http.Request, raw string) (authenticated, error) {
	deny := func(reason string) (authenticated, error) {
		s.auditAuthFailed(r, nil, "api_key", reason)
		return authenticated{}, errs.New(errs.Unauthenticated, "invalid or unusable API key")
	}

	if err := apikey.ValidateFormat(raw); err != nil {
		return deny("malformed")
	}
	// TASK-038: key + owner resolve in one round trip when the AuthChain
	// seam is wired. Revocation/expiry below still judge live columns on
	// every request — nothing about the auth contract moves.
	var (
		key             store.APIKey
		user            store.User
		authMemberships map[string]identity.Membership
	)
	if s.AuthChain != nil {
		a, err := s.AuthChain(r.Context(), apikey.Hash(raw), r.Header.Get(identity.HeaderWorkspace))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return deny("unknown_key")
			}
			s.auditAuthFailed(r, nil, "api_key", "store_error")
			return authenticated{}, errs.Wrap(err, errs.Internal, "credential lookup failed")
		}
		key, user, authMemberships = a.Key, a.User, a.Memberships
	} else {
		var err error
		key, err = s.Store.GetAPIKeyByHash(r.Context(), apikey.Hash(raw))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return deny("unknown_key")
			}
			// Storage failure: fail closed, distinct from an unknown key.
			s.auditAuthFailed(r, nil, "api_key", "store_error")
			return authenticated{}, errs.Wrap(err, errs.Internal, "credential lookup failed")
		}
		user, err = s.Store.GetUser(r.Context(), key.UserID)
		if err != nil {
			return authenticated{}, errs.Wrap(err, errs.Internal, "user lookup failed")
		}
	}

	meta := map[string]any{}
	if key.RevokedAt != nil {
		meta["reason"] = "revoked"
		meta["key_id"] = key.KeyID
		s.auditAuthFailedDetail(r, &key.UserID, "api_key", meta)
		return authenticated{}, errs.New(errs.Unauthenticated, "API key has been revoked")
	}
	if key.ExpiresAt != nil && !s.now().Before(*key.ExpiresAt) {
		meta["reason"] = "expired"
		meta["key_id"] = key.KeyID
		s.auditAuthFailedDetail(r, &key.UserID, "api_key", meta)
		return authenticated{}, errs.New(errs.Unauthenticated, "API key has expired")
	}

	allowed := true // nil limiter: no abuse protection configured, fail open
	var rerr error
	if s.Rate != nil {
		allowed, rerr = s.Rate.Allow(r.Context(), "key:"+key.KeyID)
	}
	if rerr != nil {
		log.Printf("gateway: rate limiter unavailable (fail open): %v", rerr)
	} else if !allowed {
		return authenticated{}, errs.New(errs.RateLimited, "rate limit exceeded for this key").AsRetriable()
	}

	// TASK-038/041: the last-used touch leaves the request critical path
	// through a bounded applier — drops instead of unbounded goroutine
	// growth when the database is saturated. Best-effort: a dropped touch
	// costs nothing but telemetry freshness.
	s.enqueueTouch(touchReq{keyID: key.KeyID, ip: clientIP(r), client: r.Header.Get(identity.HeaderClient)})

	return authenticated{
		Principal:   s.principalFor(r, user),
		Key:         &key,
		Memberships: authMemberships,
	}, nil
}

func (s *Server) authenticateSession(r *http.Request, raw string) (authenticated, error) {
	claims, err := s.Verifier.Verify(raw, token.AudienceAgentGateway)
	if err != nil {
		s.auditAuthFailed(r, nil, "session", "invalid_session")
		return authenticated{}, errs.New(errs.Unauthenticated, "invalid or expired session")
	}
	user, err := s.Store.GetUser(r.Context(), claims.Subject)
	if err != nil {
		return authenticated{}, errs.Wrap(err, errs.Internal, "user lookup failed")
	}
	return authenticated{Principal: s.principalFor(r, user)}, nil
}

// workspaceSelector returns the effective workspace selection input for the
// request and its origin (ADR-0024): an explicit X-Agent-Workspace header
// wins; otherwise a workspace-bound API key nominates its binding. Either way
// this is only a selection — SelectWorkspace revalidates membership on every
// request and fails closed when neither source is present.
func workspaceSelector(r *http.Request, auth authenticated) (workspace, origin string) {
	if ws := strings.TrimSpace(r.Header.Get(identity.HeaderWorkspace)); ws != "" {
		return ws, "header"
	}
	if auth.Key != nil && auth.Key.WorkspaceID != nil && strings.TrimSpace(*auth.Key.WorkspaceID) != "" {
		return strings.TrimSpace(*auth.Key.WorkspaceID), "key_binding"
	}
	return "", ""
}

// selectWorkspace resolves the request's workspace authority through the
// frozen precedence: client header → API key binding → fail closed. Session
// credentials carry no key, so their behavior is unchanged.
func (s *Server) selectWorkspace(r *http.Request, auth authenticated) (identity.Session, error) {
	ws, _ := workspaceSelector(r, auth)
	return identity.SelectWorkspace(r.Context(), auth.Principal, ws, &authMembershipResolver{store: s.Store, prefetched: auth.Memberships})
}

// authMembershipResolver answers membership from the rows the fused auth
// query already fetched (same round trip, TASK-043); anything outside the
// prefetched candidates falls back to the live resolver. Entries come from
// live rows per request — no caching, no semantics moved anywhere.
type authMembershipResolver struct {
	store      identity.MembershipResolver
	prefetched map[string]identity.Membership
}

func (m *authMembershipResolver) IsMember(ctx context.Context, userID, workspaceID string) (identity.Membership, error) {
	if m != nil {
		if mem, ok := m.prefetched[workspaceID]; ok {
			return mem, nil
		}
	}
	return m.store.IsMember(ctx, userID, workspaceID)
}

func (s *Server) principalFor(r *http.Request, u store.User) identity.Principal {
	return identity.Principal{
		UserID:         u.ID,
		OrganizationID: u.OrganizationID,
		DepartmentID:   u.DepartmentID,
		ClientID:       r.Header.Get(identity.HeaderClient),
		DeviceID:       r.Header.Get(identity.HeaderDevice),
	}
}

func (s *Server) auditAuthFailed(r *http.Request, userID *string, credential, reason string) {
	s.auditAuthFailedDetail(r, userID, credential, map[string]any{"reason": reason})
}

// auditAuthFailedDetail emits gateway.auth.failed through the reliable audit
// writer. Delivery failures are logged and remain observable; the pending
// denial is unaffected.
func (s *Server) auditAuthFailedDetail(r *http.Request, userID *string, credential string, meta map[string]any) {
	ev := audit.NewEvent(audit.EventAuthFailed, audit.OutcomeDenied)
	if userID != nil {
		ev.Actor = &identity.Principal{UserID: *userID}
	}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"credential_type": credential}
	for k, v := range meta {
		ev.Detail[k] = v
	}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
	}
}

// touchReq is one deferred api_keys last-used update.
type touchReq struct {
	keyID  string
	ip     string
	client string
}

const (
	touchQueueSize   = 1024
	touchWritePeriod = 500 * time.Millisecond
)

// StartTouchApplier runs the bounded background applier: one goroutine
// coalesces queued touches per key and flushes them periodically as
// best-effort writes. It stops when ctx is cancelled.
func (s *Server) StartTouchApplier(ctx context.Context) {
	s.touchQueue = make(chan touchReq, touchQueueSize)
	go func() {
		pending := make(map[string]touchReq)
		ticker := time.NewTicker(touchWritePeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-s.touchQueue:
				pending[req.keyID] = req // coalesce: last writer per key wins
			case <-ticker.C:
				for keyID, req := range pending {
					writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
					s.Store.TouchAPIKey(writeCtx, keyID, req.ip, req.client)
					cancel()
					delete(pending, keyID)
				}
			}
		}
	}()
}

func (s *Server) enqueueTouch(req touchReq) {
	if s.touchQueue == nil {
		// Applier not started (dev/test wiring): keep the historical
		// synchronous best-effort write.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Store.TouchAPIKey(ctx, req.keyID, req.ip, req.client)
		return
	}
	select {
	case s.touchQueue <- req:
	default:
	}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

func requestIDOf(r *http.Request) string {
	if meta, ok := tracing.FromContext(r.Context()); ok {
		return meta.RequestID
	}
	return ""
}

func traceIDOf(r *http.Request) string {
	if meta, ok := tracing.FromContext(r.Context()); ok {
		return meta.Trace.TraceID
	}
	return ""
}
