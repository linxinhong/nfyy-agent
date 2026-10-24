package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

const (
	// DefaultMaxDefinitionNodes bounds the total number of nodes in one
	// explicitly limited structured workflow definition.
	DefaultMaxDefinitionNodes = 4096
	// DefaultMaxDefinitionDepth bounds definition nesting. The root is depth 1.
	DefaultMaxDefinitionDepth = 64
	// DefaultMaxDefinitionBytes bounds the UTF-8 JSON encoding of a definition.
	DefaultMaxDefinitionBytes = 1 << 20
	// DefaultMaxConcurrentCallbacks bounds Executor and Predicate callbacks
	// entered by one explicitly limited Run or Resume.
	DefaultMaxConcurrentCallbacks = 32
	DefaultPersistenceTimeout     = 5 * time.Second
	DefaultMaxInputBytes          = 1 << 20
	DefaultMaxNodeOutputBytes     = 1 << 20
	DefaultMaxStateBytes          = 1 << 20
	DefaultMaxRunEnvelopeBytes    = 16 << 20
)

// ErrDefinitionLimitExceeded identifies a structured definition rejected by
// an explicitly configured workflow resource limit.
var ErrDefinitionLimitExceeded = errors.New("workflow definition limit exceeded")
var ErrPayloadLimitExceeded = errors.New("workflow payload limit exceeded")

type PayloadLimitError struct {
	Resource      string
	Limit, Actual int
}

func (e *PayloadLimitError) Error() string {
	return fmt.Sprintf("workflow: %s payload limit exceeded: limit=%d actual=%d", e.Resource, e.Limit, e.Actual)
}
func (e *PayloadLimitError) Unwrap() error { return ErrPayloadLimitExceeded }

// DefinitionLimitError reports which definition resource exceeded its limit.
// Actual is exact for the final JSON byte check and the first rejected value
// for traversal checks.
type DefinitionLimitError struct {
	Resource string
	Limit    int
	Actual   int
}

func (e *DefinitionLimitError) Error() string {
	return fmt.Sprintf("workflow: definition %s limit exceeded: limit=%d actual=%d", e.Resource, e.Limit, e.Actual)
}

func (e *DefinitionLimitError) Unwrap() error { return ErrDefinitionLimitExceeded }

// Limits enables resource limits for one structured Workflow. Zero fields
// select finite defaults. A nil *Limits preserves the legacy unbounded mode.
//
// This task establishes definition admission fields. Execution, payload, and
// persistence fields are added by their owning task cards.
type Limits struct {
	MaxDefinitionNodes     int           `json:"max_definition_nodes,omitempty"`
	MaxDefinitionDepth     int           `json:"max_definition_depth,omitempty"`
	MaxDefinitionBytes     int           `json:"max_definition_bytes,omitempty"`
	MaxConcurrentCallbacks int           `json:"max_concurrent_callbacks,omitempty"`
	RunTimeout             time.Duration `json:"run_timeout,omitempty"`
	PersistenceTimeout     time.Duration `json:"persistence_timeout,omitempty"`
	MaxInputBytes          int           `json:"max_input_bytes,omitempty"`
	MaxNodeOutputBytes     int           `json:"max_node_output_bytes,omitempty"`
	MaxStateBytes          int           `json:"max_state_bytes,omitempty"`
	MaxRunEnvelopeBytes    int           `json:"max_run_envelope_bytes,omitempty"`
}

func (l Limits) resolve() (Limits, error) {
	if l.MaxDefinitionNodes < 0 {
		return Limits{}, errors.New("workflow: Limits.MaxDefinitionNodes must not be negative")
	}
	if l.MaxDefinitionDepth < 0 {
		return Limits{}, errors.New("workflow: Limits.MaxDefinitionDepth must not be negative")
	}
	if l.MaxDefinitionBytes < 0 {
		return Limits{}, errors.New("workflow: Limits.MaxDefinitionBytes must not be negative")
	}
	if l.MaxConcurrentCallbacks < 0 {
		return Limits{}, errors.New("workflow: Limits.MaxConcurrentCallbacks must not be negative")
	}
	if l.RunTimeout < 0 || l.PersistenceTimeout < 0 {
		return Limits{}, errors.New("workflow: Limits.RunTimeout and Limits.PersistenceTimeout must not be negative")
	}
	if l.MaxInputBytes < 0 || l.MaxNodeOutputBytes < 0 || l.MaxStateBytes < 0 || l.MaxRunEnvelopeBytes < 0 {
		return Limits{}, errors.New("workflow: payload limits must not be negative")
	}
	if l.MaxDefinitionNodes == 0 {
		l.MaxDefinitionNodes = DefaultMaxDefinitionNodes
	}
	if l.MaxDefinitionDepth == 0 {
		l.MaxDefinitionDepth = DefaultMaxDefinitionDepth
	}
	if l.MaxDefinitionBytes == 0 {
		l.MaxDefinitionBytes = DefaultMaxDefinitionBytes
	}
	if l.MaxConcurrentCallbacks == 0 {
		l.MaxConcurrentCallbacks = DefaultMaxConcurrentCallbacks
	}
	if l.PersistenceTimeout == 0 {
		l.PersistenceTimeout = DefaultPersistenceTimeout
	}
	if l.MaxInputBytes == 0 {
		l.MaxInputBytes = DefaultMaxInputBytes
	}
	if l.MaxNodeOutputBytes == 0 {
		l.MaxNodeOutputBytes = DefaultMaxNodeOutputBytes
	}
	if l.MaxStateBytes == 0 {
		l.MaxStateBytes = DefaultMaxStateBytes
	}
	if l.MaxRunEnvelopeBytes == 0 {
		l.MaxRunEnvelopeBytes = DefaultMaxRunEnvelopeBytes
	}
	return l, nil
}

// validatePayloadJSON accepts only explicit JSON-native dynamic values. It
// never invokes Marshal/Marshaler and stops before traversing data beyond the
// configured byte or depth budget.
func validatePayloadJSON(resource string, value any, limit int) error {
	n, err := measureDynamicJSON(value, limit)
	if err != nil {
		var overflow *jsonSizeOverflow
		if errors.As(err, &overflow) {
			return &PayloadLimitError{Resource: resource, Limit: limit, Actual: overflow.actual}
		}
		return fmt.Errorf("workflow: %s is not an allowed limited payload: %w", resource, err)
	}
	_ = n
	return nil
}

func validateInternalJSON(resource string, value any, limit int) error {
	_, err := measureInternalJSON(value, limit)
	if err == nil {
		return nil
	}
	var overflow *jsonSizeOverflow
	if errors.As(err, &overflow) {
		return &PayloadLimitError{Resource: resource, Limit: limit, Actual: overflow.actual}
	}
	return fmt.Errorf("workflow: %s is not serializable in limited mode: %w", resource, err)
}

func validateNodeOutput(value NodeOutput, limit int) error {
	return validateInternalJSON("node output", value, limit)
}

func validateRunEnvelope(value *session.RunOutput, limit int) error {
	return validateInternalJSON("workflow run envelope", value, limit)
}

// normalizeRunEnvelopeRaw compacts the remaining trusted-schema RawMessage
// fields after the bounded envelope walk has proved their compact encodings fit.
// This prevents a later json.Marshal from sizing temporary buffers from an
// attacker-controlled, whitespace-padded RawMessage source.
func normalizeRunEnvelopeRaw(value *session.RunOutput, limit int) error {
	if value == nil || value.Workflow == nil {
		return nil
	}
	var err error
	if value.Workflow.Requirement != nil {
		value.Workflow.Requirement.InputSchema, err = normalizeRawJSON(value.Workflow.Requirement.InputSchema, limit)
		if err != nil {
			return fmt.Errorf("workflow: requirement input schema: %w", err)
		}
	}
	for i := range value.Workflow.RequirementHistory {
		value.Workflow.RequirementHistory[i].InputSchema, err = normalizeRawJSON(value.Workflow.RequirementHistory[i].InputSchema, limit)
		if err != nil {
			return fmt.Errorf("workflow: requirement history input schema: %w", err)
		}
	}
	return nil
}

type definitionVisit struct {
	node       *NodeSpec
	depth      int
	inParallel bool
}

// prepareLimitedDefinition validates shape before serialization, enforces the
// exact JSON byte budget without constructing the encoded definition, then
// creates a private snapshot. RawMessage values are copied into compact form
// with capacity based on measured output, never on caller-controlled source
// length.
// SnapshotLimitedDefinition applies the same finite definition admission used
// by New and returns its normalized private snapshot. It deliberately performs
// no Registry lookup, allowing assembly layers to bound and snapshot a
// programmatic definition before their own reference discovery or encoding.
func SnapshotLimitedDefinition(def Definition, configured Limits) (Definition, error) {
	limits, err := configured.resolve()
	if err != nil {
		return Definition{}, err
	}
	return prepareLimitedDefinition(def, limits)
}

func prepareLimitedDefinition(def Definition, limits Limits) (Definition, error) {
	if def.ID == "" || def.Version == "" || def.Root.ID == "" {
		return Definition{}, errors.New("workflow: definition id, version, and root are required")
	}

	seen := make(map[string]struct{})
	stack := []definitionVisit{{node: &def.Root, depth: 1}}
	if len(def.ID) > limits.MaxDefinitionBytes || len(def.Name) > limits.MaxDefinitionBytes || len(def.Version) > limits.MaxDefinitionBytes {
		return Definition{}, &DefinitionLimitError{Resource: "bytes", Limit: limits.MaxDefinitionBytes, Actual: limits.MaxDefinitionBytes + 1}
	}
	nodes := 0
	for len(stack) > 0 {
		last := len(stack) - 1
		item := stack[last]
		stack = stack[:last]
		nodes++
		if nodes > limits.MaxDefinitionNodes {
			return Definition{}, &DefinitionLimitError{Resource: "nodes", Limit: limits.MaxDefinitionNodes, Actual: nodes}
		}
		if item.depth > limits.MaxDefinitionDepth {
			return Definition{}, &DefinitionLimitError{Resource: "depth", Limit: limits.MaxDefinitionDepth, Actual: item.depth}
		}

		node := item.node
		if node == nil || node.ID == "" {
			return Definition{}, errors.New("workflow: node id is required")
		}
		if len(node.ID) > limits.MaxDefinitionBytes {
			return Definition{}, &DefinitionLimitError{Resource: "bytes", Limit: limits.MaxDefinitionBytes, Actual: limits.MaxDefinitionBytes + 1}
		}
		if _, exists := seen[node.ID]; exists {
			return Definition{}, fmt.Errorf("workflow: duplicate node id %q", node.ID)
		}
		seen[node.ID] = struct{}{}
		if node.Policy.MaxAttempts < 0 || (node.Policy.OnError != "" && node.Policy.OnError != ErrorPolicyFail && node.Policy.OnError != ErrorPolicySkip) {
			return Definition{}, fmt.Errorf("workflow: node %q has invalid execution policy", node.ID)
		}
		if item.inParallel && node.HumanReview != nil {
			return Definition{}, fmt.Errorf("workflow: human review node %q cannot be nested in parallel", node.ID)
		}

		childDepth := item.depth + 1
		switch node.Type {
		case NodeTypeStep:
			if len(node.Children) != 0 || node.Then != nil || node.Else != nil || node.PredicateRef != "" {
				return Definition{}, fmt.Errorf("workflow: step %q contains fields for another node type", node.ID)
			}
			if node.HumanReview != nil && node.HumanReview.Kind != RequirementConfirmation && node.HumanReview.Kind != RequirementUserInput {
				return Definition{}, fmt.Errorf("workflow: node %q has unsupported human review kind %q", node.ID, node.HumanReview.Kind)
			}
			if node.ExecutorRef == "" {
				return Definition{}, fmt.Errorf("workflow: step %q requires executor_ref", node.ID)
			}
		case NodeTypeSequence, NodeTypeParallel:
			if node.ExecutorRef != "" || node.PredicateRef != "" || node.Then != nil || node.Else != nil {
				return Definition{}, fmt.Errorf("workflow: %s %q contains fields for another node type", node.Type, node.ID)
			}
			if node.HumanReview != nil {
				return Definition{}, errors.New("workflow: human review is only supported on executor nodes")
			}
			if len(node.Children) == 0 {
				return Definition{}, fmt.Errorf("workflow: %s %q requires children", node.Type, node.ID)
			}
			if childDepth > limits.MaxDefinitionDepth {
				return Definition{}, &DefinitionLimitError{Resource: "depth", Limit: limits.MaxDefinitionDepth, Actual: childDepth}
			}
			if len(node.Children) > limits.MaxDefinitionNodes-nodes-len(stack) {
				return Definition{}, &DefinitionLimitError{Resource: "nodes", Limit: limits.MaxDefinitionNodes, Actual: limits.MaxDefinitionNodes + 1}
			}
			for index := len(node.Children) - 1; index >= 0; index-- {
				child := &node.Children[index]
				stack = append(stack, definitionVisit{node: child, depth: childDepth, inParallel: item.inParallel || node.Type == NodeTypeParallel})
			}
		case NodeTypeCondition:
			if node.ExecutorRef != "" || len(node.Children) != 0 {
				return Definition{}, fmt.Errorf("workflow: condition %q contains fields for another node type", node.ID)
			}
			if node.HumanReview != nil {
				return Definition{}, errors.New("workflow: human review is only supported on executor nodes")
			}
			if node.PredicateRef == "" || node.Then == nil || node.Else == nil {
				return Definition{}, fmt.Errorf("workflow: condition %q requires predicate, then, and else", node.ID)
			}
			if childDepth > limits.MaxDefinitionDepth {
				return Definition{}, &DefinitionLimitError{Resource: "depth", Limit: limits.MaxDefinitionDepth, Actual: childDepth}
			}
			if 2 > limits.MaxDefinitionNodes-nodes-len(stack) {
				return Definition{}, &DefinitionLimitError{Resource: "nodes", Limit: limits.MaxDefinitionNodes, Actual: limits.MaxDefinitionNodes + 1}
			}
			stack = append(stack,
				definitionVisit{node: node.Else, depth: childDepth, inParallel: item.inParallel},
				definitionVisit{node: node.Then, depth: childDepth, inParallel: item.inParallel},
			)
		default:
			return Definition{}, fmt.Errorf("workflow: unsupported node type %q", node.Type)
		}
	}

	if _, err := limitedDefinitionJSONSize(def, limits.MaxDefinitionBytes); err != nil {
		return Definition{}, err
	}
	snapshot, err := cloneLimitedDefinition(def, limits.MaxDefinitionBytes)
	if err != nil {
		return Definition{}, err
	}
	return snapshot, nil
}

type definitionJSONSizer struct {
	size  int
	limit int
}

func (s *definitionJSONSizer) add(size int) error {
	if size < 0 || size > s.limit-s.size {
		return &DefinitionLimitError{Resource: "bytes", Limit: s.limit, Actual: s.limit + 1}
	}
	s.size += size
	return nil
}

func (s *definitionJSONSizer) literal(value string) error { return s.add(len(value)) }

func (s *definitionJSONSizer) quoted(value string) error {
	if err := s.add(2); err != nil {
		return err
	}
	for index := 0; index < len(value); {
		current := value[index]
		if current < utf8.RuneSelf {
			size := 1
			switch current {
			case '\\', '"', '\b', '\f', '\n', '\r', '\t':
				size = 2
			default:
				if current < 0x20 || current == '<' || current == '>' || current == '&' {
					size = 6
				}
			}
			if err := s.add(size); err != nil {
				return err
			}
			index++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[index:])
		encoded := size
		if r == utf8.RuneError && size == 1 {
			encoded = 6
		} else if r == '\u2028' || r == '\u2029' {
			encoded = 6
		}
		if err := s.add(encoded); err != nil {
			return err
		}
		index += size
	}
	return nil
}

func (s *definitionJSONSizer) raw(raw json.RawMessage) error {
	size, err := compactRawJSONSize(raw, s.limit-s.size)
	if err != nil {
		return err
	}
	return s.add(size)
}

func limitedDefinitionJSONSize(def Definition, limit int) (int, error) {
	s := &definitionJSONSizer{limit: limit}
	if err := s.literal(`{"id":`); err != nil {
		return 0, err
	}
	if err := s.quoted(def.ID); err != nil {
		return 0, err
	}
	if def.Name != "" {
		if err := s.literal(`,"name":`); err != nil {
			return 0, err
		}
		if err := s.quoted(def.Name); err != nil {
			return 0, err
		}
	}
	if err := s.literal(`,"version":`); err != nil {
		return 0, err
	}
	if err := s.quoted(def.Version); err != nil {
		return 0, err
	}
	if err := s.literal(`,"root":`); err != nil {
		return 0, err
	}
	if err := s.node(def.Root); err != nil {
		return 0, err
	}
	if err := s.literal(`}`); err != nil {
		return 0, err
	}
	return s.size, nil
}

func (s *definitionJSONSizer) node(node NodeSpec) error {
	if err := s.literal(`{"id":`); err != nil {
		return err
	}
	if err := s.quoted(node.ID); err != nil {
		return err
	}
	if node.Name != "" {
		if err := s.literal(`,"name":`); err != nil {
			return err
		}
		if err := s.quoted(node.Name); err != nil {
			return err
		}
	}
	if err := s.literal(`,"type":`); err != nil {
		return err
	}
	if err := s.quoted(string(node.Type)); err != nil {
		return err
	}
	if node.ExecutorRef != "" {
		if err := s.literal(`,"executor_ref":`); err != nil {
			return err
		}
		if err := s.quoted(node.ExecutorRef); err != nil {
			return err
		}
	}
	if node.PredicateRef != "" {
		if err := s.literal(`,"predicate_ref":`); err != nil {
			return err
		}
		if err := s.quoted(node.PredicateRef); err != nil {
			return err
		}
	}
	if len(node.Config) != 0 {
		if err := s.literal(`,"config":`); err != nil {
			return err
		}
		if err := s.raw(node.Config); err != nil {
			return fmt.Errorf("workflow: node %q config is not valid JSON: %w", node.ID, err)
		}
	}
	if len(node.Children) != 0 {
		if err := s.literal(`,"children":[`); err != nil {
			return err
		}
		for index, child := range node.Children {
			if index > 0 {
				if err := s.literal(`,`); err != nil {
					return err
				}
			}
			if err := s.node(child); err != nil {
				return err
			}
		}
		if err := s.literal(`]`); err != nil {
			return err
		}
	}
	if node.Then != nil {
		if err := s.literal(`,"then":`); err != nil {
			return err
		}
		if err := s.node(*node.Then); err != nil {
			return err
		}
	}
	if node.Else != nil {
		if err := s.literal(`,"else":`); err != nil {
			return err
		}
		if err := s.node(*node.Else); err != nil {
			return err
		}
	}
	if err := s.literal(`,"policy":`); err != nil {
		return err
	}
	if err := s.policy(node.Policy); err != nil {
		return err
	}
	if node.HumanReview != nil {
		if err := s.literal(`,"human_review":`); err != nil {
			return err
		}
		if err := s.humanReview(*node.HumanReview, node.ID); err != nil {
			return err
		}
	}
	return s.literal(`}`)
}

func (s *definitionJSONSizer) policy(policy NodePolicy) error {
	if err := s.literal(`{`); err != nil {
		return err
	}
	separator := ""
	if policy.Timeout != 0 {
		if err := s.literal(`"timeout":`); err != nil {
			return err
		}
		if err := s.add(signedDecimalSize(int64(policy.Timeout))); err != nil {
			return err
		}
		separator = ","
	}
	if policy.MaxAttempts != 0 {
		if err := s.literal(separator + `"max_attempts":`); err != nil {
			return err
		}
		if err := s.add(signedDecimalSize(int64(policy.MaxAttempts))); err != nil {
			return err
		}
		separator = ","
	}
	if policy.Backoff != 0 {
		if err := s.literal(separator + `"backoff":`); err != nil {
			return err
		}
		if err := s.add(signedDecimalSize(int64(policy.Backoff))); err != nil {
			return err
		}
		separator = ","
	}
	if policy.OnError != "" {
		if err := s.literal(separator + `"on_error":`); err != nil {
			return err
		}
		if err := s.quoted(string(policy.OnError)); err != nil {
			return err
		}
	}
	return s.literal(`}`)
}

func (s *definitionJSONSizer) humanReview(review HumanReview, nodeID string) error {
	if err := s.literal(`{"kind":`); err != nil {
		return err
	}
	if err := s.quoted(string(review.Kind)); err != nil {
		return err
	}
	if review.Prompt != "" {
		if err := s.literal(`,"prompt":`); err != nil {
			return err
		}
		if err := s.quoted(review.Prompt); err != nil {
			return err
		}
	}
	if len(review.InputSchema) != 0 {
		if err := s.literal(`,"input_schema":`); err != nil {
			return err
		}
		if err := s.raw(review.InputSchema); err != nil {
			return fmt.Errorf("workflow: node %q input schema is not valid JSON: %w", nodeID, err)
		}
	}
	if review.Deadline != nil {
		if err := s.literal(`,"deadline":`); err != nil {
			return err
		}
		raw, err := json.Marshal(review.Deadline)
		if err != nil {
			return err
		}
		if err := s.add(len(raw)); err != nil {
			return err
		}
	}
	return s.literal(`}`)
}

func signedDecimalSize(value int64) int {
	size := 1
	unsigned := uint64(value)
	if value < 0 {
		size++
		unsigned = uint64(-(value + 1)) + 1
	}
	for unsigned >= 10 {
		unsigned /= 10
		size++
	}
	return size
}

func compactRawJSONSize(raw json.RawMessage, remaining int) (int, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	inString := false
	escaped := false
	size := 0
	for index := 0; index < len(raw); index++ {
		value := raw[index]
		if inString {
			encoded := 1
			if !escaped {
				if value == '<' || value == '>' || value == '&' {
					encoded = 6
				} else if value == 0xe2 && index+2 < len(raw) && raw[index+1] == 0x80 && raw[index+2]&^1 == 0xa8 {
					encoded = 6
					index += 2
				}
			}
			size += encoded
			if size > remaining {
				return size, nil
			}
			if escaped {
				escaped = false
				continue
			}
			if value == '\\' {
				escaped = true
			} else if value == '"' {
				inString = false
			}
			continue
		}
		if value == '"' {
			inString = true
			size++
			continue
		}
		switch value {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			size++
		}
		if size > remaining {
			return size, nil
		}
	}
	if !json.Valid(raw) {
		return 0, errors.New("invalid JSON")
	}
	return size, nil
}

func cloneLimitedDefinition(def Definition, limit int) (Definition, error) {
	root, err := cloneLimitedNode(def.Root, limit)
	if err != nil {
		return Definition{}, err
	}
	return Definition{ID: def.ID, Name: def.Name, Version: def.Version, Root: root}, nil
}

func cloneLimitedNode(node NodeSpec, limit int) (NodeSpec, error) {
	clone := node
	var err error
	clone.Config, err = normalizeRawJSON(node.Config, limit)
	if err != nil {
		return NodeSpec{}, fmt.Errorf("workflow: node %q config is not valid JSON: %w", node.ID, err)
	}
	if node.Children != nil {
		clone.Children = make([]NodeSpec, len(node.Children))
		for index := range node.Children {
			clone.Children[index], err = cloneLimitedNode(node.Children[index], limit)
			if err != nil {
				return NodeSpec{}, err
			}
		}
	}
	if node.Then != nil {
		thenNode, cloneErr := cloneLimitedNode(*node.Then, limit)
		if cloneErr != nil {
			return NodeSpec{}, cloneErr
		}
		clone.Then = &thenNode
	}
	if node.Else != nil {
		elseNode, cloneErr := cloneLimitedNode(*node.Else, limit)
		if cloneErr != nil {
			return NodeSpec{}, cloneErr
		}
		clone.Else = &elseNode
	}
	if node.HumanReview != nil {
		review := *node.HumanReview
		review.InputSchema, err = normalizeRawJSON(node.HumanReview.InputSchema, limit)
		if err != nil {
			return NodeSpec{}, fmt.Errorf("workflow: node %q input schema is not valid JSON: %w", node.ID, err)
		}
		if node.HumanReview.Deadline != nil {
			deadline := *node.HumanReview.Deadline
			review.Deadline = &deadline
		}
		clone.HumanReview = &review
	}
	return clone, nil
}

func normalizeRawJSON(raw json.RawMessage, limit int) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	size, err := compactRawJSONSize(raw, limit)
	if err != nil {
		return nil, err
	}
	if size > limit {
		return nil, &DefinitionLimitError{Resource: "bytes", Limit: limit, Actual: limit + 1}
	}
	out := make([]byte, 0, size)
	inString := false
	escaped := false
	const hex = "0123456789abcdef"
	for index := 0; index < len(raw); index++ {
		value := raw[index]
		if !inString {
			switch value {
			case ' ', '\t', '\r', '\n':
				continue
			}
			out = append(out, value)
			if value == '"' {
				inString = true
			}
			continue
		}
		if !escaped && (value == '<' || value == '>' || value == '&') {
			out = append(out, '\\', 'u', '0', '0', hex[value>>4], hex[value&0xf])
			continue
		}
		if !escaped && value == 0xe2 && index+2 < len(raw) && raw[index+1] == 0x80 && raw[index+2]&^1 == 0xa8 {
			out = append(out, '\\', 'u', '2', '0', '2', hex[raw[index+2]&0xf])
			index += 2
			continue
		}
		out = append(out, value)
		if escaped {
			escaped = false
			continue
		}
		if value == '\\' {
			escaped = true
		} else if value == '"' {
			inString = false
		}
	}
	if len(out) != size {
		return nil, fmt.Errorf("internal raw JSON size mismatch: measured=%d copied=%d", size, len(out))
	}
	return json.RawMessage(out), nil
}
