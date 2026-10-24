// Package ingest receives bounded, purpose-specific operational evidence and
// data objects. It never accepts conversation text or arbitrary JSON logs.
package ingest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuanpu.dev/server/internal/content"
	"yuanpu.dev/server/internal/httpkit"
	"yuanpu.dev/server/internal/identity"
)

const maxUploadBytes = 2 << 20

type HTTP struct {
	pool         *pgxpool.Pool
	resources    *content.Store
	authenticate func(http.Handler) http.Handler
}

type LogInput struct {
	IdempotencyKey string    `json:"idempotencyKey"`
	ResourceID     string    `json:"resourceId"`
	RunID          string    `json:"runId"`
	Kind           string    `json:"kind"`
	Outcome        string    `json:"outcome"`
	DurationMs     int       `json:"durationMs"`
	OccurredAt     time.Time `json:"occurredAt"`
}

type LogReceipt struct {
	ID string `json:"id"`
}

type Upload struct {
	ID          string    `json:"id"`
	ResourceID  *string   `json:"resourceId"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"contentType"`
	ByteSize    int       `json:"byteSize"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"createdAt"`
}

func NewHTTP(pool *pgxpool.Pool, resources *content.Store, authenticate func(http.Handler) http.Handler) *HTTP {
	return &HTTP{pool: pool, resources: resources, authenticate: authenticate}
}

func (h *HTTP) Mount(router chi.Router) {
	router.Group(func(protected chi.Router) {
		protected.Use(h.authenticate)
		protected.Post("/v1/logs", h.postLog)
		protected.Post("/v1/data-uploads", h.postUpload)
		protected.Get("/v1/data-uploads/{id}", h.getUpload)
	})
}

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (h *HTTP) current(w http.ResponseWriter, r *http.Request) (identity.User, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		httpkit.WriteError(w, r, 401, "UNAUTHORIZED", "Authentication required")
		return identity.User{}, false
	}
	if h.pool == nil {
		httpkit.WriteError(w, r, 503, "INGEST_UNAVAILABLE", "Ingest service is unavailable")
		return identity.User{}, false
	}
	return user, true
}

func (h *HTTP) canReadResource(w http.ResponseWriter, r *http.Request, user identity.User, resourceID string) bool {
	_, err := h.resources.Get(r.Context(), user, resourceID)
	if err == nil {
		return true
	}
	if errors.Is(err, content.ErrNotFound) {
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Resource not found")
	} else {
		httpkit.WriteError(w, r, 503, "CONTENT_UNAVAILABLE", "Content service is unavailable")
	}
	return false
}

func (h *HTTP) postLog(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	var input LogInput
	if httpkit.DecodeJSON(r, &input, 4096) != nil || len(input.IdempotencyKey) < 8 || len(input.IdempotencyKey) > 128 ||
		len(input.RunID) > 128 || input.DurationMs < 0 || input.DurationMs > 86400000 ||
		(input.Kind != "agent_run" && input.Kind != "tool_step" && input.Kind != "resource_use") ||
		(input.Outcome != "succeeded" && input.Outcome != "failed" && input.Outcome != "cancelled") ||
		input.OccurredAt.IsZero() || input.OccurredAt.After(time.Now().Add(5*time.Minute)) || input.OccurredAt.Before(time.Now().AddDate(0, -1, 0)) ||
		(input.ResourceID != "" && !httpkit.ValidID(input.ResourceID)) {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid operation log")
		return
	}
	var resourceID *string
	if input.ResourceID != "" {
		if !h.canReadResource(w, r, user, input.ResourceID) {
			return
		}
		resourceID = &input.ResourceID
	}
	id, err := newID()
	if err != nil {
		httpkit.WriteError(w, r, 503, "INGEST_UNAVAILABLE", "Ingest service is unavailable")
		return
	}
	var storedID string
	err = h.pool.QueryRow(r.Context(), `INSERT INTO yuanpu_operation_logs(id,actor_id,idempotency_key,resource_id,run_id,kind,outcome,duration_ms,occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (actor_id,idempotency_key) DO UPDATE SET id=yuanpu_operation_logs.id
		RETURNING id`, id, user.ID, input.IdempotencyKey, resourceID, input.RunID, input.Kind, input.Outcome, input.DurationMs, input.OccurredAt).Scan(&storedID)
	if err != nil {
		httpkit.WriteError(w, r, 503, "INGEST_UNAVAILABLE", "Ingest service is unavailable")
		return
	}
	httpkit.WriteJSON(w, 201, LogReceipt{ID: storedID})
}

func (h *HTTP) postUpload(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	resourceID := r.URL.Query().Get("resourceId")
	var linked *string
	if resourceID != "" {
		if !httpkit.ValidID(resourceID) {
			httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid resource ID")
			return
		}
		resource, err := h.resources.Get(r.Context(), user, resourceID)
		if err != nil || resource.OwnerID != user.ID {
			httpkit.WriteError(w, r, 403, "FORBIDDEN", "Only the author can attach data")
			return
		}
		linked = &resourceID
	}
	filename := strings.TrimSpace(r.Header.Get("X-File-Name"))
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if filename == "" || len(filename) > 180 || strings.ContainsAny(filename, "/\\\"\x00\r\n") ||
		(mediaType != "text/plain" && mediaType != "text/markdown" && mediaType != "application/pdf" && mediaType != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" && mediaType != "application/octet-stream") || err != nil {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid upload metadata")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxUploadBytes {
		httpkit.WriteError(w, r, 413, "UPLOAD_TOO_LARGE", "Upload must be between 1 byte and 2 MiB")
		return
	}
	id, err := newID()
	if err != nil {
		httpkit.WriteError(w, r, 503, "INGEST_UNAVAILABLE", "Ingest service is unavailable")
		return
	}
	digest := sha256.Sum256(data)
	var uploaded Upload
	err = h.pool.QueryRow(r.Context(), `INSERT INTO yuanpu_data_uploads(id,actor_id,resource_id,filename,content_type,byte_size,sha256,payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,resource_id,filename,content_type,byte_size,sha256,created_at`,
		id, user.ID, linked, filename, mediaType, len(data), hex.EncodeToString(digest[:]), data).
		Scan(&uploaded.ID, &uploaded.ResourceID, &uploaded.Filename, &uploaded.ContentType, &uploaded.ByteSize, &uploaded.SHA256, &uploaded.CreatedAt)
	if err != nil {
		httpkit.WriteError(w, r, 503, "INGEST_UNAVAILABLE", "Ingest service is unavailable")
		return
	}
	httpkit.WriteJSON(w, 201, uploaded)
}

func (h *HTTP) getUpload(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !httpkit.ValidID(id) {
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Upload not found")
		return
	}
	var actorID, filename, contentType string
	var data []byte
	err := h.pool.QueryRow(r.Context(), `SELECT actor_id,filename,content_type,payload FROM yuanpu_data_uploads WHERE id=$1`, id).
		Scan(&actorID, &filename, &contentType, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Upload not found")
		return
	}
	if err != nil {
		httpkit.WriteError(w, r, 503, "INGEST_UNAVAILABLE", "Ingest service is unavailable")
		return
	}
	if actorID != user.ID {
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Upload not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}
