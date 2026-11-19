package api

import (
	"encoding/json"
	"slices"
	"sync"
	"time"
)

const (
	defaultAnthropicThreadIdleTTL       = 10 * time.Minute
	defaultAnthropicThreadMaxAge        = time.Hour
	defaultAnthropicThreadMaxBytes      = 256 << 20
	defaultAnthropicThreadMaxScopeBytes = 64 << 20
)

type anthropicThreadScope struct {
	projectID int
	apiKeyID  int
}

type anthropicThreadKey struct {
	scope     anthropicThreadScope
	messageID string
}

type anthropicThreadFingerprint struct {
	model             string
	toolChoice        string
	thinking          string
	mcpServers        string
	contextManagement string
	outputConfig      string
	containerSkills   string
	cacheControl      string
	anthropicBeta     string
}

type anthropicStoredMessage struct {
	raw           json.RawMessage
	cacheControls int
}

func (m anthropicStoredMessage) byteSize() int {
	// Include slice header and small metadata overhead alongside raw JSON bytes.
	return len(m.raw) + 32
}

type anthropicThreadRoot struct {
	scope             anthropicThreadScope
	system            json.RawMessage
	tools             json.RawMessage
	systemBreakpoints int
	toolsBreakpoints  int
	fingerprint       anthropicThreadFingerprint
	createdAt         time.Time
}

func (r *anthropicThreadRoot) byteSize() int {
	f := r.fingerprint
	return 128 + len(r.system) + len(r.tools) + len(f.model) + len(f.toolChoice) + len(f.thinking) +
		len(f.mcpServers) + len(f.contextManagement) + len(f.outputConfig) + len(f.containerSkills) +
		len(f.cacheControl) + len(f.anthropicBeta)
}

type anthropicThreadNode struct {
	key        anthropicThreadKey
	group      *anthropicThreadGroup
	parent     *anthropicThreadNode
	delta      []anthropicStoredMessage
	deltaSize  int
	stopReason string
	inFlight   bool
	continued  bool
}

// messages collects the full ordered message history for this node by walking
// its immutable parent chain once.
func (n *anthropicThreadNode) messages() []*anthropicStoredMessage {
	var chain []*anthropicThreadNode
	total := 0
	for cur := n; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
		total += len(cur.delta)
	}
	out := make([]*anthropicStoredMessage, 0, total)
	for _, node := range slices.Backward(chain) {
		for j := range node.delta {
			out = append(out, &node.delta[j])
		}
	}
	return out
}

type anthropicThreadGroup struct {
	root       *anthropicThreadRoot
	nodes      map[string]*anthropicThreadNode
	bytes      int
	lastAccess time.Time
	order      uint64
}

func (g *anthropicThreadGroup) hasInFlight() bool {
	for _, node := range g.nodes {
		if node.inFlight {
			return true
		}
	}
	return false
}

// AnthropicThreadStore holds bounded, scope-isolated in-memory state for
// Anthropic message threads (message-threads-2026-08-12).
//
// Each turn shares its parent's immutable message prefix via parent pointers so
// multi-turn conversations do not copy O(N^2) bytes in RAM. Memory budgets and
// LRU eviction operate at thread-group granularity for exact accounting.
type AnthropicThreadStore struct {
	mu            sync.Mutex
	byKey         map[anthropicThreadKey]*anthropicThreadNode
	groups        map[*anthropicThreadGroup]struct{}
	scopeBytes    map[anthropicThreadScope]int
	totalBytes    int
	sequence      uint64
	idleTTL       time.Duration
	maxAge        time.Duration
	maxBytes      int
	maxScopeBytes int
	now           func() time.Time
}

// NewAnthropicThreadStore creates a bounded thread store with production defaults.
func NewAnthropicThreadStore() *AnthropicThreadStore {
	return newAnthropicThreadStoreWithConfig(
		defaultAnthropicThreadIdleTTL,
		defaultAnthropicThreadMaxAge,
		defaultAnthropicThreadMaxBytes,
		defaultAnthropicThreadMaxScopeBytes,
		time.Now,
	)
}

func newAnthropicThreadStoreWithConfig(
	idleTTL, maxAge time.Duration,
	maxBytes, maxScopeBytes int,
	now func() time.Time,
) *AnthropicThreadStore {
	if now == nil {
		now = time.Now
	}
	return &AnthropicThreadStore{
		byKey:         make(map[anthropicThreadKey]*anthropicThreadNode),
		groups:        make(map[*anthropicThreadGroup]struct{}),
		scopeBytes:    make(map[anthropicThreadScope]int),
		idleTTL:       idleTTL,
		maxAge:        maxAge,
		maxBytes:      maxBytes,
		maxScopeBytes: maxScopeBytes,
		now:           now,
	}
}

type anthropicContinueReservation struct {
	parent     *anthropicThreadNode
	root       *anthropicThreadRoot
	stored     []*anthropicStoredMessage
	stopReason string
	reservedAt time.Time
}

// reserveContinue validates lookup, scope isolation, TTL/max-age, single-use and
// in-flight constraints, Fixed-field fingerprint, and caller-supplied validation
// before atomically marking the parent node in_flight.
func (s *AnthropicThreadStore) reserveContinue(
	scope anthropicThreadScope,
	previousMessageID string,
	actualFingerprint anthropicThreadFingerprint,
	validateWithParent func(root *anthropicThreadRoot, stopReason string) *anthropicThreadError,
) (*anthropicContinueReservation, *anthropicThreadError) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.expireLocked(now)

	key := anthropicThreadKey{scope: scope, messageID: previousMessageID}
	node, ok := s.byKey[key]
	if !ok {
		return nil, newAnthropicThreadNotFoundError()
	}

	if s.isGroupExpiredLocked(node.group, now) && !node.group.hasInFlight() {
		s.removeGroupLocked(node.group)
		return nil, newAnthropicThreadNotFoundError()
	}

	if node.inFlight {
		return nil, newAnthropicThreadContinueInFlightError()
	}
	if node.continued {
		return nil, newAnthropicThreadAlreadyContinuedError()
	}

	if diffs := diffAnthropicThreadFingerprint(node.group.root.fingerprint, actualFingerprint); len(diffs) > 0 {
		return nil, newAnthropicThreadFingerprintMismatchError(diffs)
	}

	if validateWithParent != nil {
		if err := validateWithParent(node.group.root, node.stopReason); err != nil {
			return nil, err
		}
	}

	node.inFlight = true
	s.touchGroupLocked(node.group, now)

	return &anthropicContinueReservation{
		parent:     node,
		root:       node.group.root,
		stored:     node.messages(),
		stopReason: node.stopReason,
		reservedAt: now,
	}, nil
}

// releaseContinue clears the in_flight flag when a continue request fails or
// does not store a completed response so the client can retry the same turn.
func (s *AnthropicThreadStore) releaseContinue(parent *anthropicThreadNode) {
	if s == nil || parent == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if parent.inFlight {
		parent.inFlight = false
	}
}

// commitCreate stores the first completed turn of a new thread under messageID.
func (s *AnthropicThreadStore) commitCreate(
	root *anthropicThreadRoot,
	messageID string,
	delta []anthropicStoredMessage,
	stopReason string,
) bool {
	if s == nil || root == nil || messageID == "" {
		return false
	}

	deltaSize := storedMessagesByteSize(delta)
	groupSize := root.byteSize() + deltaSize
	if groupSize > s.maxScopeBytes || groupSize > s.maxBytes {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	root.createdAt = now
	s.expireLocked(now)

	key := anthropicThreadKey{scope: root.scope, messageID: messageID}
	// If an upstream somehow reused an existing message ID in the same scope,
	// drop the stale non-in-flight group first so accounting stays exact.
	if existing, ok := s.byKey[key]; ok {
		if existing.group.hasInFlight() {
			return false
		}
		s.removeGroupLocked(existing.group)
	}

	if !s.makeRoomLocked(root.scope, groupSize, nil) {
		return false
	}

	s.sequence++
	group := &anthropicThreadGroup{
		root:       root,
		nodes:      make(map[string]*anthropicThreadNode, 1),
		bytes:      groupSize,
		lastAccess: now,
		order:      s.sequence,
	}
	node := &anthropicThreadNode{
		key:        key,
		group:      group,
		parent:     nil,
		delta:      delta,
		deltaSize:  deltaSize,
		stopReason: stopReason,
	}
	group.nodes[messageID] = node
	s.byKey[key] = node
	s.groups[group] = struct{}{}
	s.totalBytes += groupSize
	s.scopeBytes[root.scope] += groupSize
	return true
}

// commitContinue appends a completed continuation turn to parent's thread group,
// marks parent as continued (single-use), and clears parent's in_flight flag.
func (s *AnthropicThreadStore) commitContinue(
	parent *anthropicThreadNode,
	messageID string,
	delta []anthropicStoredMessage,
	stopReason string,
) bool {
	if s == nil || parent == nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	parent.inFlight = false
	if messageID == "" {
		return false
	}

	group := parent.group
	if _, alive := s.groups[group]; !alive {
		return false
	}

	now := s.now()
	deltaSize := storedMessagesByteSize(delta)
	newGroupBytes := group.bytes + deltaSize
	if newGroupBytes > s.maxScopeBytes || newGroupBytes > s.maxBytes {
		// Cannot fit the grown thread within memory caps; leave parent uncontinued
		// so the caller gets a clean miss or can replay if needed.
		return false
	}

	s.expireLocked(now)
	if _, alive := s.groups[group]; !alive {
		return false
	}

	key := anthropicThreadKey{scope: group.root.scope, messageID: messageID}
	if existing, ok := s.byKey[key]; ok {
		if existing.group == group {
			return false
		}
		if existing.group.hasInFlight() {
			return false
		}
		s.removeGroupLocked(existing.group)
	}

	if !s.makeRoomLocked(group.root.scope, deltaSize, group) {
		return false
	}

	node := &anthropicThreadNode{
		key:        key,
		group:      group,
		parent:     parent,
		delta:      delta,
		deltaSize:  deltaSize,
		stopReason: stopReason,
	}
	parent.continued = true
	group.nodes[messageID] = node
	group.bytes += deltaSize
	s.byKey[key] = node
	s.totalBytes += deltaSize
	s.scopeBytes[group.root.scope] += deltaSize
	s.touchGroupLocked(group, now)
	return true
}

func (s *AnthropicThreadStore) touchGroupLocked(group *anthropicThreadGroup, now time.Time) {
	s.sequence++
	group.lastAccess = now
	group.order = s.sequence
}

func (s *AnthropicThreadStore) isGroupExpiredLocked(group *anthropicThreadGroup, now time.Time) bool {
	if s.idleTTL > 0 && now.Sub(group.lastAccess) > s.idleTTL {
		return true
	}
	if s.maxAge > 0 && now.Sub(group.root.createdAt) > s.maxAge {
		return true
	}
	return false
}

func (s *AnthropicThreadStore) expireLocked(now time.Time) {
	for group := range s.groups {
		if group.hasInFlight() {
			continue
		}
		if s.isGroupExpiredLocked(group, now) {
			s.removeGroupLocked(group)
		}
	}
}

func (s *AnthropicThreadStore) makeRoomLocked(scope anthropicThreadScope, needed int, exclude *anthropicThreadGroup) bool {
	for s.scopeBytes[scope]+needed > s.maxScopeBytes {
		if !s.evictOldestLocked(&scope, exclude) {
			return false
		}
	}
	for s.totalBytes+needed > s.maxBytes {
		if !s.evictOldestLocked(nil, exclude) {
			return false
		}
	}
	return true
}

func (s *AnthropicThreadStore) evictOldestLocked(scope *anthropicThreadScope, exclude *anthropicThreadGroup) bool {
	var oldest *anthropicThreadGroup
	for group := range s.groups {
		if group == exclude || group.hasInFlight() {
			continue
		}
		if scope != nil && group.root.scope != *scope {
			continue
		}
		if oldest == nil || group.order < oldest.order {
			oldest = group
		}
	}
	if oldest == nil {
		return false
	}
	s.removeGroupLocked(oldest)
	return true
}

func (s *AnthropicThreadStore) removeGroupLocked(group *anthropicThreadGroup) {
	if _, ok := s.groups[group]; !ok {
		return
	}
	for _, node := range group.nodes {
		delete(s.byKey, node.key)
	}
	delete(s.groups, group)
	s.totalBytes -= group.bytes
	if s.totalBytes < 0 {
		s.totalBytes = 0
	}
	scope := group.root.scope
	s.scopeBytes[scope] -= group.bytes
	if s.scopeBytes[scope] <= 0 {
		delete(s.scopeBytes, scope)
	}
}

func storedMessagesByteSize(msgs []anthropicStoredMessage) int {
	total := 0
	for _, msg := range msgs {
		total += msg.byteSize()
	}
	return total
}
