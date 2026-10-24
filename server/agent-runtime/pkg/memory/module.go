package memory

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

const maxOperationTokenLifetime = 7 * 24 * time.Hour

const maxOperationTokenBytes = 4096

// RecallTelemetry contains timing-only diagnostic information. It deliberately
// excludes query text, scopes, records, and tokens. Observers are opt-in and
// nil in normal product operation.
type RecallTelemetry struct {
	ListRecords time.Duration
	Ranking     time.Duration
	Total       time.Duration
	DeadlineAt  string
}

// RecallObserver receives one immutable timing summary for each Recall.
// Implementations must be bounded and non-blocking because this callback runs
// on the request path.
type RecallObserver interface {
	ObserveRecall(context.Context, RecallTelemetry)
}

type ModuleConfig struct {
	Store         AtomicStore
	Authorizer    Authorizer
	ContentPolicy ContentPolicy
	// Proposal is nil by default. A non-nil value explicitly enables the
	// synchronous candidate-only extraction capability.
	Proposal       *ProposalConfig
	Keys           map[string][]byte
	CurrentKey     string
	Limits         Limits
	RecallObserver RecallObserver
}

// Module is the sole trusted entry point for the strict memory subsystem.
// Configuration is copied at construction so later caller mutation cannot
// loosen policy or replace signing keys.
type Module struct {
	store          AtomicStore
	authorizer     Authorizer
	contentPolicy  ContentPolicy
	proposal       *proposalRuntime
	keys           map[string][]byte
	currentKey     string
	limits         Limits
	slots          chan struct{}
	proposalSlots  chan struct{}
	proposalCalls  atomic.Int32
	recallObserver RecallObserver
}

func NewModule(cfg ModuleConfig) (*Module, error) {
	if cfg.Store == nil || cfg.Authorizer == nil {
		return nil, fmt.Errorf("%w: store and authorizer are required", ErrInvalid)
	}
	limits := cfg.Limits
	defaults := DefaultLimits()
	fillLimits(&limits, defaults)
	if err := ValidateLimits(limits); err != nil {
		return nil, err
	}
	keys := make(map[string][]byte, len(cfg.Keys))
	for id, key := range cfg.Keys {
		if id == "" || validateIdentifier(id) != nil || len(key) < 32 {
			return nil, fmt.Errorf("%w: signing keys require an id and at least 32 bytes", ErrInvalid)
		}
		keys[id] = append([]byte(nil), key...)
	}
	if _, ok := keys[cfg.CurrentKey]; !ok {
		return nil, fmt.Errorf("%w: current signing key is unavailable", ErrInvalid)
	}
	proposal, err := newProposalRuntime(cfg.Proposal)
	if err != nil {
		return nil, err
	}
	return &Module{
		store: cfg.Store, authorizer: cfg.Authorizer, contentPolicy: cfg.ContentPolicy, keys: keys,
		currentKey: cfg.CurrentKey, limits: limits, proposal: proposal, recallObserver: cfg.RecallObserver,
		slots: make(chan struct{}, limits.MaxConcurrentOps), proposalSlots: make(chan struct{}, limits.MaxProposeConcurrent),
	}, nil
}

// ProposalEnabled reports whether this immutable Module was assembled with
// the complete synchronous proposal capability. It exposes no proposal
// dependency and cannot be used to mutate the frozen configuration.
func (m *Module) ProposalEnabled() bool {
	return m != nil && m.proposal != nil
}

func fillLimits(v *Limits, d Limits) {
	if v.MaxContentBytes == 0 {
		v.MaxContentBytes = d.MaxContentBytes
	}
	if v.MaxRecordBytes == 0 {
		v.MaxRecordBytes = d.MaxRecordBytes
	}
	if v.DefaultListLimit == 0 {
		v.DefaultListLimit = d.DefaultListLimit
	}
	if v.MaxListLimit == 0 {
		v.MaxListLimit = d.MaxListLimit
	}
	if v.MaxListBytes == 0 {
		v.MaxListBytes = d.MaxListBytes
	}
	if v.DefaultRecallLimit == 0 {
		v.DefaultRecallLimit = d.DefaultRecallLimit
	}
	if v.MaxRecallLimit == 0 {
		v.MaxRecallLimit = d.MaxRecallLimit
	}
	if v.MaxRecallCandidates == 0 {
		v.MaxRecallCandidates = d.MaxRecallCandidates
	}
	if v.MaxRecallBytes == 0 {
		v.MaxRecallBytes = d.MaxRecallBytes
	}
	if v.MaxCandidateBytes == 0 {
		v.MaxCandidateBytes = d.MaxCandidateBytes
	}
	if v.DefaultRetention == 0 {
		v.DefaultRetention = d.DefaultRetention
	}
	if v.MaxRetention == 0 {
		v.MaxRetention = d.MaxRetention
	}
	if v.MaxRecordsPerScope == 0 {
		v.MaxRecordsPerScope = d.MaxRecordsPerScope
	}
	if v.MaxContentPerScope == 0 {
		v.MaxContentPerScope = d.MaxContentPerScope
	}
	if v.MaxReceiptsPerScope == 0 {
		v.MaxReceiptsPerScope = d.MaxReceiptsPerScope
	}
	if v.MaxControlsPerScope == 0 {
		v.MaxControlsPerScope = d.MaxControlsPerScope
	}
	if v.MaxConcurrentOps == 0 {
		v.MaxConcurrentOps = d.MaxConcurrentOps
	}
	if v.CRUDTimeout == 0 {
		v.CRUDTimeout = d.CRUDTimeout
	}
	if v.RecallTimeout == 0 {
		v.RecallTimeout = d.RecallTimeout
	}
	if v.MaxProposeConcurrent == 0 {
		v.MaxProposeConcurrent = d.MaxProposeConcurrent
	}
	if v.MaxProposeInputBytes == 0 {
		v.MaxProposeInputBytes = d.MaxProposeInputBytes
	}
	if v.MaxProposeCandidates == 0 {
		v.MaxProposeCandidates = d.MaxProposeCandidates
	}
	if v.ProposeTimeout == 0 {
		v.ProposeTimeout = d.ProposeTimeout
	}
}

// ValidateLimits rejects unbounded or internally inconsistent policy limits.
func ValidateLimits(v Limits) error {
	if v.MaxContentBytes <= 0 || v.MaxRecordBytes < v.MaxContentBytes ||
		v.DefaultListLimit <= 0 || v.MaxListLimit < v.DefaultListLimit || v.MaxListBytes <= 0 ||
		v.DefaultRecallLimit <= 0 || v.MaxRecallLimit < v.DefaultRecallLimit ||
		v.MaxRecallCandidates < v.MaxRecallLimit || v.MaxRecallBytes <= 0 || v.MaxCandidateBytes <= 0 ||
		v.DefaultRetention <= 0 || v.MaxRetention < v.DefaultRetention ||
		v.MaxRecordsPerScope <= 0 || v.MaxContentPerScope <= 0 ||
		v.MaxReceiptsPerScope <= 0 || v.MaxControlsPerScope <= 0 || v.MaxConcurrentOps <= 0 ||
		v.CRUDTimeout <= 0 || v.RecallTimeout <= 0 || v.MaxProposeConcurrent <= 0 ||
		v.MaxProposeInputBytes <= 0 || v.MaxProposeCandidates <= 0 || v.ProposeTimeout <= 0 ||
		v.MaxProposeConcurrent > 4 || v.MaxProposeInputBytes > 32<<10 ||
		v.MaxProposeCandidates > 8 || v.ProposeTimeout > 10*time.Second {
		return fmt.Errorf("%w: all memory limits must be finite and internally consistent", ErrInvalid)
	}
	return nil
}

func (m *Module) acquire() error {
	select {
	case m.slots <- struct{}{}:
		return nil
	default:
		return ErrOverloaded
	}
}

func (m *Module) release() { <-m.slots }

func (m *Module) crudContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, m.limits.CRUDTimeout)
}

func (m *Module) authorizeScope(ctx context.Context, p Principal, scope Scope, action Action) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := p.validate(scope.Namespace); err != nil {
		return err
	}
	if err := m.authorizer.Authorize(ctx, AuthorizeRequest{Principal: p, Action: action, Scope: scope, Subject: scope.Subject()}); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

func (m *Module) authorizeSubject(ctx context.Context, p Principal, subject SubjectScope, action Action) error {
	if err := subject.Validate(); err != nil {
		return err
	}
	if err := p.validate(subject.Namespace); err != nil {
		return err
	}
	if err := m.authorizer.Authorize(ctx, AuthorizeRequest{Principal: p, Action: action, Subject: subject}); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	return nil
}

type tokenClaims struct {
	Version           uint8        `json:"v"`
	KeyID             string       `json:"kid"`
	Nonce             string       `json:"nonce"`
	Action            Action       `json:"action"`
	Scope             Scope        `json:"scope,omitempty"`
	Subject           SubjectScope `json:"subject"`
	SubjectGeneration uint64       `json:"generation"`
	PolicyGeneration  uint64       `json:"policy_generation"`
	ProposalEpoch     uint64       `json:"proposal_epoch,omitempty"`
	IssuedAtUnixNano  int64        `json:"iat"`
	ExpiresAtUnixNano int64        `json:"exp"`
}

// IssueOperationToken signs a finite token for one complete scope and action.
// Issuance performs authorization but execution always authorizes again.
func (m *Module) IssueOperationToken(ctx context.Context, p Principal, scope Scope, action Action, lifetime time.Duration) (string, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if action != ActionCreate && action != ActionCreateVerifiedFact && action != ActionRevise && action != ActionRetract && action != ActionRetractSource && action != ActionUpdatePolicy && action != ActionClaimPurge && action != ActionPropose && action != ActionConfirm && action != ActionPurge {
		return "", fmt.Errorf("%w: operation token action", ErrInvalid)
	}
	if err := m.authorizeScope(ctx, p, scope, action); err != nil {
		return "", err
	}
	if lifetime <= 0 || lifetime > maxOperationTokenLifetime {
		return "", fmt.Errorf("%w: operation token lifetime", ErrInvalid)
	}
	if err := m.acquire(); err != nil {
		return "", err
	}
	defer m.release()
	snapshot, err := m.store.Snapshot(ctx, scope, "")
	if err != nil {
		return "", err
	}
	if snapshot.SubjectDisabled && !actionAllowedWhenDisabled(action) {
		return "", ErrConflict
	}
	nonce, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("memory: generate operation nonce: %w", err)
	}
	return m.signClaims(tokenClaims{
		Version: 1, KeyID: m.currentKey, Nonce: nonce, Action: action,
		Scope: scope, Subject: scope.Subject(), SubjectGeneration: snapshot.SubjectGeneration,
		PolicyGeneration: snapshot.PolicyGeneration, ProposalEpoch: snapshot.CollectionProposalEpoch,
		IssuedAtUnixNano: snapshot.StoreNow.UnixNano(), ExpiresAtUnixNano: snapshot.StoreNow.Add(lifetime).UnixNano(),
	})
}

func actionAllowedWhenDisabled(action Action) bool {
	return action == ActionRetractSource || action == ActionEnableSubject || action == ActionUpdatePolicy || action == ActionClaimPurge || action == ActionPurge
}

func (m *Module) IssueForgetToken(ctx context.Context, p Principal, subject SubjectScope, lifetime time.Duration) (string, error) {
	return m.issueSubjectToken(ctx, p, subject, ActionForgetSubject, lifetime)
}

// IssueEnableSubjectToken authorizes and signs a subject-scoped re-enable
// operation. It deliberately cannot be issued through a collection scope.
func (m *Module) IssueEnableSubjectToken(ctx context.Context, p Principal, subject SubjectScope, lifetime time.Duration) (string, error) {
	return m.issueSubjectToken(ctx, p, subject, ActionEnableSubject, lifetime)
}

func (m *Module) issueSubjectToken(ctx context.Context, p Principal, subject SubjectScope, action Action, lifetime time.Duration) (string, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if action != ActionForgetSubject && action != ActionEnableSubject {
		return "", fmt.Errorf("%w: subject operation token action", ErrInvalid)
	}
	if err := m.authorizeSubject(ctx, p, subject, action); err != nil {
		return "", err
	}
	if lifetime <= 0 || lifetime > maxOperationTokenLifetime {
		return "", fmt.Errorf("%w: operation token lifetime", ErrInvalid)
	}
	if err := m.acquire(); err != nil {
		return "", err
	}
	defer m.release()
	snapshot, err := m.store.SnapshotSubject(ctx, subject)
	if err != nil {
		return "", err
	}
	if action == ActionEnableSubject && !snapshot.SubjectDisabled {
		return "", ErrConflict
	}
	nonce, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("memory: generate operation nonce: %w", err)
	}
	return m.signClaims(tokenClaims{
		Version: 1, KeyID: m.currentKey, Nonce: nonce, Action: action,
		Subject: subject, SubjectGeneration: snapshot.SubjectGeneration, PolicyGeneration: snapshot.PolicyGeneration,
		IssuedAtUnixNano: snapshot.StoreNow.UnixNano(), ExpiresAtUnixNano: snapshot.StoreNow.Add(lifetime).UnixNano(),
	})
}

func (m *Module) signClaims(claims tokenClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, m.keys[claims.KeyID])
	_, _ = mac.Write(payload)
	token := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(token) > maxOperationTokenBytes {
		return "", fmt.Errorf("%w: operation token exceeds protocol limit", ErrInvalid)
	}
	return token, nil
}

func (m *Module) verifyToken(token string, action Action, scope Scope, subject SubjectScope) (tokenClaims, error) {
	var zero tokenClaims
	if len(token) == 0 || len(token) > maxOperationTokenBytes {
		return zero, fmt.Errorf("%w: operation token size", ErrUnauthorized)
	}
	parts := splitToken(token)
	if len(parts) != 2 || len(parts[1]) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return zero, fmt.Errorf("%w: malformed operation token", ErrUnauthorized)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return zero, fmt.Errorf("%w: malformed operation token", ErrUnauthorized)
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return zero, fmt.Errorf("%w: malformed operation token", ErrUnauthorized)
	}
	key := m.keys[claims.KeyID]
	if len(key) == 0 {
		return zero, fmt.Errorf("%w: unknown operation token key", ErrUnauthorized)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(sig) != sha256.Size {
		return zero, fmt.Errorf("%w: malformed operation token", ErrUnauthorized)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	nonce, nonceErr := hex.DecodeString(claims.Nonce)
	if !hmac.Equal(sig, mac.Sum(nil)) || claims.Version != 1 || len(nonce) != 16 || nonceErr != nil || len(claims.KeyID) > 256 || claims.Action != action || claims.Subject != subject {
		return zero, fmt.Errorf("%w: operation token binding", ErrUnauthorized)
	}
	if action != ActionForgetSubject && action != ActionEnableSubject && claims.Scope != scope {
		return zero, fmt.Errorf("%w: operation token scope", ErrUnauthorized)
	}
	return claims, nil
}

func (m *Module) validateContentPolicy(ctx context.Context, p Principal, scope Scope, action Action, content string, topics []string) error {
	if err := ValidatePersistableContent(content, topics, m.limits); err != nil {
		return err
	}
	if m.contentPolicy != nil {
		if err := m.contentPolicy.ValidateMemoryContent(ctx, ContentPolicyRequest{Principal: p, Action: action, Scope: scope, Content: content, Topics: append([]string(nil), topics...)}); err != nil {
			return fmt.Errorf("%w: host content policy rejected memory", ErrInvalid)
		}
	}
	return nil
}

func splitToken(token string) []string {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			return []string{token[:i], token[i+1:]}
		}
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (m *Module) operation(claims tokenClaims, command any) (Operation, error) {
	b, err := json.Marshal(command)
	if err != nil {
		return Operation{}, err
	}
	key := m.keys[claims.KeyID]
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(b)
	return Operation{
		ID: claims.KeyID + ":" + claims.Nonce, KeyID: claims.KeyID, Action: claims.Action,
		Scope: claims.Scope, Subject: claims.Subject,
		SubjectGeneration: claims.SubjectGeneration,
		IssuedAt:          time.Unix(0, claims.IssuedAtUnixNano).UTC(), ExpiresAt: time.Unix(0, claims.ExpiresAtUnixNano).UTC(),
		CommandDigest: mac.Sum(nil),
	}, nil
}

type CreateRequest struct {
	Content        string
	Topics         []string
	Kind           Kind
	Retention      time.Duration
	OperationToken string
}

func (m *Module) Create(ctx context.Context, p Principal, scope Scope, req CreateRequest) (*Record, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeScope(ctx, p, scope, ActionCreate); err != nil {
		return nil, err
	}
	if req.Kind != KindUserStatement && req.Kind != KindPreference {
		return nil, fmt.Errorf("%w: ordinary create kind", ErrInvalid)
	}
	return m.create(ctx, p, scope, req, nil, ActionCreate)
}

func (m *Module) CreateVerifiedFact(ctx context.Context, p Principal, scope Scope, req CreateRequest, att SourceAttestation) (*Record, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeScope(ctx, p, scope, ActionCreateVerifiedFact); err != nil {
		return nil, err
	}
	att = CanonicalSourceAttestation(att)
	if req.Kind != "" && req.Kind != KindVerifiedFact {
		return nil, fmt.Errorf("%w: verified fact kind", ErrInvalid)
	}
	if att.Scope != scope || att.Validate() != nil {
		return nil, fmt.Errorf("%w: trusted source attestation", ErrInvalid)
	}
	req.Kind = KindVerifiedFact
	return m.create(ctx, p, scope, req, &att, ActionCreateVerifiedFact)
}

func (m *Module) create(ctx context.Context, p Principal, scope Scope, req CreateRequest, att *SourceAttestation, action Action) (*Record, error) {
	if err := m.validateContentPolicy(ctx, p, scope, action, req.Content, req.Topics); err != nil {
		return nil, err
	}
	retention, err := normalizeRetention(req.Retention, m.limits)
	if err != nil {
		return nil, err
	}
	claims, err := m.verifyToken(req.OperationToken, action, scope, scope.Subject())
	if err != nil {
		return nil, err
	}
	canonical := struct {
		Scope         Scope
		Content       string
		Topics        []string
		Kind          Kind
		Retention     int64
		SourceRef     string
		SourceVersion string
		SourceDigest  string
		SourceEpoch   uint64
		SourceExpires int64
		Verifier      string
	}{
		Scope: scope, Content: req.Content, Topics: append([]string(nil), req.Topics...), Kind: req.Kind, Retention: int64(retention),
	}
	if att != nil {
		canonical.SourceRef = att.SourceRef
		canonical.SourceVersion = att.Version
		canonical.SourceDigest = att.Digest
		canonical.SourceEpoch = att.SourceEpoch
		canonical.SourceExpires = att.ExpiresAt.UnixNano()
		canonical.Verifier = att.Verifier
	}
	if encoded, marshalErr := json.Marshal(canonical); marshalErr != nil || len(encoded) > m.limits.MaxRecordBytes {
		return nil, fmt.Errorf("%w: record exceeds %d bytes", ErrInvalid, m.limits.MaxRecordBytes)
	}
	op, err := m.operation(claims, canonical)
	if err != nil {
		return nil, err
	}
	idHash := hmac.New(sha256.New, m.keys[claims.KeyID])
	_, _ = idHash.Write([]byte("memory:" + op.ID))
	memoryID := hex.EncodeToString(idHash.Sum(nil)[:16])
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()
	result, err := m.store.CommitMutation(ctx, Mutation{
		Type: map[bool]MutationType{false: MutationCreate, true: MutationAttestCreate}[att != nil], Scope: scope, Subject: scope.Subject(), Operation: op,
		Preconditions: Preconditions{SubjectGeneration: claims.SubjectGeneration, PolicyGeneration: claims.PolicyGeneration, CollectionProposalEpoch: claims.ProposalEpoch},
		MemoryID:      memoryID, Kind: req.Kind, Content: req.Content, Topics: append([]string(nil), req.Topics...), Retention: retention, Attestation: cloneAttestation(att),
		Audit: AuditEvent{OperationID: op.ID, Scope: scope, Subject: scope.Subject(), Action: action, Actor: p.Subject, Issuer: p.Issuer, MemoryID: memoryID},
	})
	if err != nil {
		return nil, err
	}
	return cloneRecord(result.Record), nil
}

type ReviseRequest struct {
	MemoryID         string
	ExpectedRevision uint64
	Content          string
	Topics           []string
	Retention        time.Duration
	OperationToken   string
}

func (m *Module) Revise(ctx context.Context, p Principal, scope Scope, req ReviseRequest) (*Record, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeScope(ctx, p, scope, ActionRevise); err != nil {
		return nil, err
	}
	if req.MemoryID == "" || req.ExpectedRevision == 0 {
		return nil, fmt.Errorf("%w: memory id and expected revision", ErrInvalid)
	}
	if err := m.validateContentPolicy(ctx, p, scope, ActionRevise, req.Content, req.Topics); err != nil {
		return nil, err
	}
	retention, err := normalizeRetention(req.Retention, m.limits)
	if err != nil {
		return nil, err
	}
	claims, err := m.verifyToken(req.OperationToken, ActionRevise, scope, scope.Subject())
	if err != nil {
		return nil, err
	}
	canonical := struct {
		Scope     Scope
		ID        string
		Revision  uint64
		Content   string
		Topics    []string
		Retention int64
	}{scope, req.MemoryID, req.ExpectedRevision, req.Content, append([]string(nil), req.Topics...), int64(retention)}
	if encoded, marshalErr := json.Marshal(canonical); marshalErr != nil || len(encoded) > m.limits.MaxRecordBytes {
		return nil, fmt.Errorf("%w: record exceeds %d bytes", ErrInvalid, m.limits.MaxRecordBytes)
	}
	op, err := m.operation(claims, canonical)
	if err != nil {
		return nil, err
	}
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()
	result, err := m.store.CommitMutation(ctx, Mutation{Type: MutationRevise, Scope: scope, Subject: scope.Subject(), Operation: op,
		Preconditions: Preconditions{SubjectGeneration: claims.SubjectGeneration, ExpectedRevision: req.ExpectedRevision, PolicyGeneration: claims.PolicyGeneration},
		MemoryID:      req.MemoryID, Content: req.Content, Topics: append([]string(nil), req.Topics...), Retention: retention,
		Audit: AuditEvent{OperationID: op.ID, Scope: scope, Subject: scope.Subject(), Action: ActionRevise, Actor: p.Subject, Issuer: p.Issuer, MemoryID: req.MemoryID},
	})
	if err != nil {
		return nil, err
	}
	return cloneRecord(result.Record), nil
}

type RetractRequest struct {
	MemoryID         string
	ExpectedRevision uint64
	OperationToken   string
}

func (m *Module) RetractRecord(ctx context.Context, p Principal, scope Scope, req RetractRequest) (*Record, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeScope(ctx, p, scope, ActionRetract); err != nil {
		return nil, err
	}
	if req.MemoryID == "" || req.ExpectedRevision == 0 {
		return nil, fmt.Errorf("%w: memory id and expected revision", ErrInvalid)
	}
	claims, err := m.verifyToken(req.OperationToken, ActionRetract, scope, scope.Subject())
	if err != nil {
		return nil, err
	}
	canonical := struct {
		Scope    Scope
		ID       string
		Revision uint64
	}{scope, req.MemoryID, req.ExpectedRevision}
	op, err := m.operation(claims, canonical)
	if err != nil {
		return nil, err
	}
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()
	result, err := m.store.CommitMutation(ctx, Mutation{Type: MutationRetract, Scope: scope, Subject: scope.Subject(), Operation: op,
		Preconditions: Preconditions{SubjectGeneration: claims.SubjectGeneration, ExpectedRevision: req.ExpectedRevision, PolicyGeneration: claims.PolicyGeneration}, MemoryID: req.MemoryID,
		Audit: AuditEvent{OperationID: op.ID, Scope: scope, Subject: scope.Subject(), Action: ActionRetract, Actor: p.Subject, Issuer: p.Issuer, MemoryID: req.MemoryID},
	})
	if err != nil {
		return nil, err
	}
	return cloneRecord(result.Record), nil
}

func (m *Module) ForgetSubject(ctx context.Context, p Principal, subject SubjectScope, operationToken string) (uint64, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeSubject(ctx, p, subject, ActionForgetSubject); err != nil {
		return 0, err
	}
	claims, err := m.verifyToken(operationToken, ActionForgetSubject, Scope{}, subject)
	if err != nil {
		return 0, err
	}
	canonical := struct{ Subject SubjectScope }{subject}
	op, err := m.operation(claims, canonical)
	if err != nil {
		return 0, err
	}
	if err := m.acquire(); err != nil {
		return 0, err
	}
	defer m.release()
	result, err := m.store.CommitMutation(ctx, Mutation{Type: MutationForget, Subject: subject, Operation: op,
		Preconditions: Preconditions{SubjectGeneration: claims.SubjectGeneration, PolicyGeneration: claims.PolicyGeneration},
		Audit:         AuditEvent{OperationID: op.ID, Subject: subject, Action: ActionForgetSubject, Actor: p.Subject, Issuer: p.Issuer},
	})
	if err != nil {
		return 0, err
	}
	return result.SubjectGeneration, nil
}

// EnableSubject explicitly re-enables a forgotten subject after a fresh
// subject-level authorization check. It does not grant collection authority.
func (m *Module) EnableSubject(ctx context.Context, p Principal, subject SubjectScope, operationToken string) (uint64, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeSubject(ctx, p, subject, ActionEnableSubject); err != nil {
		return 0, err
	}
	claims, err := m.verifyToken(operationToken, ActionEnableSubject, Scope{}, subject)
	if err != nil {
		return 0, err
	}
	canonical := struct{ Subject SubjectScope }{subject}
	op, err := m.operation(claims, canonical)
	if err != nil {
		return 0, err
	}
	if err := m.acquire(); err != nil {
		return 0, err
	}
	defer m.release()
	result, err := m.store.CommitMutation(ctx, Mutation{
		Type:          MutationEnableSubject,
		Subject:       subject,
		Operation:     op,
		Preconditions: Preconditions{SubjectGeneration: claims.SubjectGeneration, PolicyGeneration: claims.PolicyGeneration},
		Audit:         AuditEvent{OperationID: op.ID, Subject: subject, Action: ActionEnableSubject, Actor: p.Subject, Issuer: p.Issuer},
	})
	if err != nil {
		return 0, err
	}
	return result.SubjectGeneration, nil
}

func (m *Module) Read(ctx context.Context, p Principal, scope Scope, memoryID string) (*Record, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeScope(ctx, p, scope, ActionRead); err != nil {
		return nil, err
	}
	if memoryID == "" {
		return nil, fmt.Errorf("%w: memory id", ErrInvalid)
	}
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()
	record, snapshot, err := m.store.ReadRecord(ctx, scope, memoryID)
	if err != nil {
		return nil, err
	}
	if record == nil || snapshot.SubjectDisabled || record.SubjectGeneration != snapshot.SubjectGeneration || record.Status != StatusActive || record.Expired(snapshot.StoreNow) {
		return nil, ErrNotFound
	}
	return cloneRecord(record), nil
}

func (m *Module) List(ctx context.Context, p Principal, scope Scope, limit int) (ListResult, error) {
	page, err := m.ListPage(ctx, p, scope, ListPageRequest{Limit: limit})
	return page.ListResult, err
}

func cloneRecord(r *Record) *Record {
	if r == nil {
		return nil
	}
	c := *r
	c.Topics = append([]string(nil), r.Topics...)
	return &c
}

func cloneAttestation(a *SourceAttestation) *SourceAttestation {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}
