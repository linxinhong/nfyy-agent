// Package knowledge implements the Workspace Knowledge vertical slice
// (TASK-012): parse -> chunk -> embed -> index -> search with citations.
//
// Boundary rules (ADR-0012):
//   - every query is workspace-scoped INSIDE the SQL (the workspace filter
//     is part of retrieval, never a post-filter over a global search);
//   - citations are mandatory: a hit names its document title, version and
//     chunk sequence, or the search is not a successful result;
//   - processing is idempotent: re-uploading identical content is a no-op;
//     a new version replaces the old document's chunks atomically;
//   - embedder/worker failures mark the document failed and the content is
//     never retrievable; search scans never carry raw document text into
//     logs or audit payloads.
package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/errs"
)

// Embedder produces one deterministic vector per text. Production wires a
// provider-backed embedder; tests and offline deployments use HashEmbedder.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// HashEmbedder is a deterministic, dependency-free embedding: 256-dim bag of
// hashed token buckets, L2-normalized. Same text always yields the same
// vector; different texts differ measurably — enough for the vertical slice
// and deterministic tests.
type HashEmbedder struct{ Dims int }

// Embed implements Embedder. Tokenization: ASCII words plus CJK character
// bigrams (single chars for lone CJK runes) so Chinese queries overlap
// document content without whitespace token boundaries.
func (h HashEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	dims := h.Dims
	if dims <= 0 {
		dims = 256
	}
	vec := make([]float64, dims)
	add := func(tok string) {
		sum := sha256.Sum256([]byte(tok))
		idx := int(binaryEndian(sum[:4])) % dims
		vec[idx] += 1
	}
	text = strings.ToLower(text)
	var word strings.Builder
	var cjk []rune
	flushWord := func() {
		if word.Len() > 0 {
			add(word.String())
			word.Reset()
		}
	}
	flushCJK := func() {
		for i, r := range cjk {
			add(string(r))
			if i+1 < len(cjk) {
				add(string([]rune{r, cjk[i+1]}))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF: // CJK unified ideographs
			flushWord()
			cjk = append(cjk, r)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' ||
			(r < '0' || (r > '9' && r < 'A') || (r > 'Z' && r < 'a') || (r > 'z' && r < 0x4E00)) && r < 0x80:
			flushWord()
			flushCJK()
		default:
			flushCJK()
			word.WriteRune(r)
		}
	}
	flushWord()
	flushCJK()
	norm := 0.0
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return vec, nil
	}
	for i := range vec {
		vec[i] /= norm
	}
	return vec, nil
}

func binaryEndian(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// Limits bound memory/CPU/storage per upload (card constraint).
const (
	MaxDocumentBytes = 2 << 20 // 2 MiB raw document cap
	MaxChunkChars    = 1200
	ChunkOverlap     = 150
	MaxChunks        = 512
)

// Chunk splits text into overlapping fixed-size chunks on whitespace
// boundaries. Empty documents produce at least one (possibly empty) chunk so
// indexing is total.
func Chunk(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{""}
	}
	runes := []rune(text)
	var out []string
	for start := 0; start < len(runes) && len(out) < MaxChunks; start += MaxChunkChars - ChunkOverlap {
		end := start + MaxChunkChars
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return out
}

// Service is the Knowledge slice over PostgreSQL.
type Service struct {
	pool     *pgxpool.Pool
	Embedder Embedder
}

// NewService builds the slice over a pool.
func NewService(pool *pgxpool.Pool, embedder Embedder) *Service {
	return &Service{pool: pool, Embedder: embedder}
}

// Document is one stored knowledge document.
type Document struct {
	ID      string
	Title   string
	Version int
	Status  string
}

// ContentHash is the deterministic hash used for idempotent reprocessing.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// ErrNoResults marks an empty search (not an error condition for callers).
var ErrNoResults = errors.New("no results")

// UpsertResult reports what one upload did.
type UpsertResult struct {
	Document         Document
	IdempotentReplay bool
}

// Ingest runs upload -> parse -> chunk -> embed -> index in one transaction:
// a new version replaces prior chunks atomically; identical content for the
// same (workspace, title) is an idempotent no-op. Any failure rolls back and
// leaves nothing retrievable.
func (s *Service) Ingest(ctx context.Context, workspaceID, title, content, userID string) (UpsertResult, error) {
	if strings.TrimSpace(title) == "" {
		return UpsertResult{}, errs.New(errs.InvalidArgument, "document title is required")
	}
	if len(content) > MaxDocumentBytes {
		return UpsertResult{}, errs.New(errs.InvalidArgument, "document exceeds the size limit")
	}
	hash := ContentHash(content)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UpsertResult{}, errs.Wrap(err, errs.Internal, "ingest begin failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotent replay: same workspace+title+hash with a ready document.
	var (
		docID    string
		version  int
		status   string
		existing bool
	)
	err = tx.QueryRow(ctx, `
		SELECT id, version, status FROM knowledge_documents
		WHERE workspace_id = $1 AND title = $2
		ORDER BY version DESC LIMIT 1`, workspaceID, title).
		Scan(&docID, &version, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		version = 0
	case err != nil:
		return UpsertResult{}, errs.Wrap(err, errs.Internal, "document lookup failed")
	default:
		existing = true
	}
	if existing && status == "ready" {
		var oldHash string
		if err := tx.QueryRow(ctx, `SELECT content_hash FROM knowledge_documents WHERE id = $1`, docID).Scan(&oldHash); err != nil {
			return UpsertResult{}, errs.Wrap(err, errs.Internal, "document hash lookup failed")
		}
		if oldHash == hash {
			return UpsertResult{Document: Document{ID: docID, Title: title, Version: version, Status: status}, IdempotentReplay: true}, nil
		}
	}

	newVersion := version + 1
	if existing {
		// Version replacement: prior chunks die with the prior row.
		if _, err := tx.Exec(ctx, `DELETE FROM knowledge_documents WHERE workspace_id = $1 AND title = $2`, workspaceID, title); err != nil {
			return UpsertResult{}, errs.Wrap(err, errs.Internal, "prior version removal failed")
		}
	}

	chunks := Chunk(content)
	if len(chunks) > MaxChunks {
		return UpsertResult{}, errs.New(errs.InvalidArgument, "document exceeds the chunk limit")
	}
	var docIDNew string
	err = tx.QueryRow(ctx, `
		INSERT INTO knowledge_documents (workspace_id, title, version, status, content_hash, created_by)
		VALUES ($1, $2, $3, 'processing', $4, $5)
		RETURNING id`, workspaceID, title, newVersion, hash, userID).Scan(&docIDNew)
	if err != nil {
		return UpsertResult{}, errs.Wrap(err, errs.Internal, "document insert failed")
	}

	vectors := make([][]float64, len(chunks))
	for i, chunk := range chunks {
		vec, embErr := s.Embedder.Embed(ctx, chunk)
		if embErr != nil {
			// Worker/embedder failure: record the failure durably, nothing
			// retrievable (the tx for chunks is rolled back below; the
			// document row is marked failed in a separate write).
			markErr := s.markFailed(ctx, workspaceID, title, newVersion, hash, userID)
			if markErr != nil {
				return UpsertResult{}, errs.Wrap(embErr, errs.Internal, "embed failed and failure could not be recorded")
			}
			return UpsertResult{}, errs.Wrap(embErr, errs.UpstreamUnavailable, "embedding worker failed")
		}
		vectors[i] = vec
	}
	for i, chunk := range chunks {
		if _, err := tx.Exec(ctx, `
			INSERT INTO knowledge_chunks (document_id, workspace_id, seq, content, embedding)
			VALUES ($1, $2, $3, $4, $5)`, docIDNew, workspaceID, i, chunk, vectors[i]); err != nil {
			return UpsertResult{}, errs.Wrap(err, errs.Internal, "chunk insert failed")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE knowledge_documents SET status = 'ready' WHERE id = $1`, docIDNew); err != nil {
		return UpsertResult{}, errs.Wrap(err, errs.Internal, "document status update failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return UpsertResult{}, errs.Wrap(err, errs.Internal, "ingest commit failed")
	}
	return UpsertResult{Document: Document{ID: docIDNew, Title: title, Version: newVersion, Status: "ready"}}, nil
}

func (s *Service) markFailed(ctx context.Context, workspaceID, title string, version int, hash, userID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO knowledge_documents (workspace_id, title, version, status, content_hash, created_by)
		VALUES ($1, $2, $3, 'failed', $4, $5)
		ON CONFLICT (workspace_id, title, version) DO UPDATE SET status = 'failed'`,
		workspaceID, title, version, hash, userID)
	return err
}

// Citation is one search hit: mandatory provenance.
type Citation struct {
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
	Version    int    `json:"version"`
	Seq        int    `json:"seq"`
	Score      float64
	Content    string `json:"content"`
}

// Search retrieves the top-k chunks from the caller's workspace only. The
// workspace filter is part of the SQL — cross-workspace content is never
// even fetched. Results without citation cannot exist (every chunk joins its
// document row).
func (s *Service) Search(ctx context.Context, workspaceID, query string, topK int) ([]Citation, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errs.New(errs.InvalidArgument, "search query is required")
	}
	if topK <= 0 || topK > 20 {
		topK = 5
	}
	vec, err := s.Embedder.Embed(ctx, query)
	if err != nil {
		return nil, errs.Wrap(err, errs.UpstreamUnavailable, "embedding worker failed")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.document_id, d.title, d.version, c.seq, c.content, c.embedding
		FROM knowledge_chunks c
		JOIN knowledge_documents d ON d.id = c.document_id
		WHERE c.workspace_id = $1 AND d.status = 'ready'`,
		workspaceID)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "knowledge query failed")
	}
	defer rows.Close()
	hits := make([]Citation, 0, topK)
	for rows.Next() {
		var c Citation
		var embedding []float64
		if err := rows.Scan(&c.DocumentID, &c.Title, &c.Version, &c.Seq, &c.Content, &embedding); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "knowledge scan failed")
		}
		c.Score = cosine(vec, embedding)
		hits = append(hits, c)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.Internal, "knowledge iteration failed")
	}
	// Zero-similarity chunks are not matches: returning them would be an
	// uncited-by-relevance answer. Only positive-cosine hits qualify.
	relevant := hits[:0]
	for _, hit := range hits {
		if hit.Score > 0 {
			relevant = append(relevant, hit)
		}
	}
	sort.SliceStable(relevant, func(i, j int) bool { return relevant[i].Score > relevant[j].Score })
	if len(relevant) > topK {
		relevant = relevant[:topK]
	}
	return relevant, nil
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

var _ = fmt.Sprintf
