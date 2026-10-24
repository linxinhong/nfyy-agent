package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

type Definition = workflowschema.Definition
type NodeSpec = workflowschema.NodeSpec
type NodeType = workflowschema.NodeType
type NodePolicy = workflowschema.NodePolicy
type ErrorPolicy = workflowschema.ErrorPolicy
type HumanReview = workflowschema.HumanReview
type NodeResult = workflowschema.NodeResult
type Requirement = workflowschema.Requirement
type RequirementKind = workflowschema.RequirementKind

// RunOutput is a workflow run envelope. Workflow-specific data is available
// through RunOutput.Workflow while common lifecycle fields remain queryable by AgentOS.
type RunOutput = session.WorkflowRunOutput

const (
	NodeTypeStep      = workflowschema.NodeTypeStep
	NodeTypeSequence  = workflowschema.NodeTypeSequence
	NodeTypeParallel  = workflowschema.NodeTypeParallel
	NodeTypeCondition = workflowschema.NodeTypeCondition

	RequirementConfirmation = workflowschema.RequirementConfirmation
	RequirementUserInput    = workflowschema.RequirementUserInput
	ErrorPolicyFail         = workflowschema.ErrorPolicyFail
	ErrorPolicySkip         = workflowschema.ErrorPolicySkip
)

type ResumeAction string

const (
	ResumeActionApprove ResumeAction = "approve"
	ResumeActionReject  ResumeAction = "reject"
	ResumeActionSubmit  ResumeAction = "submit"
	// ResumeActionRetryApplying explicitly re-runs a step whose previous
	// execution left an "applying" claim behind (crash window: side effects
	// may have happened). Any other action fails closed.
	ResumeActionRetryApplying ResumeAction = "retry_applying"
)

type RunRequest struct {
	Input     any
	RunID     string
	UserID    string
	SessionID string
	Metadata  map[string]string
}

type ResumeRequest struct {
	RunID         string
	RequirementID string
	Action        ResumeAction
	Input         any
	ActorID       string
	UserID        string
}

type NodeInput struct {
	Value       any
	State       map[string]any
	ResumeInput any
	RunID       string
	WorkflowID  string
	NodeID      string
	SessionID   string
	UserID      string
	Config      json.RawMessage
	Metadata    map[string]string
}

type NodeOutput struct {
	Value       any
	StatePatch  map[string]any
	Metadata    map[string]any
	ChildRunIDs []string
}

type Executor interface {
	Execute(context.Context, NodeInput) (NodeOutput, error)
}

type ExecutorFunc func(context.Context, NodeInput) (NodeOutput, error)

func (f ExecutorFunc) Execute(ctx context.Context, input NodeInput) (NodeOutput, error) {
	return f(ctx, input)
}

type Predicate interface {
	Evaluate(context.Context, NodeInput) (bool, error)
}

type PredicateFunc func(context.Context, NodeInput) (bool, error)

func (f PredicateFunc) Evaluate(ctx context.Context, input NodeInput) (bool, error) {
	return f(ctx, input)
}

type Registry struct {
	mu         sync.RWMutex
	executors  map[string]Executor
	predicates map[string]Predicate
}

func NewRegistry() *Registry {
	return &Registry{executors: make(map[string]Executor), predicates: make(map[string]Predicate)}
}

func (r *Registry) RegisterExecutor(ref string, executor Executor) error {
	if ref == "" || executor == nil {
		return errors.New("workflow: executor reference and implementation are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.executors[ref]; exists {
		return fmt.Errorf("workflow: executor %q is already registered", ref)
	}
	r.executors[ref] = executor
	return nil
}

func (r *Registry) RegisterPredicate(ref string, predicate Predicate) error {
	if ref == "" || predicate == nil {
		return errors.New("workflow: predicate reference and implementation are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.predicates[ref]; exists {
		return fmt.Errorf("workflow: predicate %q is already registered", ref)
	}
	r.predicates[ref] = predicate
	return nil
}

func (r *Registry) executor(ref string) (Executor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.executors[ref]
	return v, ok
}

func (r *Registry) predicate(ref string) (Predicate, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.predicates[ref]
	return v, ok
}

func ExecutorNode(id, executorRef string) NodeSpec {
	return NodeSpec{ID: id, Type: NodeTypeStep, ExecutorRef: executorRef}
}

func Sequence(id string, children ...NodeSpec) NodeSpec {
	return NodeSpec{ID: id, Type: NodeTypeSequence, Children: children}
}

func Parallel(id string, children ...NodeSpec) NodeSpec {
	return NodeSpec{ID: id, Type: NodeTypeParallel, Children: children}
}

func Condition(id, predicateRef string, thenNode, elseNode NodeSpec) NodeSpec {
	return NodeSpec{ID: id, Type: NodeTypeCondition, PredicateRef: predicateRef, Then: &thenNode, Else: &elseNode}
}

func validateDefinition(def Definition, registry *Registry) error {
	if def.ID == "" || def.Version == "" || def.Root.ID == "" {
		return errors.New("workflow: definition id, version, and root are required")
	}
	seen := make(map[string]struct{})
	var visit func(NodeSpec, bool) error
	visit = func(node NodeSpec, inParallel bool) error {
		if node.ID == "" {
			return errors.New("workflow: node id is required")
		}
		if _, exists := seen[node.ID]; exists {
			return fmt.Errorf("workflow: duplicate node id %q", node.ID)
		}
		seen[node.ID] = struct{}{}
		if node.Policy.MaxAttempts < 0 || (node.Policy.OnError != "" && node.Policy.OnError != ErrorPolicyFail && node.Policy.OnError != ErrorPolicySkip) {
			return fmt.Errorf("workflow: node %q has invalid execution policy", node.ID)
		}
		if inParallel && node.HumanReview != nil {
			return fmt.Errorf("workflow: human review node %q cannot be nested in parallel", node.ID)
		}
		switch node.Type {
		case NodeTypeStep:
			if node.HumanReview != nil && node.HumanReview.Kind != RequirementConfirmation && node.HumanReview.Kind != RequirementUserInput {
				return fmt.Errorf("workflow: node %q has unsupported human review kind %q", node.ID, node.HumanReview.Kind)
			}
			if node.ExecutorRef == "" {
				return fmt.Errorf("workflow: step %q requires executor_ref", node.ID)
			}
			if _, ok := registry.executor(node.ExecutorRef); !ok {
				return fmt.Errorf("workflow: executor %q is not registered", node.ExecutorRef)
			}
		case NodeTypeSequence, NodeTypeParallel:
			if node.HumanReview != nil {
				return fmt.Errorf("workflow: human review is only supported on executor nodes")
			}
			if len(node.Children) == 0 {
				return fmt.Errorf("workflow: %s %q requires children", node.Type, node.ID)
			}
			for _, child := range node.Children {
				if err := visit(child, inParallel || node.Type == NodeTypeParallel); err != nil {
					return err
				}
			}
		case NodeTypeCondition:
			if node.HumanReview != nil {
				return fmt.Errorf("workflow: human review is only supported on executor nodes")
			}
			if node.PredicateRef == "" || node.Then == nil || node.Else == nil {
				return fmt.Errorf("workflow: condition %q requires predicate, then, and else", node.ID)
			}
			if _, ok := registry.predicate(node.PredicateRef); !ok {
				return fmt.Errorf("workflow: predicate %q is not registered", node.PredicateRef)
			}
			if err := visit(*node.Then, inParallel); err != nil {
				return err
			}
			if err := visit(*node.Else, inParallel); err != nil {
				return err
			}
		default:
			return fmt.Errorf("workflow: unsupported node type %q", node.Type)
		}
		return nil
	}
	if err := visit(def.Root, false); err != nil {
		return err
	}
	_, err := json.Marshal(def)
	return err
}

func definitionHash(def Definition) (string, error) {
	raw, err := json.Marshal(def)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

var errWorkflowPaused = errors.New("workflow paused")

// ErrApplyingUnknown is returned when resuming a run that crashed while a
// step was applying: the step's side effects may have happened but their
// outcome is unknown. Recovery requires an explicit ResumeActionRetryApplying.
var ErrApplyingUnknown = errors.New("workflow: node applying with unknown outcome")

func valueString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	raw, _ := json.Marshal(source)
	var clone map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&clone)
	return clone
}
