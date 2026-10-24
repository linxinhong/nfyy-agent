// Package token defines the EAP internal JWT contract (Layer 2 identity):
// short-lived, asymmetrically signed tokens that the Agent Gateway issues and
// downstream capability services verify locally. Employee API keys never
// travel downstream; only these tokens do.
//
// Contract rules (ADR-0001, refined in ADR-0002):
//   - Ed25519 (EdDSA) signatures with a key id ("kid") header for rotation.
//   - The audience claim is a single registered value and is strictly
//     enforced: a token minted for llm-gateway is rejected by mcp-gateway.
//   - The workspace claim is mandatory for capability audiences (llm-gateway,
//     mcp-gateway, knowledge, audit, control-plane-api). Tokens for
//     agent-gateway are control-plane login sessions minted before any
//     workspace selection and are the single exempt audience; they carry no
//     workspace authority and are never accepted downstream.
//   - TTL is capped at 15 minutes.
package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"enterprise-agent-platform/packages/identity"
)

// Algorithm is the only accepted JWS algorithm for internal tokens.
const Algorithm = "EdDSA"

// DefaultTTL and MaxTTL bound token lifetime (PRD: 5-15 minutes).
const (
	DefaultTTL = 10 * time.Minute
	MaxTTL     = 15 * time.Minute
)

// ClaimWorkspace is the frozen claim name carrying workspace authority.
const ClaimWorkspace = "workspace"

// Audience enumerates the registered internal token audiences. Strictly
// single-valued: every internal service verifies against exactly one.
type Audience string

const (
	AudienceAgentGateway Audience = "agent-gateway"
	AudienceLLMGateway   Audience = "llm-gateway"
	AudienceMCPGateway   Audience = "mcp-gateway"
	AudienceKnowledge    Audience = "knowledge"
	AudienceControlPlane Audience = "control-plane-api"
	AudienceAudit        Audience = "audit"
)

// Known reports whether the audience is registered. Unknown audiences are
// rejected at issuance and verification to prevent typo-scope confusion.
func (a Audience) Known() bool {
	switch a {
	case AudienceAgentGateway, AudienceLLMGateway, AudienceMCPGateway,
		AudienceKnowledge, AudienceControlPlane, AudienceAudit:
		return true
	default:
		return false
	}
}

// Contract error sentinels; use errors.Is to classify.
var (
	ErrAudienceMismatch = errors.New("internal jwt audience does not match the expected audience")
	ErrExpired          = errors.New("internal jwt is expired")
	ErrMalformed        = errors.New("internal jwt is malformed")
	ErrUnknownKeyID     = errors.New("internal jwt signing key id is unknown")
	ErrBadAlgorithm     = errors.New("internal jwt algorithm is not allowed")
	ErrMissingClaim     = errors.New("internal jwt is missing a required claim")
)

// Claims is the internal JWT payload (PRD 6.4, frozen set).
type Claims struct {
	Issuer         string
	Subject        string // user id
	Audience       Audience
	IssuedAt       time.Time
	ExpiresAt      time.Time
	JWTID          string
	WorkspaceID    string
	OrganizationID string
	DepartmentID   string
	Roles          []string
	Scopes         []string
	ClientID       string
	DeviceID       string
}

// validate enforces the structural rules beyond signature verification.
func (c *Claims) validate(expected Audience) error {
	if c.Audience != expected || !c.Audience.Known() {
		return ErrAudienceMismatch
	}
	if c.Subject == "" {
		return fmt.Errorf("%w: sub", ErrMissingClaim)
	}
	if c.JWTID == "" {
		return fmt.Errorf("%w: jti", ErrMissingClaim)
	}
	// Workspace authority is mandatory everywhere except the gateway's own
	// control-plane sessions (ADR-0002).
	if expected != AudienceAgentGateway && c.WorkspaceID == "" {
		return fmt.Errorf("%w: %s", ErrMissingClaim, ClaimWorkspace)
	}
	if c.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: exp", ErrMissingClaim)
	}
	if !c.ExpiresAt.After(time.Now()) {
		return ErrExpired
	}
	return nil
}

// Session rebuilds an authorized downstream Session from verified claims.
func (c *Claims) Session() (identity.Session, error) {
	s := identity.Session{
		Principal: identity.Principal{
			UserID:         c.Subject,
			OrganizationID: c.OrganizationID,
			DepartmentID:   c.DepartmentID,
			Roles:          c.Roles,
			Scopes:         c.Scopes,
			ClientID:       c.ClientID,
			DeviceID:       c.DeviceID,
		},
		Workspace: identity.WorkspaceContext{
			WorkspaceID: c.WorkspaceID,
			Source:      identity.WorkspaceSourceInternalJWT,
		},
	}
	if err := s.Validate(); err != nil {
		return identity.Session{}, err
	}
	return s, nil
}

// IssueInput describes one token issuance.
type IssueInput struct {
	Session  identity.Session
	Audience Audience
	TTL      time.Duration // zero selects DefaultTTL; values above MaxTTL are clamped
	Now      time.Time     // optional clock override for tests
}

// Signer mints internal tokens.
type Signer interface {
	Sign(in IssueInput) (string, error)
}

// Verifier verifies tokens against one expected audience. A token is valid
// only if the signature, key id, algorithm and audience all check out.
type Verifier interface {
	Verify(raw string, expected Audience) (*Claims, error)
}

// VerificationKey is one public key in the verifier key set, addressed by kid.
type VerificationKey struct {
	KeyID  string
	Public ed25519.PublicKey
}

// Ed25519Signer signs internal tokens with one active key.
type Ed25519Signer struct {
	keyID  string
	issuer string
	key    ed25519.PrivateKey
}

// NewEd25519Signer builds a signer. The issuer is the platform token issuer
// URL (for example https://agent.company.com/).
func NewEd25519Signer(keyID, issuer string, key ed25519.PrivateKey) (*Ed25519Signer, error) {
	if keyID == "" {
		return nil, errors.New("ed25519 signer requires a key id")
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("ed25519 signer requires an ed25519 private key")
	}
	if issuer == "" {
		return nil, errors.New("ed25519 signer requires an issuer")
	}
	return &Ed25519Signer{keyID: keyID, issuer: issuer, key: key}, nil
}

// Sign implements Signer.
func (s *Ed25519Signer) Sign(in IssueInput) (string, error) {
	if in.Audience == AudienceAgentGateway {
		// Control-plane login session: principal identity only, no workspace
		// authority has been selected yet (ADR-0002).
		if err := in.Session.Principal.Validate(); err != nil {
			return "", fmt.Errorf("refusing to issue session without a valid principal: %w", err)
		}
	} else if err := in.Session.Validate(); err != nil {
		return "", fmt.Errorf("refusing to issue token without a valid session: %w", err)
	}
	if !in.Audience.Known() {
		return "", fmt.Errorf("refusing to issue token for unregistered audience %q", in.Audience)
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	claims := wireClaims{
		Issuer:       s.issuer,
		Subject:      in.Session.Principal.UserID,
		Audience:     string(in.Audience),
		IssuedAt:     now.Unix(),
		ExpiresAt:    now.Add(ttl).Unix(),
		JWTID:        newTokenID(),
		WorkspaceID:  in.Session.Workspace.WorkspaceID,
		Organization: in.Session.Principal.OrganizationID,
		Department:   in.Session.Principal.DepartmentID,
		Roles:        in.Session.Principal.Roles,
		Scopes:       in.Session.Principal.Scopes,
		ClientID:     in.Session.Principal.ClientID,
		DeviceID:     in.Session.Principal.DeviceID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, &claims)
	tok.Header["kid"] = s.keyID
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", fmt.Errorf("signing internal jwt: %w", err)
	}
	return signed, nil
}

// Ed25519Verifier verifies tokens against a key set indexed by kid.
type Ed25519Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewEd25519Verifier builds a verifier; pass at least the active key, plus
// previous keys still accepted during rotation.
func NewEd25519Verifier(keys ...VerificationKey) (*Ed25519Verifier, error) {
	if len(keys) == 0 {
		return nil, errors.New("ed25519 verifier requires at least one key")
	}
	m := make(map[string]ed25519.PublicKey, len(keys))
	for _, k := range keys {
		if k.KeyID == "" {
			return nil, errors.New("verification key requires a key id")
		}
		if len(k.Public) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("verification key %q is not an ed25519 public key", k.KeyID)
		}
		m[k.KeyID] = k.Public
	}
	return &Ed25519Verifier{keys: m}, nil
}

// Verify implements Verifier. Only EdDSA tokens signed by a known kid are
// accepted; anything else (none, HMAC, unknown key) is rejected.
func (v *Ed25519Verifier) Verify(raw string, expected Audience) (*Claims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{Algorithm}),
		jwt.WithExpirationRequired(),
	)
	wc := &wireClaims{}
	tok, err := parser.ParseWithClaims(raw, wc, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		pub, ok := v.keys[kid]
		if !ok {
			return nil, ErrUnknownKeyID
		}
		return pub, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpired
		}
		// Double %w keeps contract sentinels (unknown kid) reachable via errors.Is.
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if !tok.Valid {
		return nil, ErrMalformed
	}

	c := &Claims{
		Issuer:         wc.Issuer,
		Subject:        wc.Subject,
		Audience:       Audience(wc.Audience),
		JWTID:          wc.JWTID,
		WorkspaceID:    wc.WorkspaceID,
		OrganizationID: wc.Organization,
		DepartmentID:   wc.Department,
		Roles:          wc.Roles,
		Scopes:         wc.Scopes,
		ClientID:       wc.ClientID,
		DeviceID:       wc.DeviceID,
	}
	if wc.IssuedAt > 0 {
		c.IssuedAt = time.Unix(wc.IssuedAt, 0).UTC()
	}
	if wc.ExpiresAt > 0 {
		c.ExpiresAt = time.Unix(wc.ExpiresAt, 0).UTC()
	}

	if err := c.validate(expected); err != nil {
		return nil, err
	}
	return c, nil
}

// wireClaims is the frozen JWT claim set on the wire. It doubles as the
// jwt.Claims adapter so the parser validates exp/aud directly against the
// populated struct. aud is a single string by contract.
type wireClaims struct {
	Issuer       string   `json:"iss,omitempty"`
	Subject      string   `json:"sub,omitempty"`
	Audience     string   `json:"aud,omitempty"`
	ExpiresAt    int64    `json:"exp,omitempty"`
	IssuedAt     int64    `json:"iat,omitempty"`
	NotBefore    int64    `json:"nbf,omitempty"`
	JWTID        string   `json:"jti,omitempty"`
	WorkspaceID  string   `json:"workspace,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Department   string   `json:"department,omitempty"`
	Roles        []string `json:"roles,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	DeviceID     string   `json:"device_id,omitempty"`
}

func (w *wireClaims) GetExpirationTime() (*jwt.NumericDate, error) { return unixDate(w.ExpiresAt), nil }
func (w *wireClaims) GetIssuedAt() (*jwt.NumericDate, error)       { return unixDate(w.IssuedAt), nil }
func (w *wireClaims) GetNotBefore() (*jwt.NumericDate, error)      { return unixDate(w.NotBefore), nil }
func (w *wireClaims) GetIssuer() (string, error)                   { return w.Issuer, nil }
func (w *wireClaims) GetSubject() (string, error)                  { return w.Subject, nil }
func (w *wireClaims) GetJti() (string, error)                      { return w.JWTID, nil }

func (w *wireClaims) GetAudience() (jwt.ClaimStrings, error) {
	if w.Audience == "" {
		return nil, nil
	}
	return jwt.ClaimStrings{w.Audience}, nil
}

func unixDate(sec int64) *jwt.NumericDate {
	if sec == 0 {
		return nil
	}
	return jwt.NewNumericDate(time.Unix(sec, 0))
}

// newTokenID returns a random 128-bit id, base64url unpadded (jti).
func newTokenID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable; tokens must stay unique.
		panic(fmt.Errorf("crypto/rand unavailable: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
