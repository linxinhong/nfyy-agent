package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxRecallQueryBytes = 256
	MaxRecallKinds      = 3
	MaxRecallTopics     = 32
	MaxRecallTopicBytes = 128
)

// Recall returns a deterministic, finite selection from one authoritative
// store snapshot. It never caches record content across calls.
func (m *Module) Recall(ctx context.Context, p Principal, scope Scope, req RecallRequest) (result RecallResult, err error) {
	started := time.Now()
	trace := RecallTelemetry{}
	phase := "authorize"
	ctx, cancel := context.WithTimeout(ctx, m.limits.RecallTimeout)
	defer cancel()
	defer func() {
		if m.recallObserver == nil {
			return
		}
		trace.Total = time.Since(started)
		if errors.Is(err, context.DeadlineExceeded) {
			trace.DeadlineAt = phase
		}
		m.recallObserver.ObserveRecall(ctx, trace)
	}()
	if err := m.authorizeScope(ctx, p, scope, ActionRecall); err != nil {
		return RecallResult{}, err
	}
	if !utf8.ValidString(req.Query) || len(req.Query) > MaxRecallQueryBytes {
		return RecallResult{}, fmt.Errorf("%w: recall query", ErrInvalid)
	}
	if req.Limit == 0 {
		req.Limit = m.limits.DefaultRecallLimit
	}
	if req.Limit < 0 || req.Limit > m.limits.MaxRecallLimit || !validRecallFilters(req.Kinds, req.Topics) {
		return RecallResult{}, fmt.Errorf("%w: recall bounds", ErrInvalid)
	}
	if err := m.acquire(); err != nil {
		return RecallResult{}, err
	}
	defer m.release()
	phase = "list_records"
	listStarted := time.Now()
	listed, err := m.store.ListRecords(ctx, ListRequest{
		Scope: scope, Limit: m.limits.MaxRecallCandidates,
		Kinds: append([]Kind(nil), req.Kinds...), Topics: append([]string(nil), req.Topics...),
		CandidateLimit: m.limits.MaxRecallCandidates, CandidateBytes: m.limits.MaxCandidateBytes,
	})
	trace.ListRecords = time.Since(listStarted)
	if err != nil {
		return RecallResult{}, err
	}
	phase = "after_list_records"
	if err := ctx.Err(); err != nil {
		return RecallResult{}, err
	}
	if len(listed.Records) > m.limits.MaxRecallCandidates {
		return RecallResult{}, fmt.Errorf("%w: store exceeded recall candidate limit", ErrUnavailable)
	}
	candidateBytes := 2 // JSON array delimiters.
	var candidateJSON bytes.Buffer
	candidateEncoder := json.NewEncoder(&candidateJSON)
	for i := range listed.Records {
		candidateJSON.Reset()
		if err := candidateEncoder.Encode(&listed.Records[i]); err != nil {
			return RecallResult{}, fmt.Errorf("%w: invalid store record", ErrUnavailable)
		}
		// Encoder uses the same JSON representation as Marshal, plus a newline.
		candidateBytes += candidateJSON.Len() - 1
		if i > 0 {
			candidateBytes++
		}
		if candidateBytes > m.limits.MaxCandidateBytes {
			return RecallResult{}, fmt.Errorf("%w: store exceeded recall candidate bytes", ErrUnavailable)
		}
	}
	phase = "ranking"
	rankingStarted := time.Now()
	defer func() { trace.Ranking = time.Since(rankingStarted) }()

	type ranked struct {
		item      RecallItem
		score     int
		updatedAt time.Time
	}
	queryTokens := tokenSet(req.Query)
	normalizedQuery := strings.ToLower(strings.TrimSpace(req.Query))
	rankedItems := make([]ranked, 0, len(listed.Records))
	for i := range listed.Records {
		if err := ctx.Err(); err != nil {
			return RecallResult{}, err
		}
		r := listed.Records[i]
		// Adapters own the filter-before-limit guarantee. These checks are a
		// second fail-closed boundary against a non-conforming adapter.
		if r.Scope != scope || r.Status != StatusActive || r.SubjectGeneration != listed.Generation || r.Expired(listed.StoreNow) || !matchesFilters(r, req) {
			return RecallResult{}, fmt.Errorf("%w: store returned ineligible recall candidate", ErrUnavailable)
		}
		item := RecallItem{
			MemoryID: r.MemoryID, Revision: r.Revision, SubjectGeneration: r.SubjectGeneration,
			PolicyGeneration: r.PolicyGeneration, CollectionProposalEpoch: r.CollectionProposalEpoch,
			Kind: r.Kind, Content: r.Content, Topics: append([]string(nil), r.Topics...),
			SourceRef: r.Provenance.SourceRef, SourceVersion: r.Provenance.SourceVersion,
			SourceEpoch: r.Provenance.SourceEpoch, ExpiresAt: r.ExpiresAt,
		}
		rankedItems = append(rankedItems, ranked{item: item, score: relevance(normalizedQuery, queryTokens, r), updatedAt: r.UpdatedAt})
	}
	sort.SliceStable(rankedItems, func(i, j int) bool {
		if rankedItems[i].score != rankedItems[j].score {
			return rankedItems[i].score > rankedItems[j].score
		}
		if !rankedItems[i].updatedAt.Equal(rankedItems[j].updatedAt) {
			return rankedItems[i].updatedAt.After(rankedItems[j].updatedAt)
		}
		return rankedItems[i].item.MemoryID > rankedItems[j].item.MemoryID
	})

	out := RecallResult{StoreNow: listed.StoreNow, Generation: listed.Generation, CandidateCount: len(listed.Records), CandidateTruncated: listed.Truncated}
	for i := range rankedItems {
		if len(out.Items) == req.Limit {
			out.OutputTruncated = i < len(rankedItems)
			break
		}
		encoded, marshalErr := json.Marshal(rankedItems[i].item)
		if marshalErr != nil {
			return RecallResult{}, marshalErr
		}
		delta := len(encoded)
		if len(out.Items) > 0 {
			delta++
		}
		if out.OutputBytes+delta > m.limits.MaxRecallBytes {
			out.OutputTruncated = true
			break
		}
		out.Items = append(out.Items, rankedItems[i].item)
		out.OutputBytes += delta
		if out.EarliestExpiresAt.IsZero() || rankedItems[i].item.ExpiresAt.Before(out.EarliestExpiresAt) {
			out.EarliestExpiresAt = rankedItems[i].item.ExpiresAt
		}
	}
	return out, nil
}

func validRecallFilters(kinds []Kind, topics []string) bool {
	if len(kinds) > MaxRecallKinds || len(topics) > MaxRecallTopics {
		return false
	}
	for _, k := range kinds {
		if k != KindUserStatement && k != KindPreference && k != KindVerifiedFact {
			return false
		}
	}
	for _, topic := range topics {
		if topic == "" || !utf8.ValidString(topic) || len(topic) > MaxRecallTopicBytes {
			return false
		}
	}
	return true
}

func matchesFilters(r Record, req RecallRequest) bool {
	if len(req.Kinds) > 0 {
		ok := false
		for _, k := range req.Kinds {
			if r.Kind == k {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(req.Topics) > 0 {
		for _, want := range req.Topics {
			for _, got := range r.Topics {
				if got == want {
					return true
				}
			}
		}
		return false
	}
	return true
}

func tokenSet(s string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		set[token] = struct{}{}
	}
	return set
}

func relevance(normalizedQuery string, query map[string]struct{}, r Record) int {
	if len(query) == 0 {
		return 0
	}
	haystack := strings.ToLower(r.Content + " " + strings.Join(r.Topics, " "))
	score := 0
	if normalizedQuery != "" && strings.Contains(haystack, normalizedQuery) {
		score += 100
	}
	// Only matching query tokens affect the score. Scan the same Unicode
	// letter/number boundaries without allocating every body token and span.
	seen := make(map[string]struct{})
	match := func(token string) {
		if _, ok := query[token]; ok {
			if _, duplicate := seen[token]; !duplicate {
				seen[token] = struct{}{}
				score += 10
			}
		}
	}
	start := -1
	for i, r := range haystack {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			match(haystack[start:i])
			start = -1
		}
	}
	if start >= 0 {
		match(haystack[start:])
	}
	return score
}
