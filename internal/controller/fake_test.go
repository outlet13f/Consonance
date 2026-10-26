package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
)

// fakeConnector stands in for the agent and the supervisors of every project namespace. A
// supervisor answers only while its branch pod exists, so the operator must make the pod first;
// PostgreSQL then runs or not as the pod's AUTOSTART says, like the real supervisor.
type fakeConnector struct {
	pods client.Reader

	mu       sync.Mutex
	projects map[string]*fakeProject
}

func newFakeConnector(pods client.Reader) *fakeConnector {
	return &fakeConnector{pods: pods, projects: map[string]*fakeProject{}}
}

// project returns the fake volume and servers of a project namespace.
func (c *fakeConnector) project(ns string) *fakeProject {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.projects[ns]
	if p == nil {
		p = &fakeProject{
			ns: ns, pods: c.pods,
			preflight: agentapi.Preflight{Reflink: true, Kernel: "6.8.0"},
			data:      map[string]*fakeData{}, pg: map[string]*fakePG{},
			restorePoints: map[string]string{}, resetPoints: map[string]string{},
			failures: map[string][]error{}, lost: map[string]error{}, badGenerations: map[string]int{}, tokens: map[string]bool{},
		}
		c.projects[ns] = p
	}
	return p
}

func (c *fakeConnector) Agent(ns, token string) AgentAPI {
	return &fakeAgent{p: c.project(ns), token: token}
}

func (c *fakeConnector) Supervisor(ns, branch, token string) SupervisorAPI {
	return &fakeSupervisor{p: c.project(ns), branch: branch, token: token}
}

// fakeProject is one project volume, its agent and its branches' PostgreSQL.
type fakeProject struct {
	ns   string
	pods client.Reader

	mu             sync.Mutex
	preflight      agentapi.Preflight
	data           map[string]*fakeData // branch → its generations
	pg             map[string]*fakePG   // branch → its PostgreSQL, while its pod exists
	restorePoints  map[string]string    // restore point ID → branch
	resetPoints    map[string]string    // "branch/n" → the restore point a reset made of data-n
	failures       map[string][]error   // operation → errors for its next calls, in order
	lost           map[string]error     // operation → the error its calls answer with after doing their work
	badGenerations map[string]int       // branch → a generation PostgreSQL does not start on
	tokens         map[string]bool
	calls          []string
	callIDs        []string // the X-Request-Id of each call in calls, "" when it had none
}

type fakeData struct {
	current int
	gens    []int // complete generations, ascending; includes current
}

type fakePG struct {
	state        supervisorapi.PGState
	gen          int // the generation it runs; while stopped, current's target
	bootstrapped bool
}

// failNext makes the next calls of an operation fail with errs, in order. Operations are the
// agent method names and, for supervisors, "Status(branch)", "Start(branch)", "Stop(branch)"
// and "Bootstrap(branch)".
func (p *fakeProject) failNext(op string, errs ...error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures[op] = append(p.failures[op], errs...)
}

// loseAnswers makes the calls of an operation do their work and then fail with err, as an answer
// lost on its way back does, until it is called again with nil. Only DetachGeneration heeds it.
func (p *fakeProject) loseAnswers(op string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lost[op] = err
}

func (p *fakeProject) setPreflight(pre agentapi.Preflight) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preflight = pre
}

// addRestorePoint puts a restore point of branch on the volume.
func (p *fakeProject) addRestorePoint(id, branch string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.restorePoints[id] = branch
}

// failToStart makes PostgreSQL of branch fail to start on generation n.
func (p *fakeProject) failToStart(branch string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.badGenerations[branch] = n
}

func (p *fakeProject) callLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// requestIDs returns the request ID of every call whose description starts with prefix, in
// order; "" stands for a call without one.
func (p *fakeProject) requestIDs(prefix string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for i, c := range p.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, p.callIDs[i])
		}
	}
	return out
}

func (p *fakeProject) generations(branch string) (current int, gens []int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.data[branch]
	if d == nil {
		return 0, nil, false
	}
	return d.current, slices.Clone(d.gens), true
}

func (p *fakeProject) postgres(branch string) (supervisorapi.PGState, int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pg := p.pg[branch]
	if pg == nil {
		return "", 0, false
	}
	return pg.state, pg.gen, pg.bootstrapped
}

func (p *fakeProject) seenTokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for t := range p.tokens {
		out = append(out, t)
	}
	return out
}

// call records a call and returns the error injected for it, if any. p.mu is held.
func (p *fakeProject) call(ctx context.Context, token, op, desc string) error {
	p.tokens[token] = true
	p.calls = append(p.calls, desc)
	id, _ := agentapi.RequestIDFrom(ctx)
	p.callIDs = append(p.callIDs, id)
	if errs := p.failures[op]; len(errs) > 0 {
		p.failures[op] = errs[1:]
		return errs[0]
	}
	return nil
}

// up returns the PostgreSQL of branch if its pod exists; a missing pod is a supervisor that does
// not answer. A pod that just came up runs PostgreSQL if its AUTOSTART says so. p.mu is held.
func (p *fakeProject) up(branch string) (*fakePG, error) {
	var pod corev1.Pod
	err := p.pods.Get(context.Background(), client.ObjectKey{Namespace: p.ns, Name: BranchObjectName(branch)}, &pod)
	if apierrors.IsNotFound(err) || err == nil && pod.DeletionTimestamp != nil {
		delete(p.pg, branch)
		return nil, fmt.Errorf("dial tcp: lookup %s.%s.svc: no such host", BranchObjectName(branch), p.ns)
	}
	if err != nil {
		return nil, err
	}
	pg := p.pg[branch]
	if pg == nil {
		pg = &fakePG{state: supervisorapi.PGStopped}
		if d := p.data[branch]; d != nil {
			pg.gen = d.current
			if autostart(pod) && p.badGenerations[branch] != d.current {
				pg.state = supervisorapi.PGRunning
			}
		}
		p.pg[branch] = pg
	}
	return pg, nil
}

func autostart(pod corev1.Pod) bool {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "AUTOSTART" {
			return e.Value == "true"
		}
	}
	return true
}

func agentErr(status int, code agentapi.ErrorCode, msg string) error {
	return &agentclient.APIError{StatusCode: status, Code: code, Message: msg}
}

func supervisorErr(status int, msg string) error {
	return &supervisorclient.APIError{StatusCode: status, Message: msg}
}

func genName(n int) string { return fmt.Sprintf("data-%d", n) }

// fakeAgent is a project's agent: it keeps the volume's generations as the real one does on disk.
type fakeAgent struct {
	p     *fakeProject
	token string
}

func (a *fakeAgent) Preflight(ctx context.Context) (agentapi.Preflight, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "Preflight", "Preflight"); err != nil {
		return agentapi.Preflight{}, err
	}
	return p.preflight, nil
}

func (a *fakeAgent) Usage(ctx context.Context) (agentapi.Usage, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "Usage", "Usage"); err != nil {
		return agentapi.Usage{}, err
	}
	return agentapi.Usage{CapacityBytes: 10 << 30, UsedBytes: 1 << 30, AvailableBytes: 9 << 30}, nil
}

func (a *fakeAgent) InitMain(ctx context.Context) (agentapi.Generation, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "InitMain", "InitMain"); err != nil {
		return agentapi.Generation{}, err
	}
	if p.data[agentapi.MainBranch] == nil {
		p.data[agentapi.MainBranch] = &fakeData{current: 1, gens: []int{1}}
	}
	return agentapi.Generation{Generation: p.data[agentapi.MainBranch].current}, nil
}

func (a *fakeAgent) CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "CreateBranchData", fmt.Sprintf("CreateBranchData(%s,%s,%s)", name, req.Parent, req.Source.Type)); err != nil {
		return agentapi.BranchData{}, err
	}
	if d := p.data[name]; d != nil { // a retry: the data is there
		return agentapi.BranchData{Generation: d.current}, nil
	}
	lsn := ""
	switch req.Source.Type {
	case agentapi.SourceParentLatest:
		if p.data[req.Parent] == nil {
			return agentapi.BranchData{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "parent has no data")
		}
		pg, err := p.up(req.Parent)
		if err != nil {
			return agentapi.BranchData{}, agentErr(http.StatusBadGateway, agentapi.CodeSupervisorUnavailable, err.Error())
		}
		if pg.state != supervisorapi.PGRunning {
			return agentapi.BranchData{}, agentErr(http.StatusConflict, agentapi.CodeNotRunning, "parent is not running")
		}
		lsn = "0/3000028"
	case agentapi.SourceRestorePoint:
		if p.restorePoints[req.Source.RestorePointID] != req.Parent {
			return agentapi.BranchData{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such restore point")
		}
	}
	p.data[name] = &fakeData{current: 1, gens: []int{1}}
	return agentapi.BranchData{Generation: 1, BackupStartLSN: lsn}, nil
}

func (a *fakeAgent) DeleteBranchData(ctx context.Context, name string) error {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "DeleteBranchData", fmt.Sprintf("DeleteBranchData(%s)", name)); err != nil {
		return err
	}
	if name == agentapi.MainBranch {
		return agentErr(http.StatusConflict, agentapi.CodeConflict, "main cannot be deleted")
	}
	// Like the agent: a supervisor that answers must report PostgreSQL stopped.
	if pg, err := p.up(name); err == nil && pg.state != supervisorapi.PGStopped {
		return agentErr(http.StatusConflict, agentapi.CodeConflict, "branch "+name+" is "+string(pg.state))
	}
	delete(p.data, name)
	for id, b := range p.restorePoints {
		if b == name {
			delete(p.restorePoints, id)
		}
	}
	return nil
}

func (a *fakeAgent) ListGenerations(ctx context.Context, branch string) (agentapi.Generations, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "ListGenerations", fmt.Sprintf("ListGenerations(%s)", branch)); err != nil {
		return agentapi.Generations{}, err
	}
	d := p.data[branch]
	if d == nil {
		return agentapi.Generations{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "branch has no data")
	}
	return agentapi.Generations{Current: d.current, Generations: slices.Clone(d.gens)}, nil
}

func (a *fakeAgent) CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "CreateGeneration", fmt.Sprintf("CreateGeneration(%s,%s)", branch, req.Source.Type)); err != nil {
		return agentapi.Generation{}, err
	}
	d := p.data[branch]
	if d == nil {
		return agentapi.Generation{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "branch has no data")
	}
	switch req.Source.Type {
	case agentapi.SourceRestorePoint:
		if p.restorePoints[req.Source.RestorePointID] != branch {
			return agentapi.Generation{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such restore point")
		}
	case agentapi.SourceParentLatest:
		if p.data[req.Parent] == nil {
			return agentapi.Generation{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "parent has no data")
		}
		pg, err := p.up(req.Parent)
		if err != nil {
			return agentapi.Generation{}, agentErr(http.StatusBadGateway, agentapi.CodeSupervisorUnavailable, err.Error())
		}
		if pg.state != supervisorapi.PGRunning {
			return agentapi.Generation{}, agentErr(http.StatusConflict, agentapi.CodeNotRunning, "parent is not running")
		}
	}
	n := slices.Max(d.gens) + 1
	d.gens = append(d.gens, n)
	return agentapi.Generation{Generation: n}, nil
}

func (a *fakeAgent) ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "ActivateGeneration", fmt.Sprintf("ActivateGeneration(%s,%d)", branch, n)); err != nil {
		return agentapi.Generations{}, err
	}
	d := p.data[branch]
	if d == nil || !slices.Contains(d.gens, n) {
		return agentapi.Generations{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such generation")
	}
	if n != d.current {
		pg, err := p.up(branch)
		if err != nil {
			return agentapi.Generations{}, agentErr(http.StatusBadGateway, agentapi.CodeSupervisorUnavailable, err.Error())
		}
		if pg.state != supervisorapi.PGStopped {
			return agentapi.Generations{}, agentErr(http.StatusConflict, agentapi.CodeConflict, "stop it before switching generations")
		}
		d.current = n
		pg.gen = n
	}
	return agentapi.Generations{Current: d.current, Generations: slices.Clone(d.gens)}, nil
}

func (a *fakeAgent) DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	resp, err := a.detachGeneration(ctx, branch, n, req)
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	if lost := a.p.lost["DetachGeneration"]; err == nil && lost != nil {
		return agentapi.DetachResponse{}, lost
	}
	return resp, err
}

func (a *fakeAgent) detachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error) {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "DetachGeneration", fmt.Sprintf("DetachGeneration(%s,%d,%s,%v)", branch, n, req.ToBranch, req.AsRestorePoint)); err != nil {
		return agentapi.DetachResponse{}, err
	}
	d := p.data[branch]
	if d == nil {
		return agentapi.DetachResponse{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "branch has no data")
	}
	key := fmt.Sprintf("%s/%d", branch, n)
	if !slices.Contains(d.gens, n) { // a retry: answer from what is on the volume
		if req.ToBranch != "" && p.data[req.ToBranch] != nil {
			return agentapi.DetachResponse{Branch: req.ToBranch}, nil
		}
		if id := p.resetPoints[key]; req.AsRestorePoint && id != "" {
			return agentapi.DetachResponse{RestorePoint: &agentapi.RestorePoint{ID: id, Branch: branch, Reason: agentapi.ReasonReset}}, nil
		}
		return agentapi.DetachResponse{}, agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such generation")
	}
	if n == d.current {
		return agentapi.DetachResponse{}, agentErr(http.StatusConflict, agentapi.CodeConflict, "the current generation")
	}
	pg, err := p.up(branch)
	if err != nil {
		return agentapi.DetachResponse{}, agentErr(http.StatusBadGateway, agentapi.CodeSupervisorUnavailable, err.Error())
	}
	if pg.state != supervisorapi.PGStopped && pg.gen == n {
		return agentapi.DetachResponse{}, agentErr(http.StatusConflict, agentapi.CodeConflict, "postgres runs it")
	}
	if req.ToBranch != "" && p.data[req.ToBranch] != nil {
		return agentapi.DetachResponse{}, agentErr(http.StatusConflict, agentapi.CodeConflict, "the target branch exists")
	}
	d.gens = slices.DeleteFunc(d.gens, func(g int) bool { return g == n })
	if req.ToBranch != "" {
		p.data[req.ToBranch] = &fakeData{current: 1, gens: []int{1}}
		return agentapi.DetachResponse{Branch: req.ToBranch}, nil
	}
	id := fmt.Sprintf("rp-20261007T031500Z-%04d", len(p.restorePoints))
	p.restorePoints[id] = branch
	p.resetPoints[key] = id
	return agentapi.DetachResponse{RestorePoint: &agentapi.RestorePoint{ID: id, Branch: branch, Reason: agentapi.ReasonReset}}, nil
}

func (a *fakeAgent) DeleteGeneration(ctx context.Context, branch string, n int) error {
	p := a.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.call(ctx, a.token, "DeleteGeneration", fmt.Sprintf("DeleteGeneration(%s,%d)", branch, n)); err != nil {
		return err
	}
	d := p.data[branch]
	if d == nil || !slices.Contains(d.gens, n) {
		return agentErr(http.StatusNotFound, agentapi.CodeNotFound, "no such generation")
	}
	if n == d.current {
		return agentErr(http.StatusConflict, agentapi.CodeConflict, "the current generation")
	}
	pg, err := p.up(branch)
	if err != nil {
		return agentErr(http.StatusBadGateway, agentapi.CodeSupervisorUnavailable, err.Error())
	}
	if pg.state != supervisorapi.PGStopped && pg.gen == n {
		return agentErr(http.StatusConflict, agentapi.CodeConflict, "postgres runs it")
	}
	d.gens = slices.DeleteFunc(d.gens, func(g int) bool { return g == n })
	return nil
}

// fakeSupervisor is one branch's supervisor.
type fakeSupervisor struct {
	p      *fakeProject
	branch string
	token  string
}

func (s *fakeSupervisor) status(pg *fakePG) supervisorapi.Status {
	return supervisorapi.Status{PG: pg.state, Generation: genName(pg.gen)}
}

func (s *fakeSupervisor) Status(ctx context.Context) (supervisorapi.Status, error) {
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	op := "Status(" + s.branch + ")"
	if err := p.call(ctx, s.token, op, op); err != nil {
		return supervisorapi.Status{}, err
	}
	pg, err := p.up(s.branch)
	if err != nil {
		return supervisorapi.Status{}, err
	}
	return s.status(pg), nil
}

func (s *fakeSupervisor) Start(ctx context.Context) (supervisorapi.Status, error) {
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	op := "Start(" + s.branch + ")"
	if err := p.call(ctx, s.token, op, op); err != nil {
		return supervisorapi.Status{}, err
	}
	pg, err := p.up(s.branch)
	if err != nil {
		return supervisorapi.Status{}, err
	}
	if pg.state == supervisorapi.PGRunning {
		return s.status(pg), nil
	}
	d := p.data[s.branch]
	if d == nil {
		return supervisorapi.Status{}, supervisorErr(http.StatusInternalServerError, "data directory does not resolve")
	}
	pg.gen = d.current
	if p.badGenerations[s.branch] == d.current {
		pg.state = supervisorapi.PGStopped
		return supervisorapi.Status{}, supervisorErr(http.StatusInternalServerError, "postgres exited during startup")
	}
	pg.state = supervisorapi.PGRunning
	return s.status(pg), nil
}

func (s *fakeSupervisor) Stop(ctx context.Context) (supervisorapi.Status, error) {
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	op := "Stop(" + s.branch + ")"
	if err := p.call(ctx, s.token, op, op); err != nil {
		return supervisorapi.Status{}, err
	}
	pg, err := p.up(s.branch)
	if err != nil {
		return supervisorapi.Status{}, err
	}
	pg.state = supervisorapi.PGStopped
	if d := p.data[s.branch]; d != nil {
		pg.gen = d.current
	}
	return s.status(pg), nil
}

func (s *fakeSupervisor) Bootstrap(ctx context.Context) (supervisorapi.Status, error) {
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	op := "Bootstrap(" + s.branch + ")"
	if err := p.call(ctx, s.token, op, op); err != nil {
		return supervisorapi.Status{}, err
	}
	pg, err := p.up(s.branch)
	if err != nil {
		return supervisorapi.Status{}, err
	}
	if s.branch != agentapi.MainBranch {
		return supervisorapi.Status{}, supervisorErr(http.StatusNotFound, "bootstrap is disabled")
	}
	if pg.state != supervisorapi.PGRunning {
		return supervisorapi.Status{}, supervisorErr(http.StatusConflict, "postgres is not running")
	}
	pg.bootstrapped = true
	return s.status(pg), nil
}
