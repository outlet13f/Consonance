package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
)

// AgentAPI is the part of a project's volume agent API the operator calls (spec 7.2).
// *agentclient.Client implements it.
type AgentAPI interface {
	Preflight(ctx context.Context) (agentapi.Preflight, error)
	Usage(ctx context.Context) (agentapi.Usage, error)
	InitMain(ctx context.Context) (agentapi.Generation, error)
	CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error)
	DeleteBranchData(ctx context.Context, name string) error
	ListGenerations(ctx context.Context, branch string) (agentapi.Generations, error)
	CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error)
	ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error)
	DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error)
	DeleteGeneration(ctx context.Context, branch string, n int) error
}

// SupervisorAPI is the part of a branch supervisor's API the operator calls (spec 7.3).
// *supervisorclient.Client implements it.
type SupervisorAPI interface {
	Status(ctx context.Context) (supervisorapi.Status, error)
	Start(ctx context.Context) (supervisorapi.Status, error)
	Stop(ctx context.Context) (supervisorapi.Status, error)
	Bootstrap(ctx context.Context) (supervisorapi.Status, error)
}

var (
	_ AgentAPI      = (*agentclient.Client)(nil)
	_ SupervisorAPI = (*supervisorclient.Client)(nil)
)

// Connector gives the clients of a project's agent and of a branch's supervisor. Tests swap in
// a fake.
type Connector interface {
	Agent(namespace, token string) AgentAPI
	Supervisor(namespace, branch, token string) SupervisorAPI
}

// Time limits of the operator's calls. The HTTP client has none: each call's context bounds it.
const (
	callTimeout   = 10 * time.Second // lists, preflight, usage, activate, detach, deletes
	statusTimeout = 2 * time.Second  // a supervisor's status: one that answers does so at once, and a dead node's endpoint must not hold a worker
	copyTimeout   = 3 * time.Minute  // CreateBranchData, CreateGeneration: a backup start waits for a checkpoint
	initTimeout   = 5 * time.Minute  // InitMain: initdb on the agent's 100m CPU (carry-over from plan 2)
	startTimeout  = 75 * time.Second // the supervisor waits up to 60s for PostgreSQL to accept connections
	stopTimeout   = 60 * time.Second // a 30s fast shutdown, then an immediate one
)

// HTTPConnector reaches the agent and the supervisors through their services, from inside the
// cluster.
type HTTPConnector struct {
	HTTP *http.Client
}

// NewHTTPConnector returns a connector whose calls are bounded by their contexts only.
func NewHTTPConnector() HTTPConnector { return HTTPConnector{HTTP: &http.Client{}} }

// AgentURL is the address of a project's agent: its service volume-agent.
func AgentURL(namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", AgentName, namespace, agentapi.DefaultPort)
}

// SupervisorURL is the address of a branch's supervisor: its service br-<branch>.
func SupervisorURL(namespace, branch string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", BranchObjectName(branch), namespace, supervisorapi.DefaultPort)
}

func (c HTTPConnector) Agent(namespace, token string) AgentAPI {
	return agentclient.New(AgentURL(namespace), token, c.HTTP)
}

func (c HTTPConnector) Supervisor(namespace, branch, token string) SupervisorAPI {
	return supervisorclient.New(SupervisorURL(namespace, branch), token, c.HTTP)
}

// call runs f with a context bounded by d.
func call[T any](ctx context.Context, d time.Duration, f func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx)
}
