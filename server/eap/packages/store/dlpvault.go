package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
)

// DLP token vault (TASK-028): originals behind tokenize-class pseudonyms.
// Same value in the same workspace keeps one stable token; reveal is a
// governed, audited gateway operation scoped to the owning workspace.

// TokenizeDLP maps a sensitive value to its stable pseudonym, creating the
// vault row on first sight. workspaceID may be empty (path without workspace
// context); those tokens are revealable only inside their workspace or by
// the NULL-workspace fallback check.
func (s *Store) TokenizeDLP(ctx context.Context, workspaceID, ruleName, value string) (string, error) {
	var token string
	err := s.pool.QueryRow(ctx, `
		SELECT token FROM dlp_tokens
		WHERE workspace_id IS NOT DISTINCT FROM $1 AND rule_name = $2 AND original_value = $3`,
		nullableText(workspaceID), ruleName, value).Scan(&token)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("dlp token lookup: %w", err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		raw := make([]byte, 6)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("dlp token entropy: %w", err)
		}
		candidate := hex.EncodeToString(raw)
		err = s.pool.QueryRow(ctx, `
			INSERT INTO dlp_tokens (workspace_id, rule_name, token, original_value)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (workspace_id, rule_name, original_value) DO UPDATE
			    SET rule_name = EXCLUDED.rule_name
			RETURNING token`,
			nullableText(workspaceID), ruleName, candidate, value).Scan(&token)
		if err == nil {
			return token, nil
		}
		if isUniqueViolation(err) {
			// Either the token collided (retry) or a concurrent request
			// created the mapping (the SELECT above returns it next time).
			var existing string
			if err2 := s.pool.QueryRow(ctx, `
				SELECT token FROM dlp_tokens
				WHERE workspace_id IS NOT DISTINCT FROM $1 AND rule_name = $2 AND original_value = $3`,
				nullableText(workspaceID), ruleName, value).Scan(&existing); err2 == nil {
				return existing, nil
			}
			continue
		}
		return "", fmt.Errorf("dlp token insert: %w", err)
	}
	return "", fmt.Errorf("dlp token insert exhausted retries")
}

// RevealDLP restores the original for one token within the caller's
// workspace authority. Unknown or foreign-workspace tokens are
// indistinguishable (fail closed).
func (s *Store) RevealDLP(ctx context.Context, workspaceID, token string) (ruleName, value string, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT rule_name, original_value FROM dlp_tokens
		WHERE token = $1 AND (workspace_id IS NOT DISTINCT FROM $2 OR workspace_id IS NULL)`,
		token, nullableText(workspaceID)).Scan(&ruleName, &value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", pgx.ErrNoRows
	}
	if err != nil {
		return "", "", fmt.Errorf("dlp reveal lookup: %w", err)
	}
	return ruleName, value, nil
}

// DLPReveal is one vault reveal result.
type DLPReveal struct {
	RuleName string
	Value    string
}

// tokenizeMemo is a bounded process-local value->token cache over the vault
// (TASK-043). A value's pseudonym is immutable once minted (the vault's
// DO UPDATE keeps the pre-existing winning token), so serving repeats from
// memory cannot diverge from the vault. It is server-side only — never
// persisted, never logged — bounded by maxEntries with random eviction, and
// dropping it must not (and does not) change behavior, only the read count.
type tokenizeMemo struct {
	mu      sync.Mutex
	max     int
	entries map[dlpMemoKey]string
}

type dlpMemoKey struct {
	workspace string // nullableText form ('' for global rules)
	rule      string
	value     string
}

const dlpMemoMaxEntries = 10000

func newTokenizeMemo() *tokenizeMemo {
	return &tokenizeMemo{max: dlpMemoMaxEntries, entries: make(map[dlpMemoKey]string)}
}

func (m *tokenizeMemo) get(k dlpMemoKey) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.entries[k]
	return t, ok
}

func (m *tokenizeMemo) put(k dlpMemoKey, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= m.max {
		for mk := range m.entries { // random eviction: map iteration order
			delete(m.entries, mk)
			break
		}
	}
	m.entries[k] = token
}

// BatchTokenizeDLP maps many values of one rule to their stable pseudonyms
// in a bounded number of statements (one SELECT plus at most one batched
// INSERT), instead of two round trips per value. Pseudonyms keep the exact
// TokenizeDLP semantics: workspace-scoped, deterministic per value, and on
// a concurrent first sight the pre-existing vault row wins.
func (s *Store) BatchTokenizeDLP(ctx context.Context, workspaceID, ruleName string, values []string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	if len(values) == 0 {
		return out, nil
	}
	if s.tokenizeMemo == nil {
		s.tokenizeMemo = newTokenizeMemo()
	}
	wsKey := nullableText(workspaceID).(string)
	// Memo hits skip the vault read entirely (TASK-043); misses go to the
	// existing batched path. The memo never answers a reveal.
	var uncached []string
	for _, v := range values {
		k := dlpMemoKey{workspace: wsKey, rule: ruleName, value: v}
		if tok, ok := s.tokenizeMemo.get(k); ok {
			out[v] = tok
			continue
		}
		uncached = append(uncached, v)
	}
	values = uncached
	if len(values) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT original_value, token FROM dlp_tokens
		WHERE workspace_id IS NOT DISTINCT FROM $1 AND rule_name = $2 AND original_value = ANY($3)`,
		wsKey, ruleName, values)
	if err != nil {
		return nil, fmt.Errorf("dlp token batch lookup: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value, token string
		if err := rows.Scan(&value, &token); err != nil {
			return nil, fmt.Errorf("dlp token batch scan: %w", err)
		}
		out[value] = token
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dlp token batch rows: %w", err)
	}

	var missing []string
	for _, v := range values {
		if _, ok := out[v]; !ok {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	tokens := make([]string, len(missing))
	for i := range tokens {
		raw := make([]byte, 6)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("dlp token entropy: %w", err)
		}
		tokens[i] = hex.EncodeToString(raw)
	}
	// DO UPDATE (not DO NOTHING) so RETURNING emits every row — including
	// rows a concurrent request just created — carrying the winning token.
	// unnest yields columns in argument order: (value, token).
	insertRows, err := s.pool.Query(ctx, `
		INSERT INTO dlp_tokens (workspace_id, rule_name, token, original_value)
		SELECT $1, $2, u.token, u.value
		FROM unnest($3::text[], $4::text[]) AS u(value, token)
		ON CONFLICT (workspace_id, rule_name, original_value) DO UPDATE
		    SET rule_name = EXCLUDED.rule_name
		RETURNING original_value, token`,
		nullableText(workspaceID), ruleName, missing, tokens)
	if err != nil {
		return nil, fmt.Errorf("dlp token batch insert: %w", err)
	}
	defer insertRows.Close()
	for insertRows.Next() {
		var value, token string
		if err := insertRows.Scan(&value, &token); err != nil {
			return nil, fmt.Errorf("dlp token batch insert scan: %w", err)
		}
		out[value] = token
	}
	if err := insertRows.Err(); err != nil {
		return nil, fmt.Errorf("dlp token batch insert rows: %w", err)
	}
	for _, v := range missing {
		if _, ok := out[v]; !ok {
			return nil, fmt.Errorf("dlp token batch insert lost value %q", v)
		}
	}
	// Memoize every resolved mapping (SELECT hits and freshly minted tokens
	// alike); entries are immutable and bounded.
	for _, v := range values {
		if tok, ok := out[v]; ok {
			s.tokenizeMemo.put(dlpMemoKey{workspace: wsKey, rule: ruleName, value: v}, tok)
		}
	}
	return out, nil
}

// BatchRevealDLP restores originals for many tokens within the caller's
// workspace authority in one query. Unknown and foreign-workspace tokens
// are simply absent from the result (fail closed, indistinguishable).
func (s *Store) BatchRevealDLP(ctx context.Context, workspaceID string, tokens []string) (map[string]DLPReveal, error) {
	out := make(map[string]DLPReveal, len(tokens))
	if len(tokens) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT token, rule_name, original_value FROM dlp_tokens
		WHERE token = ANY($1) AND (workspace_id IS NOT DISTINCT FROM $2 OR workspace_id IS NULL)`,
		tokens, nullableText(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("dlp reveal batch lookup: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var token string
		var r DLPReveal
		if err := rows.Scan(&token, &r.RuleName, &r.Value); err != nil {
			return nil, fmt.Errorf("dlp reveal batch scan: %w", err)
		}
		out[token] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dlp reveal batch rows: %w", err)
	}
	return out, nil
}

func nullableTextArray(v []string) any {
	if v == nil {
		return nil
	}
	return v
}

func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
