package agentclient_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"consonance/internal/agent"
	"consonance/internal/agent/agenttest"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/volume"
)

const rpID = "rp-20261007T031500Z-ab12"

func newAgent(t *testing.T, ops *agenttest.FakeOps) string {
	t.Helper()
	srv := httptest.NewServer((&agent.Server{Token: "t", Ops: ops, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestClientCallsEveryOperation(t *testing.T) {
	ops := &agenttest.FakeOps{
		PreflightResult:   agentapi.Preflight{Reflink: true, Kernel: "6.8.0-63-generic"},
		UsageResult:       agentapi.Usage{CapacityBytes: 100, UsedBytes: 10, AvailableBytes: 90},
		GenerationResult:  agentapi.Generation{Generation: 2},
		BranchDataResult:  agentapi.BranchData{Generation: 1, BackupStartLSN: "0/3000028"},
		RestorePoint:      agentapi.RestorePoint{ID: rpID, Branch: "main", Reason: agentapi.ReasonAPI},
		RestorePoints:     []agentapi.RestorePoint{{ID: rpID, Branch: "main"}},
		GenerationsResult: agentapi.Generations{Current: 2, Generations: []int{1, 2}},
		DetachResult:      agentapi.DetachResponse{Branch: "main-before-20261007031500"},
	}
	c := agentclient.New(newAgent(t, ops)+"/", "t", nil) // a trailing slash must not matter
	ctx := context.Background()
	check := func(what string, err error, want agenttest.Call) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := ops.Last(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s reached %+v, want %+v", what, got, want)
		}
	}

	p, err := c.Preflight(ctx)
	check("Preflight", err, agenttest.Call{Op: "Preflight"})
	if p != ops.PreflightResult {
		t.Errorf("Preflight = %+v", p)
	}
	u, err := c.Usage(ctx)
	check("Usage", err, agenttest.Call{Op: "Usage"})
	if u != ops.UsageResult {
		t.Errorf("Usage = %+v", u)
	}
	g, err := c.InitMain(ctx)
	check("InitMain", err, agenttest.Call{Op: "InitMain"})
	if g.Generation != 2 {
		t.Errorf("InitMain = %+v", g)
	}
	branchReq := agentapi.CreateBranchDataRequest{Parent: "main", Source: agentapi.Source{Type: agentapi.SourceParentLatest}}
	d, err := c.CreateBranchData(ctx, "dev", branchReq)
	check("CreateBranchData", err, agenttest.Call{Op: "CreateBranchData", Branch: "dev", Body: branchReq})
	if d != ops.BranchDataResult {
		t.Errorf("CreateBranchData = %+v", d)
	}
	check("DeleteBranchData", c.DeleteBranchData(ctx, "dev"), agenttest.Call{Op: "DeleteBranchData", Branch: "dev"})
	rpReq := agentapi.CreateRestorePointRequest{Label: "before"}
	rp, err := c.CreateRestorePoint(ctx, "main", rpReq)
	check("CreateRestorePoint", err, agenttest.Call{Op: "CreateRestorePoint", Branch: "main", Body: rpReq})
	if rp.ID != rpID || rp.Reason != agentapi.ReasonAPI {
		t.Errorf("CreateRestorePoint = %+v", rp)
	}
	rps, err := c.ListRestorePoints(ctx, "main")
	check("ListRestorePoints", err, agenttest.Call{Op: "ListRestorePoints", Branch: "main"})
	if len(rps) != 1 || rps[0].ID != rpID {
		t.Errorf("ListRestorePoints = %+v", rps)
	}
	check("DeleteRestorePoint", c.DeleteRestorePoint(ctx, "main", rpID), agenttest.Call{Op: "DeleteRestorePoint", Branch: "main", ID: rpID})
	gens, err := c.ListGenerations(ctx, "main")
	check("ListGenerations", err, agenttest.Call{Op: "ListGenerations", Branch: "main"})
	if gens.Current != 2 || len(gens.Generations) != 2 {
		t.Errorf("ListGenerations = %+v", gens)
	}
	genReq := agentapi.CreateGenerationRequest{Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: rpID}}
	g, err = c.CreateGeneration(ctx, "main", genReq)
	check("CreateGeneration", err, agenttest.Call{Op: "CreateGeneration", Branch: "main", Body: genReq})
	if g.Generation != 2 {
		t.Errorf("CreateGeneration = %+v", g)
	}
	gens, err = c.ActivateGeneration(ctx, "main", 2)
	check("ActivateGeneration", err, agenttest.Call{Op: "ActivateGeneration", Branch: "main", N: 2})
	if gens.Current != 2 {
		t.Errorf("ActivateGeneration = %+v", gens)
	}
	detReq := agentapi.DetachRequest{ToBranch: "main-before-20261007031500"}
	det, err := c.DetachGeneration(ctx, "main", 1, detReq)
	check("DetachGeneration", err, agenttest.Call{Op: "DetachGeneration", Branch: "main", N: 1, Body: detReq})
	if det.Branch != detReq.ToBranch {
		t.Errorf("DetachGeneration = %+v", det)
	}
	check("DeleteGeneration", c.DeleteGeneration(ctx, "main", 3), agenttest.Call{Op: "DeleteGeneration", Branch: "main", N: 3})
}

func TestClientErrors(t *testing.T) {
	ops := &agenttest.FakeOps{}
	url := newAgent(t, ops)
	c := agentclient.New(url, "t", nil)
	ctx := context.Background()

	ops.SetErr(&volume.RateLimitError{Source: "main", RetryAfter: 1500 * time.Millisecond})
	_, err := c.CreateRestorePoint(ctx, "main", agentapi.CreateRestorePointRequest{})
	var apiErr *agentclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests ||
		apiErr.Code != agentapi.CodeRateLimited || apiErr.RetryAfter != 2*time.Second {
		t.Fatalf("rate limited: %#v", err)
	}
	ops.SetErr(fmt.Errorf("%w (waited 2s)", volume.ErrBusy))
	err = c.DeleteBranchData(ctx, "dev")
	if code := agentclient.CodeOf(err); code != agentapi.CodeBusy {
		t.Fatalf("busy: code %q", code)
	}
	if prefix := "agent: 409 " + string(agentapi.CodeBusy) + ": "; !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("busy: Error() = %q, want prefix %q", err.Error(), prefix)
	}
	ops.SetErr(nil)
	if code := agentclient.CodeOf(agentclient.New(url, "wrong", nil).DeleteBranchData(ctx, "dev")); code != agentapi.CodeUnauthorized {
		t.Fatalf("wrong token: code %q", code)
	}
	if code := agentclient.CodeOf(errors.New("plain")); code != "" {
		t.Fatalf("CodeOf(plain error) = %q", code)
	}
}

func TestClientReadsForeignErrorBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream connect error", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	_, err := agentclient.New(srv.URL, "t", nil).Usage(context.Background())
	var apiErr *agentclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway || apiErr.Code != "" || apiErr.Message != "upstream connect error" {
		t.Fatalf("got %#v", err)
	}
	if got, want := err.Error(), "agent: 502: upstream connect error"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// The agent's mux answers a "." or ".." segment with a redirect to the cleaned path (307 since
// Go 1.27, 301 before). Following it
// would turn DELETE /v1/branches/main/restore-points/. into GET /v1/branches/main/restore-points
// and report success without deleting anything.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	ops := &agenttest.FakeOps{}
	url := newAgent(t, ops)
	hc := &http.Client{}
	for name, c := range map[string]*agentclient.Client{
		"default client": agentclient.New(url, "t", nil),
		"given client":   agentclient.New(url, "t", hc),
	} {
		err := c.DeleteRestorePoint(context.Background(), "main", ".")
		var apiErr *agentclient.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode < 300 || apiErr.StatusCode > 399 {
			t.Fatalf("%s: DeleteRestorePoint(main, \".\") = %v, want a 3xx APIError", name, err)
		}
		if got := ops.Last(); got.Op != "" {
			t.Fatalf("%s: the agent ran %+v", name, got)
		}
	}
	if hc.CheckRedirect != nil {
		t.Fatal("New changed the caller's http.Client")
	}
}
