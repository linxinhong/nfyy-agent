// Package blob stores contribution attachments content-addressed by SHA-256
// under a local or NAS-mounted root, indexed in the blobs table.
//
// Why: local agents upload attachments before the contribution that cites
// them, so the server must verify the bytes it was promised and reject
// anything that is not a document or image.
//
// Invariants and pitfalls:
//   - Bytes are streamed to a temporary file, hashed and size-capped while
//     streaming, and renamed into place only when the hash matches the URL.
//   - The declared media type must be allowlisted and agree with sniffing;
//     executables and archives are never accepted.
//   - Paths are derived from the Workspace id and the hash only; no client
//     string ever reaches the filesystem path.
//   - With a Scanner configured, every new blob is scanned before it is
//     indexed: infected bytes are refused (never stored) and a scanner
//     failure refuses the upload (fail closed). Without one, blobs are
//     honestly 'unscanned'. Blobs stored before a scanner was configured are
//     not rescanned when re-uploaded.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/audit"
)

// MaxSize is the largest accepted attachment (contract: 50 MiB).
const MaxSize = 50 << 20

var (
	ErrInvalid      = errors.New("invalid blob")
	ErrHashMismatch = errors.New("blob bytes do not match the sha256 in the path")
	ErrTooLarge     = errors.New("blob exceeds 50 MiB")
	ErrMediaType    = errors.New("media type not allowed or does not match the content")
	ErrUnavailable  = errors.New("blob storage is not configured")
	ErrBlocked      = errors.New("attachment blocked by the malware scan")
	ErrScan         = errors.New("malware scan unavailable")

	shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	wsPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// allowed maps declared media types to the sniffed prefixes they may carry.
// Office formats sniff as zip; plain text formats sniff as text/plain.
var allowed = map[string][]string{
	"application/pdf":  {"application/pdf"},
	"image/png":        {"image/png"},
	"image/jpeg":       {"image/jpeg"},
	"image/gif":        {"image/gif"},
	"image/webp":       {"image/webp"},
	"text/plain":       {"text/plain"},
	"text/markdown":    {"text/plain"},
	"text/csv":         {"text/plain"},
	"application/json": {"text/plain"},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {"application/zip"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {"application/zip"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {"application/zip"},
}

// IsText reports whether a media type is text the DLP engine should scan.
func IsText(mediaType string) bool {
	return mediaType == "text/plain" || mediaType == "text/markdown" || mediaType == "text/csv" || mediaType == "application/json"
}

// Blob is the index row of one stored attachment.
type Blob struct {
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	MediaType  string `json:"media_type"`
	ScanStatus string `json:"scan_status"`
}

// TxAudit writes audit events inside a caller-owned transaction.
type TxAudit interface {
	WriteBatchTx(ctx context.Context, tx pgx.Tx, events []audit.Event) error
}

// Store writes blobs under root and indexes them in PostgreSQL.
type Store struct {
	pool    *pgxpool.Pool
	audit   TxAudit
	root    string
	scanner Scanner
}

// WithScanner makes every new upload pass a malware scan.
func (s *Store) WithScanner(scanner Scanner) *Store {
	s.scanner = scanner
	return s
}

// New returns a store; an empty root disables uploads (ErrUnavailable).
func New(pool *pgxpool.Pool, txAudit TxAudit, root string) *Store {
	return &Store{pool: pool, audit: txAudit, root: root}
}

// NormalizeMediaType strips parameters and lowercases.
func NormalizeMediaType(header string) string {
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return strings.ToLower(mt)
}

func (s *Store) path(workspaceID, sum string) string {
	return filepath.Join(s.root, workspaceID, sum[:2], sum)
}

// Lookup returns the index row or pgx.ErrNoRows.
func (s *Store) Lookup(ctx context.Context, workspaceID, sum string) (Blob, error) {
	var b Blob
	err := s.pool.QueryRow(ctx, `SELECT sha256, size, media_type, scan_status FROM blobs WHERE workspace_id = $1 AND sha256 = $2`,
		workspaceID, sum).Scan(&b.SHA256, &b.Size, &b.MediaType, &b.ScanStatus)
	return b, err
}

// Put stores body as sum; it returns (blob, created). An existing blob with
// the same hash is returned without reading body.
func (s *Store) Put(ctx context.Context, workspaceID, uploader, sum, mediaType string, body io.Reader, ev audit.Event) (Blob, bool, error) {
	if s.root == "" {
		return Blob{}, false, ErrUnavailable
	}
	if !shaPattern.MatchString(sum) || !wsPattern.MatchString(workspaceID) {
		return Blob{}, false, ErrInvalid
	}
	sniffAs, ok := allowed[mediaType]
	if !ok {
		return Blob{}, false, ErrMediaType
	}
	if existing, err := s.Lookup(ctx, workspaceID, sum); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Blob{}, false, fmt.Errorf("lookup blob: %w", err)
	}

	dir := filepath.Dir(s.path(workspaceID, sum))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Blob{}, false, fmt.Errorf("blob dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return Blob{}, false, fmt.Errorf("blob temp: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	hash := sha256.New()
	head := make([]byte, 0, 512)
	size, err := io.Copy(io.MultiWriter(tmp, hash, &capture{buf: &head}), io.LimitReader(body, MaxSize+1))
	closeErr := tmp.Close()
	if err != nil {
		return Blob{}, false, fmt.Errorf("blob write: %w", err)
	}
	if closeErr != nil {
		return Blob{}, false, fmt.Errorf("blob close: %w", closeErr)
	}
	switch {
	case size > MaxSize:
		return Blob{}, false, ErrTooLarge
	case size == 0:
		return Blob{}, false, ErrInvalid
	case hex.EncodeToString(hash.Sum(nil)) != sum:
		return Blob{}, false, ErrHashMismatch
	}
	sniffed := http.DetectContentType(head)
	match := false
	for _, prefix := range sniffAs {
		match = match || strings.HasPrefix(sniffed, prefix)
	}
	if !match {
		return Blob{}, false, ErrMediaType
	}
	status := "unscanned"
	if s.scanner != nil {
		verdict, err := s.scan(ctx, tmp.Name())
		if err != nil {
			return Blob{}, false, fmt.Errorf("%w: %v", ErrScan, err)
		}
		if !verdict.Clean {
			blocked := ev
			blocked.Type = "blob.blocked"
			blocked.Outcome = audit.OutcomeDenied
			blocked.Resource = "blob/" + sum
			blocked.Detail = map[string]any{"size": size, "media_type": mediaType, "signature": verdict.Signature}
			if err := s.auditAlone(ctx, blocked); err != nil {
				return Blob{}, false, err
			}
			return Blob{}, false, ErrBlocked
		}
		status = "clean"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Blob{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO blobs (workspace_id, sha256, size, media_type, scan_status, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, workspaceID, sum, size, mediaType, status, uploader)
	if err != nil {
		return Blob{}, false, fmt.Errorf("index blob: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path(workspaceID, sum)); err != nil {
		return Blob{}, false, fmt.Errorf("place blob: %w", err)
	}
	ev.Type = "blob.stored"
	ev.Resource = "blob/" + sum
	ev.Detail = map[string]any{"size": size, "media_type": mediaType, "created": tag.RowsAffected() == 1}
	if err := s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev}); err != nil {
		return Blob{}, false, fmt.Errorf("blob audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Blob{}, false, fmt.Errorf("commit: %w", err)
	}
	stored, err := s.Lookup(ctx, workspaceID, sum)
	return stored, tag.RowsAffected() == 1, err
}

// Open returns the stored bytes of an indexed blob; the caller closes it.
func (s *Store) Open(ctx context.Context, workspaceID, sum string) (*os.File, Blob, error) {
	b, err := s.Lookup(ctx, workspaceID, sum)
	if err != nil {
		return nil, Blob{}, err
	}
	f, err := os.Open(s.path(workspaceID, sum))
	return f, b, err
}

// capture keeps the first 512 bytes for content sniffing.
type capture struct{ buf *[]byte }

func (c *capture) Write(p []byte) (int, error) {
	if room := 512 - len(*c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		*c.buf = append(*c.buf, p[:room]...)
	}
	return len(p), nil
}

func (s *Store) scan(ctx context.Context, name string) (Verdict, error) {
	f, err := os.Open(name)
	if err != nil {
		return Verdict{}, err
	}
	defer func() { _ = f.Close() }()
	return s.scanner.Scan(ctx, f)
}

func (s *Store) auditAlone(ctx context.Context, ev audit.Event) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev}); err != nil {
		return fmt.Errorf("blob audit: %w", err)
	}
	return tx.Commit(ctx)
}
