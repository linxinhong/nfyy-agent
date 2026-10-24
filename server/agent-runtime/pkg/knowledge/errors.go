// Package knowledge provides interfaces and utilities for knowledge-base retrieval.
package knowledge

import "errors"

var (
	// ErrNoEmbedder is returned when a knowledge operation requires an embedder
	// but none is configured.
	ErrNoEmbedder = errors.New("knowledge: no embedder configured")
	// ErrNotImplemented is returned by knowledge backends that do not implement
	// the requested operation.
	ErrNotImplemented = errors.New("knowledge: not implemented")
)
