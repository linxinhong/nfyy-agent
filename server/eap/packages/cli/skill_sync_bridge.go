package cli

// Bridge between the CLI command shell and the skillsync engine (ADR-0017):
// the CLI downloads via the public registry endpoints; the sync engine owns
// verification, atomic apply and the lockfile.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillsync"
)

// DefaultPackageKeys retrieves the platform package-key discovery document
// (/.well-known/package-keys.json) used for offline release verification.
func DefaultPackageKeys(ctx context.Context, server string) ([]skillreg.PkgKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/.well-known/package-keys.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("package key discovery failed (%d)", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	out := make([]skillreg.PkgKey, 0, len(doc.Keys))
	for _, k := range doc.Keys {
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			continue
		}
		out = append(out, skillreg.PkgKey{Kid: k.Kid, PublicKey: x, Active: true})
	}
	return out, nil
}

type gatewayDownloader struct {
	server    string
	bearer    string
	workspace string
}

// Download implements skillsync.Client via the public download endpoint.
func (g *gatewayDownloader) Download(ctx context.Context, name, version string) ([]byte, string, []byte, error) {
	path := fmt.Sprintf("/enterprise/v1/skills/%s/versions/%s/download", name, version)
	req, err := httpNewRequestWithContext(ctx, httpGet, g.server+path, nil)
	if err != nil {
		return nil, "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.bearer)
	req.Header.Set("X-Agent-Workspace", g.workspace)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, "", nil, fmt.Errorf("cannot reach the gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", nil, fmt.Errorf("download failed (%d): %s", resp.StatusCode, redact(string(body)))
	}
	digest := strings.TrimPrefix(resp.Header.Get("X-Skill-Digest"), "sha256-")
	sig := resp.Header.Get("X-Skill-Signature")
	if digest == "" || sig == "" {
		return nil, "", nil, fmt.Errorf("download is missing digest/signature headers")
	}
	return body, digest, []byte(sig), nil
}

func syncOptions(clientRoot, lockPath, client string, keys []skillreg.PkgKey) skillsync.Options {
	return skillsync.Options{
		Client:       client,
		InstallRoot:  clientRoot,
		LockfilePath: lockPath,
		Keys:         keys,
	}
}

// catalogSource adapts the workspace skill catalog (GET /enterprise/v1/skills)
// to the skillsync planner. Only registry rows are exposed; desired-state
// resolution never invents versions.
type catalogSource struct {
	server    string
	bearer    string
	workspace string
}

// Catalog implements skillsync.CatalogSource.
func (c *catalogSource) Catalog(ctx context.Context) ([]skillsync.CatalogEntry, error) {
	req, err := httpNewRequestWithContext(ctx, httpGet, c.server+"/enterprise/v1/skills", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.bearer)
	req.Header.Set("X-Agent-Workspace", c.workspace)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skill catalog failed (%d): %s", resp.StatusCode, redact(string(body)))
	}
	var doc struct {
		Skills []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Status  string `json:"status"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("skill catalog is corrupt: %v", err)
	}
	out := make([]skillsync.CatalogEntry, 0, len(doc.Skills))
	for _, s := range doc.Skills {
		out = append(out, skillsync.CatalogEntry{Name: s.Name, Version: s.Version, Status: s.Status})
	}
	return out, nil
}

func skillSync(ctx context.Context, c skillsync.Client, o skillsync.Options, name, version string, policyRemoval bool) (skillsync.SyncResult, error) {
	return skillsync.Sync(ctx, c, o, name, version, policyRemoval)
}
