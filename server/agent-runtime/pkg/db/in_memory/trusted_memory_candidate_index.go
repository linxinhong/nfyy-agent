package in_memory

import (
	"encoding/binary"
	"hash/maphash"
	"sync/atomic"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

const (
	// These are adapter safety ceilings, independent of mutable policy values.
	// They preserve the frozen 200-record candidate window even if a valid
	// project policy contains an extremely large MaxRecallCandidates value.
	memoryCandidateMaterializationLimit = 200
	memoryCandidateVisitMultiplier      = 64
	memoryCandidateMaxVisitBudget       = memoryCandidateMaterializationLimit * memoryCandidateVisitMultiplier
)

// memoryCandidateVisitLimit saturates before multiplication. The allowance is
// shared by every topic stream, so adversarial filters or stale source epochs
// cannot turn one ListRecords call into a complete hot-scope walk.
func memoryCandidateVisitLimit(candidateLimit int) int {
	if candidateLimit >= memoryCandidateMaterializationLimit {
		return memoryCandidateMaxVisitBudget
	}
	return candidateLimit * memoryCandidateVisitMultiplier
}

type memoryCandidateOrderKey struct {
	UpdatedAt time.Time
	MemoryID  string
}

type memoryCandidateIndexRecord struct {
	OrderKey      memoryCandidateOrderKey
	Generation    uint64
	Kind          memory.Kind
	Topics        []string
	Source        *memoryCandidateSourceIdentity
	EligibleUntil time.Time
}

type memoryCandidateSourceIdentity struct {
	Ref     string
	Version string
	Epoch   uint64
}

type memoryCandidateScopeIndex struct {
	seed maphash.Seed
	// A generation mismatch invalidates the complete visible index in O(1).
	// The lifecycle worker removes old record metadata in bounded purge pages.
	generation uint64
	all        memoryCandidateOrderedSet
	byTopic    map[string]*memoryCandidateOrderedSet
	sources    map[memoryCandidateSourceIdentity]*memoryCandidateSourceIdentity
	sourceRefs map[*memoryCandidateSourceIdentity]int
	recordKeys map[memoryRecordKey]struct{}
}

type memoryCandidateOrderedSet struct {
	root *memoryCandidateTreapNode
	len  int
}

type memoryCandidateTreapNode struct {
	orderKey         memoryCandidateOrderKey
	recordID         string
	priority         uint64
	kind             memory.Kind
	kinds            uint8
	source           *memoryCandidateSourceIdentity
	uniformSource    bool
	eligibleUntil    time.Time
	maxEligibleUntil time.Time
	left             *memoryCandidateTreapNode
	right            *memoryCandidateTreapNode
}

func newMemoryCandidateScopeIndex(generation uint64) *memoryCandidateScopeIndex {
	return &memoryCandidateScopeIndex{
		seed:       maphash.MakeSeed(),
		generation: generation,
		byTopic:    make(map[string]*memoryCandidateOrderedSet),
		sources:    make(map[memoryCandidateSourceIdentity]*memoryCandidateSourceIdentity),
		sourceRefs: make(map[*memoryCandidateSourceIdentity]int),
		recordKeys: make(map[memoryRecordKey]struct{}),
	}
}

func (s *MemoryStore) indexCandidateRecord(record *memory.Record) {
	key := memoryRecordKey{Scope: record.Scope, ID: record.MemoryID}
	s.deindexCandidateRecord(key)
	if record.Status != memory.StatusActive || record.Content == "" {
		return
	}
	subject := s.subjects[record.Scope.Subject()]
	if subject.Disabled || record.SubjectGeneration != generationOrInitial(subject.Generation) {
		return
	}
	eligibleUntil := record.ExpiresAt
	var sourceIdentityValue memoryCandidateSourceIdentity
	if record.Provenance.SourceRef != "" {
		sourceIdentityValue = memoryCandidateSourceIdentity{Ref: record.Provenance.SourceRef, Version: record.Provenance.SourceVersion, Epoch: record.Provenance.SourceEpoch}
		source := s.sources[memorySourceKey{Scope: record.Scope, Ref: record.Provenance.SourceRef}]
		if source.Revoked || source.Epoch != record.Provenance.SourceEpoch || source.Version != record.Provenance.SourceVersion {
			return
		}
		if source.ExpiresAt.Before(eligibleUntil) {
			eligibleUntil = source.ExpiresAt
		}
	}
	index := s.candidateScopes[record.Scope]
	if index == nil || index.generation != record.SubjectGeneration {
		index = newMemoryCandidateScopeIndex(record.SubjectGeneration)
		s.candidateScopes[record.Scope] = index
	}
	var sourceIdentity *memoryCandidateSourceIdentity
	if sourceIdentityValue.Ref != "" {
		sourceIdentity = index.sources[sourceIdentityValue]
		if sourceIdentity == nil {
			identity := sourceIdentityValue
			sourceIdentity = &identity
			index.sources[identity] = sourceIdentity
		}
		index.sourceRefs[sourceIdentity]++
	}
	orderKey := memoryCandidateOrderKey{UpdatedAt: record.UpdatedAt, MemoryID: record.MemoryID}
	metadata := memoryCandidateIndexRecord{
		OrderKey: orderKey, Generation: record.SubjectGeneration, Kind: record.Kind, Topics: uniqueMemoryTopics(record.Topics),
		Source: sourceIdentity, EligibleUntil: eligibleUntil,
	}
	priority := memoryCandidatePriority(index.seed, orderKey)
	s.candidateRecords[key] = metadata
	index.recordKeys[key] = struct{}{}
	index.all.insert(orderKey, key.ID, priority, record.Kind, sourceIdentity, eligibleUntil)
	for _, topic := range metadata.Topics {
		topicSet := index.byTopic[topic]
		if topicSet == nil {
			topicSet = &memoryCandidateOrderedSet{}
			index.byTopic[topic] = topicSet
		}
		topicSet.insert(orderKey, key.ID, priority, record.Kind, sourceIdentity, eligibleUntil)
	}
}

func (s *MemoryStore) deindexCandidateRecord(key memoryRecordKey) {
	metadata, ok := s.candidateRecords[key]
	if !ok {
		return
	}
	index := s.candidateScopes[key.Scope]
	if index != nil && index.generation == metadata.Generation {
		index.all.remove(metadata.OrderKey)
		for _, topic := range metadata.Topics {
			if set := index.byTopic[topic]; set != nil {
				set.remove(metadata.OrderKey)
				if set.len == 0 {
					delete(index.byTopic, topic)
				}
			}
		}
		if metadata.Source != nil {
			if refs := index.sourceRefs[metadata.Source]; refs <= 1 {
				delete(index.sourceRefs, metadata.Source)
				delete(index.sources, *metadata.Source)
			} else {
				index.sourceRefs[metadata.Source] = refs - 1
			}
		}
		delete(index.recordKeys, key)
	}
	delete(s.candidateRecords, key)
	if index != nil && len(index.recordKeys) == 0 {
		delete(s.candidateScopes, key.Scope)
	}
}

func (s *MemoryStore) candidateSets(req memory.ListRequest) []*memoryCandidateOrderedSet {
	index := s.candidateScopes[req.Scope]
	if index == nil {
		return nil
	}
	topics := uniqueMemoryTopics(req.Topics)
	if index.generation != generationOrInitial(s.subjects[req.Scope.Subject()].Generation) {
		return nil
	}
	if len(topics) == 0 {
		return []*memoryCandidateOrderedSet{&index.all}
	}
	sets := make([]*memoryCandidateOrderedSet, 0, len(topics))
	for _, topic := range topics {
		if set := index.byTopic[topic]; set != nil {
			sets = append(sets, set)
		}
	}
	return sets
}

type memoryCandidateMergedIterator struct {
	streams []memoryCandidateIteratorStream
	budget  *memoryCandidateVisitBudget
}

type memoryCandidateVisitBudget struct {
	remaining int
	exhausted bool
	visited   uint64
	examined  *atomic.Uint64
	recorded  bool
}

type memoryCandidateIteratorStream struct {
	iterator memoryCandidateDescendingIterator
	current  *memoryCandidateTreapNode
}

func newMemoryCandidateMergedIterator(store *MemoryStore, scope memory.Scope, sets []*memoryCandidateOrderedSet, cursor *memoryCandidateOrderKey, kinds []memory.Kind, now time.Time, visitLimit int, examined *atomic.Uint64) *memoryCandidateMergedIterator {
	budget := &memoryCandidateVisitBudget{remaining: visitLimit, examined: examined}
	merged := &memoryCandidateMergedIterator{streams: make([]memoryCandidateIteratorStream, 0, len(sets)), budget: budget}
	kindMask := memoryCandidateKindMask(kinds)
	for _, set := range sets {
		iterator := newMemoryCandidateDescendingIterator(store, scope, set.root, cursor, kindMask, now, budget)
		current := iterator.next()
		if current != nil {
			merged.streams = append(merged.streams, memoryCandidateIteratorStream{iterator: iterator, current: current})
		}
	}
	return merged
}

func (m *memoryCandidateMergedIterator) exhausted() bool {
	return m.budget.exhausted
}

func (m *memoryCandidateMergedIterator) recordExamined() {
	if m.budget.recorded {
		return
	}
	m.budget.recorded = true
	m.budget.examined.Add(m.budget.visited)
}

func (m *memoryCandidateMergedIterator) next() (string, bool) {
	best := -1
	for i := range m.streams {
		if m.streams[i].current == nil {
			continue
		}
		if best < 0 || compareMemoryCandidateOrder(m.streams[i].current.orderKey, m.streams[best].current.orderKey) > 0 {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	bestNode := m.streams[best].current
	for i := range m.streams {
		current := m.streams[i].current
		if current != nil && compareMemoryCandidateOrder(current.orderKey, bestNode.orderKey) == 0 {
			m.streams[i].current = m.streams[i].iterator.next()
		}
	}
	return bestNode.recordID, true
}

type memoryCandidateDescendingIterator struct {
	root     *memoryCandidateTreapNode
	store    *MemoryStore
	scope    memory.Scope
	upper    memoryCandidateOrderKey
	hasUpper bool
	kindMask uint8
	now      time.Time
	budget   *memoryCandidateVisitBudget
}

func newMemoryCandidateDescendingIterator(store *MemoryStore, scope memory.Scope, root *memoryCandidateTreapNode, cursor *memoryCandidateOrderKey, kindMask uint8, now time.Time, budget *memoryCandidateVisitBudget) memoryCandidateDescendingIterator {
	iterator := memoryCandidateDescendingIterator{root: root, store: store, scope: scope, kindMask: kindMask, now: now, budget: budget}
	if cursor != nil {
		iterator.upper = *cursor
		iterator.hasUpper = true
	}
	return iterator
}

func (i *memoryCandidateDescendingIterator) next() *memoryCandidateTreapNode {
	var upper *memoryCandidateOrderKey
	if i.hasUpper {
		upper = &i.upper
	}
	node := greatestEligibleMemoryCandidate(i.store, i.scope, i.root, upper, i.kindMask, i.now, i.budget)
	if node == nil {
		return nil
	}
	i.upper = node.orderKey
	i.hasUpper = true
	return node
}

func greatestEligibleMemoryCandidate(store *MemoryStore, scope memory.Scope, root *memoryCandidateTreapNode, upper *memoryCandidateOrderKey, kindMask uint8, now time.Time, budget *memoryCandidateVisitBudget) *memoryCandidateTreapNode {
	if root == nil {
		return nil
	}
	if budget.remaining == 0 {
		budget.exhausted = true
		return nil
	}
	budget.remaining--
	budget.visited++
	if !root.maxEligibleUntil.After(now) {
		return nil
	}
	if kindMask != 0 && root.kinds&kindMask == 0 {
		return nil
	}
	// Source revocation and epoch advancement only update authoritative source
	// state. Uniform-source subtrees make that logical fence immediately
	// invisible without synchronously deleting every record under the store
	// write lock; physical purge later removes those nodes in bounded pages.
	if root.uniformSource && !store.memoryCandidateSourceEligible(scope, root.source, now) {
		return nil
	}
	if upper != nil && compareMemoryCandidateOrder(root.orderKey, *upper) >= 0 {
		return greatestEligibleMemoryCandidate(store, scope, root.left, upper, kindMask, now, budget)
	}
	if candidate := greatestEligibleMemoryCandidate(store, scope, root.right, upper, kindMask, now, budget); candidate != nil {
		return candidate
	}
	if root.eligibleUntil.After(now) && (kindMask == 0 || memoryCandidateKindBit(root.kind)&kindMask != 0) && store.memoryCandidateSourceEligible(scope, root.source, now) {
		return root
	}
	return greatestEligibleMemoryCandidate(store, scope, root.left, nil, kindMask, now, budget)
}

func (s *memoryCandidateOrderedSet) insert(orderKey memoryCandidateOrderKey, recordID string, priority uint64, kind memory.Kind, source *memoryCandidateSourceIdentity, eligibleUntil time.Time) {
	var inserted bool
	bit := memoryCandidateKindBit(kind)
	s.root, inserted = insertMemoryCandidateTreap(s.root, &memoryCandidateTreapNode{orderKey: orderKey, recordID: recordID, priority: priority, kind: kind, kinds: bit, source: source, uniformSource: true, eligibleUntil: eligibleUntil, maxEligibleUntil: eligibleUntil})
	if inserted {
		s.len++
	}
}

func insertMemoryCandidateTreap(root, node *memoryCandidateTreapNode) (*memoryCandidateTreapNode, bool) {
	if root == nil {
		return node, true
	}
	switch comparison := compareMemoryCandidateOrder(node.orderKey, root.orderKey); {
	case comparison < 0:
		var inserted bool
		root.left, inserted = insertMemoryCandidateTreap(root.left, node)
		if root.left.priority > root.priority {
			root = rotateMemoryCandidateRight(root)
		}
		refreshMemoryCandidateTreap(root)
		return root, inserted
	case comparison > 0:
		var inserted bool
		root.right, inserted = insertMemoryCandidateTreap(root.right, node)
		if root.right.priority > root.priority {
			root = rotateMemoryCandidateLeft(root)
		}
		refreshMemoryCandidateTreap(root)
		return root, inserted
	default:
		root.recordID = node.recordID
		root.kind = node.kind
		root.source = node.source
		root.eligibleUntil = node.eligibleUntil
		refreshMemoryCandidateTreap(root)
		return root, false
	}
}

func (s *memoryCandidateOrderedSet) remove(orderKey memoryCandidateOrderKey) {
	var removed bool
	s.root, removed = removeMemoryCandidateTreap(s.root, orderKey)
	if removed {
		s.len--
	}
}

func removeMemoryCandidateTreap(root *memoryCandidateTreapNode, orderKey memoryCandidateOrderKey) (*memoryCandidateTreapNode, bool) {
	if root == nil {
		return nil, false
	}
	switch comparison := compareMemoryCandidateOrder(orderKey, root.orderKey); {
	case comparison < 0:
		var removed bool
		root.left, removed = removeMemoryCandidateTreap(root.left, orderKey)
		refreshMemoryCandidateTreap(root)
		return root, removed
	case comparison > 0:
		var removed bool
		root.right, removed = removeMemoryCandidateTreap(root.right, orderKey)
		refreshMemoryCandidateTreap(root)
		return root, removed
	default:
		return mergeMemoryCandidateTreaps(root.left, root.right), true
	}
}

func mergeMemoryCandidateTreaps(left, right *memoryCandidateTreapNode) *memoryCandidateTreapNode {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	if left.priority > right.priority {
		left.right = mergeMemoryCandidateTreaps(left.right, right)
		refreshMemoryCandidateTreap(left)
		return left
	}
	right.left = mergeMemoryCandidateTreaps(left, right.left)
	refreshMemoryCandidateTreap(right)
	return right
}

func rotateMemoryCandidateLeft(root *memoryCandidateTreapNode) *memoryCandidateTreapNode {
	next := root.right
	root.right = next.left
	next.left = root
	refreshMemoryCandidateTreap(root)
	refreshMemoryCandidateTreap(next)
	return next
}

func rotateMemoryCandidateRight(root *memoryCandidateTreapNode) *memoryCandidateTreapNode {
	next := root.left
	root.left = next.right
	next.right = root
	refreshMemoryCandidateTreap(root)
	refreshMemoryCandidateTreap(next)
	return next
}

func refreshMemoryCandidateTreap(node *memoryCandidateTreapNode) {
	if node == nil {
		return
	}
	node.maxEligibleUntil = node.eligibleUntil
	node.kinds = memoryCandidateKindBit(node.kind)
	node.uniformSource = true
	if node.left != nil && node.left.maxEligibleUntil.After(node.maxEligibleUntil) {
		node.maxEligibleUntil = node.left.maxEligibleUntil
	}
	if node.left != nil {
		node.kinds |= node.left.kinds
		node.uniformSource = node.uniformSource && node.left.uniformSource && node.left.source == node.source
	}
	if node.right != nil && node.right.maxEligibleUntil.After(node.maxEligibleUntil) {
		node.maxEligibleUntil = node.right.maxEligibleUntil
	}
	if node.right != nil {
		node.kinds |= node.right.kinds
		node.uniformSource = node.uniformSource && node.right.uniformSource && node.right.source == node.source
	}
}

func (s *MemoryStore) memoryCandidateSourceEligible(scope memory.Scope, source *memoryCandidateSourceIdentity, now time.Time) bool {
	if source == nil {
		return true
	}
	state, ok := s.sources[memorySourceKey{Scope: scope, Ref: source.Ref}]
	return ok && !state.Revoked && state.Epoch == source.Epoch && state.Version == source.Version && state.ExpiresAt.After(now)
}

func memoryCandidateKindBit(kind memory.Kind) uint8 {
	switch kind {
	case memory.KindUserStatement:
		return 1 << 0
	case memory.KindPreference:
		return 1 << 1
	case memory.KindVerifiedFact:
		return 1 << 2
	default:
		return 0
	}
}

func memoryCandidateKindMask(kinds []memory.Kind) uint8 {
	var result uint8
	for _, kind := range kinds {
		result |= memoryCandidateKindBit(kind)
	}
	return result
}

func compareMemoryCandidateOrder(a, b memoryCandidateOrderKey) int {
	if a.UpdatedAt.Before(b.UpdatedAt) {
		return -1
	}
	if a.UpdatedAt.After(b.UpdatedAt) {
		return 1
	}
	if a.MemoryID < b.MemoryID {
		return -1
	}
	if a.MemoryID > b.MemoryID {
		return 1
	}
	return 0
}

func memoryCandidatePriority(seed maphash.Seed, key memoryCandidateOrderKey) uint64 {
	var hash maphash.Hash
	hash.SetSeed(seed)
	var encodedTime [8]byte
	binary.LittleEndian.PutUint64(encodedTime[:], uint64(key.UpdatedAt.UnixNano()))
	_, _ = hash.Write(encodedTime[:])
	_, _ = hash.WriteString(key.MemoryID)
	return hash.Sum64()
}

func uniqueMemoryKinds(values []memory.Kind) []memory.Kind {
	result := make([]memory.Kind, 0, len(values))
	seen := make(map[memory.Kind]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func uniqueMemoryTopics(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
