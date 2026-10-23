package httpapi

import (
	"context"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
)

// AgentAPI is the part of a project's volume agent the API server calls itself (the rest of the
// agent's work goes through the operator). It is declared here, where it is used, so tests can
// stand in for the agent.
type AgentAPI interface {
	CreateRestorePoint(ctx context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error)
	ListRestorePoints(ctx context.Context, branch string) ([]agentapi.RestorePoint, error)
	DeleteRestorePoint(ctx context.Context, branch, id string) error
}

// AgentConnector makes the client of one project's agent, given the project namespace and the
// token of its internal-token Secret.
type AgentConnector interface {
	Agent(namespace, token string) AgentAPI
}

var _ AgentAPI = (*agentclient.Client)(nil)
