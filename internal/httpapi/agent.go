package httpapi

import (
	"context"
	"net/http"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/controller"
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

// HTTPAgents reaches the agents through their services, from inside the cluster.
type HTTPAgents struct {
	HTTP *http.Client // nil gets the agent client's own, with a 60s timeout
}

func (a HTTPAgents) Agent(namespace, token string) AgentAPI {
	return agentclient.New(controller.AgentURL(namespace), token, a.HTTP)
}

var _ AgentConnector = HTTPAgents{}
