//go:build integration

// Attachment blobs: hash verified while streaming, media type allowlisted and
// sniffed, text DLP-scanned, content-addressed dedupe, and contributions may
// only cite a blob with the same size and type.

package main_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/blob"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/contribution"
	"enterprise-agent-platform/packages/dlp"
)

func TestAttachmentBlobs(t *testing.T) {
	env := newSliceEnv(t, 1000)
	env.srv.OIDCStore = env.store
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.Content = content.New(env.pool, env.store)
	env.srv.Contributions = contribution.New(env.pool, env.store)
	env.srv.Blobs = blob.New(env.pool, env.store, t.TempDir())
	env.handler = env.srv.Handler()
	session := mcpLogin(t, env, "blob-author@example.com", "secops")

	put := func(sum, mediaType, body string) (int, string) {
		rec := env.do(t, http.MethodPut, "/enterprise/v1/blobs/"+sum, session, body,
			map[string]string{"X-Agent-Workspace": "secops", "Content-Type": mediaType})
		return rec.Code, rec.Body.String()
	}
	hash := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		return hex.EncodeToString(sum[:])
	}

	note := "判决要点摘录：回购条款有效。"
	if code, body := put(hash("other"), "text/plain", note); code != http.StatusBadRequest || !strings.Contains(body, "sha256") {
		t.Fatalf("hash mismatch: %d %s", code, body)
	}
	if code, _ := put(hash(note), "image/png", note); code != http.StatusBadRequest {
		t.Fatalf("declared type must match sniffed bytes: %d", code)
	}
	if code, _ := put(hash(note), "application/x-msdownload", note); code != http.StatusBadRequest {
		t.Fatalf("non-allowlisted type: %d", code)
	}
	secret := "token sk-ABCDEFGHIJKLMNOPQRSTUVWX"
	if code, body := put(hash(secret), "text/plain", secret); code != http.StatusUnprocessableEntity || !strings.Contains(body, "dlp_blocked") {
		t.Fatalf("sensitive text attachment: %d %s", code, body)
	}
	code, body := put(hash(note), "text/plain", note)
	var stored struct {
		Size       int64  `json:"size"`
		ScanStatus string `json:"scan_status"`
	}
	_ = json.Unmarshal([]byte(body), &stored)
	if code != http.StatusCreated || stored.ScanStatus != "unscanned" || stored.Size != int64(len(note)) {
		t.Fatalf("store: %d %s", code, body)
	}
	if code, _ := put(hash(note), "text/plain", note); code != http.StatusOK {
		t.Fatalf("same bytes again must be a no-op: %d", code)
	}

	upload := func(key string, size int64, mediaType string) (int, string) {
		raw := fmt.Sprintf(`{"local_id":"wiki/with-attachment","title":"附件","markdown":"见附件","requested_scope":"organization",`+
			`"redaction_declared":true,"upload_mode":"manual","client":{"name":"t","version":"1","schema_version":1},`+
			`"attachments":[{"sha256":"%s","name":"摘录.txt","media_type":"%s","size":%d}]}`, hash(note), mediaType, size)
		rec := env.do(t, http.MethodPost, "/enterprise/v1/contributions", session, raw,
			map[string]string{"X-Agent-Workspace": "secops", "Idempotency-Key": key})
		return rec.Code, rec.Body.String()
	}
	if code, body := upload("att-wrong-size", stored.Size+1, "text/plain"); code != http.StatusBadRequest || !strings.Contains(body, "attachment_missing") {
		t.Fatalf("wrong size must not match the blob: %d %s", code, body)
	}
	if code, body := upload("att-ok", stored.Size, "text/plain"); code != http.StatusCreated {
		t.Fatalf("cite stored blob: %d %s", code, body)
	}

	// With a scanner: infected bytes are refused and never indexed, clean
	// bytes are stored as clean, and a scanner outage fails closed.
	scanner := &fakeScanner{}
	env.srv.Blobs.WithScanner(scanner)
	eicar := "EICAR test payload " + note
	if code, body := put(hash(eicar), "text/plain", eicar); code != http.StatusBadRequest || !strings.Contains(body, "malware") {
		t.Fatalf("infected attachment: %d %s", code, body)
	}
	var indexed int
	if err := env.pool.QueryRow(context.Background(), `SELECT count(*) FROM blobs WHERE sha256 = $1`, hash(eicar)).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("infected blob indexed: %d %v", indexed, err)
	}
	clean := "另一份摘录：" + note
	if code, body := put(hash(clean), "text/plain", clean); code != http.StatusCreated || !strings.Contains(body, `"scan_status":"clean"`) {
		t.Fatalf("scanned clean attachment: %d %s", code, body)
	}
	scanner.down = true
	other := "第三份摘录：" + note
	if code, _ := put(hash(other), "text/plain", other); code != http.StatusServiceUnavailable && code != http.StatusBadGateway {
		t.Fatalf("scanner outage must refuse the upload: %d", code)
	}
}

type fakeScanner struct{ down bool }

func (f *fakeScanner) Scan(_ context.Context, r io.Reader) (blob.Verdict, error) {
	if f.down {
		return blob.Verdict{}, errors.New("clamd unreachable")
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return blob.Verdict{}, err
	}
	if strings.Contains(string(raw), "EICAR") {
		return blob.Verdict{Signature: "Eicar-Test-Signature"}, nil
	}
	return blob.Verdict{Clean: true}, nil
}
