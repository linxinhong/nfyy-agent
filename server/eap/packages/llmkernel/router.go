package llmkernel

import (
	"context"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/token"
)

// DecisionTrace is the auditable outcome of one routed decision: effects,
// rule anchor, scan redaction counts and outcome class. It carries NO raw
// content — details are safe for the reliable audit path.
type DecisionTrace struct {
	Effect             string
	RuleID             string
	Outcome            string // allowed | redacted | dlp_blocked | denied | error
	RequestRedactions  int
	ResponseRedactions int
	DLPRuleNames       []string
}

// ProviderRoute is one candidate (provider, provider-native model).
type ProviderRoute struct {
	Provider  string
	Model     string
	LocalOnly bool // provider flagged local-only (LOCAL_MODEL_ONLY effect)
}

// RoutePlanView is the plan projection the kernel needs (decoupled from the
// governance package to avoid an import cycle).
type RoutePlanView struct {
	Alias  string
	Routes []ProviderRoute
}

// ProviderClient executes one chat call against one target. Failures advance
// the route cursor deterministically; EAP-shaped denials do not.
type ProviderClient interface {
	Chat(ctx context.Context, target ProviderTarget, credential string, req ChatRequest) (*ChatResult, error)
}

// Router executes chat calls across versioned route plans with deterministic
// failover. It is the multi-provider extension of the TASK-002 kernel; the
// security pipeline (audience/workspace -> policy -> DLP) runs once, in
// front, and never re-runs per route.
type Router struct {
	ResolvePlan   func(ctx context.Context, alias string) (RoutePlanView, error)
	ResolveTarget func(ctx context.Context, provider string) (ProviderTarget, error)
	Credential    CredentialLookup
	Client        ProviderClient
}

// GenerateChatRouted runs the gates once, then walks the plan in order: the
// first successful route wins; provider-side failures and credential
// failures deterministically advance to the next route. Authorization-shaped
// denials from our own layer do NOT fail over.
//
// Effects (ADR-0010): deny and require_approval return a stable non-
// executing decision; local_model_only filters the plan to local providers;
// allow_with_redaction downgrades DLP deny matches to redactions and always
// applies the redacted payload to both the request and the provider response.
func (r *Router) GenerateChatRouted(
	ctx context.Context,
	claims *token.Claims,
	policy PolicyEngine,
	dlp DLPEngine,
	req ChatRequest,
	onAttempt func(RouteAttempt),
) (*ChatResult, RoutePlanView, []RouteAttempt, DecisionTrace, error) {
	trace := DecisionTrace{Outcome: "error"}
	if r.ResolvePlan == nil || r.ResolveTarget == nil || r.Credential == nil || r.Client == nil {
		return nil, RoutePlanView{}, nil, trace, errs.PolicyFailClosed("router is not fully configured")
	}
	// Gates first — identical semantics to the single-provider kernel.
	if _, _, err := authorizeLLM(claims); err != nil {
		return nil, RoutePlanView{}, nil, trace, err
	}
	if policy == nil || dlp == nil {
		return nil, RoutePlanView{}, nil, trace, errs.PolicyFailClosed("policy or DLP engine is not configured")
	}
	decision, err := policy.Decide(ctx, PolicyInput{
		Session: claimSession{userID: claims.Subject, workspace: claims.WorkspaceID},
		Model:   req.Model,
		Action:  "llm.chat",
	})
	if err != nil {
		return nil, RoutePlanView{}, nil, trace, errs.PolicyFailClosed(err.Error())
	}
	trace.Effect = decision.Effect
	trace.RuleID = decision.RuleID
	if !decision.Allowed {
		trace.Outcome = "denied"
		return nil, RoutePlanView{}, nil, trace, errs.New(errs.PolicyDenied, "policy denied the request: "+decision.Reason)
	}
	effect := decision.Effect
	if effect == "" {
		effect = EffectAllow
	}
	// Non-executing effects: deny and require_approval return a stable,
	// rule-anchored decision and never reach a provider.
	switch effect {
	case EffectDeny:
		trace.Outcome = "denied"
		return nil, RoutePlanView{}, nil, trace, errs.New(errs.PolicyDenied,
			"policy denied the request: "+decision.Reason)
	case EffectRequireApproval:
		trace.Outcome = "denied"
		return nil, RoutePlanView{}, nil, trace, errs.New(errs.PolicyDenied,
			"approval required: "+decision.RuleID)
	case EffectAllow, EffectRedact, EffectLocalModelOnly:
		// executable effects
	default:
		return nil, RoutePlanView{}, nil, trace, errs.PolicyFailClosed("unknown policy effect: " + effect)
	}

	mode := ScanEnforce
	if effect == EffectRedact {
		mode = ScanRedact
	}
	// 逐消息扫描并原地脱敏：provider 永远看到脱敏后的文本，原始密钥不出网。
	scan, err := scanMessages(ctx, dlp, req, mode)
	if err != nil {
		return nil, RoutePlanView{}, nil, trace, errs.DLPFailClosed(err.Error())
	}
	trace.RequestRedactions = scan.RedactionCount
	trace.DLPRuleNames = scan.RuleNames
	if scan.Blocked {
		trace.Outcome = "dlp_blocked"
		return nil, RoutePlanView{}, nil, trace, errs.New(errs.DLPBlocked, "DLP blocked outbound content: "+scan.Reason)
	}
	if scan.RedactionCount > 0 {
		trace.Outcome = "redacted"
	}

	plan, err := r.ResolvePlan(ctx, req.Model)
	if err != nil {
		return nil, RoutePlanView{}, nil, trace, err
	}
	if effect == EffectLocalModelOnly {
		local := make([]ProviderRoute, 0, len(plan.Routes))
		for _, route := range plan.Routes {
			if route.LocalOnly {
				local = append(local, route)
			}
		}
		if len(local) == 0 {
			trace.Outcome = "denied"
			return nil, RoutePlanView{}, nil, trace, errs.New(errs.PolicyDenied,
				"policy requires a local model route and none is configured for "+req.Model)
		}
		plan.Routes = local
	}

	attempts := make([]RouteAttempt, 0, len(plan.Routes))
	record := func(a RouteAttempt) {
		attempts = append(attempts, a)
		if onAttempt != nil {
			onAttempt(a)
		}
	}

	var lastErr error
	for _, route := range plan.Routes {
		target, err := r.ResolveTarget(ctx, route.Provider)
		if err != nil {
			lastErr = err
			record(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "failed", Err: "target_unavailable"})
			continue
		}
		credential, credErr := r.Credential(ctx, target.CredentialRef)
		if credErr != nil {
			lastErr = errs.Wrap(credErr, errs.Internal, "credential resolution failed")
			record(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "failed", Err: "credential_unavailable"})
			continue
		}
		// The provider receives the route's concrete model, never the
		// logical alias (real providers reject unknown names).
		routeReq := req
		routeReq.Model = route.Model
		result, chatErr := r.Client.Chat(ctx, target, credential, routeReq)
		if chatErr == nil {
			// Response pre-release scan: the provider echo never releases
			// secrets, and redactions apply to the released payload.
			respScan, scanErr := scanWithMode(ctx, dlp, result.Content, mode)
			if scanErr != nil {
				return nil, plan, attempts, trace, errs.DLPFailClosed(scanErr.Error())
			}
			if respScan.Blocked {
				record(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "failed", Err: "response_blocked"})
				trace.Outcome = "dlp_blocked"
				trace.ResponseRedactions = respScan.RedactionCount
				return nil, plan, attempts, trace, errs.New(errs.DLPBlocked, "DLP blocked the provider response: "+respScan.Reason)
			}
			if respScan.RedactionCount > 0 {
				result.Content = respScan.Redacted
				trace.ResponseRedactions = respScan.RedactionCount
				trace.Outcome = "redacted"
			}
			record(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "selected"})
			if trace.Outcome != "redacted" {
				trace.Outcome = "allowed"
			}
			return result, plan, attempts, trace, nil
		}
		// Authorization-shaped denials are decisions, not outages: they must
		// not fail over to another provider (that would circumvent the deny).
		// Every other error — including the EAP-wrapped upstream failures the
		// real client produces — is a provider failure and fails over.
		if p, isEAP := errs.From(chatErr); isEAP {
			switch p.Code {
			case errs.Unauthenticated, errs.PermissionDenied, errs.WorkspaceForbidden,
				errs.PolicyDenied, errs.DLPBlocked, errs.InvalidArgument:
				record(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "failed", Err: "denied"})
				return nil, plan, attempts, trace, chatErr
			}
		}
		lastErr = chatErr
		record(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "failed", Err: "provider_error"})
	}
	if lastErr == nil {
		lastErr = errs.New(errs.UpstreamUnavailable, "all routes exhausted")
	}
	return nil, plan, attempts, trace, lastErr
}
