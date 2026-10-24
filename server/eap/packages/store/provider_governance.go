package store

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/governance"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/skillreg"
)

// RoutePlanFor implements the kernel's ResolvePlan seam from control data,
// carrying each provider's local-only flag for the LOCAL_MODEL_ONLY effect.
func (s *Store) RoutePlanFor(ctx context.Context, alias string) (llmkernel.RoutePlanView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.provider, r.model, p.local_only
		FROM model_routes r
		JOIN model_aliases a ON a.alias = r.alias
		JOIN providers p ON p.name = r.provider
		WHERE r.alias = $1 AND r.enabled AND a.enabled AND p.enabled
		ORDER BY r.priority, r.provider, r.model`, alias)
	if err != nil {
		return llmkernel.RoutePlanView{}, errs.Wrap(err, errs.Internal, "route lookup failed")
	}
	defer rows.Close()
	view := llmkernel.RoutePlanView{Alias: alias}
	for rows.Next() {
		var route llmkernel.ProviderRoute
		if err := rows.Scan(&route.Provider, &route.Model, &route.LocalOnly); err != nil {
			return llmkernel.RoutePlanView{}, errs.Wrap(err, errs.Internal, "route scan failed")
		}
		view.Routes = append(view.Routes, route)
	}
	if len(view.Routes) == 0 {
		// Preserve the public missing-model contract at the gateway rather
		// than leaking a bare domain sentinel that normalizes to 500.
		return llmkernel.RoutePlanView{}, errs.New(errs.NotFound, "logical model "+alias+" has no enabled route")
	}
	return view, rows.Err()
}

// ResolveProviderTarget maps a provider name to its endpoint + credential ref.
func (s *Store) ResolveProviderTarget(ctx context.Context, provider string) (llmkernel.ProviderTarget, error) {
	var (
		baseURL string
		ref     string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT base_url, credential_ref FROM providers WHERE name = $1 AND enabled`, provider).
		Scan(&baseURL, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return llmkernel.ProviderTarget{}, errs.New(errs.NotFound, "provider is not configured or disabled")
	}
	if err != nil {
		return llmkernel.ProviderTarget{}, errs.Wrap(err, errs.Internal, "provider lookup failed")
	}
	return llmkernel.ProviderTarget{Name: provider, BaseURL: baseURL, CredentialRef: ref}, nil
}

// PricingForDelegates exposes pricing with a known flag for the ledger.
func (s *Store) PricingKnown(ctx context.Context, alias, provider string) (governance.Pricing, bool, int, error) {
	return s.Governance.PricingFor(ctx, alias, provider)
}

// PricingLookup is the single-value shape of one pricing resolution so the
// control cache can hold it as one entry (ADR-0025).
type PricingLookup struct {
	Pricing governance.Pricing
	Known   bool
	Version int
}

// PricingLookupFor resolves pricing in cache-friendly single-value form.
func (s *Store) PricingLookupFor(ctx context.Context, alias, provider string) (PricingLookup, error) {
	p, known, v, err := s.PricingKnown(ctx, alias, provider)
	if err != nil {
		return PricingLookup{}, err
	}
	return PricingLookup{Pricing: p, Known: known, Version: v}, nil
}

// InsertUsage delegates to the governance writer.
func (s *Store) InsertUsage(ctx context.Context, u governance.UsageRecord) error {
	return s.Governance.InsertUsage(ctx, u)
}

// Governance returns the control-data accessor (set at construction).
func (s *Store) RouteCount(ctx context.Context, alias string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM model_routes WHERE alias = $1 AND enabled`, alias).Scan(&n); err != nil {
		return 0, fmt.Errorf("route count: %w", err)
	}
	return n, nil
}

// LoadPolicyRules supplies the active policy rule set from control data
// (policy.TablePolicy's RulesLoader seam).
func (s *Store) LoadPolicyRules(ctx context.Context) ([]policy.TableRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rule_id, workspace_id, model_alias, effect, priority
		FROM policy_rules WHERE enabled ORDER BY priority, rule_id`)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "policy rule load failed")
	}
	defer rows.Close()
	var out []policy.TableRule
	for rows.Next() {
		var r policy.TableRule
		if err := rows.Scan(&r.RuleID, &r.WorkspaceID, &r.ModelAlias, &r.Effect, &r.Priority); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "policy rule scan failed")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AdminDLPRule is one DLP rule row for the admin inspection/management
// surface (no dictionary terms expansion).
type AdminDLPRule struct {
	Name        string   `json:"name"`
	Pattern     string   `json:"pattern"`
	Action      string   `json:"action"`
	Replacement string   `json:"replacement"`
	Enabled     bool     `json:"enabled"`
	Version     int      `json:"version"`
	ExceptRoles []string `json:"except_roles"`
}

// ListDLPRulesAll returns every DLP rule including disabled ones (admin).
func (s *Store) ListDLPRulesAll(ctx context.Context) ([]AdminDLPRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, pattern, action, replacement, enabled, version, except_roles
		FROM dlp_rules ORDER BY name`)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "dlp rules listing failed")
	}
	defer rows.Close()
	out := []AdminDLPRule{}
	for rows.Next() {
		var r AdminDLPRule
		if err := rows.Scan(&r.Name, &r.Pattern, &r.Action, &r.Replacement, &r.Enabled, &r.Version, &r.ExceptRoles); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "dlp rules scan failed")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateDLPRuleAction toggles one rule's action/enabled/exempt-roles state
// and bumps the version. A nil exceptRoles leaves exemptions untouched. The
// scanner cache (60s TTL) bounds propagation delay.
func (s *Store) UpdateDLPRuleAction(ctx context.Context, name, action string, enabled bool, exceptRoles []string) (AdminDLPRule, error) {
	var zero AdminDLPRule
	if action != "alert" && action != "deny" && action != "redact" && action != "tokenize" {
		return zero, errs.New(errs.InvalidArgument, "action must be alert, deny, redact or tokenize")
	}
	var r AdminDLPRule
	err := s.pool.QueryRow(ctx, `
		UPDATE dlp_rules SET action = $1, enabled = $2, version = version + 1, updated_at = now(),
			except_roles = COALESCE($4, except_roles)
		WHERE name = $3
		RETURNING name, pattern, action, replacement, enabled, version, except_roles`,
		action, enabled, name, nullableTextArray(exceptRoles)).Scan(&r.Name, &r.Pattern, &r.Action, &r.Replacement, &r.Enabled, &r.Version, &r.ExceptRoles)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, errs.New(errs.NotFound, "no such dlp rule")
	}
	if err != nil {
		return zero, errs.Wrap(err, errs.Internal, "dlp rule update failed")
	}
	return r, nil
}

// LoadDLPRules supplies the active DLP rule set with dictionary terms.
func (s *Store) LoadDLPRules(ctx context.Context) ([]dlp.Rule, error) {
	ws := dlp.WorkspaceFrom(ctx)
	rows, err := s.pool.Query(ctx, `
		SELECT r.name, r.pattern, r.action, COALESCE(r.replacement, ''), r.version, r.except_roles
		FROM dlp_rules r
		WHERE r.enabled AND (r.workspace_id = '*' OR ($1 <> '' AND r.workspace_id = $1))
		ORDER BY r.name`, ws)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "dlp rule load failed")
	}
	defer rows.Close()
	var out []dlp.Rule
	termsByRule := map[string][]string{}
	termRows, termErr := s.pool.Query(ctx, `
		SELECT rule_name, term FROM dlp_dictionary_terms ORDER BY rule_name, term`)
	if termErr != nil {
		return nil, errs.Wrap(termErr, errs.Internal, "dlp dictionary load failed")
	}
	haveTerms := true
	for termRows.Next() {
		var name, term string
		if err := termRows.Scan(&name, &term); err != nil {
			termRows.Close()
			return nil, errs.Wrap(termErr, errs.Internal, "dlp dictionary scan failed")
		}
		termsByRule[name] = append(termsByRule[name], term)
	}
	termRows.Close()
	if err := termRows.Err(); err != nil {
		haveTerms = false
	}
	_ = haveTerms

	for rows.Next() {
		var r dlp.Rule
		if err := rows.Scan(&r.Name, &r.Pattern, &r.Action, &r.Replacement, &r.Version, &r.ExceptRoles); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "dlp rule scan failed")
		}
		r.Validator = dlp.ValidatorFor(r.Name)
		r.Terms = termsByRule[r.Name]
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListEnabledAliases returns the enabled logical model aliases (control data)
// for /v1/models discovery; per-workspace policy filtering happens upstream.
func (s *Store) ListEnabledAliases(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT alias FROM model_aliases WHERE enabled ORDER BY alias`)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "alias listing failed")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "alias scan failed")
		}
		out = append(out, alias)
	}
	return out, rows.Err()
}

// ListPackageKeys returns all release-signing public keys (active and
// historical) for the discovery document.
func (s *Store) ListPackageKeys(ctx context.Context) ([]skillreg.PkgKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kid, public_key, active, created_at, retired_at
		FROM skill_package_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "package key listing failed")
	}
	defer rows.Close()
	var out []skillreg.PkgKey
	for rows.Next() {
		var (
			k         skillreg.PkgKey
			pubB64    string
			retiredAt *time.Time
		)
		if err := rows.Scan(&k.Kid, &pubB64, &k.Active, &k.CreatedAt, &retiredAt); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "package key scan failed")
		}
		raw, derr := base64.RawURLEncoding.DecodeString(pubB64)
		if derr != nil || len(raw) != ed25519.PublicKeySize {
			return nil, errs.Wrap(derr, errs.Internal, "package key material invalid")
		}
		k.PublicKey = ed25519.PublicKey(raw)
		k.RetiredAt = retiredAt
		out = append(out, k)
	}
	return out, rows.Err()
}

// ActivePackageKeyID returns the kid of the active release-signing key.
func (s *Store) ActivePackageKeyID(ctx context.Context) (string, error) {
	var kid string
	err := s.pool.QueryRow(ctx,
		`SELECT kid FROM skill_package_keys WHERE active ORDER BY created_at DESC LIMIT 1`).Scan(&kid)
	if err != nil {
		return "", errs.Wrap(err, errs.Internal, "active package key lookup failed")
	}
	return kid, nil
}

// RegisterPackageKey records the gateway's release-signing public key and
// makes it the active one; any other active key is retired but kept, so
// releases it signed stay verifiable. Idempotent across instances sharing
// one key file.
func (s *Store) RegisterPackageKey(ctx context.Context, kid string, pub ed25519.PublicKey) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO skill_package_keys (kid, public_key, active) VALUES ($1, $2, true)
		ON CONFLICT (kid) DO UPDATE SET active = true, retired_at = NULL`,
		kid, base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		return fmt.Errorf("register package key: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE skill_package_keys SET active = false, retired_at = now()
		WHERE kid <> $1 AND active`, kid); err != nil {
		return fmt.Errorf("retire package keys: %w", err)
	}
	return tx.Commit(ctx)
}
