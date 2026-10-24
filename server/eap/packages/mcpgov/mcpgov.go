// Package mcpgov adds central Policy/risk/approval semantics on top of the
// governed MCP execution service (TASK-011, ADR-0011):
//   - discovery never exceeds execution authority (same workspace predicate);
//   - R1 executes directly when allowed; R2+ return approval-required with
//     ZERO upstream egress until an approved continuation exists;
//   - unknown risk is never treated as R1 (fail closed);
//   - upstream destinations pass an SSRF policy on the initial request and
//     on every redirect hop; credentials never reach the caller.
package mcpgov

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enterprise-agent-platform/packages/errs"
)

// Risk levels mirrored from the registry (PRD §6.6).
const (
	RiskR1 = "R1"
	RiskR2 = "R2"
	RiskR3 = "R3"
	RiskR4 = "R4"
	RiskR5 = "R5"
)

// Approval decision outcomes for the stable non-executing contract.
const (
	ApprovalRequired = "approval_required"
	ApprovalGranted  = "approved"
)

// PolicyDecision is the central-vocabulary projection mcpgov consumes —
// structurally identical to llmkernel.Decision so one engine type serves both
// planes without a second vocabulary.
type PolicyDecision struct {
	Allowed bool
	Reason  string
	Effect  string // allow | deny | ... (llmkernel canonical effects)
	RuleID  string
}

// PolicyEngine is the central decision point for MCP actions.
type PolicyEngine interface {
	DecideMCP(ctx context.Context, workspace, server, tool, action string) (PolicyDecision, error)
}

// ApprovalStore grants and consumes one-time approval continuations bound to
// (principal, tool). An approval is a bounded record, not a workflow engine.
type ApprovalStore interface {
	// Grant records an approved continuation and returns its token.
	Grant(ctx context.Context, principalID, tool string, ttl time.Duration) (string, error)
	// Consume atomically marks one token used; reuse fails closed.
	Consume(ctx context.Context, token, principalID, tool string) (bool, error)
}

func randomToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// MemoryApprovals is an in-process ApprovalStore (tests, single instance).
type MemoryApprovals struct {
	tokens map[string]approvalKey
}

type approvalKey struct{ principalID, tool string }

// NewMemoryApprovals builds an empty store.
func NewMemoryApprovals() *MemoryApprovals {
	return &MemoryApprovals{tokens: map[string]approvalKey{}}
}

// Grant implements ApprovalStore.
func (m *MemoryApprovals) Grant(_ context.Context, principalID, tool string, _ time.Duration) (string, error) {
	token := randomToken()
	m.tokens[token] = approvalKey{principalID: principalID, tool: tool}
	return token, nil
}

// Consume implements ApprovalStore.
func (m *MemoryApprovals) Consume(_ context.Context, token, principalID, tool string) (bool, error) {
	key, ok := m.tokens[token]
	if !ok || key.principalID != principalID || key.tool != tool {
		return false, nil
	}
	delete(m.tokens, token)
	return true, nil
}

// SSRFPolicy validates outbound destinations. Private/loopback/link-local
// targets are denied unless explicitly allowed (isolated test deployments
// stub upstreams on loopback).
type SSRFPolicy struct {
	AllowPrivate bool
}

// Check validates one URL. Redirect hops call this again.
func (p *SSRFPolicy) Check(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errs.New(errs.InvalidArgument, "upstream endpoint is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errs.New(errs.InvalidArgument, "upstream scheme is not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return errs.New(errs.InvalidArgument, "upstream host is missing")
	}
	if ip := net.ParseIP(host); ip != nil {
		return p.checkIP(ip)
	}
	if p.AllowPrivate && (host == "127.0.0.1" || host == "localhost") {
		return nil
	}
	// Resolve to catch DNS rebinding toward private space.
	ips, err := net.LookupIP(host)
	if err != nil {
		return errs.New(errs.InvalidArgument, "upstream host does not resolve")
	}
	for _, ip := range ips {
		if err := p.checkIP(ip); err != nil {
			return err
		}
	}
	return nil
}

func (p *SSRFPolicy) checkIP(ip net.IP) error {
	if p.AllowPrivate && (ip.IsLoopback() || ip.IsPrivate()) {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return errs.New(errs.InvalidArgument, "upstream resolves to a private network address")
	}
	return nil
}

// RedirectChecker returns an http.CheckRedirect that revalidates every hop
// against the SSRF policy (initial request validated separately).
func (p *SSRFPolicy) RedirectChecker() func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("too many redirects")
		}
		if err := p.Check(req.URL.String()); err != nil {
			return fmt.Errorf("redirect blocked by SSRF policy: %v", err)
		}
		// Authorization headers must not leak across origins on redirect.
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}
}

// SafeHTTPClient builds a client with redirect revalidation.
func (p *SSRFPolicy) SafeHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: p.RedirectChecker(),
	}
}

// ApprovalOutcome is the structured result of an approval check.
type ApprovalOutcome struct {
	Required bool
	Granted  bool
	Token    string
}

// EvaluateApproval decides whether a tool call may execute: R1 needs no
// approval; R2+ require a valid continuation token. Unknown risk fails
// closed. Returns (outcome, denyErr) — denyErr non-nil means the call is
// refused regardless of continuation.
func EvaluateApproval(riskLevel string) (ApprovalOutcome, error) {
	out := ApprovalOutcome{}
	switch strings.ToUpper(riskLevel) {
	case RiskR1:
		return out, nil
	case RiskR2, RiskR3, RiskR4, RiskR5:
		out.Required = true
		return out, nil
	default:
		// Unknown risk is never R1 (fail closed).
		return out, errs.New(errs.InvalidArgument, "tool carries an unknown risk level: "+riskLevel)
	}
}
