package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
)

func TestServiceURLs(t *testing.T) {
	if got := AgentURL("cns-a1b2c3d4e5f6"); got != "http://volume-agent.cns-a1b2c3d4e5f6.svc:8443" {
		t.Errorf("AgentURL = %q", got)
	}
	if got := SupervisorURL("cns-a1b2c3d4e5f6", "dev"); got != "http://br-dev.cns-a1b2c3d4e5f6.svc:8008" {
		t.Errorf("SupervisorURL = %q", got)
	}
}

func TestRetryLater(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		after   time.Duration
		backoff bool
	}{
		{"rate limited", &agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited, RetryAfter: 3 * time.Second}, 3 * time.Second, false},
		{"rate limited without Retry-After", &agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited}, time.Second, false},
		{"source not running", agentErr(409, agentapi.CodeNotRunning, ""), waitShort, false},
		{"busy", agentErr(409, agentapi.CodeBusy, ""), 0, true},
		{"supervisor unavailable", agentErr(502, agentapi.CodeSupervisorUnavailable, ""), 0, true},
		{"a copy's conflict", agentErr(409, agentapi.CodeConflict, ""), 0, true},
		{"no answer", errors.New("dial tcp: connection refused"), 0, true},
	}
	for _, c := range cases {
		res, err := retryLater(c.err)
		if res.RequeueAfter != c.after || (err != nil) != c.backoff {
			t.Errorf("%s: %+v, %v", c.name, res, err)
		}
	}
}

func TestPermanentReason(t *testing.T) {
	cases := map[error]string{
		agentErr(501, agentapi.CodeReflinkUnsupported, ""):  v1alpha1.ReasonReflinkUnsupported,
		agentErr(507, agentapi.CodeInsufficientStorage, ""): v1alpha1.ReasonInsufficientStorage,
		agentErr(400, agentapi.CodeInvalid, ""):             v1alpha1.ReasonInvalid,
		agentErr(409, agentapi.CodeBusy, ""):                "",
		agentErr(500, agentapi.CodeInternal, ""):            "",
		errors.New("dial tcp: connection refused"):          "",
	}
	for err, want := range cases {
		if got := permanentReason(err); got != want {
			t.Errorf("permanentReason(%v) = %q, want %q", err, got, want)
		}
	}
	if !stillStarting(supervisorErr(http.StatusServiceUnavailable, "still starting")) || stillStarting(supervisorErr(500, "exited")) {
		t.Error("stillStarting")
	}
}

// The fake supervisor answers only while its pod exists and starts as the pod's AUTOSTART says:
// the controller tests rely on both.
func TestFakeSupervisorFollowsItsPod(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	conn := newFakeConnector(c)
	agent := conn.Agent("cns-x", "tok")
	if _, err := agent.InitMain(ctx); err != nil {
		t.Fatal(err)
	}
	sup := conn.Supervisor("cns-x", "main", "tok")
	if _, err := sup.Status(ctx); err == nil {
		t.Fatal("a supervisor without a pod answered")
	}
	pod := desiredBranchPod(testProject("cns-x"), testBranch("cns-x", "main", ""), "img")
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	st, err := sup.Status(ctx)
	if err != nil || st.PG != supervisorapi.PGRunning || st.Generation != "data-1" {
		t.Fatalf("status %+v, %v", st, err)
	}
	if _, err := agent.ActivateGeneration(ctx, "main", 1); err != nil {
		t.Fatalf("activate the current generation: %v", err)
	}
	if _, err := sup.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, boot := conn.project("cns-x").postgres("main"); !boot {
		t.Fatal("not bootstrapped")
	}
}

// The fake agent asks a branch's supervisor before it moves, deletes or copies from a generation,
// as the real one does: no pod is a supervisor that does not answer (502).
func TestFakeAgentAsksTheSupervisor(t *testing.T) {
	ctx := context.Background()
	conn := newFakeConnector(fake.NewClientBuilder().Build())
	agent := conn.Agent("cns-x", "tok")
	if _, err := agent.InitMain(ctx); err != nil {
		t.Fatal(err)
	}
	p := conn.project("cns-x")
	p.mu.Lock()
	p.data["main"].gens = append(p.data["main"].gens, 2) // a second, non-current generation
	p.mu.Unlock()
	if _, err := agent.DetachGeneration(ctx, "main", 2, agentapi.DetachRequest{AsRestorePoint: true}); agentCode(err) != agentapi.CodeSupervisorUnavailable {
		t.Errorf("detach without a pod: %v, want supervisor-unavailable", err)
	}
	if err := agent.DeleteGeneration(ctx, "main", 2); agentCode(err) != agentapi.CodeSupervisorUnavailable {
		t.Errorf("delete a generation without a pod: %v, want supervisor-unavailable", err)
	}
	req := agentapi.CreateBranchDataRequest{Parent: "main", Source: agentapi.Source{Type: agentapi.SourceParentLatest}}
	if _, err := agent.CreateBranchData(ctx, "dev", req); agentCode(err) != agentapi.CodeSupervisorUnavailable {
		t.Errorf("copy from a parent without a pod: %v, want supervisor-unavailable", err)
	}
	gen := agentapi.CreateGenerationRequest{Parent: "nodata", Source: agentapi.Source{Type: agentapi.SourceParentLatest}}
	if _, err := agent.CreateGeneration(ctx, "main", gen); agentCode(err) != agentapi.CodeNotFound {
		t.Errorf("reset from a parent without data: %v, want not-found", err)
	}
}
