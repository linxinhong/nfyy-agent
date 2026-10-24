package providers

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// Registry holds providers keyed by ID and resolves "provider:model"
// references into ready-to-use runtime models.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	apiKeys   map[string]string
}

// NewRegistry creates a registry pre-populated with the given providers.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{
		providers: make(map[string]Provider, len(providers)),
		apiKeys:   make(map[string]string),
	}
	for _, p := range providers {
		r.providers[p.ID] = p
	}
	return r
}

var defaultRegistry = NewRegistry(BuiltinProviders()...)

// DefaultRegistry returns the process-wide registry holding the builtin
// providers. Mutating it (Register, SetAPIKey) affects all package-level
// helpers such as Resolve and MustModel.
func DefaultRegistry() *Registry {
	return defaultRegistry
}

// Register adds or replaces a provider.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.ID] = p
}

// SetAPIKey sets an explicit API key override for a provider, taking
// precedence over environment variables.
func (r *Registry) SetAPIKey(providerID, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apiKeys[providerID] = key
}

// Provider returns the registered provider with the given ID.
func (r *Registry) Provider(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// Providers returns all registered providers sorted by ID.
func (r *Registry) Providers() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Model resolves a model by provider and model ID. Catalog entries are
// returned with provider defaults filled in. An unlisted model ID under a
// registered provider yields a synthesized Model from provider defaults;
// an unknown provider is an error.
func (r *Registry) Model(providerID, modelID string) (Model, error) {
	r.mu.RLock()
	p, ok := r.providers[providerID]
	r.mu.RUnlock()
	if !ok {
		return Model{}, fmt.Errorf("providers: unknown provider %q", providerID)
	}
	for _, m := range p.Models {
		if m.ID == modelID {
			return m.withProviderDefaults(p), nil
		}
	}
	m := Model{
		ID:       modelID,
		Name:     modelID,
		API:      p.defaultAPI(),
		Provider: p.ID,
		BaseURL:  p.BaseURL,
	}
	m.Reasoning = detectFamily(m) == familyDeepSeekR1
	return m, nil
}

// Models returns all catalog models across providers, sorted by reference.
func (r *Registry) Models() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Model
	for _, p := range r.providers {
		for _, m := range p.Models {
			out = append(out, m.withProviderDefaults(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref() < out[j].Ref() })
	return out
}

// Resolve parses a "provider:model" reference and returns a runtime model
// ready for use. The API key is taken from SetAPIKey overrides first, then
// from the provider's environment variables; a provider declaring APIKeyEnv
// with no resolvable key is an error.
func (r *Registry) Resolve(ref string) (models.Model, error) {
	providerID, modelID, ok := strings.Cut(ref, ":")
	if !ok || providerID == "" || modelID == "" {
		return nil, fmt.Errorf("providers: invalid model reference %q, want \"provider:model\"", ref)
	}
	m, err := r.Model(providerID, modelID)
	if err != nil {
		return nil, err
	}
	key, err := r.resolveKey(providerID)
	if err != nil {
		return nil, err
	}
	return m.Chat(ChatOptions{APIKey: key})
}

// MustModel is Resolve but panics on error. Intended for wiring during
// process startup.
func (r *Registry) MustModel(ref string) models.Model {
	m, err := r.Resolve(ref)
	if err != nil {
		panic(err)
	}
	return m
}

// Resolve parses a "provider:model" reference against the default registry.
func Resolve(ref string) (models.Model, error) {
	return defaultRegistry.Resolve(ref)
}

// MustModel resolves a "provider:model" reference against the default
// registry and panics on error.
func MustModel(ref string) models.Model {
	return defaultRegistry.MustModel(ref)
}

func (r *Registry) resolveKey(providerID string) (string, error) {
	r.mu.RLock()
	key := r.apiKeys[providerID]
	p, ok := r.providers[providerID]
	r.mu.RUnlock()
	if key != "" {
		return key, nil
	}
	if !ok {
		return "", fmt.Errorf("providers: unknown provider %q", providerID)
	}
	if key := p.APIKey(); key != "" {
		return key, nil
	}
	if len(p.APIKeyEnv) > 0 {
		return "", fmt.Errorf("providers: no API key for provider %q (set one of %s, or Registry.SetAPIKey)", providerID, strings.Join(p.APIKeyEnv, ", "))
	}
	return "", nil
}

func (p Provider) defaultAPI() API {
	for _, m := range p.Models {
		return m.API
	}
	return APIOpenAICompletions
}

func (m Model) withProviderDefaults(p Provider) Model {
	if m.Provider == "" {
		m.Provider = p.ID
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if m.BaseURL == "" {
		m.BaseURL = p.BaseURL
	}
	if m.API == "" {
		m.API = p.defaultAPI()
	}
	return m
}
