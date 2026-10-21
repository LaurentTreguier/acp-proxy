package acp

import (
	"context"
	"fmt"
)

// AgentInfo describes one resolvable ACP agent.
type AgentInfo struct {
	ID          string
	Name        string
	Version     string
	Description string
}

// Resolver turns an agent id into a runnable ACP agent, on demand. It is
// the contract between the API packages and the registry.
type Resolver interface {
	// List returns every known agent.
	List(ctx context.Context) ([]AgentInfo, error)
	// Resolve returns the agent to spawn for an id. Agents are spawned
	// per request, so Resolve must be safe for concurrent use.
	Resolve(ctx context.Context, id string) (Agent, error)
}

// UnknownAgentError reports an agent id that a Resolver does not know;
// the API packages map it to HTTP 404.
type UnknownAgentError struct {
	ID string
}

func (e *UnknownAgentError) Error() string {
	return fmt.Sprintf("unknown agent %q, see GET /agents", e.ID)
}
