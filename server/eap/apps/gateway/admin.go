package main

// Governed admin inspection (TASK-022): read-only control-plane views for
// authorized administrators. Every handler is fail-closed (admin role
// required server-side), every read is itself audited on the reliable path,
// and no response ever carries key material, credential references, signing
// material or storage paths.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/store"
)

// requirePermission authenticates a control-plane session and enforces a
// Workspace-wide permission server-side. Hiding UI navigation is not
// authorization.
func (s *Server) requirePermission(w http.ResponseWriter, r *http.Request, p rbac.Permission) (gatewayIdentities, bool) {
	session, ok := s.requireControlPlane(w, r)
	if !ok {
		return gatewayIdentities{}, false
	}
	if !session.grants.Has(p) {
		writeProblem(w, r, errs.New(errs.PermissionDenied, "permission required: "+string(p)))
		return gatewayIdentities{}, false
	}
	return session, true
}

// grantsFor resolves permissions; without a member store (unit-test servers)
// only the built-in roles apply.
func (s *Server) grantsFor(ctx context.Context, workspaceID, userID string, roles []string) (rbac.Grants, error) {
	if s.Members == nil {
		return rbac.Resolve(rbac.Builtin(), roles, nil, ""), nil
	}
	return s.Members.Grants(ctx, workspaceID, userID, roles)
}

// auditAdminRead records the admin read itself on the reliable audit path;
// a delivery failure fails the read (audit failure is never swallowed).
func (s *Server) auditAdminRead(r *http.Request, session gatewayIdentities, resource string) error {
	ev := audit.NewEvent("admin.read", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.userID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = resource
	ev.Detail = map[string]any{"resource": resource, "workspace": session.workspace}
	return audit.RequireWrite(r.Context(), s.Store, ev)
}

// adminRead wraps the common shape: authorize → serve → audit (fail closed).
func (s *Server) adminRead(w http.ResponseWriter, r *http.Request, resource string, serve func() (any, error)) {
	session, ok := s.requirePermission(w, r, rbac.PlatformAdmin)
	if !ok {
		return
	}
	payload, err := serve()
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if err := s.auditAdminRead(r, session, resource); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "admin audit delivery failed — read refused"))
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// parsePagination bounds limit/offset server-side; the Portal keeps no local
// copy of control data and pages through the contract.
func parsePagination(r *http.Request) (limit, offset int, err error) {
	limit, offset = 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 || limit > 200 {
			return 0, 0, errs.New(errs.InvalidArgument, "limit must be 1..200")
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			return 0, 0, errs.New(errs.InvalidArgument, "offset must be >= 0")
		}
	}
	return limit, offset, nil
}

// handleAdminUsers lists users with API key posture (counts only — never key
// material, prefixes or hashes).
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.users", func() (any, error) {
		limit, offset, err := parsePagination(r)
		if err != nil {
			return nil, err
		}
		rows, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT u.id, u.email, u.display_name,
			       count(k.key_id) FILTER (WHERE k.revoked_at IS NULL) AS active_keys,
			       count(k.key_id) AS total_keys
			FROM users u LEFT JOIN api_keys k ON k.user_id = u.id
			GROUP BY u.id, u.email, u.display_name
			ORDER BY u.email
			LIMIT $1 OFFSET $2`, limit, offset)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin user listing failed")
		}
		defer rows.Close()
		users := []map[string]any{}
		for rows.Next() {
			var id, email, name string
			var active, total int
			if err := rows.Scan(&id, &email, &name, &active, &total); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin user scan failed")
			}
			users = append(users, map[string]any{
				"id": id, "email": email, "display_name": name,
				"active_keys": active, "total_keys": total,
			})
		}
		return map[string]any{"users": users, "limit": limit, "offset": offset}, nil
	})
}

// handleAdminAgents lists registered agent releases (metadata only).
func (s *Server) handleAdminAgents(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.agents", func() (any, error) {
		limit, offset, err := parsePagination(r)
		if err != nil {
			return nil, err
		}
		rows, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT client, version, os, arch, minimum_version, status, workspace_id
			FROM agent_releases
			ORDER BY client, version DESC
			LIMIT $1 OFFSET $2`, limit, offset)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin agent listing failed")
		}
		defer rows.Close()
		releases := []map[string]any{}
		for rows.Next() {
			var client, version, goos, goarch, minimum, status, ws string
			if err := rows.Scan(&client, &version, &goos, &goarch, &minimum, &status, &ws); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin agent scan failed")
			}
			releases = append(releases, map[string]any{
				"client": client, "version": version, "os": goos, "arch": goarch,
				"minimum_version": minimum, "status": status, "workspace": ws,
			})
		}
		return map[string]any{"releases": releases, "limit": limit, "offset": offset}, nil
	})
}

// handleAdminModels lists model aliases, routes and pricing presence.
func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.models", func() (any, error) {
		aliasRows, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT a.alias, a.enabled, count(r.id) AS routes
			FROM model_aliases a LEFT JOIN model_routes r ON r.alias = a.alias
			GROUP BY a.alias, a.enabled ORDER BY a.alias LIMIT 200`)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin model listing failed")
		}
		defer aliasRows.Close()
		aliases := []map[string]any{}
		for aliasRows.Next() {
			var alias string
			var enabled bool
			var routes int
			if err := aliasRows.Scan(&alias, &enabled, &routes); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin model scan failed")
			}
			aliases = append(aliases, map[string]any{"alias": alias, "enabled": enabled, "routes": routes})
		}
		routeRows, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT alias, provider, model, priority, enabled
			FROM model_routes ORDER BY alias, priority LIMIT 200`)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin route listing failed")
		}
		defer routeRows.Close()
		routes := []map[string]any{}
		for routeRows.Next() {
			var alias, provider, model string
			var priority int
			var enabled bool
			if err := routeRows.Scan(&alias, &provider, &model, &priority, &enabled); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin route scan failed")
			}
			routes = append(routes, map[string]any{
				"alias": alias, "provider": provider, "model": model,
				"priority": priority, "enabled": enabled,
			})
		}
		return map[string]any{"aliases": aliases, "routes": routes}, nil
	})
}

// handleAdminUsage aggregates usage/cost from usage_records only (no second
// statistics source of truth).
func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.usage", func() (any, error) {
		byAlias, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT alias, provider, count(*) AS calls, COALESCE(sum(total_tokens), 0) AS tokens,
			       COALESCE(sum(cost_micros), 0) AS cost_micros
			FROM usage_records
			GROUP BY alias, provider ORDER BY calls DESC LIMIT 100`)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin usage rollup failed")
		}
		defer byAlias.Close()
		rollups := []map[string]any{}
		for byAlias.Next() {
			var alias, provider string
			var calls, tokens int64
			var cost int64
			if err := byAlias.Scan(&alias, &provider, &calls, &tokens, &cost); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin usage scan failed")
			}
			rollups = append(rollups, map[string]any{
				"alias": alias, "provider": provider, "calls": calls,
				"total_tokens": tokens, "cost_micros": cost,
			})
		}
		var totals struct{ calls, tokens, cost int64 }
		if err := s.OIDCStore.Pool().QueryRow(r.Context(), `
			SELECT count(*), COALESCE(sum(total_tokens), 0), COALESCE(sum(cost_micros), 0)
			FROM usage_records`).Scan(&totals.calls, &totals.tokens, &totals.cost); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin usage totals failed")
		}
		return map[string]any{
			"rollups": rollups,
			"totals":  map[string]any{"calls": totals.calls, "total_tokens": totals.tokens, "cost_micros": totals.cost},
		}, nil
	})
}

// handleAdminMCP lists governed tools without endpoint URLs or credential
// references (internal configuration never leaves the server).
func (s *Server) handleAdminMCP(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.mcp", func() (any, error) {
		rows, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT name, server, risk_level, workspaces
			FROM mcp_tools ORDER BY server, name LIMIT 200`)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin mcp listing failed")
		}
		defer rows.Close()
		tools := []map[string]any{}
		for rows.Next() {
			var name, server, risk string
			var workspaces []string
			if err := rows.Scan(&name, &server, &risk, &workspaces); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin mcp scan failed")
			}
			tools = append(tools, map[string]any{
				"name": name, "server": server, "risk_level": risk, "workspaces": workspaces,
			})
		}
		return map[string]any{"tools": tools}, nil
	})
}

// handleAdminKnowledge lists knowledge documents across workspaces.
func (s *Server) handleAdminKnowledge(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.knowledge", func() (any, error) {
		limit, offset, err := parsePagination(r)
		if err != nil {
			return nil, err
		}
		rows, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT workspace_id, title, version, status
			FROM knowledge_documents
			ORDER BY workspace_id, updated_at DESC
			LIMIT $1 OFFSET $2`, limit, offset)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin knowledge listing failed")
		}
		defer rows.Close()
		docs := []map[string]any{}
		for rows.Next() {
			var ws, title, status string
			var version int
			if err := rows.Scan(&ws, &title, &version, &status); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin knowledge scan failed")
			}
			docs = append(docs, map[string]any{
				"workspace": ws, "title": title, "version": version, "status": status,
			})
		}
		return map[string]any{"documents": docs, "limit": limit, "offset": offset}, nil
	})
}

// handleAdminDLP summarizes DLP hits from audit_events (single source).
func (s *Server) handleAdminDLP(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.dlp", func() (any, error) {
		rows, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT outcome, count(*) FROM audit_events
			WHERE type = 'dlp.hit' GROUP BY outcome`)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin dlp summary failed")
		}
		defer rows.Close()
		byOutcome := map[string]any{}
		for rows.Next() {
			var outcome string
			var n int
			if err := rows.Scan(&outcome, &n); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin dlp scan failed")
			}
			byOutcome[outcome] = n
		}
		var total int
		if err := s.OIDCStore.Pool().QueryRow(r.Context(),
			`SELECT count(*) FROM audit_events WHERE type = 'dlp.hit'`).Scan(&total); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin dlp total failed")
		}
		return map[string]any{"total": total, "by_outcome": byOutcome}, nil
	})
}

// handleAdminDLPRules lists every DLP rule (admin management surface).
func (s *Server) handleAdminDLPRules(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.dlp_rules", func() (any, error) {
		rules, err := s.Store.ListDLPRulesAll(r.Context())
		if err != nil {
			return nil, err
		}
		// Patterns are control data shown to administrators, but long
		// regexes render poorly — return them verbatim, the UI truncates.
		return map[string]any{"rules": rules}, nil
	})
}

// handleAdminUpdateDLPRule toggles one DLP rule's action/enabled state
// (TASK-026 monitor-then-enforce). The change is itself audited on the
// reliable path; delivery failure refuses the change.
func (s *Server) handleAdminUpdateDLPRule(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.PlatformAdmin)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var body struct {
		Action      string   `json:"action"`
		Enabled     *bool    `json:"enabled"`
		ExceptRoles []string `json:"except_roles"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	if body.Action == "" && body.Enabled == nil && body.ExceptRoles == nil {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "action, enabled or except_roles is required"))
		return
	}
	// Fetch-then-update so the audit captures the before state.
	rules, err := s.Store.ListDLPRulesAll(r.Context())
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	var before *store.AdminDLPRule
	for i := range rules {
		if rules[i].Name == name {
			before = &rules[i]
		}
	}
	if before == nil {
		writeProblem(w, r, errs.New(errs.NotFound, "no such dlp rule"))
		return
	}
	action, enabled := before.Action, before.Enabled
	if body.Action != "" {
		action = body.Action
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	updated, err := s.Store.UpdateDLPRuleAction(r.Context(), name, action, enabled, body.ExceptRoles)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	ev := audit.NewEvent("admin.dlp_rule_updated", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.userID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = "dlp_rules/" + name
	ev.Detail = map[string]any{
		"rule": name, "action_before": before.Action, "action_after": updated.Action,
		"enabled_before": before.Enabled, "enabled_after": enabled,
		"except_roles": body.ExceptRoles,
	}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "admin audit delivery failed — change refused"))
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleAdminDLPRuleTest is the admin sandbox (TASK-031): paste text, see
// which rules fire and the resulting preview. Nothing is persisted — no
// vault writes, no content in the audit record (only the call itself).
func (s *Server) handleAdminDLPRuleTest(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.PlatformAdmin)
	if !ok {
		return
	}
	var body struct {
		Content   string `json:"content"`
		Workspace string `json:"workspace"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "content is required"))
		return
	}
	sub := dlp.Subject{Roles: session.roles, ForProvider: true}
	ctx := dlp.ContextWithWorkspace(dlp.ContextWithSubject(r.Context(), sub), body.Workspace)
	res, err := s.DLP.Scan(ctx, body.Content)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "sandbox scan failed"))
		return
	}
	ev := audit.NewEvent("admin.dlp_rule_test", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.userID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"workspace": body.Workspace} // no content
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "admin audit delivery failed — sandbox refused"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rules":           res.RuleNames,
		"blocked":         res.Blocked,
		"reason":          res.Reason,
		"redaction_count": res.RedactionCount,
		"preview":         res.Redacted,
	})
}

// handleAdminAudit lists audit events with server-side filters/pagination.
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	s.adminRead(w, r, "admin.audit", func() (any, error) {
		limit, offset, err := parsePagination(r)
		if err != nil {
			return nil, err
		}
		filter := r.URL.Query().Get("type")
		rows, err := s.OIDCStore.Pool().Query(r.Context(), `
			SELECT type, outcome, actor_user_id, workspace_id, trace_id, resource, time
			FROM audit_events
			WHERE ($1 = '' OR type = $1)
			ORDER BY time DESC
			LIMIT $2 OFFSET $3`, filter, limit, offset)
		if err != nil {
			return nil, errs.Wrap(err, errs.Internal, "admin audit listing failed")
		}
		defer rows.Close()
		events := []map[string]any{}
		for rows.Next() {
			var typ, outcome string
			var timeVal time.Time
			var actor, ws, trace, resource *string
			if err := rows.Scan(&typ, &outcome, &actor, &ws, &trace, &resource, &timeVal); err != nil {
				return nil, errs.Wrap(err, errs.Internal, "admin audit scan failed")
			}
			events = append(events, map[string]any{
				"type": typ, "outcome": outcome, "actor": actor, "workspace": ws,
				"trace_id": trace, "resource": resource, "time": timeVal,
			})
		}
		return map[string]any{"events": events, "limit": limit, "offset": offset, "filter": filter}, nil
	})
}
