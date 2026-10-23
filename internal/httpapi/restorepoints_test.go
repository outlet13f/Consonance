package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/controller"
)

const (
	rpURL = branchesURL + "/main/restore-points"
	rpOne = "rp-20261007T031500Z-ab12"
	rpTwo = "rp-20261007T031000Z-cd34"
)

func TestCreateRestorePoint(t *testing.T) {
	srv, _, agents := newTestServer(t, readyProjectObjs(testProject)...)
	resp, body := call(t, srv, http.MethodPost, rpURL, map[string]any{"label": "before schema change"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", resp.StatusCode, body)
	}
	v := decode[restorePointView](t, body)
	if v.Reason != "api" || v.Label != "before schema change" || v.Branch != "main" || !agentapi.ValidRestorePointID(v.ID) || v.BackupStartLSN == "" || v.CreatedAt.IsZero() {
		t.Errorf("view = %+v", v)
	}
	if got := agents.seen(); len(got) != 1 || got[0] != testToken {
		t.Errorf("tokens = %v, want the one of the Secret", got)
	}
	if got := agents.restorePoints(testNS, "main"); len(got) != 1 || got[0].ID != v.ID {
		t.Errorf("the agent has %v", got)
	}
	// The body is optional.
	if resp, body := call(t, srv, http.MethodPost, rpURL, nil); resp.StatusCode != http.StatusCreated {
		t.Errorf("without a body: status = %d; body %s", resp.StatusCode, body)
	}
}

func TestCreateRestorePointRejects(t *testing.T) {
	objs := append(readyProjectObjs(testProject), branchObj("stopped", "main", v1alpha1.BranchStopped))
	objs = append(objs, projectObjs("bbbbbbbbbbbb", "provisioning", v1alpha1.ProjectStatus{})...)
	srv, _, agents := newTestServer(t, objs...)
	for name, tc := range map[string]struct {
		path   string
		body   map[string]any
		status int
		typ    problemType
	}{
		"257 byte label":       {rpURL, map[string]any{"label": strings.Repeat("a", 257)}, 400, typeValidation},
		"unknown field":        {rpURL, map[string]any{"name": "x"}, 400, typeValidation},
		"stopped branch":       {branchesURL + "/stopped/restore-points", nil, 409, typeConflict},
		"no branch":            {branchesURL + "/nope/restore-points", nil, 404, typeNotFound},
		"project not ready":    {"/v1/projects/bbbbbbbbbbbb/branches/main/restore-points", nil, 409, typeConflict},
		"no project":           {"/v1/projects/cccccccccccc/branches/main/restore-points", nil, 404, typeNotFound},
		"branch of a bad form": {branchesURL + "/Main/restore-points", nil, 404, typeNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			resp, b := call(t, srv, http.MethodPost, tc.path, tc.body)
			wantProblem(t, resp, b, tc.status, tc.typ)
		})
	}
	// 256 bytes are fine.
	if resp, b := call(t, srv, http.MethodPost, rpURL, map[string]any{"label": strings.Repeat("a", 256)}); resp.StatusCode != http.StatusCreated {
		t.Errorf("256 byte label: status = %d; body %s", resp.StatusCode, b)
	}
	if got := agents.seen(); len(got) != 1 {
		t.Errorf("the agent was called %d times; only the 256 byte label gets that far", len(got))
	}
}

func TestCreateRestorePointAgentErrors(t *testing.T) {
	srv, _, agents := newTestServer(t, readyProjectObjs(testProject)...)
	agents.failNext("CreateRestorePoint", &agentclient.APIError{
		StatusCode: 429, Code: agentapi.CodeRateLimited, Message: "too soon", RetryAfter: 3 * time.Second,
	})
	resp, body := call(t, srv, http.MethodPost, rpURL, nil)
	wantProblem(t, resp, body, http.StatusTooManyRequests, typeRateLimited)
	if got := resp.Header.Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}

	agents.failNext("CreateRestorePoint", &agentclient.APIError{StatusCode: 507, Code: agentapi.CodeInsufficientStorage, Message: "volume is 91% full"})
	resp, body = call(t, srv, http.MethodPost, rpURL, nil)
	wantProblem(t, resp, body, http.StatusInsufficientStorage, typeInsufficientStorage)

	agents.failNext("CreateRestorePoint", errors.New("connection refused"))
	resp, body = call(t, srv, http.MethodPost, rpURL, nil)
	wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)
}

func TestListRestorePoints(t *testing.T) {
	srv, _, agents := newTestServer(t, readyProjectObjs(testProject)...)

	// The agent has no data for the branch yet: no restore points, not an error.
	resp, body := call(t, srv, http.MethodGet, rpURL, nil)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != `{"restorePoints":[]}` {
		t.Fatalf("no data: status = %d, body %s, want 200 and an empty list", resp.StatusCode, body)
	}

	agents.add(testNS, agentapi.RestorePoint{ID: rpOne, Branch: "main", Reason: "api", Label: "newer"})
	agents.add(testNS, agentapi.RestorePoint{ID: rpTwo, Branch: "main", Reason: "reset"})
	resp, body = call(t, srv, http.MethodGet, rpURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	got := decode[struct {
		RestorePoints []restorePointView `json:"restorePoints"`
	}](t, body).RestorePoints
	if len(got) != 2 || got[0].ID != rpOne || got[1].ID != rpTwo || got[0].Label != "newer" || got[1].Reason != "reset" {
		t.Errorf("restore points = %+v, want the agent's two in its order", got)
	}

	agents.failNext("ListRestorePoints", errors.New("boom"))
	resp, body = call(t, srv, http.MethodGet, rpURL, nil)
	wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)

	resp, body = call(t, srv, http.MethodGet, branchesURL+"/nope/restore-points", nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
}

func TestDeleteRestorePoint(t *testing.T) {
	srv, _, agents := newTestServer(t, readyProjectObjs(testProject)...)
	agents.add(testNS, agentapi.RestorePoint{ID: rpOne, Branch: "main"})

	resp, body := call(t, srv, http.MethodDelete, rpURL+"/"+rpOne, nil)
	if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("status = %d, body %q, want 204 and none", resp.StatusCode, body)
	}
	if got := agents.restorePoints(testNS, "main"); len(got) != 0 {
		t.Errorf("the agent still has %v", got)
	}

	// Deleting again is the agent's 404 (decision 5).
	resp, body = call(t, srv, http.MethodDelete, rpURL+"/"+rpOne, nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)

	before := len(agents.seen())
	for _, id := range []string{"rp-1", "RP-20261007T031500Z-ab12", "rp-20261007T031500Z-ab12x"} {
		resp, body = call(t, srv, http.MethodDelete, rpURL+"/"+id, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("id %q: status = %d; body %s", id, resp.StatusCode, body)
		}
	}
	if got := len(agents.seen()); got != before {
		t.Errorf("the agent was called %d times for ids of the wrong form", got-before)
	}
}

func TestMissingTokenIsInternal(t *testing.T) {
	var objs []client.Object
	for _, o := range readyProjectObjs(testProject) {
		if o.GetName() != controller.InternalTokenSecret {
			objs = append(objs, o)
		}
	}
	srv, _, agents := newTestServer(t, objs...)
	resp, body := call(t, srv, http.MethodGet, rpURL, nil)
	wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)
	if len(agents.seen()) != 0 {
		t.Error("the agent was called without a token")
	}
}

func TestEmptyTokenIsInternal(t *testing.T) {
	objs := readyProjectObjs(testProject)
	for _, o := range objs {
		if s, ok := o.(*corev1.Secret); ok {
			s.Data[controller.TokenKey] = nil
		}
	}
	srv, _, _ := newTestServer(t, objs...)
	resp, body := call(t, srv, http.MethodGet, rpURL, nil)
	wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)
}
