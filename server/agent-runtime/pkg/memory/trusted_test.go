package memory

import (
	"errors"
	"testing"
	"time"
)

func TestMemoryPolicyDefaultsAreFinite(t *testing.T) {
	got := DefaultLimits()
	want := Limits{
		MaxContentBytes: 4 << 10, MaxRecordBytes: 8 << 10,
		DefaultListLimit: 20, MaxListLimit: 100, MaxListBytes: 1 << 20,
		DefaultRecallLimit: 10, MaxRecallLimit: 50, MaxRecallCandidates: 200,
		MaxRecallBytes: 16 << 10, MaxCandidateBytes: 1600 << 10,
		DefaultRetention: 30 * 24 * time.Hour, MaxRetention: 365 * 24 * time.Hour,
		MaxRecordsPerScope: 1000, MaxContentPerScope: 4 << 20,
		MaxReceiptsPerScope: 10000, MaxControlsPerScope: 10000, MaxConcurrentOps: 32,
		CRUDTimeout: time.Second, RecallTimeout: 100 * time.Millisecond,
		MaxProposeConcurrent: 4, MaxProposeInputBytes: 32 << 10, MaxProposeCandidates: 8,
		ProposeTimeout: 10 * time.Second,
	}
	if got != want {
		t.Fatalf("defaults = %#v, want %#v", got, want)
	}
}

func TestMemoryScopeRejectsMissingAndControlComponents(t *testing.T) {
	valid := Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "subject", CollectionID: "collection"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, scope := range map[string]Scope{
		"namespace":  {ProjectID: "project", SubjectID: "subject", CollectionID: "collection"},
		"project":    {Namespace: "tenant", SubjectID: "subject", CollectionID: "collection"},
		"subject":    {Namespace: "tenant", ProjectID: "project", CollectionID: "collection"},
		"collection": {Namespace: "tenant", ProjectID: "project", SubjectID: "subject"},
		"control":    {Namespace: "tenant\x00escape", ProjectID: "project", SubjectID: "subject", CollectionID: "collection"},
	} {
		if err := scope.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s scope error = %v", name, err)
		}
	}
}
