package agent_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"consonance/internal/agent"
	"consonance/internal/agent/agenttest"
	"consonance/internal/agentapi"
	"consonance/internal/volume"
)

const token = "test-token"

func newServer(t *testing.T, ops *agenttest.FakeOps, tok string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer((&agent.Server{Token: tok, Ops: ops}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, withToken bool) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if withToken {
		req.Header.Set(agentapi.TokenHeader, token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

const rpID = "rp-20261007T031500Z-ab12"

// routes lists every /v1 route with a request that reaches the volume.
var routes = []struct {
	method, path, body string
	status             int
	want               agenttest.Call
}{
	{"GET", "/v1/preflight", "", 200, agenttest.Call{Op: "Preflight"}},
	{"GET", "/v1/usage", "", 200, agenttest.Call{Op: "Usage"}},
	{"POST", "/v1/init-main", "", 200, agenttest.Call{Op: "InitMain"}},
	{"POST", "/v1/branches/dev/data", `{"parent":"main","source":{"type":"ParentLatest"}}`, 200,
		agenttest.Call{Op: "CreateBranchData", Branch: "dev", Body: agentapi.CreateBranchDataRequest{
			Parent: "main", Source: agentapi.Source{Type: agentapi.SourceParentLatest}}}},
	{"DELETE", "/v1/branches/dev/data", "", 204, agenttest.Call{Op: "DeleteBranchData", Branch: "dev"}},
	{"POST", "/v1/branches/main/restore-points", `{"label":"before"}`, 200,
		agenttest.Call{Op: "CreateRestorePoint", Branch: "main", Body: agentapi.CreateRestorePointRequest{Label: "before"}}},
	{"GET", "/v1/branches/main/restore-points", "", 200, agenttest.Call{Op: "ListRestorePoints", Branch: "main"}},
	{"DELETE", "/v1/branches/main/restore-points/" + rpID, "", 204,
		agenttest.Call{Op: "DeleteRestorePoint", Branch: "main", ID: rpID}},
	{"GET", "/v1/branches/main/generations", "", 200, agenttest.Call{Op: "ListGenerations", Branch: "main"}},
	{"POST", "/v1/branches/main/generations", `{"source":{"type":"RestorePoint","restorePointId":"` + rpID + `"}}`, 200,
		agenttest.Call{Op: "CreateGeneration", Branch: "main", Body: agentapi.CreateGenerationRequest{
			Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: rpID}}}},
	{"POST", "/v1/branches/main/generations/2/activate", "", 200,
		agenttest.Call{Op: "ActivateGeneration", Branch: "main", N: 2}},
	{"POST", "/v1/branches/main/generations/1/detach", `{"toBranch":"main-before-20261007031500"}`, 200,
		agenttest.Call{Op: "DetachGeneration", Branch: "main", N: 1, Body: agentapi.DetachRequest{ToBranch: "main-before-20261007031500"}}},
	{"DELETE", "/v1/branches/main/generations/3", "", 204, agenttest.Call{Op: "DeleteGeneration", Branch: "main", N: 3}},
}

func TestRoutesReachTheVolume(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	for _, rt := range routes {
		resp, body := call(t, srv, rt.method, rt.path, rt.body, true)
		if resp.StatusCode != rt.status {
			t.Errorf("%s %s = %d %s, want %d", rt.method, rt.path, resp.StatusCode, body, rt.status)
			continue
		}
		if got := ops.Last(); !reflect.DeepEqual(got, rt.want) {
			t.Errorf("%s %s reached %+v, want %+v", rt.method, rt.path, got, rt.want)
		}
	}
}

func TestEveryV1RouteNeedsTheToken(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	noToken := newServer(t, ops, "") // a server without a token lets nobody in
	for _, rt := range routes {
		resp, body := call(t, srv, rt.method, rt.path, rt.body, false)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `"code":"unauthorized"`) {
			t.Errorf("%s %s without token = %d %s", rt.method, rt.path, resp.StatusCode, body)
		}
		if resp, _ := call(t, noToken, rt.method, rt.path, rt.body, true); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s on a server without token = %d", rt.method, rt.path, resp.StatusCode)
		}
	}
	if got := ops.Last(); got.Op != "" {
		t.Fatalf("an unauthorized request reached the volume: %+v", got)
	}
	if resp, _ := call(t, srv, "GET", "/healthz", "", false); resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 without a token", resp.StatusCode)
	}
}

func TestErrorsMapToStatusAndCode(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	for _, tc := range []struct {
		err    error
		status int
		code   agentapi.ErrorCode
	}{
		{fmt.Errorf("%w: branch name %q", volume.ErrInvalid, "Dev"), 400, agentapi.CodeInvalid},
		{fmt.Errorf("%w: branch %q", volume.ErrNotFound, "dev"), 404, agentapi.CodeNotFound},
		{fmt.Errorf("%w: branch %q is stopped", volume.ErrNotRunning, "main"), 409, agentapi.CodeNotRunning},
		{fmt.Errorf("%w (waited 2s)", volume.ErrBusy), 409, agentapi.CodeBusy},
		{fmt.Errorf("%w: branch %q exists", volume.ErrConflict, "x"), 409, agentapi.CodeConflict},
		{fmt.Errorf("%w: 91.0%% used", volume.ErrInsufficientStorage), 507, agentapi.CodeInsufficientStorage},
		{fmt.Errorf("%w: status of branch %q", volume.ErrSupervisorUnavailable, "main"), 502, agentapi.CodeSupervisorUnavailable},
		{&volume.RateLimitError{Source: "main", RetryAfter: 1200 * time.Millisecond}, 429, agentapi.CodeRateLimited},
		{errors.New("disk on fire"), 500, agentapi.CodeInternal},
	} {
		ops.SetErr(tc.err)
		resp, body := call(t, srv, "POST", "/v1/branches/dev/data", `{"parent":"main","source":{"type":"ParentLatest"}}`, true)
		var e agentapi.Error
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatalf("%v: body %q: %v", tc.err, body, err)
		}
		if resp.StatusCode != tc.status || e.Code != tc.code || e.Error != tc.err.Error() {
			t.Errorf("%v → %d %+v, want %d %s", tc.err, resp.StatusCode, e, tc.status, tc.code)
		}
		if tc.status == 429 && resp.Header.Get("Retry-After") != "2" {
			t.Errorf("Retry-After = %q, want 2 (1.2s rounded up)", resp.Header.Get("Retry-After"))
		}
	}
}

func TestBadRequestsStopAtTheAPI(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/v1/branches/dev/data", `{"parent":`},                  // broken JSON
		{"POST", "/v1/branches/dev/data", `{"parent":"main","extra":1}`}, // unknown field
		{"POST", "/v1/branches/main/generations/0/activate", ""},
		{"POST", "/v1/branches/main/generations/x/activate", ""},
		{"DELETE", "/v1/branches/main/generations/-1", ""},
		{"POST", "/v1/branches/main/restore-points", `{"label":"` + strings.Repeat("x", 70<<10) + `"}`}, // over 64 KiB
	} {
		resp, body := call(t, srv, rt.method, rt.path, rt.body, true)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"invalid"`) {
			t.Errorf("%s %s = %d %s, want 400 invalid", rt.method, rt.path, resp.StatusCode, body)
		}
	}
	if got := ops.Last(); got.Op != "" {
		t.Fatalf("a bad request reached the volume: %+v", got)
	}
}

func TestEmptyBodyIsAnEmptyRequest(t *testing.T) {
	ops := &agenttest.FakeOps{}
	srv := newServer(t, ops, token)
	if resp, body := call(t, srv, "POST", "/v1/branches/main/restore-points", "", true); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST without a body = %d %s", resp.StatusCode, body)
	}
	if got := ops.Last(); got.Body != (agentapi.CreateRestorePointRequest{}) {
		t.Fatalf("the volume got %+v", got.Body)
	}
}

func TestRestorePointListIsNeverNull(t *testing.T) {
	ops := &agenttest.FakeOps{} // returns a nil slice
	srv := newServer(t, ops, token)
	_, body := call(t, srv, "GET", "/v1/branches/main/restore-points", "", true)
	if strings.TrimSpace(body) != `{"restorePoints":[]}` {
		t.Fatalf("body = %s", body)
	}
}
