package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits are the finite safety and resource bounds shared by every adapter.
// A zero field is replaced by the corresponding default; zero never means
// unlimited.
type Limits struct {
	MaxContentBytes      int
	MaxRecordBytes       int
	DefaultListLimit     int
	MaxListLimit         int
	MaxListBytes         int
	DefaultRecallLimit   int
	MaxRecallLimit       int
	MaxRecallCandidates  int
	MaxRecallBytes       int
	MaxCandidateBytes    int
	DefaultRetention     time.Duration
	MaxRetention         time.Duration
	MaxRecordsPerScope   int
	MaxContentPerScope   int
	MaxReceiptsPerScope  int
	MaxControlsPerScope  int
	MaxConcurrentOps     int
	CRUDTimeout          time.Duration
	RecallTimeout        time.Duration
	MaxProposeConcurrent int
	MaxProposeInputBytes int
	MaxProposeCandidates int
	ProposeTimeout       time.Duration
}

// DefaultLimits is the frozen initial policy from docs/memory/design.md.
func DefaultLimits() Limits {
	return Limits{
		MaxContentBytes:      4 << 10,
		MaxRecordBytes:       8 << 10,
		DefaultListLimit:     20,
		MaxListLimit:         100,
		MaxListBytes:         1 << 20,
		DefaultRecallLimit:   10,
		MaxRecallLimit:       50,
		MaxRecallCandidates:  200,
		MaxRecallBytes:       16 << 10,
		MaxCandidateBytes:    1600 << 10,
		DefaultRetention:     30 * 24 * time.Hour,
		MaxRetention:         365 * 24 * time.Hour,
		MaxRecordsPerScope:   1000,
		MaxContentPerScope:   4 << 20,
		MaxReceiptsPerScope:  10000,
		MaxControlsPerScope:  10000,
		MaxConcurrentOps:     32,
		CRUDTimeout:          time.Second,
		RecallTimeout:        100 * time.Millisecond,
		MaxProposeConcurrent: 4,
		MaxProposeInputBytes: 32 << 10,
		MaxProposeCandidates: 8,
		ProposeTimeout:       10 * time.Second,
	}
}

var (
	ErrInvalid       = errors.New("memory: invalid request")
	ErrUnauthorized  = errors.New("memory: unauthorized")
	ErrNotFound      = errors.New("memory: not found")
	ErrConflict      = errors.New("memory: conflict")
	ErrExpired       = errors.New("memory: expired")
	ErrQuota         = errors.New("memory: quota exceeded")
	ErrOverloaded    = errors.New("memory: overloaded")
	ErrUnavailable   = errors.New("memory: unavailable")
	ErrAudit         = errors.New("memory: audit persistence failed")
	ErrCommitUnknown = errors.New("memory: commit outcome unknown")
)

// Scope is the complete private-memory authority and storage key. Components
// are kept as independent fields: adapters must not concatenate them with an
// ambiguous delimiter.
type Scope struct {
	Namespace    string `json:"namespace"`
	ProjectID    string `json:"project_id"`
	SubjectID    string `json:"subject_id"`
	CollectionID string `json:"collection_id"`
}

func (s Scope) Validate() error {
	if s.Namespace == "" || s.ProjectID == "" || s.SubjectID == "" || s.CollectionID == "" {
		return fmt.Errorf("%w: complete scope is required", ErrInvalid)
	}
	for _, value := range []string{s.Namespace, s.ProjectID, s.SubjectID, s.CollectionID} {
		if err := validateIdentifier(value); err != nil {
			return err
		}
	}
	return nil
}

// SubjectScope identifies the generation guard shared by every collection of
// one subject.
type SubjectScope struct {
	Namespace string `json:"namespace"`
	ProjectID string `json:"project_id"`
	SubjectID string `json:"subject_id"`
}

func (s SubjectScope) Validate() error {
	if s.Namespace == "" || s.ProjectID == "" || s.SubjectID == "" {
		return fmt.Errorf("%w: complete subject scope is required", ErrInvalid)
	}
	for _, value := range []string{s.Namespace, s.ProjectID, s.SubjectID} {
		if err := validateIdentifier(value); err != nil {
			return err
		}
	}
	return nil
}

func (s Scope) Subject() SubjectScope {
	return SubjectScope{Namespace: s.Namespace, ProjectID: s.ProjectID, SubjectID: s.SubjectID}
}

// Principal is installed by a trusted host authentication layer. Resource
// ownership remains in Scope; Principal fields are never populated from a
// content DTO.
type Principal struct {
	Subject    string
	Namespace  string
	Issuer     string
	AuthzEpoch uint64
}

func (p Principal) validate(namespace string) error {
	if p.Subject == "" || p.Namespace == "" || p.Issuer == "" {
		return fmt.Errorf("%w: trusted principal is required", ErrUnauthorized)
	}
	for _, value := range []string{p.Subject, p.Namespace, p.Issuer} {
		if err := validateIdentifier(value); err != nil {
			return fmt.Errorf("%w: invalid trusted principal", ErrUnauthorized)
		}
	}
	if p.Namespace != namespace {
		return ErrUnauthorized
	}
	return nil
}

type Action string

const (
	ActionRead               Action = "read"
	ActionList               Action = "list"
	ActionRecall             Action = "recall"
	ActionCreate             Action = "create"
	ActionCreateVerifiedFact Action = "create_verified_fact"
	ActionRevise             Action = "revise"
	ActionRetract            Action = "retract"
	ActionRetractSource      Action = "retract_source"
	ActionForgetSubject      Action = "forget_subject"
	ActionEnableSubject      Action = "enable_subject"
	ActionUpdatePolicy       Action = "update_policy"
	ActionClaimPurge         Action = "claim_purge"
	ActionPurge              Action = "purge"
	ActionPropose            Action = "propose"
	ActionConfirm            Action = "confirm"
)

type AuthorizeRequest struct {
	Principal Principal
	Action    Action
	Scope     Scope
	Subject   SubjectScope
}

type Authorizer interface {
	Authorize(context.Context, AuthorizeRequest) error
}

type AuthorizerFunc func(context.Context, AuthorizeRequest) error

func (f AuthorizerFunc) Authorize(ctx context.Context, req AuthorizeRequest) error {
	return f(ctx, req)
}

type ContentPolicyRequest struct {
	Principal Principal
	Action    Action
	Scope     Scope
	Content   string
	Topics    []string
}

// ContentPolicy lets a trusted host reject application-specific sensitive
// data before storage or model processing. The built-in credential checks
// always run even when this optional policy is nil.
type ContentPolicy interface {
	ValidateMemoryContent(context.Context, ContentPolicyRequest) error
}

type ContentPolicyFunc func(context.Context, ContentPolicyRequest) error

func (f ContentPolicyFunc) ValidateMemoryContent(ctx context.Context, req ContentPolicyRequest) error {
	return f(ctx, req)
}

type Kind string

const (
	KindUserStatement Kind = "user_statement"
	KindPreference    Kind = "preference"
	KindVerifiedFact  Kind = "verified_fact"
)

type Status string

const (
	StatusCandidate Status = "candidate"
	StatusActive    Status = "active"
	StatusRetracted Status = "retracted"
)

type Provenance struct {
	SourceRef     string
	SourceVersion string
	SourceEpoch   uint64
	Producer      string
	PolicyVersion string
}

type Decision struct {
	Actor     string
	Basis     string
	DecidedAt time.Time
}

// Record is a detached value. Adapters and Module implementations must clone
// slices before storage and return so caller mutation cannot alter truth.
type Record struct {
	Scope                   Scope
	MemoryID                string
	Revision                uint64
	SubjectGeneration       uint64
	PolicyGeneration        uint64
	CollectionProposalEpoch uint64
	Kind                    Kind
	Content                 string
	Topics                  []string
	Status                  Status
	Provenance              Provenance
	Decision                Decision
	CreatedAt               time.Time
	UpdatedAt               time.Time
	ExpiresAt               time.Time
}

func (r Record) Expired(now time.Time) bool {
	return !r.ExpiresAt.After(now)
}

type SourceAttestation struct {
	Scope       Scope
	SourceRef   string
	Version     string
	Digest      string
	SourceEpoch uint64
	ExpiresAt   time.Time
	Verifier    string
}

// CanonicalSourceAttestation applies the finest timestamp precision shared by
// every supported adapter. PostgreSQL timestamps preserve microseconds, so
// source equality and command signing canonicalize before persistence.
func CanonicalSourceAttestation(a SourceAttestation) SourceAttestation {
	a.ExpiresAt = a.ExpiresAt.UTC().Truncate(time.Microsecond)
	return a
}

func (a SourceAttestation) Validate() error {
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if a.SourceEpoch == 0 || a.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: source epoch and expiry are required", ErrInvalid)
	}
	for _, value := range []string{a.SourceRef, a.Version, a.Digest, a.Verifier} {
		if value == "" {
			return fmt.Errorf("%w: complete source attestation", ErrInvalid)
		}
		if err := validateIdentifier(value); err != nil {
			return err
		}
	}
	return nil
}

type ControlSnapshot struct {
	SubjectGeneration       uint64
	SubjectDisabled         bool
	PolicyGeneration        uint64
	CollectionProposalEpoch uint64
	// Source is nil only when Snapshot was asked about a source that does not
	// yet exist. It contains no memory content.
	Source *SourceControl
	// SourceEpoch is retained for compatibility with existing consumers. New
	// source eligibility decisions must use the complete Source value.
	SourceEpoch uint64
	StoreNow    time.Time
	Policy      Limits
}

// SourceControl is the authoritative, non-content source state captured by
// the store in the same snapshot as generation and policy controls.
type SourceControl struct {
	Version   string
	Digest    string
	Epoch     uint64
	ExpiresAt time.Time
	Verifier  string
	Revoked   bool
}

type Operation struct {
	ID                string
	KeyID             string
	Action            Action
	Scope             Scope
	Subject           SubjectScope
	SubjectGeneration uint64
	IssuedAt          time.Time
	ExpiresAt         time.Time
	CommandDigest     []byte
}

type MutationType string

const (
	MutationCreate               MutationType = "create"
	MutationRevise               MutationType = "revise"
	MutationRetract              MutationType = "retract"
	MutationForget               MutationType = "forget"
	MutationPurge                MutationType = "purge"
	MutationAttestCreate         MutationType = "attest_create"
	MutationCreateCandidateBatch MutationType = "create_candidate_batch"
	MutationConfirmCandidate     MutationType = "confirm_candidate"
	MutationRetractSource        MutationType = "retract_source"
	MutationUpdatePolicy         MutationType = "update_policy"
	MutationEnableSubject        MutationType = "enable_subject"
	MutationClaimPurgeLease      MutationType = "claim_purge_lease"
)

type CandidateDraft struct {
	MemoryID  string
	Kind      Kind
	Content   string
	Topics    []string
	Retention time.Duration
}

// CandidateBatchIdentity is the structured, scope-bound identity used to
// suppress the same extraction batch even when a caller changes operation
// tokens. Generations and the collection proposal epoch are added by the
// adapter from Mutation.Preconditions.
type CandidateBatchIdentity struct {
	SourceRef          string
	SourceVersion      string
	Producer           string
	ModelConfigVersion string
}

func (b CandidateBatchIdentity) Validate() error {
	for _, value := range []string{b.SourceRef, b.SourceVersion, b.Producer, b.ModelConfigVersion} {
		if value == "" {
			return fmt.Errorf("%w: complete candidate batch identity", ErrInvalid)
		}
		if err := validateIdentifier(value); err != nil {
			return err
		}
	}
	return nil
}

type PolicyUpdate struct {
	ExpectedGeneration uint64
	Limits             Limits
}

type PurgeLease struct {
	TaskID                string
	OwnerToken            string
	LeaseEpoch            uint64
	LeaseExpiresAt        time.Time
	SelectionCutoff       time.Time
	PurgeBeforeGeneration uint64
	Cursor                string
}

type PurgeTarget struct {
	MemoryID         string
	ExpectedRevision uint64
	SelectedBefore   time.Time
}

// Preconditions are checked by the adapter inside the same transaction as
// every mutation effect. Zero only means not applicable where documented by
// the mutation type, never wildcard scope.
type Preconditions struct {
	SubjectGeneration        uint64
	ExpectedRevision         uint64
	ExpectedReplacedRevision uint64
	PolicyGeneration         uint64
	SourceEpoch              uint64
	CollectionProposalEpoch  uint64
}

// Mutation is the single command-level atomic storage seam. An adapter must
// update record/control state, quota, receipt, and audit together. It must not
// call external authorization, models, or networks while its transaction is
// open.
type Mutation struct {
	Type             MutationType
	Scope            Scope
	Subject          SubjectScope
	Operation        Operation
	Preconditions    Preconditions
	MemoryID         string
	ReplacedMemoryID string
	Kind             Kind
	Content          string
	Topics           []string
	Retention        time.Duration
	Attestation      *SourceAttestation
	Candidates       []CandidateDraft
	CandidateBatch   *CandidateBatchIdentity
	SourceRef        string
	PolicyUpdate     *PolicyUpdate
	PurgeLease       PurgeLease
	LeaseDuration    time.Duration
	PurgeTargets     []PurgeTarget
	PurgeCursor      string
	Audit            AuditEvent
}

type MutationResult struct {
	Record            *Record
	Records           []Record
	SubjectGeneration uint64
	PolicyGeneration  uint64
	StoreNow          time.Time
	Replayed          bool
	PurgeLease        PurgeLease
	PurgedRecords     int
	ReleasedBytes     int
}

type AuditEvent struct {
	OperationID string
	Scope       Scope
	Subject     SubjectScope
	Action      Action
	Actor       string
	Issuer      string
	MemoryID    string
	Revision    uint64
	Generation  uint64
	OccurredAt  time.Time
}

type ListRequest struct {
	Scope          Scope
	Limit          int
	Kinds          []Kind
	Topics         []string
	AfterScope     Scope
	AfterUpdatedAt time.Time
	AfterMemoryID  string
	CandidateLimit int
	CandidateBytes int
}

type ListResult struct {
	Records    []Record
	StoreNow   time.Time
	Generation uint64
	Truncated  bool
}

// RecallRequest describes one finite relevance query. An empty query is
// allowed and produces deterministic recency ranking. Limit zero selects the
// configured default; it never means unlimited.
type RecallRequest struct {
	Query  string
	Limit  int
	Kinds  []Kind
	Topics []string
}

// RecallItem contains only model-consumable memory data and the metadata
// required to explain which authoritative version was used. Audit decisions
// and other storage-internal fields are deliberately excluded.
type RecallItem struct {
	MemoryID                string    `json:"memory_id"`
	Revision                uint64    `json:"revision"`
	SubjectGeneration       uint64    `json:"subject_generation"`
	PolicyGeneration        uint64    `json:"policy_generation"`
	CollectionProposalEpoch uint64    `json:"collection_proposal_epoch"`
	Kind                    Kind      `json:"kind"`
	Content                 string    `json:"content"`
	Topics                  []string  `json:"topics,omitempty"`
	SourceRef               string    `json:"source_ref,omitempty"`
	SourceVersion           string    `json:"source_version,omitempty"`
	SourceEpoch             uint64    `json:"source_epoch,omitempty"`
	ExpiresAt               time.Time `json:"expires_at"`
}

// RecallResult reports both the selected result and the bounded-window
// limitations. CandidateTruncated means older records may not have entered the
// finite candidate window; OutputTruncated means eligible candidates were
// omitted by TopK or the output-byte budget.
type RecallResult struct {
	Items              []RecallItem `json:"items"`
	StoreNow           time.Time    `json:"store_now"`
	Generation         uint64       `json:"generation"`
	EarliestExpiresAt  time.Time    `json:"earliest_expires_at,omitempty"`
	CandidateCount     int          `json:"candidate_count"`
	CandidateTruncated bool         `json:"candidate_truncated"`
	OutputTruncated    bool         `json:"output_truncated"`
	OutputBytes        int          `json:"output_bytes"`
}

// AtomicStore is separate from the legacy db.MemoryStore. All modification
// effects cross one command-level method so SQL adapters can own their actual
// transaction and lock ordering.
type AtomicStore interface {
	SnapshotSubject(context.Context, SubjectScope) (ControlSnapshot, error)
	Snapshot(context.Context, Scope, string) (ControlSnapshot, error)
	ReadRecord(context.Context, Scope, string) (*Record, ControlSnapshot, error)
	ListRecords(context.Context, ListRequest) (ListResult, error)
	CommitMutation(context.Context, Mutation) (MutationResult, error)
}

func normalizeRetention(v time.Duration, limits Limits) (time.Duration, error) {
	if v == 0 {
		return limits.DefaultRetention, nil
	}
	if v < 0 || v > limits.MaxRetention {
		return 0, fmt.Errorf("%w: retention is outside the finite policy", ErrInvalid)
	}
	return v, nil
}

func validateContent(content string, topics []string, limits Limits) error {
	if content == "" || !utf8.ValidString(content) || len(content) > limits.MaxContentBytes {
		return fmt.Errorf("%w: content must be valid UTF-8 within %d bytes", ErrInvalid, limits.MaxContentBytes)
	}
	if len(topics) > 32 {
		return fmt.Errorf("%w: too many topics", ErrInvalid)
	}
	for _, topic := range topics {
		if topic == "" || !utf8.ValidString(topic) || len(topic) > 128 {
			return fmt.Errorf("%w: invalid topic", ErrInvalid)
		}
	}
	return nil
}

func validateIdentifier(value string) error {
	if !utf8.ValidString(value) || len(value) > 256 {
		return fmt.Errorf("%w: invalid scope component", ErrInvalid)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: invalid scope component", ErrInvalid)
		}
	}
	return nil
}

// ValidateScopeComponent applies the exact finite identifier contract used by
// Scope and SubjectScope. Trusted hosts use it before freezing project-side
// scope components into an assembled runtime.
func ValidateScopeComponent(value string) error {
	if value == "" {
		return fmt.Errorf("%w: empty scope component", ErrInvalid)
	}
	return validateIdentifier(value)
}

// ValidatePersistableContent applies the adapter-independent validation that
// every write path, including internal candidate batches, must enforce.
func ValidatePersistableContent(content string, topics []string, limits Limits) error {
	if err := validateContent(content, topics, limits); err != nil {
		return err
	}
	if containsKnownCredential(content) {
		return fmt.Errorf("%w: credentials cannot be persisted as memory", ErrInvalid)
	}
	for _, topic := range topics {
		if containsKnownCredential(topic) {
			return fmt.Errorf("%w: credentials cannot be persisted as memory", ErrInvalid)
		}
	}
	return nil
}

func containsKnownCredential(value string) bool {
	upper := strings.ToUpper(value)
	for offset := 0; offset < len(upper); {
		at := strings.Index(upper[offset:], "-----BEGIN ")
		if at < 0 {
			break
		}
		begin := offset + at
		end := begin + 96
		if end > len(upper) {
			end = len(upper)
		}
		if strings.Contains(upper[begin:end], "PRIVATE KEY-----") {
			return true
		}
		offset = begin + len("-----BEGIN ")
	}
	for _, marker := range []string{"AUTHORIZATION: BEARER ", "API_KEY=", "API_KEY:", "ACCESS_TOKEN=", "ACCESS_TOKEN:", "REFRESH_TOKEN=", "REFRESH_TOKEN:", "CLIENT_SECRET=", "CLIENT_SECRET:"} {
		if hasAssignedCredential(upper, marker) {
			return true
		}
	}
	for _, prefix := range []string{"GHP_", "GITHUB_PAT_", "SK-PROJ-", "XOXB-", "XOXA-", "XOXP-", "AKIA"} {
		if hasCredentialPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func hasAssignedCredential(value, marker string) bool {
	for offset := 0; offset < len(value); {
		at := strings.Index(value[offset:], marker)
		if at < 0 {
			return false
		}
		start := offset + at + len(marker)
		for start < len(value) && (value[start] == ' ' || value[start] == '\t' || value[start] == '\'' || value[start] == '"') {
			start++
		}
		end := start
		for end < len(value) && value[end] > ' ' && value[end] != '\'' && value[end] != '"' && value[end] != ',' && value[end] != ';' {
			end++
		}
		if end-start >= 8 {
			return true
		}
		offset = start
	}
	return false
}

func hasCredentialPrefix(value, prefix string) bool {
	for offset := 0; offset < len(value); {
		at := strings.Index(value[offset:], prefix)
		if at < 0 {
			return false
		}
		start := offset + at
		end := start + len(prefix)
		for end < len(value) && ((value[end] >= 'A' && value[end] <= 'Z') || (value[end] >= '0' && value[end] <= '9') || value[end] == '_' || value[end] == '-') {
			end++
		}
		if end-start >= len(prefix)+12 {
			return true
		}
		offset = start + len(prefix)
	}
	return false
}
