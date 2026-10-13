package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
		agentErr(501, agentapi.CodeReflinkUnsupported, ""):  ReasonReflinkUnsupported,
		agentErr(507, agentapi.CodeInsufficientStorage, ""): ReasonInsufficientStorage,
		agentErr(400, agentapi.CodeInvalid, ""):             ReasonInvalid,
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
