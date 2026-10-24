package memory

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const listCursorLifetime = 15 * time.Minute

type ListPageRequest struct {
	Limit  int
	Cursor string
}

type ListPageResult struct {
	ListResult
	NextCursor string `json:"next_cursor,omitempty"`
}

type listCursorClaims struct {
	Version     uint8  `json:"v"`
	KeyID       string `json:"kid"`
	Scope       Scope  `json:"scope"`
	Generation  uint64 `json:"generation"`
	UpdatedAtNS int64  `json:"updated_at_ns"`
	MemoryID    string `json:"memory_id"`
	ExpiresAtNS int64  `json:"expires_at_ns"`
}

func (m *Module) ListPage(ctx context.Context, p Principal, scope Scope, req ListPageRequest) (ListPageResult, error) {
	ctx, cancel := m.crudContext(ctx)
	defer cancel()
	if err := m.authorizeScope(ctx, p, scope, ActionList); err != nil {
		return ListPageResult{}, err
	}
	if req.Limit == 0 {
		req.Limit = m.limits.DefaultListLimit
	}
	if req.Limit < 0 || req.Limit > m.limits.MaxListLimit {
		return ListPageResult{}, fmt.Errorf("%w: list limit", ErrInvalid)
	}
	storeReq := ListRequest{Scope: scope, Limit: req.Limit, CandidateLimit: m.limits.MaxRecallCandidates, CandidateBytes: m.limits.MaxCandidateBytes}
	var cursor *listCursorClaims
	if req.Cursor != "" {
		claims, err := m.verifyListCursor(req.Cursor)
		if err != nil {
			return ListPageResult{}, err
		}
		if claims.Scope != scope {
			return ListPageResult{}, ErrNotFound
		}
		if claims.ExpiresAtNS <= time.Now().UTC().UnixNano() {
			return ListPageResult{}, ErrExpired
		}
		cursor = &claims
		storeReq.AfterScope, storeReq.AfterUpdatedAt, storeReq.AfterMemoryID = scope, time.Unix(0, claims.UpdatedAtNS).UTC(), claims.MemoryID
	}
	if err := m.acquire(); err != nil {
		return ListPageResult{}, err
	}
	result, err := m.store.ListRecords(ctx, storeReq)
	m.release()
	if err != nil {
		return ListPageResult{}, err
	}
	if cursor != nil && cursor.Generation != result.Generation {
		return ListPageResult{}, fmt.Errorf("%w: list cursor generation", ErrConflict)
	}
	out, err := m.boundListResult(result, req.Limit)
	if err != nil {
		return ListPageResult{}, err
	}
	page := ListPageResult{ListResult: out}
	if out.Truncated && len(out.Records) > 0 {
		last := out.Records[len(out.Records)-1]
		page.NextCursor, err = m.signListCursor(listCursorClaims{Version: 1, KeyID: m.currentKey, Scope: scope, Generation: out.Generation, UpdatedAtNS: last.UpdatedAt.UnixNano(), MemoryID: last.MemoryID, ExpiresAtNS: time.Now().UTC().Add(listCursorLifetime).UnixNano()})
		if err != nil {
			return ListPageResult{}, err
		}
	}
	return page, nil
}

func (m *Module) boundListResult(result ListResult, limit int) (ListResult, error) {
	out := result
	out.Records = make([]Record, 0, len(result.Records))
	empty := out
	encodedEmpty, err := json.Marshal(empty)
	if err != nil {
		return ListResult{}, err
	}
	used := len(encodedEmpty)
	for i := range result.Records {
		r := result.Records[i]
		if r.Status != StatusActive || r.SubjectGeneration != result.Generation || r.Expired(result.StoreNow) {
			continue
		}
		encodedRecord, err := json.Marshal(r)
		if err != nil {
			return ListResult{}, err
		}
		delta := len(encodedRecord)
		if len(out.Records) > 0 {
			delta++
		}
		if used+delta > m.limits.MaxListBytes {
			out.Truncated = true
			break
		}
		out.Records = append(out.Records, *cloneRecord(&r))
		used += delta
		if len(out.Records) == limit {
			out.Truncated = out.Truncated || i+1 < len(result.Records)
			break
		}
	}
	return out, nil
}

func (m *Module) signListCursor(claims listCursorClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, m.keys[claims.KeyID])
	_, _ = mac.Write([]byte("memory-list-cursor:" + encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (m *Module) verifyListCursor(cursor string) (listCursorClaims, error) {
	var claims listCursorClaims
	if len(cursor) > maxOperationTokenBytes {
		return claims, fmt.Errorf("%w: list cursor", ErrInvalid)
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 {
		return claims, fmt.Errorf("%w: list cursor", ErrInvalid)
	}
	payload, payloadErr := base64.RawURLEncoding.DecodeString(parts[0])
	signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[1])
	if payloadErr != nil || signatureErr != nil || json.Unmarshal(payload, &claims) != nil || claims.Version != 1 || claims.KeyID == "" || claims.MemoryID == "" || claims.Generation == 0 || claims.UpdatedAtNS == 0 {
		return listCursorClaims{}, fmt.Errorf("%w: list cursor", ErrInvalid)
	}
	key, ok := m.keys[claims.KeyID]
	if !ok {
		return listCursorClaims{}, fmt.Errorf("%w: list cursor key", ErrUnauthorized)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("memory-list-cursor:" + parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return listCursorClaims{}, fmt.Errorf("%w: list cursor signature", ErrUnauthorized)
	}
	return claims, nil
}
