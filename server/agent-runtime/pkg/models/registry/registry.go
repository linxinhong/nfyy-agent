// Package models/registry defines the managed model transport contract:
// hosts resolve model refs to approved endpoints and transport instances,
// so every Runtime-created model call flows through a governed transport.
// Any host/tool that dials the network itself is outside this boundary
// (D-11); production additionally requires NetworkPolicy/egress default-deny.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// EndpointMetadata is the immutable, auditable description of one approved
// model endpoint. It propagates to Run metrics/audit but never into model
// messages.
type EndpointMetadata struct {
	Provider string
	Model    string
	Region   string
	Revision string
	// BaseURL is the HTTPS endpoint (HTTP only for test loopback).
	BaseURL string
}

// EndpointRegistry resolves model refs to approved endpoints and their
// transport instances. The returned models.Model is owned by the registry.
type EndpointRegistry interface {
	// Resolve returns the approved endpoint + model for a ref.
	Resolve(ctx context.Context, modelRef string) (EndpointMetadata, models.Model, error)
	// ApprovedRoutes returns the current approved route set (for fallback
	// decisions and tests).
	ApprovedRoutes() []EndpointMetadata
}

// StaticRegistry is an in-memory registry over an explicit allowlist. It is
// the reference implementation: unknown refs are rejected, non-HTTPS
// endpoints (outside test loopback) are rejected, and fallback may only
// pick from ApprovedRoutes.
type StaticRegistry struct {
	entries map[string]endpointEntry
}

type endpointEntry struct {
	meta  EndpointMetadata
	model models.Model
}

// NewStaticRegistry builds a registry from a static allowlist.
func NewStaticRegistry(entries []endpointEntry) *StaticRegistry {
	reg := &StaticRegistry{entries: make(map[string]endpointEntry, len(entries))}
	for _, e := range entries {
		reg.entries[e.meta.Model] = e
	}
	return reg
}

// ErrEndpointNotApproved is returned for unknown model refs.
var ErrEndpointNotApproved = errors.New("models: endpoint is not approved")

// ErrInsecureEndpoint is returned for non-HTTPS endpoints outside test
// loopback.
var ErrInsecureEndpoint = errors.New("models: endpoint must be HTTPS")

// ValidateEndpoint checks scheme/userinfo rules (D-11 / RC-09 step 7).
func ValidateEndpoint(baseURL string, allowHTTPLoopback bool) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("models: invalid endpoint: %w", err)
	}
	if u.Scheme != "https" {
		if allowHTTPLoopback && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") {
			return nil
		}
		return ErrInsecureEndpoint
	}
	if u.User != nil {
		return errors.New("models: endpoint userinfo is not allowed")
	}
	return nil
}

// Add registers one approved entry (used by tests and hosts).
func (r *StaticRegistry) Add(meta EndpointMetadata, model models.Model) error {
	if err := ValidateEndpoint(meta.BaseURL, false); err != nil {
		return err
	}
	if model == nil {
		return errors.New("models: model is required")
	}
	r.entries[meta.Model] = endpointEntry{meta: meta, model: model}
	return nil
}

// AddLoopback registers one approved entry permitting an http:// loopback
// endpoint. Hosts must validate their own opt-in before calling this (e.g.
// an explicit configuration flag for a local same-host gateway); it exists so
// embedded hosts can route through a loopback LLM gateway without disabling
// endpoint validation entirely. Non-loopback http and userinfo remain
// rejected.
func (r *StaticRegistry) AddLoopback(meta EndpointMetadata, model models.Model) error {
	if err := ValidateEndpoint(meta.BaseURL, true); err != nil {
		return err
	}
	if model == nil {
		return errors.New("models: model is required")
	}
	r.entries[meta.Model] = endpointEntry{meta: meta, model: model}
	return nil
}

// Resolve implements EndpointRegistry.
func (r *StaticRegistry) Resolve(ctx context.Context, modelRef string) (EndpointMetadata, models.Model, error) {
	entry, ok := r.entries[modelRef]
	if !ok {
		return EndpointMetadata{}, nil, ErrEndpointNotApproved
	}
	return entry.meta, entry.model, nil
}

// ApprovedRoutes implements EndpointRegistry.
func (r *StaticRegistry) ApprovedRoutes() []EndpointMetadata {
	out := make([]EndpointMetadata, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.meta)
	}
	return out
}

// FallbackAllowed reports whether candidate is within the approved route
// set (RC-09 step 8: fallback may never escape the approved set).
func FallbackAllowed(registry EndpointRegistry, candidate EndpointMetadata) bool {
	for _, route := range registry.ApprovedRoutes() {
		if route.Model == candidate.Model && route.BaseURL == candidate.BaseURL {
			return true
		}
	}
	return false
}
