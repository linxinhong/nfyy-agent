// Package agents defines HTTP schema types for the agents router.
package agents

// AgentSummary is the minimal public summary for an agent.
type AgentSummary struct {
	// ID is the agent identifier.
	ID string `json:"id"`
	// Name is the human-readable agent name.
	Name string `json:"name"`
}

// ListAgentsResponse is returned by GET /agents.
type ListAgentsResponse struct {
	// Agents is the list of available agents.
	Agents []AgentSummary `json:"agents"`
}

// AgentInfoResponse is returned by GET /agents/{id}.
type AgentInfoResponse struct {
	// ID is the agent identifier.
	ID string `json:"id"`
	// Name is the human-readable agent name.
	Name string `json:"name"`
}
