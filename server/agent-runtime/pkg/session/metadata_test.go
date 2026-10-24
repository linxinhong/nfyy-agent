package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestRunMetadataIsValidatedClonedAndStoredInContext(t *testing.T) {
	metadata := map[string]string{"project_id": "alpha", "actor_id": "user-1"}
	cloned, err := session.CloneRunMetadata(metadata)
	if err != nil {
		t.Fatalf("CloneRunMetadata: %v", err)
	}
	metadata["project_id"] = "mutated"
	if cloned["project_id"] != "alpha" {
		t.Fatalf("clone changed: %+v", cloned)
	}

	ctx := session.ContextWithRunMetadata(context.Background(), cloned)
	got, ok := session.RunMetadataFromContext(ctx)
	if !ok || got["actor_id"] != "user-1" {
		t.Fatalf("RunMetadataFromContext = %+v, %v", got, ok)
	}
	got["actor_id"] = "mutated"
	again, _ := session.RunMetadataFromContext(ctx)
	if again["actor_id"] != "user-1" {
		t.Fatalf("context metadata was mutable: %+v", again)
	}
}

func TestRunMetadataRejectsMoreThanEightKiB(t *testing.T) {
	_, err := session.CloneRunMetadata(map[string]string{"value": strings.Repeat("x", 8193)})
	if !errors.Is(err, session.ErrRunMetadataTooLarge) {
		t.Fatalf("error = %v", err)
	}
}
