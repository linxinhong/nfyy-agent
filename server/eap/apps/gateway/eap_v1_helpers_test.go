//go:build integration

package main_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/skillreg"
)

func postJSONHelper(t *testing.T, env *sliceEnv, path, bearer, body string, headers map[string]string) map[string]any {
	t.Helper()
	rec := env.do(t, http.MethodPost, path, bearer, body, headers)
	payload := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	payload["__status"] = float64(rec.Code)
	return payload
}

func ed25519Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

func base64Encode(key ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func timeNowUTC() time.Time { return time.Now().UTC() }

func parseKeysFromDiscovery(t *testing.T, env *sliceEnv) []skillreg.PkgKey {
	t.Helper()
	rec := env.do(t, http.MethodGet, "/.well-known/package-keys.json", "", "", nil)
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	out := make([]skillreg.PkgKey, 0, len(doc.Keys))
	for _, k := range doc.Keys {
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			continue
		}
		out = append(out, skillreg.PkgKey{Kid: k.Kid, PublicKey: x, Active: true})
	}
	return out
}

func findKeyID(t *testing.T, env *sliceEnv, keyValue string) string {
	t.Helper()
	hash := sha256HexOf(keyValue)
	var id string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT key_id FROM api_keys WHERE key_hash = $1`, hash).Scan(&id); err != nil {
		t.Fatalf("find key id: %v", err)
	}
	return id
}

func sha256HexOf(s string) string {
	sum := sha256SumBytes([]byte(s))
	return fmt.Sprintf("%x", sum)
}

func sha256SumBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// skillDownload adapts the slice env gateway into the skillsync Client.
type skillDownload struct {
	env *sliceEnv
	key string
}

func (d *skillDownload) Download(ctx context.Context, name, version string) ([]byte, string, []byte, error) {
	path := fmt.Sprintf("/enterprise/v1/skills/%s/versions/%s/download", name, version)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+d.key)
	req.Header.Set("X-Agent-Workspace", "secops")
	rec := httptest.NewRecorder()
	d.env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, "", nil, fmt.Errorf("download failed (%d): %s", rec.Code, rec.Body.String())
	}
	digest := strings.TrimPrefix(rec.Header().Get("X-Skill-Digest"), "sha256-")
	sig := rec.Header().Get("X-Skill-Signature")
	if digest == "" || sig == "" {
		return nil, "", nil, fmt.Errorf("missing digest/signature headers")
	}
	return rec.Body.Bytes(), digest, []byte(sig), nil
}
