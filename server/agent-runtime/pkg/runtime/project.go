package runtime

import (
	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	agentos "github.com/linxinhong/xingding-agent-runtime/pkg/os"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

// Project has a fixed component registry assembled from one spec snapshot.
// Returned execution objects retain their own public interfaces and should not
// be reconfigured while they are serving concurrent runs.
type Project struct {
	spec       ProjectSpec
	agents     map[string]*agent.Agent
	teams      map[string]*team.Team
	workflows  map[string]*workflow.Workflow
	knowledges map[string]knowledge.Knowledge
	memory     *memory.Module
	agentOS    *agentos.AgentOS
	specHash   string
}

func (p *Project) ID() string         { return p.spec.ID }
func (p *Project) Version() string    { return p.spec.Version }
func (p *Project) SchemaVersion() int { return p.spec.SchemaVersion }
func (p *Project) SpecHash() string   { return p.specHash }
func (p *Project) Name() string       { return projectName(p.spec) }

func (p *Project) Agent(id string) (*agent.Agent, bool) {
	value, ok := p.agents[id]
	return value, ok
}

func (p *Project) Team(id string) (*team.Team, bool) {
	value, ok := p.teams[id]
	return value, ok
}

func (p *Project) Workflow(id string) (*workflow.Workflow, bool) {
	value, ok := p.workflows[id]
	return value, ok
}

func (p *Project) Knowledge(id string) (knowledge.Knowledge, bool) {
	value, ok := p.knowledges[id]
	return value, ok
}

// Memory returns the same strict module used by Agent recall and AgentOS.
func (p *Project) Memory() (*memory.Module, bool) { return p.memory, p.memory != nil }

// AgentOS returns the HTTP runtime assembled from the same project graph.
func (p *Project) AgentOS() *agentos.AgentOS { return p.agentOS }
