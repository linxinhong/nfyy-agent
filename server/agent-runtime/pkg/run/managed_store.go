package run

import (
	"context"
	"errors"
	"reflect"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// ManagedRunSnapshot is a run together with its storage revision. Revision is
// independent from Claim.FencingToken: the former detects concurrent state
// changes, while the latter rejects writes from an execution owner that has
// lost its lease.
type ManagedRunSnapshot struct {
	Run      *session.RunOutput
	Revision uint64
}

// ManagedRunCommit describes one atomic managed-run write. ExpectedRevision
// zero together with an empty ExpectedStatus creates a new run at revision 1.
// Updates require both the current non-zero revision and current status.
type ManagedRunCommit struct {
	Run              *session.RunOutput
	ExpectedRevision uint64
	ExpectedStatus   session.RunStatus
}

// FencedRunStore persists managed runs while atomically checking execution
// ownership. Implementations must validate scope, run ID, worker ID,
// generation (Claim.FencingToken), and the authoritative lease expiry in the
// same atomic domain as the revision/status CAS and run write.
//
// A locally cached Claim.LeaseExpires is not proof of ownership. In
// particular, a caller that receives an unknown Heartbeat result may only
// attempt CommitManagedRun; the store must re-check its authoritative lease
// state and fail closed when that state no longer proves ownership.
type FencedRunStore interface {
	GetManagedRun(ctx context.Context, scope execution.NamespaceScope, runID string) (*ManagedRunSnapshot, error)
	CommitManagedRun(ctx context.Context, scope execution.NamespaceScope, claim *Claim, commit ManagedRunCommit) (uint64, error)
}

// ManagedStore is the optional strict-managed persistence capability. The
// same object owns dispatch leases and run data so CommitManagedRun can fence
// and persist atomically; composing a Dispatcher and FencedRunStore backed by
// different atomic domains does not satisfy this contract.
type ManagedStore interface {
	db.RunStore
	Dispatcher
	FencedRunStore
}

var (
	// ErrManagedStoreRequired reports that strict managed execution was
	// requested with a legacy RunStore that cannot atomically fence commits.
	ErrManagedStoreRequired = errors.New("run: managed store capability required")
	// ErrInvalidClaim reports a malformed claim or a scope/run mismatch.
	ErrInvalidClaim = errors.New("run: invalid ownership claim")
	// ErrLeaseExpired reports that the authoritative ownership lease expired.
	ErrLeaseExpired = errors.New("run: ownership lease expired")
)

// RequireManagedStore negotiates the optional strict-managed capability.
// Legacy RunStore implementations remain valid for non-managed callers, but
// strict callers must invoke this before claim or execution and fail closed.
func RequireManagedStore(store db.RunStore) (ManagedStore, error) {
	managed, ok := store.(ManagedStore)
	if !ok || isNilCapability(managed) {
		return nil, ErrManagedStoreRequired
	}
	return managed, nil
}

func isNilCapability(capability any) bool {
	if capability == nil {
		return true
	}
	value := reflect.ValueOf(capability)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
