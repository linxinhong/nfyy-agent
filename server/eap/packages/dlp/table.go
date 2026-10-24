package dlp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"enterprise-agent-platform/packages/controlcache"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/llmkernel"
)

// Rule is one versioned DLP rule (control data). Action deny blocks the
// request; action redact replaces matches in the released payload; action
// alert passes content unchanged while the hit stays audited (the
// monitor-then-enforce lifecycle); action tokenize swaps matches for
// reversible [rule:token] pseudonyms via the injected Tokenizer (the
// original lives only in the vault behind the audited reveal API).
type Rule struct {
	Name        string
	Pattern     string
	Action      string // deny | redact | alert | tokenize
	Replacement string
	Terms       []string // custom dictionary terms (compiled into the pattern)
	Version     int
	// ExceptRoles see ORIGINALS on employee-facing surfaces (knowledge
	// results, LLM responses, MCP responses, reveal). Provider-facing egress
	// never applies exemptions.
	ExceptRoles []string
	// Validator (optional): regex candidates must pass this national
	// checksum (GB 11643 / Luhn / GB 32100) before counting as hits — kills
	// digit-permutation false positives.
	Validator func(candidate string) bool
}

// compiled is the prebuilt matcher for one rule.
type compiled struct {
	rule Rule
	re   *regexp.Regexp
}

// TableScanner is the production rule-driven engine over versioned control
// data. Rules are loaded through LoadRules (PostgreSQL-backed from
// dlp_rules/dlp_dictionary_terms); a load error fails the scan closed.
type TableScanner struct {
	LoadRules func(ctx context.Context) ([]Rule, error)
	// Tokenizer mints the stable pseudonym for one tokenize-class match.
	// Injected by the gateway (vault-backed); nil + a tokenize rule hit is a
	// configuration failure and fails the scan closed.
	Tokenizer func(ctx context.Context, ruleName, value string) (string, error)
	// TokenizeBatch resolves many unique matches of one rule in a single
	// vault exchange (TASK-037). When set it takes precedence over
	// Tokenizer; the returned map must carry every requested value.
	TokenizeBatch func(ctx context.Context, ruleName string, values []string) (map[string]string, error)

	Now      func() time.Time // rules cache TTL anchor
	CacheTTL time.Duration

	// compileCache holds the fully compiled rule set (ADR-0025). Loads are
	// single-flight — one reload serves every concurrent scan — and the lock
	// is never held across LoadRules, so an expiry no longer serializes the
	// scanner behind one database round trip.
	compileCache *controlcache.Cache[*ruleSet]
	cacheOnce    sync.Once
}

// ruleSet is one compiled load: the rules plus the derived max-match bound
// the streaming sanitizer needs (TASK-039).
type ruleSet struct {
	rules []compiled
	// maxMatch is the longest possible match in bytes across the set.
	maxMatch int
	// bounded is false when any pattern can match unbounded length; the
	// streaming surface then applies its documented cap.
	bounded bool
}

// MaxMatchBytes exposes the derived max-match bound for the live rule set.
// bounded=false means some pattern matches unbounded length.
func (t *TableScanner) MaxMatchBytes(ctx context.Context) (int, bool, error) {
	// The key MUST include the scan workspace (TASK-045): workspace-scoped
	// rule sets must never be served across workspaces, whatever the order
	// of first scans.
	set, err := t.scannerCache().Get(ctx, "ws:"+WorkspaceFrom(ctx), t.compileRules)
	if err != nil {
		return 0, false, err
	}
	return set.maxMatch, set.bounded, nil
}

// NewTableScanner builds a scanner with a 60s rule cache.
func NewTableScanner(load func(ctx context.Context) ([]Rule, error)) *TableScanner {
	return &TableScanner{LoadRules: load, CacheTTL: time.Minute, Now: time.Now}
}

// Subject carries who is looking and why the scan runs. ForProvider=true
// means provider egress: exemptions NEVER apply (the outbound defense is
// company-wide). ForProvider=false means an employee-facing surface:
// rules whose ExceptRoles intersect the subject's roles are skipped.
type Subject struct {
	Roles       []string
	ForProvider bool
}

type workspaceKey struct{}

// WorkspaceFrom reads the scan workspace from the context (empty = global).
func WorkspaceFrom(ctx context.Context) string {
	ws, _ := ctx.Value(workspaceKey{}).(string)
	return ws
}

// ContextWithWorkspace binds the workspace for rule scoping.
func ContextWithWorkspace(ctx context.Context, workspaceID string) context.Context {
	return context.WithValue(ctx, workspaceKey{}, workspaceID)
}

type subjectKey struct{}

// ContextWithSubject binds the scan subject onto a context.
func ContextWithSubject(ctx context.Context, sub Subject) context.Context {
	return context.WithValue(ctx, subjectKey{}, sub)
}

func subjectFrom(ctx context.Context) Subject {
	sub, _ := ctx.Value(subjectKey{}).(Subject)
	return sub
}

// subjectFilter applies per-call exemptions to a compiled rule set: rules
// whose ExceptRoles hit the subject's roles are dropped on employee-facing
// surfaces; provider-facing scans keep every rule.
func subjectFilter(rules []compiled, sub Subject) []compiled {
	if sub.ForProvider {
		return rules
	}
	out := make([]compiled, 0, len(rules))
	for _, c := range rules {
		if len(c.rule.ExceptRoles) > 0 && rolesIntersect(sub.Roles, c.rule.ExceptRoles) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// replaceValid applies fn only to regex candidates that pass the rule's
// checksum validator (no-op when the rule has none).
func replaceValid(c compiled, text string, fn func(string) string) string {
	if c.rule.Validator == nil {
		return c.re.ReplaceAllStringFunc(text, fn)
	}
	return c.re.ReplaceAllStringFunc(text, func(m string) string {
		if c.rule.Validator(keepDigitsForRule(m)) {
			return fn(m)
		}
		return m
	})
}

// keepDigitsForRule strips separators for checksum evaluation (USCC keeps
// its letters — handled by the validator itself receiving the raw match
// when it needs letters; digit-only validators get digits only).
func keepDigitsForRule(m string) string {
	return m // validators sanitize internally (keepDigits)
}

func rolesIntersect(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// RulesForSubject exposes the effective rules for a subject (exemptions
// applied). Used by the gateway for surface decisions.
func (t *TableScanner) RulesForSubject(ctx context.Context, sub Subject) ([]Rule, error) {
	compiled, err := t.rules(context.WithValue(ctx, subjectKey{}, sub))
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(compiled))
	for _, c := range compiled {
		out = append(out, c.rule)
	}
	return out, nil
}

// AllRules returns every enabled rule ignoring exemptions (for exemption
// checks themselves).
func (t *TableScanner) AllRules(ctx context.Context) ([]Rule, error) {
	compiled, err := t.rules(context.WithValue(ctx, subjectKey{}, Subject{ForProvider: true}))
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(compiled))
	for _, c := range compiled {
		out = append(out, c.rule)
	}
	return out, nil
}

// rules returns the effective compiled rule set: the cached full set with
// subject exemptions applied per call (never cached — privileged and
// ordinary scans must not poison each other).
func (t *TableScanner) rules(ctx context.Context) ([]compiled, error) {
	// The key MUST include the scan workspace (TASK-045): workspace-scoped
	// rule sets must never be served across workspaces, whatever the order
	// of first scans.
	set, err := t.scannerCache().Get(ctx, "ws:"+WorkspaceFrom(ctx), t.compileRules)
	if err != nil {
		return nil, err
	}
	return subjectFilter(set.rules, subjectFrom(ctx)), nil
}

func (t *TableScanner) scannerCache() *controlcache.Cache[*ruleSet] {
	t.cacheOnce.Do(func() {
		c := controlcache.New[*ruleSet](t.ttl)
		c.Now = func() time.Time {
			if t.Now != nil {
				return t.Now()
			}
			return time.Now()
		}
		t.compileCache = c
	})
	return t.compileCache
}

func (t *TableScanner) ttl() time.Duration {
	if t.CacheTTL <= 0 {
		// Documented controlcache contract: <=0 means always reload — a
		// disabled cache must not silently become a 60s cache (TASK-045).
		return 0
	}
	return t.CacheTTL
}

// compileRules loads and compiles the enabled rule set. It runs outside any
// scanner lock; a load or compile error is returned (fail closed) and not
// cached, so the next scan retries.
func (t *TableScanner) compileRules(ctx context.Context) (*ruleSet, error) {
	rules, err := t.LoadRules(ctx)
	if err != nil {
		return nil, err
	}
	out := &ruleSet{rules: make([]compiled, 0, len(rules)), maxMatch: 0, bounded: true}
	for _, r := range rules {
		// Custom dictionary terms EXTEND the rule pattern (union), never
		// replace it: a rule must keep matching its own regex.
		pattern := r.Pattern
		if len(r.Terms) > 0 {
			terms := make([]string, 0, len(r.Terms)+1)
			terms = append(terms, "(?:"+pattern+")")
			for _, term := range r.Terms {
				terms = append(terms, "(?i:"+regexp.QuoteMeta(term)+")")
			}
			pattern = strings.Join(terms, "|")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, errs.New(errs.Internal, fmt.Sprintf("dlp rule %q has an invalid pattern", r.Name))
		}
		out.rules = append(out.rules, compiled{rule: r, re: re})
		if m, ok := ruleMaxMatch(pattern); ok {
			if m > out.maxMatch {
				out.maxMatch = m
			}
		} else {
			out.bounded = false
		}
	}
	return out, nil
}

// ScanWithMode implements the ModeScanner contract. Under ScanRedact every
// match (including deny-class rules) becomes a redaction; under ScanEnforce
// the first deny-class match blocks, redact-class matches are applied.
func (t *TableScanner) ScanWithMode(ctx context.Context, text string, mode llmkernel.ScanMode) (llmkernel.DLPResult, error) {
	rules, err := t.rules(ctx)
	if err != nil {
		// Fail closed with classification preserved for the caller's log.
		return llmkernel.DLPResult{}, errs.Wrap(err, errs.Internal, "dlp rule load failed")
	}
	redacted := text
	redactions := 0
	var ruleNames []string
	var denyHit *compiled

	for _, c := range rules {
		matches := c.re.FindAllString(redacted, -1)
		if c.rule.Validator != nil {
			// Checksum-backed rule: only candidates passing the national
			// checksum count; permutations stay untouched.
			valid := matches[:0]
			for _, m := range matches {
				if c.rule.Validator(keepDigitsForRule(m)) {
					valid = append(valid, m)
				}
			}
			matches = valid
		}
		if len(matches) == 0 {
			continue
		}
		ruleNames = append(ruleNames, c.rule.Name)
		action := c.rule.Action
		if mode == llmkernel.ScanRedact {
			action = "redact" // allow_with_redaction downgrades deny to redact
		}
		if action == "alert" {
			// Monitor mode: the hit is recorded via RuleNames (the caller
			// audits it), content passes untouched — never blocked, never
			// rewritten.
			continue
		}
		if action == "tokenize" {
			// TASK-037: unique matches are resolved through the batch seam
			// (one vault exchange per rule per scan) when wired; every
			// occurrence of a value gets the same stable pseudonym either
			// way. No batch seam keeps the per-match Tokenizer contract.
			if t.TokenizeBatch != nil {
				unique := make([]string, 0, len(matches))
				seen := make(map[string]struct{}, len(matches))
				for _, m := range matches {
					if _, ok := seen[m]; !ok {
						seen[m] = struct{}{}
						unique = append(unique, m)
					}
				}
				tokens, err := t.TokenizeBatch(ctx, c.rule.Name, unique)
				if err != nil {
					return llmkernel.DLPResult{}, errs.Wrap(err, errs.Internal, "dlp tokenize failed")
				}
				replaced := 0
				redacted = replaceValid(c, redacted, func(m string) string {
					tok, ok := tokens[m]
					if !ok {
						return m
					}
					replaced++
					return "[" + c.rule.Name + ":" + tok + "]"
				})
				redactions += replaced
				continue
			}
			if t.Tokenizer == nil {
				return llmkernel.DLPResult{}, errs.New(errs.Internal,
					"dlp rule "+c.rule.Name+" requires a tokenizer")
			}
			var tokErr error
			replaced := 0
			redacted = replaceValid(c, redacted, func(m string) string {
				if tokErr != nil {
					return m
				}
				tok, err := t.Tokenizer(ctx, c.rule.Name, m)
				if err != nil {
					tokErr = err
					return m
				}
				replaced++
				return "[" + c.rule.Name + ":" + tok + "]"
			})
			if tokErr != nil {
				return llmkernel.DLPResult{}, errs.Wrap(tokErr, errs.Internal, "dlp tokenize failed")
			}
			redactions += replaced
			continue
		}
		if action == "deny" {
			denyHit = &c
			continue
		}
		replacement := c.rule.Replacement
		if replacement == "" {
			replacement = "[REDACTED:" + c.rule.Name + "]"
		}
		if c.rule.Validator != nil {
			redacted = replaceValid(c, redacted, func(string) string { return replacement })
			redactions += len(matches)
			continue
		}
		redacted = c.re.ReplaceAllString(redacted, replacement)
		redactions += len(matches)
	}
	if denyHit != nil {
		return llmkernel.DLPResult{
			Blocked:   true,
			Redacted:  redacted,
			Reason:    "content matches DLP rule " + denyHit.rule.Name,
			RuleNames: ruleNames,
		}, nil
	}
	return llmkernel.DLPResult{
		Redacted:       redacted,
		RedactionCount: redactions,
		RuleNames:      ruleNames,
	}, nil
}

// Scan implements llmkernel.DLPEngine with enforce semantics.
func (t *TableScanner) Scan(ctx context.Context, text string) (llmkernel.DLPResult, error) {
	return t.ScanWithMode(ctx, text, llmkernel.ScanEnforce)
}
