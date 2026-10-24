package providers

import "os"

// Provider is a thin registration: an identity, a base URL, where to find
// credentials, and a catalog of known models. Adding a provider never
// requires writing protocol code; it references a shared API dialect.
type Provider struct {
	ID      string
	Name    string
	BaseURL string
	// APIKeyEnv lists environment variables tried in order when resolving
	// the provider's API key. Empty means the endpoint needs no key.
	APIKeyEnv []string
	// Models is the known model catalog. Resolving an unlisted model ID
	// under a registered provider synthesizes a Model from provider
	// defaults, so catalogs only need entries for models with accurate
	// cost/context metadata.
	Models []Model
}

// APIKey resolves the provider API key from the environment.
func (p Provider) APIKey() string {
	return resolveAPIKey(p.APIKeyEnv)
}

func resolveAPIKey(envVars []string) string {
	for _, env := range envVars {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}
