package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const MaxRunMetadataBytes = 8 * 1024

var ErrRunMetadataTooLarge = errors.New("run metadata exceeds 8 KiB")

type runMetadataKey struct{}

// CloneRunMetadata validates the serialized size and returns an independent map.
func CloneRunMetadata(metadata map[string]string) (map[string]string, error) {
	if metadata == nil {
		return nil, nil
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal run metadata: %w", err)
	}
	if len(raw) > MaxRunMetadataBytes {
		return nil, ErrRunMetadataTooLarge
	}
	clone := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	return clone, nil
}

// ContextWithRunMetadata attaches an isolated copy of trusted run metadata.
func ContextWithRunMetadata(ctx context.Context, metadata map[string]string) context.Context {
	clone := cloneRunMetadata(metadata)
	return context.WithValue(ctx, runMetadataKey{}, clone)
}

// RunMetadataFromContext retrieves an isolated copy of trusted run metadata.
func RunMetadataFromContext(ctx context.Context) (map[string]string, bool) {
	metadata, ok := ctx.Value(runMetadataKey{}).(map[string]string)
	if !ok {
		return nil, false
	}
	return cloneRunMetadata(metadata), true
}

func cloneRunMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	clone := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	return clone
}
