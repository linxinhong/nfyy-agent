package memory

import (
	"context"
	"reflect"
)

// ProposalSourceResolver is implemented by the trusted host boundary used by
// B/S adapters. Browser input may select an opaque source reference, but only
// this resolver may return the attestation and finite fragments sent to the
// proposal extractor.
type ProposalSourceResolver interface {
	ResolveProposalSource(context.Context, Principal, Scope, string) (SourceAttestation, []ProposalFragment, error)
}

type ProposalSourceResolverFunc func(context.Context, Principal, Scope, string) (SourceAttestation, []ProposalFragment, error)

func (f ProposalSourceResolverFunc) ResolveProposalSource(ctx context.Context, principal Principal, scope Scope, sourceRef string) (SourceAttestation, []ProposalFragment, error) {
	if f == nil {
		return SourceAttestation{}, nil, ErrUnavailable
	}
	return f(ctx, principal, scope, sourceRef)
}

// ProposalSourceResolverAvailable rejects typed-nil host dependencies before
// an HTTP route can invoke them.
func ProposalSourceResolverAvailable(value ProposalSourceResolver) bool {
	if value == nil {
		return false
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !rv.IsNil()
	default:
		return true
	}
}
