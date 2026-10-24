package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// MemoryRecaller is the narrow strict-memory capability consumed by Agent.
type MemoryRecaller interface {
	Recall(context.Context, memory.Principal, memory.Scope, memory.RecallRequest) (memory.RecallResult, error)
}

// MemoryRecallFailureMode controls whether a transient memory outage prevents
// the model call. Authorization, validation and caller cancellation always
// fail closed in every mode.
type MemoryRecallFailureMode uint8

const (
	MemoryRecallStrict MemoryRecallFailureMode = iota
	MemoryRecallOmitUnavailable
)

// MemoryRecallObservation is intentionally content-free.
type MemoryRecallObservation struct {
	Outcome            string
	ItemCount          int
	CandidateCount     int
	CandidateTruncated bool
	OutputTruncated    bool
}

type MemoryRecallObserver interface {
	ObserveMemoryRecall(MemoryRecallObservation)
}

// MemoryAuthorityResolver supplies the trusted authority for one execution.
// Multi-user hosts must resolve it from authenticated context values, never
// request content, metadata, session records, or mutable Agent fields.
type MemoryAuthorityResolver interface {
	ResolveMemoryAuthority(context.Context) (memory.Principal, memory.Scope, error)
}

type MemoryAuthorityResolverFunc func(context.Context) (memory.Principal, memory.Scope, error)

func (f MemoryAuthorityResolverFunc) ResolveMemoryAuthority(ctx context.Context) (memory.Principal, memory.Scope, error) {
	if f == nil {
		return memory.Principal{}, memory.Scope{}, fmt.Errorf("%w: memory authority resolver", memory.ErrUnauthorized)
	}
	return f(ctx)
}

// MemoryContextProviderConfig binds a trusted immutable authority to an Agent
// context provider. The host, never request content, supplies Principal/Scope.
type MemoryContextProviderConfig struct {
	Recaller MemoryRecaller
	// AuthorityResolver is the B/S mode. It is mutually exclusive with the
	// fixed Principal/Scope compatibility mode below.
	AuthorityResolver MemoryAuthorityResolver
	Principal         memory.Principal
	Scope             memory.Scope
	Limit             int
	Kinds             []memory.Kind
	Topics            []string
	FailureMode       MemoryRecallFailureMode
	Observer          MemoryRecallObserver
}

type memoryContextProvider struct {
	recaller          MemoryRecaller
	authorityResolver MemoryAuthorityResolver
	principal         memory.Principal
	scope             memory.Scope
	limit             int
	kinds             []memory.Kind
	topics            []string
	failureMode       MemoryRecallFailureMode
	observer          MemoryRecallObserver
}

type memoryContextMarker struct{}

const memoryContextProperty = "xingding.runtime.memory_context"

// NewMemoryContextProvider returns nil when strict memory is disabled. This is
// the only zero-I/O disabled mode; partial configurations are rejected.
func NewMemoryContextProvider(cfg MemoryContextProviderConfig) (ContextProvider, error) {
	if isNilMemoryRecaller(cfg.Recaller) {
		return nil, nil
	}
	if cfg.AuthorityResolver != nil && isNilMemoryAuthorityResolver(cfg.AuthorityResolver) {
		return nil, fmt.Errorf("%w: memory authority resolver is nil", memory.ErrInvalid)
	}
	if cfg.AuthorityResolver != nil {
		if cfg.Principal != (memory.Principal{}) || cfg.Scope != (memory.Scope{}) {
			return nil, fmt.Errorf("%w: fixed and dynamic memory authority are mutually exclusive", memory.ErrInvalid)
		}
	} else {
		if err := validateMemoryAuthority(cfg.Principal, cfg.Scope); err != nil {
			return nil, err
		}
	}
	if cfg.Limit < 0 || cfg.Limit > memory.DefaultLimits().MaxRecallLimit || cfg.FailureMode > MemoryRecallOmitUnavailable {
		return nil, fmt.Errorf("%w: memory context configuration", memory.ErrInvalid)
	}
	return &memoryContextProvider{
		recaller: cfg.Recaller, authorityResolver: cfg.AuthorityResolver, principal: cfg.Principal, scope: cfg.Scope, limit: cfg.Limit,
		kinds: append([]memory.Kind(nil), cfg.Kinds...), topics: append([]string(nil), cfg.Topics...),
		failureMode: cfg.FailureMode, observer: cfg.Observer,
	}, nil
}

func isNilMemoryAuthorityResolver(v MemoryAuthorityResolver) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func isNilMemoryRecaller(v MemoryRecaller) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func (p *memoryContextProvider) Apply(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	return p.apply(ctx, req)
}

func (p *memoryContextProvider) ApplyBeforeModelCall(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	return p.apply(ctx, req)
}

func (p *memoryContextProvider) apply(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	// Strip only the segment carrying our unforgeable private marker. Business
	// input and the protected Current continuation are never rewritten.
	req.History = stripMemoryContext(req.History)
	query := boundedRecallQuery(req.Input.Text())
	principal, scope, err := p.authority(ctx)
	if err != nil {
		p.observe(MemoryRecallObservation{Outcome: "failed"})
		return req, err
	}
	result, err := p.recaller.Recall(ctx, principal, scope, memory.RecallRequest{
		Query: query, Limit: p.limit, Kinds: append([]memory.Kind(nil), p.kinds...), Topics: append([]string(nil), p.topics...),
	})
	if err != nil {
		if p.failureMode == MemoryRecallOmitUnavailable && ctx.Err() == nil && (errors.Is(err, memory.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)) {
			p.observe(MemoryRecallObservation{Outcome: "omitted_unavailable"})
			return req, nil
		}
		p.observe(MemoryRecallObservation{Outcome: "failed"})
		return req, err
	}
	if len(result.Items) == 0 {
		p.observe(observation("empty", result))
		return req, nil
	}
	payload := struct {
		Type               string              `json:"type"`
		Trust              string              `json:"trust"`
		Generation         uint64              `json:"generation"`
		CandidateTruncated bool                `json:"candidate_truncated"`
		OutputTruncated    bool                `json:"output_truncated"`
		Items              []memory.RecallItem `json:"items"`
	}{
		Type: "private_memory", Trust: "untrusted_data", Generation: result.Generation,
		CandidateTruncated: result.CandidateTruncated, OutputTruncated: result.OutputTruncated,
		Items: result.Items,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return req, fmt.Errorf("encode memory context: %w", err)
	}
	req.History = append(req.History, HistoryRun{RunID: "memory-context", Messages: []models.Message{{
		Role: models.RoleUser, Content: string(encoded),
		AdditionalProperties: map[string]any{memoryContextProperty: memoryContextMarker{}},
	}}})
	p.observe(observation("applied", result))
	return req, nil
}

func (p *memoryContextProvider) authority(ctx context.Context) (memory.Principal, memory.Scope, error) {
	if p.authorityResolver == nil {
		return p.principal, p.scope, nil
	}
	principal, scope, err := p.authorityResolver.ResolveMemoryAuthority(ctx)
	if err != nil {
		return memory.Principal{}, memory.Scope{}, fmt.Errorf("%w: resolve trusted memory authority", memory.ErrUnauthorized)
	}
	if err := validateMemoryAuthority(principal, scope); err != nil {
		return memory.Principal{}, memory.Scope{}, err
	}
	return principal, scope, nil
}

func validateMemoryAuthority(principal memory.Principal, scope memory.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if principal.Subject == "" || principal.Namespace == "" || principal.Issuer == "" || principal.Namespace != scope.Namespace || principal.Subject != scope.SubjectID {
		return fmt.Errorf("%w: trusted principal", memory.ErrUnauthorized)
	}
	return nil
}

func (*memoryContextProvider) strictMemoryContextProvider() {}

func stripMemoryContext(history []HistoryRun) []HistoryRun {
	out := make([]HistoryRun, 0, len(history))
	for _, run := range history {
		messages := make([]models.Message, 0, len(run.Messages))
		for _, msg := range run.Messages {
			if _, ok := msg.AdditionalProperties[memoryContextProperty].(memoryContextMarker); ok {
				continue
			}
			messages = append(messages, msg)
		}
		if len(messages) > 0 {
			run.Messages = messages
			out = append(out, run)
		}
	}
	return out
}

func boundedRecallQuery(input string) string {
	if len(input) <= memory.MaxRecallQueryBytes {
		return input
	}
	input = input[:memory.MaxRecallQueryBytes]
	for !utf8.ValidString(input) {
		input = input[:len(input)-1]
	}
	return input
}

func observation(outcome string, result memory.RecallResult) MemoryRecallObservation {
	return MemoryRecallObservation{Outcome: outcome, ItemCount: len(result.Items), CandidateCount: result.CandidateCount, CandidateTruncated: result.CandidateTruncated, OutputTruncated: result.OutputTruncated}
}

func (p *memoryContextProvider) observe(event MemoryRecallObservation) {
	if p.observer != nil {
		p.observer.ObserveMemoryRecall(event)
	}
}

var _ ContextProvider = (*memoryContextProvider)(nil)
var _ PerModelCallContextProvider = (*memoryContextProvider)(nil)
