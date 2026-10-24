package memory

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

const (
	maxProposalFragments   = 64
	maxProposalOutputBytes = 128 << 10
	maxModelOutputTokens   = 4096
	defaultModelOutputSize = 1024
)

// ProposalConfig is an explicit, trusted-host opt in to synchronous memory
// candidate extraction. It is copied by NewModule and never exposed to model
// output.
type ProposalConfig struct {
	Extractor          ProposalExtractor
	OutboundPolicy     ProposalOutboundPolicy
	Producer           string
	ModelConfigVersion string
}

type proposalRuntime struct {
	extractor          ProposalExtractor
	outboundPolicy     ProposalOutboundPolicy
	producer           string
	modelConfigVersion string
}

func newProposalRuntime(cfg *ProposalConfig) (*proposalRuntime, error) {
	if cfg == nil {
		return nil, nil
	}
	if cfg.Extractor == nil || cfg.OutboundPolicy == nil {
		return nil, fmt.Errorf("%w: proposal extractor and outbound policy are required", ErrInvalid)
	}
	if cfg.Producer == "" || cfg.ModelConfigVersion == "" || validateIdentifier(cfg.Producer) != nil || validateIdentifier(cfg.ModelConfigVersion) != nil {
		return nil, fmt.Errorf("%w: proposal producer and model config version", ErrInvalid)
	}
	return &proposalRuntime{extractor: cfg.Extractor, outboundPolicy: cfg.OutboundPolicy, producer: cfg.Producer, modelConfigVersion: cfg.ModelConfigVersion}, nil
}

// ProposalFragment is a finite host-selected excerpt. The Module never reads
// conversation history or existing memories to populate it implicitly.
type ProposalFragment struct {
	Content string `json:"content"`
}

type ProposalRequest struct {
	Fragments      []ProposalFragment
	Source         SourceAttestation
	OperationToken string
}

// CandidateDTO is the complete model-controlled result. In particular it has
// no scope, source, identity, status, revision, or executable action fields.
type CandidateDTO struct {
	Kind             Kind     `json:"kind"`
	Content          string   `json:"content"`
	Topics           []string `json:"topics,omitempty"`
	RetentionSeconds int64    `json:"retention_seconds,omitempty"`
}

type ProposalExtractionRequest struct {
	Fragments     []ProposalFragment
	SourceRef     string
	SourceVersion string
	MaxCandidates int
}

type ProposalExtractionResult struct {
	Candidates []CandidateDTO `json:"candidates"`
}

// ProposalExtractor has no storage capability. Implementations receive only
// the bounded DTO and return candidate DTOs; they are never passed Module or
// AtomicStore handles.
type ProposalExtractor interface {
	Extract(context.Context, ProposalExtractionRequest) (ProposalExtractionResult, error)
}

type ProposalOutboundRequest struct {
	Principal Principal
	Scope     Scope
	Source    SourceAttestation
	Fragments []ProposalFragment
}

// ProposalOutboundPolicy is a mandatory trusted-host decision point whenever
// proposals are enabled. It runs after scope/source/input validation and before
// the extractor sees any bytes.
type ProposalOutboundPolicy interface {
	AuthorizeProposalOutbound(context.Context, ProposalOutboundRequest) error
}

type ProposalOutboundPolicyFunc func(context.Context, ProposalOutboundRequest) error

func (f ProposalOutboundPolicyFunc) AuthorizeProposalOutbound(ctx context.Context, req ProposalOutboundRequest) error {
	return f(ctx, req)
}

type ProposalResult struct {
	Candidates []Record
	Replayed   bool
}

// Propose performs one synchronous, deadline-bound candidate extraction and
// atomically persists the complete batch. It never activates a memory.
func (m *Module) Propose(ctx context.Context, p Principal, scope Scope, req ProposalRequest) (ProposalResult, error) {
	return m.propose(ctx, p, scope, req.Source.SourceRef, req.OperationToken, func(context.Context) (ProposalRequest, error) {
		return req, nil
	})
}

// ProposeResolved performs the authorization, token, current-policy and
// fail-fast concurrency gates before invoking the trusted host resolver. The
// resolver is therefore never called for a browser request that cannot be
// committed, while the normal post-extraction snapshot and store transaction
// still close the control-state race window.
func (m *Module) ProposeResolved(ctx context.Context, p Principal, scope Scope, sourceRef, operationToken string, resolver ProposalSourceResolver) (ProposalResult, error) {
	if !ProposalSourceResolverAvailable(resolver) {
		return ProposalResult{}, ErrUnavailable
	}
	return m.propose(ctx, p, scope, sourceRef, operationToken, func(resolveCtx context.Context) (ProposalRequest, error) {
		source, fragments, err := resolver.ResolveProposalSource(resolveCtx, p, scope, sourceRef)
		if err != nil {
			if resolveCtx.Err() != nil {
				return ProposalRequest{}, resolveCtx.Err()
			}
			return ProposalRequest{}, fmt.Errorf("%w: proposal source resolution", ErrUnauthorized)
		}
		return ProposalRequest{Fragments: fragments, Source: source, OperationToken: operationToken}, nil
	})
}

func (m *Module) propose(ctx context.Context, p Principal, scope Scope, sourceRef, operationToken string, resolve func(context.Context) (ProposalRequest, error)) (ProposalResult, error) {
	if m.proposal == nil {
		return ProposalResult{}, ErrUnavailable
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, m.limits.ProposeTimeout)
	defer cancel()
	if err := m.authorizeScope(ctx, p, scope, ActionPropose); err != nil {
		return ProposalResult{}, err
	}
	if sourceRef == "" || validateIdentifier(sourceRef) != nil {
		return ProposalResult{}, fmt.Errorf("%w: proposal source reference", ErrInvalid)
	}
	claims, err := m.verifyToken(operationToken, ActionPropose, scope, scope.Subject())
	if err != nil {
		return ProposalResult{}, err
	}
	// Reserve global Module capacity before any trusted resolver or model work
	// and retain it through the commit. Otherwise unrelated CRUD could fill the
	// gate after outbound bytes have already left the process.
	if err := m.acquire(); err != nil {
		return ProposalResult{}, err
	}
	defer m.release()
	initial, err := m.store.Snapshot(ctx, scope, sourceRef)
	if err != nil {
		return ProposalResult{}, err
	}
	if err := validateProposalControlSnapshot(initial, claims); err != nil {
		return ProposalResult{}, err
	}
	remaining := initial.Policy.ProposeTimeout - time.Since(started)
	if remaining <= 0 {
		return ProposalResult{}, context.DeadlineExceeded
	}
	policyCtx, policyCancel := context.WithTimeout(ctx, remaining)
	defer policyCancel()
	ctx = policyCtx
	if err := m.acquireProposal(ctx, initial.Policy.MaxProposeConcurrent); err != nil {
		return ProposalResult{}, err
	}
	defer m.releaseProposal()
	req, err := resolve(ctx)
	if err != nil {
		return ProposalResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProposalResult{}, err
	}
	req.OperationToken = operationToken
	req.Source = CanonicalSourceAttestation(req.Source)
	if req.Source.SourceRef != sourceRef {
		return ProposalResult{}, fmt.Errorf("%w: proposal source binding", ErrUnauthorized)
	}
	if err := validateProposalInput(req, scope, m.limits); err != nil {
		return ProposalResult{}, err
	}
	if err := validateProposalSnapshot(initial, claims, req.Source); err != nil {
		return ProposalResult{}, err
	}
	if err := validateProposalInput(req, scope, initial.Policy); err != nil {
		return ProposalResult{}, err
	}
	for _, fragment := range req.Fragments {
		if m.contentPolicy != nil {
			if err := m.contentPolicy.ValidateMemoryContent(ctx, ContentPolicyRequest{Principal: p, Action: ActionPropose, Scope: scope, Content: fragment.Content}); err != nil {
				return ProposalResult{}, fmt.Errorf("%w: host content policy rejected proposal input", ErrInvalid)
			}
		}
	}
	outbound := ProposalOutboundRequest{Principal: p, Scope: scope, Source: req.Source, Fragments: cloneProposalFragments(req.Fragments)}
	if err := m.proposal.outboundPolicy.AuthorizeProposalOutbound(ctx, outbound); err != nil {
		return ProposalResult{}, fmt.Errorf("%w: proposal outbound policy rejected request", ErrUnauthorized)
	}
	extracted, extractErr := m.proposal.extractor.Extract(ctx, ProposalExtractionRequest{
		Fragments: cloneProposalFragments(req.Fragments), SourceRef: req.Source.SourceRef,
		SourceVersion: req.Source.Version, MaxCandidates: initial.Policy.MaxProposeCandidates,
	})
	if extractErr != nil {
		return ProposalResult{}, fmt.Errorf("memory: proposal extraction failed: %w", extractErr)
	}
	if err := ctx.Err(); err != nil {
		return ProposalResult{}, err
	}
	if len(extracted.Candidates) == 0 {
		return ProposalResult{}, nil
	}
	drafts, err := m.prepareCandidateDrafts(ctx, p, scope, extracted.Candidates, initial.Policy, req.Source, claims)
	if err != nil {
		return ProposalResult{}, err
	}
	current, err := m.store.Snapshot(ctx, scope, req.Source.SourceRef)
	if err != nil {
		return ProposalResult{}, err
	}
	if !sameProposalSnapshot(initial, current) {
		return ProposalResult{}, fmt.Errorf("%w: proposal control state changed", ErrConflict)
	}
	// Re-apply the current persisted policy after the model call. The store
	// repeats all fences and validation in the transaction to close this check's
	// race window.
	drafts, err = m.prepareCandidateDrafts(ctx, p, scope, extracted.Candidates, current.Policy, req.Source, claims)
	if err != nil {
		return ProposalResult{}, err
	}
	batch := CandidateBatchIdentity{SourceRef: req.Source.SourceRef, SourceVersion: req.Source.Version, Producer: m.proposal.producer, ModelConfigVersion: m.proposal.modelConfigVersion}
	canonical := struct {
		Scope      Scope
		Source     SourceAttestation
		Batch      CandidateBatchIdentity
		Candidates []CandidateDraft
	}{scope, req.Source, batch, drafts}
	op, err := m.operation(claims, canonical)
	if err != nil {
		return ProposalResult{}, err
	}
	result, err := m.store.CommitMutation(ctx, Mutation{
		Type: MutationCreateCandidateBatch, Scope: scope, Subject: scope.Subject(), Operation: op,
		Preconditions: Preconditions{SubjectGeneration: initial.SubjectGeneration, PolicyGeneration: initial.PolicyGeneration, CollectionProposalEpoch: initial.CollectionProposalEpoch, SourceEpoch: req.Source.SourceEpoch},
		Candidates:    drafts, CandidateBatch: &batch, Attestation: cloneAttestation(&req.Source),
		Audit: AuditEvent{OperationID: op.ID, Scope: scope, Subject: scope.Subject(), Action: ActionPropose, Actor: p.Subject, Issuer: m.proposal.producer},
	})
	if err != nil {
		return ProposalResult{}, err
	}
	out := ProposalResult{Candidates: make([]Record, len(result.Records)), Replayed: result.Replayed}
	for i := range result.Records {
		out.Candidates[i] = *cloneRecord(&result.Records[i])
	}
	return out, nil
}

func (m *Module) snapshot(ctx context.Context, scope Scope, sourceRef string) (ControlSnapshot, error) {
	if err := m.acquire(); err != nil {
		return ControlSnapshot{}, err
	}
	defer m.release()
	return m.store.Snapshot(ctx, scope, sourceRef)
}

func (m *Module) acquireProposal(ctx context.Context, policyMax int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.proposalSlots <- struct{}{}:
	default:
		return ErrOverloaded
	}
	for {
		current := m.proposalCalls.Load()
		if current >= int32(policyMax) {
			<-m.proposalSlots
			return ErrOverloaded
		}
		if m.proposalCalls.CompareAndSwap(current, current+1) {
			return nil
		}
	}
}

func (m *Module) releaseProposal() {
	m.proposalCalls.Add(-1)
	<-m.proposalSlots
}

func validateProposalInput(req ProposalRequest, scope Scope, limits Limits) error {
	if req.Source.Scope != scope || req.Source.Validate() != nil {
		return fmt.Errorf("%w: trusted proposal source", ErrInvalid)
	}
	if len(req.Fragments) == 0 || len(req.Fragments) > maxProposalFragments {
		return fmt.Errorf("%w: proposal fragment count", ErrInvalid)
	}
	bytes := 0
	for _, fragment := range req.Fragments {
		if fragment.Content == "" || !utf8.ValidString(fragment.Content) {
			return fmt.Errorf("%w: proposal fragment", ErrInvalid)
		}
		bytes += len(fragment.Content)
		if bytes > limits.MaxProposeInputBytes || containsKnownCredential(fragment.Content) {
			return fmt.Errorf("%w: proposal input is unsafe or exceeds %d bytes", ErrInvalid, limits.MaxProposeInputBytes)
		}
	}
	return nil
}

func validateProposalSnapshot(snapshot ControlSnapshot, claims tokenClaims, source SourceAttestation) error {
	if err := validateProposalControlSnapshot(snapshot, claims); err != nil {
		return err
	}
	if !source.ExpiresAt.After(snapshot.StoreNow) {
		return fmt.Errorf("%w: stale proposal source", ErrConflict)
	}
	if snapshot.Source != nil {
		stored := snapshot.Source
		if stored.Revoked || !stored.ExpiresAt.After(snapshot.StoreNow) || stored.Epoch != source.SourceEpoch || stored.Version != source.Version || stored.Digest != source.Digest || stored.Verifier != source.Verifier || !stored.ExpiresAt.Equal(source.ExpiresAt) {
			return fmt.Errorf("%w: proposal source is ineligible", ErrConflict)
		}
	}
	return nil
}

func validateProposalControlSnapshot(snapshot ControlSnapshot, claims tokenClaims) error {
	if err := ValidateLimits(snapshot.Policy); err != nil {
		return fmt.Errorf("%w: invalid persisted proposal policy", ErrUnavailable)
	}
	if snapshot.SubjectDisabled || claims.SubjectGeneration != snapshot.SubjectGeneration || claims.PolicyGeneration != snapshot.PolicyGeneration || claims.ProposalEpoch != snapshot.CollectionProposalEpoch {
		return fmt.Errorf("%w: stale proposal control state", ErrConflict)
	}
	if claims.IssuedAtUnixNano > snapshot.StoreNow.UnixNano() || claims.ExpiresAtUnixNano <= snapshot.StoreNow.UnixNano() {
		return ErrExpired
	}
	return nil
}

func sameProposalSnapshot(a, b ControlSnapshot) bool {
	return a.SubjectGeneration == b.SubjectGeneration && a.SubjectDisabled == b.SubjectDisabled &&
		a.PolicyGeneration == b.PolicyGeneration && a.CollectionProposalEpoch == b.CollectionProposalEpoch &&
		a.SourceEpoch == b.SourceEpoch && sameSourceControl(a.Source, b.Source) && a.Policy == b.Policy
}

func sameSourceControl(a, b *SourceControl) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Version == b.Version && a.Digest == b.Digest && a.Epoch == b.Epoch && a.ExpiresAt.Equal(b.ExpiresAt) && a.Verifier == b.Verifier && a.Revoked == b.Revoked
}

func (m *Module) prepareCandidateDrafts(ctx context.Context, p Principal, scope Scope, candidates []CandidateDTO, limits Limits, source SourceAttestation, claims tokenClaims) ([]CandidateDraft, error) {
	if len(candidates) > limits.MaxProposeCandidates || len(candidates) > m.limits.MaxProposeCandidates {
		return nil, fmt.Errorf("%w: too many proposal candidates", ErrInvalid)
	}
	drafts := make([]CandidateDraft, 0, len(candidates))
	for i, candidate := range candidates {
		if candidate.Kind != KindUserStatement && candidate.Kind != KindPreference {
			return nil, fmt.Errorf("%w: candidate kind", ErrInvalid)
		}
		if candidate.RetentionSeconds < 0 || candidate.RetentionSeconds > int64((time.Duration(1<<63-1))/time.Second) {
			return nil, fmt.Errorf("%w: candidate retention", ErrInvalid)
		}
		retention, err := normalizeRetention(time.Duration(candidate.RetentionSeconds)*time.Second, limits)
		if err != nil {
			return nil, err
		}
		if err := ValidatePersistableContent(candidate.Content, candidate.Topics, limits); err != nil {
			return nil, err
		}
		if m.contentPolicy != nil {
			if err := m.contentPolicy.ValidateMemoryContent(ctx, ContentPolicyRequest{Principal: p, Action: ActionPropose, Scope: scope, Content: candidate.Content, Topics: append([]string(nil), candidate.Topics...)}); err != nil {
				return nil, fmt.Errorf("%w: host content policy rejected proposal candidate", ErrInvalid)
			}
		}
		identity := struct {
			Scope              Scope
			SourceRef          string
			SourceVersion      string
			Producer           string
			ModelConfigVersion string
			SubjectGeneration  uint64
			PolicyGeneration   uint64
			ProposalEpoch      uint64
			Index              int
			Candidate          CandidateDTO
		}{scope, source.SourceRef, source.Version, m.proposal.producer, m.proposal.modelConfigVersion, claims.SubjectGeneration, claims.PolicyGeneration, claims.ProposalEpoch, i, candidate}
		encoded, err := json.Marshal(identity)
		if err != nil {
			return nil, err
		}
		mac := hmac.New(sha256.New, m.keys[m.currentKey])
		_, _ = mac.Write(encoded)
		drafts = append(drafts, CandidateDraft{MemoryID: hex.EncodeToString(mac.Sum(nil)[:16]), Kind: candidate.Kind, Content: candidate.Content, Topics: append([]string(nil), candidate.Topics...), Retention: retention})
	}
	return drafts, nil
}

func cloneProposalFragments(in []ProposalFragment) []ProposalFragment {
	return append([]ProposalFragment(nil), in...)
}

type ConfirmRequest struct {
	MemoryID                 string
	ExpectedRevision         uint64
	ReplacedMemoryID         string
	ExpectedReplacedRevision uint64
	SourceRef                string
	SourceEpoch              uint64
	OperationToken           string
}

// Confirm is independently authorized and performs no model call. Candidate,
// replacement, generation, source, proposal, and current-policy checks are all
// repeated by AtomicStore in one transaction.
func (m *Module) Confirm(ctx context.Context, p Principal, scope Scope, req ConfirmRequest) (*Record, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeScope(ctx, p, scope, ActionConfirm); err != nil {
		return nil, err
	}
	if req.MemoryID == "" || validateIdentifier(req.MemoryID) != nil || req.ExpectedRevision == 0 || req.SourceRef == "" || validateIdentifier(req.SourceRef) != nil || req.SourceEpoch == 0 || (req.ReplacedMemoryID == "" && req.ExpectedReplacedRevision != 0) || (req.ReplacedMemoryID != "" && (validateIdentifier(req.ReplacedMemoryID) != nil || req.ExpectedReplacedRevision == 0)) {
		return nil, fmt.Errorf("%w: confirmation CAS fields", ErrInvalid)
	}
	claims, err := m.verifyToken(req.OperationToken, ActionConfirm, scope, scope.Subject())
	if err != nil {
		return nil, err
	}
	snapshot, err := m.snapshot(ctx, scope, req.SourceRef)
	if err != nil {
		return nil, err
	}
	if snapshot.SubjectDisabled || snapshot.SubjectGeneration != claims.SubjectGeneration || snapshot.PolicyGeneration != claims.PolicyGeneration || snapshot.CollectionProposalEpoch != claims.ProposalEpoch || snapshot.SourceEpoch != req.SourceEpoch {
		return nil, fmt.Errorf("%w: stale confirmation control state", ErrConflict)
	}
	if claims.IssuedAtUnixNano > snapshot.StoreNow.UnixNano() || claims.ExpiresAtUnixNano <= snapshot.StoreNow.UnixNano() {
		return nil, ErrExpired
	}
	canonical := struct {
		Scope                    Scope
		MemoryID                 string
		ExpectedRevision         uint64
		ReplacedMemoryID         string
		ExpectedReplacedRevision uint64
		SourceRef                string
		SourceEpoch              uint64
	}{scope, req.MemoryID, req.ExpectedRevision, req.ReplacedMemoryID, req.ExpectedReplacedRevision, req.SourceRef, req.SourceEpoch}
	op, err := m.operation(claims, canonical)
	if err != nil {
		return nil, err
	}
	if err := m.acquire(); err != nil {
		return nil, err
	}
	result, err := m.store.CommitMutation(ctx, Mutation{Type: MutationConfirmCandidate, Scope: scope, Subject: scope.Subject(), Operation: op,
		Preconditions: Preconditions{SubjectGeneration: claims.SubjectGeneration, ExpectedRevision: req.ExpectedRevision, ExpectedReplacedRevision: req.ExpectedReplacedRevision, PolicyGeneration: claims.PolicyGeneration, SourceEpoch: req.SourceEpoch, CollectionProposalEpoch: claims.ProposalEpoch},
		MemoryID:      req.MemoryID, ReplacedMemoryID: req.ReplacedMemoryID,
		Audit: AuditEvent{OperationID: op.ID, Scope: scope, Subject: scope.Subject(), Action: ActionConfirm, Actor: p.Subject, Issuer: p.Issuer, MemoryID: req.MemoryID},
	})
	m.release()
	if err != nil {
		return nil, err
	}
	return cloneRecord(result.Record), nil
}

// ModelProposalExtractor adapts the existing provider-neutral Model interface
// to the candidate-only extraction seam. It always disables tools and strictly
// decodes one finite JSON object.
type ModelProposalExtractor struct {
	model           models.Model
	maxOutputTokens int
}

func NewModelProposalExtractor(m models.Model, maxOutputTokens int) (*ModelProposalExtractor, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: proposal model", ErrInvalid)
	}
	if maxOutputTokens == 0 {
		maxOutputTokens = defaultModelOutputSize
	}
	if maxOutputTokens < 1 || maxOutputTokens > maxModelOutputTokens {
		return nil, fmt.Errorf("%w: finite proposal output token limit", ErrInvalid)
	}
	return &ModelProposalExtractor{model: m, maxOutputTokens: maxOutputTokens}, nil
}

func (e *ModelProposalExtractor) Extract(ctx context.Context, req ProposalExtractionRequest) (ProposalExtractionResult, error) {
	payload, err := json.Marshal(struct {
		SourceRef     string             `json:"source_ref"`
		SourceVersion string             `json:"source_version"`
		Fragments     []ProposalFragment `json:"fragments"`
	}{req.SourceRef, req.SourceVersion, req.Fragments})
	if err != nil {
		return ProposalExtractionResult{}, err
	}
	format := &models.ResponseFormat{Type: models.ResponseFormatJSON}
	if capable, ok := e.model.(models.JSONSchemaCapable); ok && capable.SupportsNativeJSONSchema() {
		format.Type = models.ResponseFormatJSONSchema
		format.JSONSchema = proposalJSONSchema(req.MaxCandidates)
	}
	choice := models.ToolChoice{Mode: models.ToolChoiceNone}
	response, err := e.model.Response(ctx, []models.Message{
		{Role: models.RoleSystem, Content: "Extract only durable user statements or preferences. Return JSON matching the schema. Never return actions, scope, source authority, or verified facts."},
		{Role: models.RoleUser, Content: string(payload)},
	}, models.ModelOptions{MaxTokens: &e.maxOutputTokens, ResponseFormat: format, Tools: nil, ToolChoice: &choice})
	if err != nil {
		return ProposalExtractionResult{}, err
	}
	if response == nil || len(response.ToolCalls) != 0 {
		return ProposalExtractionResult{}, fmt.Errorf("%w: proposal model returned an executable action", ErrInvalid)
	}
	var raw []byte
	if response.Parsed != nil {
		raw, err = json.Marshal(response.Parsed)
	} else {
		raw = []byte(response.Content)
	}
	if err != nil {
		return ProposalExtractionResult{}, err
	}
	if len(raw) > maxProposalOutputBytes {
		return ProposalExtractionResult{}, fmt.Errorf("%w: proposal model output exceeds finite byte limit", ErrInvalid)
	}
	var result ProposalExtractionResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ProposalExtractionResult{}, fmt.Errorf("%w: proposal model schema: %v", ErrInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ProposalExtractionResult{}, fmt.Errorf("%w: proposal model trailing data", ErrInvalid)
	}
	if result.Candidates == nil || len(result.Candidates) > req.MaxCandidates {
		return ProposalExtractionResult{}, fmt.Errorf("%w: proposal model candidate count", ErrInvalid)
	}
	return result, nil
}

func proposalJSONSchema(maxCandidates int) map[string]any {
	return map[string]any{
		"name": "memory_candidates", "strict": true,
		"schema": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"candidates"},
			"properties": map[string]any{"candidates": map[string]any{
				"type": "array", "maxItems": maxCandidates,
				"items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "content", "topics", "retention_seconds"}, "properties": map[string]any{
					"kind": map[string]any{"type": "string", "enum": []string{string(KindUserStatement), string(KindPreference)}}, "content": map[string]any{"type": "string"}, "topics": map[string]any{"type": "array", "maxItems": 32, "items": map[string]any{"type": "string"}}, "retention_seconds": map[string]any{"type": "integer", "minimum": 0},
				}},
			}},
		},
	}
}
