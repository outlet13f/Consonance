package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
)

// fakeAgents is the volume agents of every project namespace, in memory. It implements
// AgentConnector, so a server under test talks to it instead of to agent pods.
type fakeAgents struct {
	mu     sync.Mutex
	points map[string]map[string][]agentapi.RestorePoint // namespace -> branch -> restore points; a branch with an entry has data
	fail   map[string][]error                            // operation -> errors to return, in order
	tokens []string                                      // the token of every Agent call, in order
	seq    int
}

func newFakeAgents() *fakeAgents {
	return &fakeAgents{points: map[string]map[string][]agentapi.RestorePoint{}, fail: map[string][]error{}}
}

// failNext makes the next call of op (CreateRestorePoint, ListRestorePoints or DeleteRestorePoint)
// return err, in any namespace.
func (f *fakeAgents) failNext(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[op] = append(f.fail[op], err)
}

// seen returns the tokens the server presented, in order.
func (f *fakeAgents) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

// restorePoints returns the restore points of one branch.
func (f *fakeAgents) restorePoints(namespace, branch string) []agentapi.RestorePoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentapi.RestorePoint(nil), f.points[namespace][branch]...)
}

// addData gives a branch data and no restore points: until then the agent has none and answers
// the list of its restore points with 404.
func (f *fakeAgents) addData(namespace, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.points[namespace] == nil {
		f.points[namespace] = map[string][]agentapi.RestorePoint{}
	}
	if f.points[namespace][branch] == nil {
		f.points[namespace][branch] = []agentapi.RestorePoint{}
	}
}

// add puts a restore point into the agent of a namespace, as if an earlier request made it.
func (f *fakeAgents) add(namespace string, rp agentapi.RestorePoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.points[namespace] == nil {
		f.points[namespace] = map[string][]agentapi.RestorePoint{}
	}
	f.points[namespace][rp.Branch] = append(f.points[namespace][rp.Branch], rp)
}

func (f *fakeAgents) Agent(namespace, token string) AgentAPI {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	return &fakeAgent{f: f, ns: namespace}
}

type fakeAgent struct {
	f  *fakeAgents
	ns string
}

// next pops the error queued for op. The caller holds f.mu.
func (f *fakeAgents) next(op string) error {
	q := f.fail[op]
	if len(q) == 0 {
		return nil
	}
	f.fail[op] = q[1:]
	return q[0]
}

func (a *fakeAgent) CreateRestorePoint(_ context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error) {
	f := a.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.next("CreateRestorePoint"); err != nil {
		return agentapi.RestorePoint{}, err
	}
	f.seq++
	at := time.Date(2026, 10, 24, 3, 15, 0, 0, time.UTC).Add(time.Duration(f.seq) * time.Second)
	rp := agentapi.RestorePoint{
		ID:             fmt.Sprintf("rp-%sZ-%04d", at.Format("20060102T150405"), f.seq),
		Branch:         branch,
		CreatedAt:      at,
		Reason:         agentapi.ReasonAPI,
		Label:          req.Label,
		BackupStartLSN: "0/3000028",
	}
	if f.points[a.ns] == nil {
		f.points[a.ns] = map[string][]agentapi.RestorePoint{}
	}
	f.points[a.ns][branch] = append(f.points[a.ns][branch], rp)
	return rp, nil
}

func (a *fakeAgent) ListRestorePoints(_ context.Context, branch string) ([]agentapi.RestorePoint, error) {
	f := a.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.next("ListRestorePoints"); err != nil {
		return nil, err
	}
	list, ok := f.points[a.ns][branch]
	if !ok {
		return nil, &agentclient.APIError{StatusCode: http.StatusNotFound, Code: agentapi.CodeNotFound, Message: "branch " + branch + " has no data"}
	}
	return append([]agentapi.RestorePoint{}, list...), nil
}

func (a *fakeAgent) DeleteRestorePoint(_ context.Context, branch, id string) error {
	f := a.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.next("DeleteRestorePoint"); err != nil {
		return err
	}
	list := f.points[a.ns][branch]
	for i, rp := range list {
		if rp.ID == id {
			f.points[a.ns][branch] = append(list[:i:i], list[i+1:]...)
			return nil
		}
	}
	return &agentclient.APIError{StatusCode: http.StatusNotFound, Code: agentapi.CodeNotFound, Message: "restore point " + id + " not found"}
}

func TestFakeAgents(t *testing.T) {
	ctx := context.Background()
	f := newFakeAgents()
	a := f.Agent("cns-aaaaaaaaaaaa", "tok")
	rp, err := a.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{Label: "x"})
	if err != nil || !agentapi.ValidRestorePointID(rp.ID) || rp.Label != "x" || rp.Branch != "main" {
		t.Fatalf("CreateRestorePoint = %+v, %v", rp, err)
	}
	if got, err := f.Agent("cns-bbbbbbbbbbbb", "other").ListRestorePoints(ctx, "main"); agentclient.CodeOf(err) != agentapi.CodeNotFound || len(got) != 0 {
		t.Errorf("another namespace sees %v, %v; want the 404 of a branch without data", got, err)
	}
	f.addData("cns-bbbbbbbbbbbb", "main")
	if got, err := f.Agent("cns-bbbbbbbbbbbb", "other").ListRestorePoints(ctx, "main"); err != nil || len(got) != 0 {
		t.Errorf("a branch with data and no restore points lists %v, %v", got, err)
	}
	if got, _ := a.ListRestorePoints(ctx, "main"); len(got) != 1 || got[0].ID != rp.ID {
		t.Errorf("ListRestorePoints = %v", got)
	}
	f.failNext("DeleteRestorePoint", errors.New("boom"))
	if err := a.DeleteRestorePoint(ctx, "main", rp.ID); err == nil || err.Error() != "boom" {
		t.Errorf("failNext: %v", err)
	}
	if err := a.DeleteRestorePoint(ctx, "main", rp.ID); err != nil {
		t.Errorf("DeleteRestorePoint: %v", err)
	}
	err = a.DeleteRestorePoint(ctx, "main", rp.ID)
	var apiErr *agentclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("deleting twice: %v, want the agent's 404", err)
	}
	if got := f.seen(); len(got) != 3 || got[0] != "tok" || got[1] != "other" || got[2] != "other" {
		t.Errorf("seen = %v", got)
	}
}
