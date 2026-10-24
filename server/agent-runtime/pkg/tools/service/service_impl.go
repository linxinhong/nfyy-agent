package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
)

// toolDurationHist records tool execution latency (milliseconds). It is a
// no-op unless the host installs a MeterProvider; attributes are allowlisted.
var toolDurationHist, _ = observability.Meter("xingding.tools").Float64Histogram(
	"tool.execute.duration",
	metric.WithUnit("ms"),
)

// planTTL is the default ExecutionPlan validity window.
const planTTL = 5 * time.Minute

// Service is the concrete ToolService implementation: it owns the frozen
// registry, the namespace-partitioned PlanStore, and the authorizer, and it
// is the single path from descriptor lookup to executor invocation.
type Service struct {
	registry   *catalog.Registry
	plans      PlanStore
	authorizer ToolAuthorizer
	approvals  *ApprovalService
	clock      Clock
	limits     ResultLimits

	// journal/exporter wire the transactional audit ledger (RC-10). When
	// set, every execution records a requested event before the executor
	// runs and an outcome after, then drains the outbox.
	journal  run.ExecutionJournalStore
	exporter run.AuditExporter
	workerID string
}

// New creates a Service. A nil authorizer fails closed (DenyAllAuthorizer).
// The registry must already be frozen.
func New(registry *catalog.Registry, plans PlanStore, authorizer ToolAuthorizer) *Service {
	if authorizer == nil {
		authorizer = DenyAllAuthorizer{}
	}
	return &Service{
		registry:   registry,
		plans:      plans,
		authorizer: authorizer,
		clock:      Now,
		limits:     ResultLimits{}.withDefaults(),
	}
}

// SetClock injects a clock for expiry tests.
func (s *Service) SetClock(clock Clock) *Service {
	if clock != nil {
		s.clock = clock
	}
	return s
}

// SetApprovalService wires the approval flow (RC-06). Without it,
// approval-required plans refuse to execute.
func (s *Service) SetApprovalService(approvals *ApprovalService) *Service {
	s.approvals = approvals
	return s
}

// SetResultLimits overrides result size/depth limits.
func (s *Service) SetResultLimits(limits ResultLimits) *Service {
	s.limits = limits.withDefaults()
	return s
}

// SetJournal wires the transactional audit ledger and exporter (RC-10).
// When set, every execution records a requested event before the executor
// runs and an outcome after, then drains the outbox through the exporter.
func (s *Service) SetJournal(journal run.ExecutionJournalStore, exporter run.AuditExporter, workerID string) *Service {
	s.journal = journal
	s.exporter = exporter
	s.workerID = workerID
	return s
}

// List returns the descriptors visible to the principal's discovery
// authorizer, in stable name order.
func (s *Service) List(ctx context.Context, query ToolQuery) (ToolPage, error) {
	principal, err := PrincipalFromContext(ctx)
	if err != nil {
		return ToolPage{}, err
	}
	if query.Namespace != "" && query.Namespace != principal.Namespace {
		return ToolPage{}, ErrForbidden
	}
	snap := s.registry.Snapshot()
	names := make([]string, 0, len(snap.Descriptors))
	for name := range snap.Descriptors {
		names = append(names, name)
	}
	sort.Strings(names)

	var visible []catalog.ToolDescriptor
	for _, name := range names {
		desc := snap.Descriptors[name]
		decision, err := s.authorizer.AuthorizeDiscovery(ctx, principal, desc)
		if err != nil {
			return ToolPage{}, err
		}
		if decision.Allowed {
			visible = append(visible, desc)
		}
	}
	return ToolPage{Tools: visible}, nil
}

// Describe returns one descriptor after a discovery authorization.
func (s *Service) Describe(ctx context.Context, name string) (catalog.ToolDescriptor, error) {
	principal, err := PrincipalFromContext(ctx)
	if err != nil {
		return catalog.ToolDescriptor{}, err
	}
	desc, _, ok := s.registry.Get(name)
	if !ok {
		return catalog.ToolDescriptor{}, ErrUnknownTool
	}
	decision, err := s.authorizer.AuthorizeDiscovery(ctx, principal, desc)
	if err != nil {
		return catalog.ToolDescriptor{}, err
	}
	if !decision.Allowed {
		return catalog.ToolDescriptor{}, ErrForbidden
	}
	return desc, nil
}

// Prepare freezes one invocation: canonicalize+hash arguments, authorize,
// persist the plan under the trusted principal's namespace, and return an
// opaque ref.
func (s *Service) Prepare(ctx context.Context, req PrepareRequest) (PreparedExecution, error) {
	principal, err := PrincipalFromContext(ctx)
	if err != nil {
		return PreparedExecution{}, err
	}
	desc, _, ok := s.registry.Get(req.ToolName)
	if !ok {
		return PreparedExecution{}, ErrUnknownTool
	}
	arguments, hash, err := canonicalArguments(req.Arguments)
	if err != nil {
		return PreparedExecution{}, err
	}
	decision, err := s.authorizer.Authorize(ctx, AuthorizeRequest{
		Principal:      principal,
		Descriptor:     desc,
		ArgumentsHash:  hash,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return PreparedExecution{}, err
	}
	if !decision.Allowed {
		return PreparedExecution{}, ErrForbidden
	}

	now := s.clock()
	planID := newPlanID()
	plan := &ExecutionPlan{
		ID:                          planID,
		Version:                     "v1",
		ToolName:                    req.ToolName,
		Arguments:                   arguments,
		ArgumentsHash:               hash,
		RegistryRevision:            desc.RegistryRevision,
		DescriptorPolicyRevision:    desc.DescriptorPolicyRevision,
		AuthorizationPolicyRevision: decision.AuthorizationPolicyRevision,
		AuthorizationHash:           hash,
		IdentityBindingHash:         identityHash(principal),
		Principal:                   principal,
		IdempotencyKey:              req.IdempotencyKey,
		Approval: ApprovalSpec{
			Required:     decision.ApprovalRequired,
			ApprovalKind: decision.ApprovalKind,
		},
		PreparedAt: now,
		ExpiresAt:  now.Add(planTTL),
	}
	if err := s.plans.Create(ctx, plan); err != nil {
		return PreparedExecution{}, err
	}
	return PreparedExecution{
		Ref:              PlanRef{ID: planID, Version: plan.Version},
		ToolName:         req.ToolName,
		ApprovalRequired: decision.ApprovalRequired,
		Display:          fmt.Sprintf("%s(%s)", req.ToolName, shortHash(hash)),
	}, nil
}

// Execute runs a prepared plan after re-validating identity, namespace,
// expiry, revision freshness, and a fresh authorization. The executor is
// invoked only after the final authorization succeeds.
func (s *Service) Execute(ctx context.Context, ref PlanRef) (ToolResult, error) {
	principal, err := PrincipalFromContext(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	plan, err := s.plans.Get(ctx, execution.NamespaceScope{Namespace: principal.Namespace}, ref.ID)
	if err != nil {
		return ToolResult{}, err
	}
	if plan.Version != ref.Version {
		return ToolResult{}, ErrPlanStale
	}
	if plan.Principal.Namespace != principal.Namespace {
		return ToolResult{}, ErrPlanNamespaceMismatch
	}
	// The namespace partitions storage, but it is not the complete execution
	// identity. Bind the authority-bearing plan to the full trusted principal
	// captured at Prepare time (subject, namespace, issuer, and authz epoch).
	// Checking both hashes also detects a plan whose stored Principal was
	// changed independently of its binding. Plans predating this binding have
	// an empty hash and fail closed before authorization or side effects.
	planIdentityHash := identityHash(plan.Principal)
	if plan.IdentityBindingHash == "" ||
		plan.IdentityBindingHash != planIdentityHash ||
		plan.IdentityBindingHash != identityHash(principal) {
		return ToolResult{}, ErrPlanIdentityMismatch
	}
	if !s.clock().Before(plan.ExpiresAt) {
		return ToolResult{}, ErrPlanExpired
	}
	desc, executor, ok := s.registry.Get(plan.ToolName)
	if !ok ||
		desc.RegistryRevision != plan.RegistryRevision ||
		desc.DescriptorPolicyRevision != plan.DescriptorPolicyRevision {
		return ToolResult{}, ErrPlanStale
	}
	// Fresh authorization: AuthzEpoch/catalog/policy changes invalidate the
	// old decision here.
	decision, err := s.authorizer.Authorize(ctx, AuthorizeRequest{
		Principal:        principal,
		Descriptor:       desc,
		ArgumentsHash:    plan.ArgumentsHash,
		ExecutionContext: execution.ExecutionContext{RequestID: plan.Context.RequestID, TaskID: plan.Context.TaskID, WorkflowRunID: plan.Context.WorkflowRunID},
		IdempotencyKey:   plan.IdempotencyKey,
	})
	if err != nil {
		return ToolResult{}, err
	}
	if !decision.Allowed {
		return ToolResult{}, ErrForbidden
	}
	// Empty revisions retain the explicitly unversioned compatibility mode
	// only when both Prepare and Execute decisions are empty. A unilateral
	// empty value or any other change requires a new plan and approval.
	if decision.AuthorizationPolicyRevision != plan.AuthorizationPolicyRevision {
		return ToolResult{}, ErrPlanAuthorizationStale
	}
	// Approval is part of the fresh authorization posture. Validate it before
	// consulting a grant so an old grant cannot authorize a changed kind, and
	// an old no-approval plan cannot be upgraded in place.
	if decision.ApprovalRequired != plan.Approval.Required ||
		decision.ApprovalKind != plan.Approval.ApprovalKind {
		return ToolResult{}, ErrPlanApprovalStale
	}
	if decision.ApprovalRequired {
		if s.approvals == nil {
			return ToolResult{}, ErrApprovalRequired
		}
		if _, err := s.approvals.GrantFor(ctx, execution.NamespaceScope{Namespace: principal.Namespace}, plan); err != nil {
			return ToolResult{}, err
		}
	}
	executionTimeout, resultLimit, err := effectiveExecutionLimits(s.limits, decision)
	if err != nil {
		return ToolResult{}, err
	}

	started := s.clock()
	result := ToolResult{
		ExecutionID: newExecutionID(plan.ID),
		Status:      "invoking",
		StartedAt:   started,
	}

	// Governed execution boundary: one span per tool invocation, with only
	// allowlisted attributes (hashes and revisions, never arguments/results).
	ctx, span := observability.Tracer("xingding.tools").Start(ctx, "tool.execute")
	span.SetAttributes(observability.SafeAttrs(
		attribute.String(observability.FieldToolName, plan.ToolName),
		attribute.String("namespace", principal.Namespace),
		attribute.String("arguments_hash", plan.ArgumentsHash),
		attribute.String("registry_revision", plan.RegistryRevision),
	)...)
	outcome := "unknown"
	defer func() {
		durationMs := s.clock().Sub(started).Milliseconds()
		span.SetAttributes(observability.SafeAttrs(
			attribute.String(observability.FieldOutcome, outcome),
			attribute.Int64(observability.FieldDurationMs, durationMs),
		)...)
		span.End()
		toolDurationHist.Record(ctx, float64(durationMs), metric.WithAttributes(observability.SafeAttrs(
			attribute.String(observability.FieldToolName, plan.ToolName),
			attribute.String(observability.FieldOutcome, outcome),
		)...))
	}()

	var args map[string]any
	if err := json.Unmarshal(plan.Arguments, &args); err != nil {
		outcome = "decode_error"
		return ToolResult{}, fmt.Errorf("service: decode plan arguments: %w", err)
	}

	// Audit intent (RC-10): requested is durably recorded before the side
	// effect. A journal failure fails the execution closed before the
	// executor runs. Outcome is appended by a deferred recorder so a crash
	// after invocation still lands an "unknown" outcome.
	scope := execution.NamespaceScope{Namespace: principal.Namespace}
	attemptID := result.ExecutionID
	if s.journal != nil {
		if err := s.recordRequested(ctx, scope, principal, plan, attemptID, started); err != nil {
			outcome = "audit_error"
			return ToolResult{}, fmt.Errorf("service: audit requested: %w", err)
		}
		defer func() {
			state := run.AttemptSucceeded
			switch outcome {
			case "succeeded":
				state = run.AttemptSucceeded
			case "unknown":
				state = run.AttemptUnknown
			default:
				state = run.AttemptFailed
			}
			_ = s.recordOutcome(ctx, scope, attemptID, state, outcome)
		}()
	}

	callCtx := ctx
	cancel := func() {}
	if executionTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, executionTimeout)
	}
	defer cancel()
	if contextErr := callCtx.Err(); contextErr != nil {
		outcome, result = contextFailure(result, contextErr, s.clock())
		return result, nil
	}

	value, callErr := executor.Call(callCtx, args)
	finished := s.clock()
	result.FinishedAt = finished
	if contextErr := callCtx.Err(); contextErr != nil {
		outcome, result = contextFailure(result, contextErr, finished)
		return result, nil
	}
	if callErr != nil {
		if errors.Is(callErr, context.DeadlineExceeded) || errors.Is(callErr, context.Canceled) {
			outcome, result = contextFailure(result, callErr, finished)
			return result, nil
		}
		outcome = "failed"
		result.Status = "failed"
		result.IsError = true
		result.Error = &ToolError{Code: "tool_error", Message: callErr.Error()}
		return result, nil
	}
	// Result size guard.
	raw, err := json.Marshal(value)
	if err != nil {
		outcome = "failed"
		result.Status = "failed"
		result.IsError = true
		result.Error = &ToolError{Code: "result_not_serializable", Message: err.Error()}
		return result, nil
	}
	if int64(len(raw)) > resultLimit {
		outcome = "result_too_large"
		return ToolResult{}, ErrResultTooLarge
	}
	outcome = "succeeded"
	result.Status = "succeeded"
	result.Content = value
	return result, nil
}

func effectiveExecutionLimits(global ResultLimits, decision AuthorizationDecision) (time.Duration, int64, error) {
	global = global.withDefaults()
	if global.MaxBytes < 0 || decision.MaxTimeoutMS < 0 || decision.MaxResultBytes < 0 {
		return 0, 0, ErrInvalidExecutionLimit
	}
	maxTimeoutMS := int64((time.Duration(1<<63 - 1)) / time.Millisecond)
	if decision.MaxTimeoutMS > maxTimeoutMS {
		return 0, 0, ErrInvalidExecutionLimit
	}
	resultLimit := global.MaxBytes
	if decision.MaxResultBytes > 0 && decision.MaxResultBytes < resultLimit {
		resultLimit = decision.MaxResultBytes
	}
	return time.Duration(decision.MaxTimeoutMS) * time.Millisecond, resultLimit, nil
}

func contextFailure(result ToolResult, err error, finished time.Time) (string, ToolResult) {
	result.FinishedAt = finished
	result.Status = "failed"
	result.IsError = true
	result.Content = nil
	if errors.Is(err, context.DeadlineExceeded) {
		result.Error = &ToolError{Code: "tool_timeout", Message: context.DeadlineExceeded.Error()}
		return "timed_out", result
	}
	result.Error = &ToolError{Code: "tool_cancelled", Message: context.Canceled.Error()}
	return "cancelled", result
}

// recordRequested durably writes the attempt (invoking), the requested audit
// record, and the outbox event in one transaction before the executor runs.
func (s *Service) recordRequested(ctx context.Context, scope execution.NamespaceScope, principal execution.Principal, plan *ExecutionPlan, attemptID string, started time.Time) error {
	attempt := &run.Attempt{
		AttemptID:      attemptID,
		RunID:          attemptID,
		Namespace:      principal.Namespace,
		State:          run.AttemptInvoking,
		FencingToken:   1,
		AttemptNumber:  1,
		IdempotencyKey: plan.IdempotencyKey,
		WorkerID:       s.workerID,
		StartedAt:      started,
	}
	requested := &run.AuditRequestedRecord{
		EventID:                     attemptID + "-requested",
		ExecutionID:                 attemptID,
		AttemptID:                   attemptID,
		RunID:                       attemptID,
		Namespace:                   principal.Namespace,
		Subject:                     principal.Subject,
		ToolName:                    plan.ToolName,
		Phase:                       "tool",
		RegistryRevision:            plan.RegistryRevision,
		DescriptorPolicyRevision:    plan.DescriptorPolicyRevision,
		AuthorizationPolicyRevision: plan.AuthorizationPolicyRevision,
		ArgumentsHash:               plan.ArgumentsHash,
		IdempotencyKeyHash:          hashBytes([]byte(plan.IdempotencyKey)),
		Timestamp:                   started,
	}
	outbox := &run.OutboxEvent{
		EventID:     attemptID + "-requested",
		AggregateID: attemptID,
		Type:        "audit.tool.requested",
		Sequence:    1,
	}
	return s.journal.BeginAttemptAndAppendRequested(ctx, scope, attempt, requested, outbox)
}

// recordOutcome appends the terminal/unknown outcome and drains the outbox
// through the exporter (at-least-once, deduplicated by event ID).
func (s *Service) recordOutcome(ctx context.Context, scope execution.NamespaceScope, attemptID string, state run.AttemptState, outcome string) error {
	outbox := &run.OutboxEvent{
		EventID:     attemptID + "-outcome",
		AggregateID: attemptID,
		Type:        "audit.tool." + outcome,
		Sequence:    2,
	}
	if err := s.journal.AppendOutcome(ctx, scope, attemptID, state, outbox); err != nil {
		return err
	}
	if s.exporter != nil {
		if _, _, err := run.ExportAndAck(ctx, s.journal, scope, s.workerID, s.exporter, 100); err != nil {
			return err
		}
	}
	return nil
}

func canonicalArguments(raw json.RawMessage) (json.RawMessage, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	// Canonicalize through a generic decode to normalize key order and
	// whitespace (Go map marshaling sorts keys deterministically).
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, "", fmt.Errorf("service: arguments are not valid JSON: %w", err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return nil, "", fmt.Errorf("service: canonicalize arguments: %w", err)
	}
	return canonical, hashBytes(canonical), nil
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func identityHash(p execution.Principal) string {
	raw, _ := json.Marshal(p)
	return hashBytes(raw)
}

func newPlanID() string {
	return fmt.Sprintf("plan-%d", time.Now().UTC().UnixNano())
}

func newExecutionID(planID string) string {
	return planID + "-exec-" + shortHash(hashBytes([]byte(fmt.Sprintf("%d", time.Now().UTC().UnixNano()))))
}

var _ ToolService = (*Service)(nil)
