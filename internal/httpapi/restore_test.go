package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/controller"
)

const (
	rollbackURL = branchesURL + "/dev/rollback"
	resetURL    = branchesURL + "/dev/reset"
	rpDev       = "rp-20261024T010000Z-ab12"
	rpMain      = "rp-20261024T010100Z-cd34"
)

// restoreObjs is a ready project with a Running dev under main, and a restore point of dev in the
// agent's hands to be added by the caller.
func restoreObjs() []client.Object {
	return append(readyProjectObjs(testProject), branchObj("dev", "main", v1alpha1.BranchRunning))
}

// operatorAnswers plays the operator: after d it waits for spec.restore of the branch to appear, as the
// request writes it, and reports lastRestore with that request id.
func operatorAnswers(t *testing.T, c client.Client, name string, d time.Duration, last v1alpha1.RestoreStatus) {
	afterDelay(d, func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var b v1alpha1.Branch
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: name}, &b); err == nil && b.Spec.Restore != nil {
				last.RequestID = b.Spec.Restore.RequestID
				setBranchStatus(t, c, testNS, name, func(s *v1alpha1.BranchStatus) { s.LastRestore = &last })
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("the request never wrote spec.restore")
	})
}

// post sends a POST with a JSON body and the headers given.
func post(t *testing.T, srv *httptest.Server, path string, body any, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func TestRollback(t *testing.T) {
	srv, c, agents := newTestServer(t, restoreObjs()...)
	agents.add(testNS, agentapi.RestorePoint{ID: rpDev, Branch: "dev"})
	operatorAnswers(t, c, "dev", 300*time.Millisecond, v1alpha1.RestoreStatus{Phase: v1alpha1.RestoreDone, BackupBranch: "dev-before-20261024010203"})
	start := time.Now()
	resp, body := call(t, srv, http.MethodPost, rollbackURL, map[string]any{"restorePointId": rpDev})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("answered after %v, before the rollback was Done", elapsed)
	}
	v := decode[restoreResult](t, body)
	if v.BackupBranch != "dev-before-20261024010203" || v.Branch.Name != "dev" || v.RestorePointID != "" || v.RequestID != "" {
		t.Errorf("result = %+v", v)
	}
	b := getBranch(t, c, "dev")
	req := b.Spec.Restore
	if req == nil || req.Source.Type != v1alpha1.SourceRestorePoint || req.Source.RestorePointID != rpDev {
		t.Fatalf("spec.restore = %+v, want a RestorePoint source of %s", req, rpDev)
	}
	if id := resp.Header.Get("X-Request-Id"); id == "" || req.RequestID != id {
		t.Errorf("spec.restore.requestId = %q, want the X-Request-Id of the response %q", req.RequestID, id)
	}
}

func TestRollbackOfMainIsAllowed(t *testing.T) {
	srv, c, agents := newTestServer(t, readyProjectObjs(testProject)...)
	agents.add(testNS, agentapi.RestorePoint{ID: rpMain, Branch: "main"})
	operatorAnswers(t, c, "main", 200*time.Millisecond, v1alpha1.RestoreStatus{Phase: v1alpha1.RestoreDone, BackupBranch: "main-before-20261024010203"})
	resp, body := call(t, srv, http.MethodPost, branchesURL+"/main/rollback", map[string]any{"restorePointId": rpMain})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
}

func TestReset(t *testing.T) {
	srv, c, _ := newTestServer(t, restoreObjs()...)
	operatorAnswers(t, c, "dev", 300*time.Millisecond, v1alpha1.RestoreStatus{Phase: v1alpha1.RestoreDone, RestorePointID: rpDev})
	resp, body := call(t, srv, http.MethodPost, resetURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if v := decode[restoreResult](t, body); v.RestorePointID != rpDev || v.BackupBranch != "" || v.Branch.Name != "dev" {
		t.Errorf("result = %+v", v)
	}
	if req := getBranch(t, c, "dev").Spec.Restore; req == nil || req.Source.Type != v1alpha1.SourceParentLatest || req.Source.RestorePointID != "" {
		t.Errorf("spec.restore = %+v, want a ParentLatest source", req)
	}
}

func TestResetOfMainIsConflict(t *testing.T) {
	srv, c, _ := newTestServer(t, readyProjectObjs(testProject)...)
	resp, body := call(t, srv, http.MethodPost, branchesURL+"/main/reset", nil)
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
	if b := getBranch(t, c, "main"); b.Spec.Restore != nil {
		t.Errorf("spec.restore = %+v, want none", b.Spec.Restore)
	}
}

func TestResetNeedsARunningParent(t *testing.T) {
	for name, parent := range map[string]*v1alpha1.Branch{
		"stopped parent": branchObj("up", "main", v1alpha1.BranchStopped),
		"no parent":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			objs := append(readyProjectObjs(testProject), branchObj("dev", "up", v1alpha1.BranchRunning))
			if parent != nil {
				objs = append(objs, parent)
			}
			srv, c, _ := newTestServer(t, objs...)
			resp, body := call(t, srv, http.MethodPost, resetURL, nil)
			wantProblem(t, resp, body, http.StatusConflict, typeConflict)
			if b := getBranch(t, c, "dev"); b.Spec.Restore != nil {
				t.Errorf("spec.restore = %+v, want none", b.Spec.Restore)
			}
		})
	}
}

func TestSecondRestoreWhileOneRunsIsConflict(t *testing.T) {
	underWay := restoreObjs()
	underWay[len(underWay)-1].(*v1alpha1.Branch).Status.LastRestore = &v1alpha1.RestoreStatus{RequestID: "r1", Phase: v1alpha1.RestoreStarting}
	asked := restoreObjs()
	dev := asked[len(asked)-1].(*v1alpha1.Branch)
	dev.Spec.Restore = &v1alpha1.RestoreRequest{RequestID: "r2", Source: v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest}}
	dev.Status.LastRestore = &v1alpha1.RestoreStatus{RequestID: "r1", Phase: v1alpha1.RestoreDone}
	for name, objs := range map[string][]client.Object{"under way": underWay, "asked for": asked} {
		for url, body := range map[string]any{resetURL: nil, rollbackURL: map[string]any{"restorePointId": rpDev}} {
			t.Run(name+url, func(t *testing.T) {
				srv, c, agents := newTestServer(t, objs...)
				agents.add(testNS, agentapi.RestorePoint{ID: rpDev, Branch: "dev"})
				before := getBranch(t, c, "dev").Spec.Restore
				resp, b := call(t, srv, http.MethodPost, url, body)
				wantProblem(t, resp, b, http.StatusConflict, typeConflict)
				if after := getBranch(t, c, "dev").Spec.Restore; (before == nil) != (after == nil) || before != nil && *before != *after {
					t.Errorf("spec.restore changed from %+v to %+v", before, after)
				}
			})
		}
	}
}

func TestRollbackToAnotherBranchesRestorePointIsNotFound(t *testing.T) {
	srv, c, agents := newTestServer(t, restoreObjs()...)
	agents.add(testNS, agentapi.RestorePoint{ID: rpMain, Branch: "main"})
	resp, body := call(t, srv, http.MethodPost, rollbackURL, map[string]any{"restorePointId": rpMain})
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
	if b := getBranch(t, c, "dev"); b.Spec.Restore != nil {
		t.Errorf("spec.restore = %+v, want none", b.Spec.Restore)
	}
}

func TestRollbackNeedsRoomForTheBackup(t *testing.T) {
	backup := func(name string) *v1alpha1.Branch {
		b := branchObj(name, "main", v1alpha1.BranchStopped)
		b.Labels = map[string]string{controller.LabelBackupOf: "dev"}
		return b
	}
	objs := append(restoreObjs(), backup("dev-before-20261024010101"), backup("dev-before-20261024010102"))
	objs[1].(*v1alpha1.Project).Spec.Limits.MaxBranches = 2
	srv, c, agents := newTestServer(t, objs...)
	agents.add(testNS, agentapi.RestorePoint{ID: rpDev, Branch: "dev"})
	resp, body := call(t, srv, http.MethodPost, rollbackURL, map[string]any{"restorePointId": rpDev})
	wantProblem(t, resp, body, http.StatusTooManyRequests, typeRateLimited)
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if b := getBranch(t, c, "dev"); b.Spec.Restore != nil {
		t.Errorf("spec.restore = %+v, want none", b.Spec.Restore)
	}
}

func TestRollbackRejects(t *testing.T) {
	srv, c, _ := newTestServer(t, restoreObjs()...)
	for name, body := range map[string]any{
		"no body":        nil,
		"empty id":       map[string]any{"restorePointId": ""},
		"id of bad form": map[string]any{"restorePointId": "rp-1"},
		"unknown field":  map[string]any{"restorePointId": rpDev, "x": 1},
	} {
		t.Run(name, func(t *testing.T) {
			resp, b := call(t, srv, http.MethodPost, rollbackURL, body)
			wantProblem(t, resp, b, http.StatusBadRequest, typeValidation)
		})
	}
	if b := getBranch(t, c, "dev"); b.Spec.Restore != nil {
		t.Errorf("spec.restore = %+v, want none", b.Spec.Restore)
	}
	for name, path := range map[string]string{
		"no branch":     branchesURL + "/nope/reset",
		"no project":    "/v1/projects/cccccccccccc/branches/dev/reset",
		"bad name form": branchesURL + "/Dev/reset",
	} {
		t.Run(name, func(t *testing.T) {
			resp, b := call(t, srv, http.MethodPost, path, nil)
			wantProblem(t, resp, b, http.StatusNotFound, typeNotFound)
		})
	}
}

func TestRestoreFailed(t *testing.T) {
	srv, c, _ := newTestServer(t, restoreObjs()...)
	operatorAnswers(t, c, "dev", 200*time.Millisecond, v1alpha1.RestoreStatus{Phase: v1alpha1.RestoreFailed, Message: "the copy failed: disk full"})
	resp, body := call(t, srv, http.MethodPost, resetURL, nil)
	p := wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)
	if p.Detail != "the copy failed: disk full" {
		t.Errorf("detail = %q, want the message of lastRestore", p.Detail)
	}
}

// The wait is Waits.Restore, 30s by default (spec 14); the test shortens it.
func TestRestoreAfterThirtySecondsIsAccepted(t *testing.T) {
	if got := (Waits{}).withDefaults().Restore; got != 30*time.Second {
		t.Fatalf("default restore wait = %v, want 30s", got)
	}
	srv, c, _ := newTestServerWith(t, func(cfg *Config) { cfg.Waits.Restore = 200 * time.Millisecond }, restoreObjs()...)
	start := time.Now()
	resp, body := call(t, srv, http.MethodPost, resetURL, nil)
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	if elapsed < 200*time.Millisecond || elapsed >= 2*time.Second {
		t.Errorf("elapsed = %v, want the wait of 200ms", elapsed)
	}
	v := decode[restoreResult](t, body)
	req := getBranch(t, c, "dev").Spec.Restore
	if v.RequestID == "" || req == nil || v.RequestID != req.RequestID || v.Branch.Name != "dev" {
		t.Errorf("result = %+v, want the request id of spec.restore %+v", v, req)
	}
}

// A lastRestore of another request, even a finished one, does not end the wait.
func TestRestoreWaitIgnoresAnEarlierResult(t *testing.T) {
	objs := restoreObjs()
	objs[len(objs)-1].(*v1alpha1.Branch).Status.LastRestore = &v1alpha1.RestoreStatus{RequestID: "old", Phase: v1alpha1.RestoreDone, BackupBranch: "dev-before-20250101000000"}
	srv, _, _ := newTestServerWith(t, func(cfg *Config) { cfg.Waits.Restore = 300 * time.Millisecond }, objs...)
	resp, body := call(t, srv, http.MethodPost, resetURL, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
}

func TestRestoreWithTheSameRequestIDReturnsTheEarlierResult(t *testing.T) {
	var updates atomic.Int32
	objs := restoreObjs()
	dev := objs[len(objs)-1].(*v1alpha1.Branch)
	dev.Status.LastRestore = &v1alpha1.RestoreStatus{RequestID: "r1", Phase: v1alpha1.RestoreDone, BackupBranch: "dev-before-20261024010203"}
	// The restore point is not in the agent: the earlier request has been through the checks.
	srv, _, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates.Add(1)
				return c.Update(ctx, obj, opts...)
			},
		})
	}, objs...)
	resp, body := post(t, srv, rollbackURL, map[string]any{"restorePointId": rpDev}, map[string]string{"X-Request-Id": "r1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if v := decode[restoreResult](t, body); v.BackupBranch != "dev-before-20261024010203" {
		t.Errorf("result = %+v, want the earlier backup branch", v)
	}
	if n := updates.Load(); n != 0 {
		t.Errorf("%d Updates, want none", n)
	}
}

// An id that was used for a rollback to a restore point is that request only: another restore
// point, or a reset, under the same id is a different request and must not get the old result.
func TestReusedRequestIDForADifferentRollbackIsConflict(t *testing.T) {
	srv, updates := reusedIDServer(t)
	resp, body := post(t, srv, rollbackURL, map[string]any{"restorePointId": rpMain}, map[string]string{"X-Request-Id": "r1"})
	p := wantProblem(t, resp, body, http.StatusConflict, typeConflict)
	if p.Detail != "X-Request-Id r1 was used for a different request" {
		t.Errorf("detail = %q", p.Detail)
	}
	if n := updates.Load(); n != 0 {
		t.Errorf("%d Updates, want none", n)
	}
	// The same request still gets its result.
	resp, body = post(t, srv, rollbackURL, map[string]any{"restorePointId": rpDev}, map[string]string{"X-Request-Id": "r1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same rollback: status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if v := decode[restoreResult](t, body); v.BackupBranch != "dev-before-20261024010203" || v.RestorePointID != "" {
		t.Errorf("result = %+v", v)
	}
}

func TestReusedRequestIDForResetAfterRollbackIsConflict(t *testing.T) {
	srv, updates := reusedIDServer(t)
	resp, body := post(t, srv, resetURL, nil, map[string]string{"X-Request-Id": "r1"})
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
	if n := updates.Load(); n != 0 {
		t.Errorf("%d Updates, want none", n)
	}
}

// The id of the open request in the spec is held to the same: it is that request only.
func TestReusedRequestIDOfTheOpenRequestForADifferentOneIsConflict(t *testing.T) {
	objs := restoreObjs()
	dev := objs[len(objs)-1].(*v1alpha1.Branch)
	dev.Spec.Restore = &v1alpha1.RestoreRequest{RequestID: "r1", Source: v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest}}
	srv, _, _ := newTestServer(t, objs...)
	resp, body := post(t, srv, rollbackURL, map[string]any{"restorePointId": rpDev}, map[string]string{"X-Request-Id": "r1"})
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
}

// reusedIDServer is a project whose dev rolled back to rpDev under the request id r1, with the
// restore points of dev and main in the agent, and a count of the Updates the server makes.
func reusedIDServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var updates atomic.Int32
	objs := restoreObjs()
	dev := objs[len(objs)-1].(*v1alpha1.Branch)
	dev.Status.LastRestore = &v1alpha1.RestoreStatus{
		RequestID: "r1", Phase: v1alpha1.RestoreDone, BackupBranch: "dev-before-20261024010203",
		Source: v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: rpDev},
	}
	srv, _, agents := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates.Add(1)
				return c.Update(ctx, obj, opts...)
			},
		})
	}, objs...)
	agents.add(testNS, agentapi.RestorePoint{ID: rpDev, Branch: "dev"})
	agents.add(testNS, agentapi.RestorePoint{ID: rpMain, Branch: "dev"})
	return srv, &updates
}

// A request repeated while its first one still waits is the same request, not a conflict.
func TestRestoreWithTheRequestIDOfTheOpenRequestWaitsForIt(t *testing.T) {
	objs := restoreObjs()
	dev := objs[len(objs)-1].(*v1alpha1.Branch)
	dev.Spec.Restore = &v1alpha1.RestoreRequest{RequestID: "r1", Source: v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest}}
	srv, c, _ := newTestServer(t, objs...)
	afterDelay(200*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) {
			s.LastRestore = &v1alpha1.RestoreStatus{RequestID: "r1", Phase: v1alpha1.RestoreDone, RestorePointID: rpDev}
		})
	})
	resp, body := post(t, srv, resetURL, nil, map[string]string{"X-Request-Id": "r1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if v := decode[restoreResult](t, body); v.RestorePointID != rpDev {
		t.Errorf("result = %+v", v)
	}
}

func TestRestoreUpdateRetriesConflict(t *testing.T) {
	var updates atomic.Int32
	srv, c, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if updates.Add(1) == 1 {
					return apierrors.NewConflict(v1alpha1.GroupVersion.WithResource("branches").GroupResource(), obj.GetName(), nil)
				}
				return c.Update(ctx, obj, opts...)
			},
		})
	}, restoreObjs()...)
	operatorAnswers(t, c, "dev", 200*time.Millisecond, v1alpha1.RestoreStatus{Phase: v1alpha1.RestoreDone, RestorePointID: rpDev})
	resp, body := call(t, srv, http.MethodPost, resetURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if n := updates.Load(); n != 2 {
		t.Errorf("%d Updates, want 2 (the conflict and the retry)", n)
	}
}

func TestRestoreUpdateGivesUpAfterThreeConflicts(t *testing.T) {
	var updates atomic.Int32
	srv, _, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates.Add(1)
				return apierrors.NewConflict(v1alpha1.GroupVersion.WithResource("branches").GroupResource(), obj.GetName(), nil)
			},
		})
	}, restoreObjs()...)
	resp, body := call(t, srv, http.MethodPost, resetURL, nil)
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
	if n := updates.Load(); n != 3 {
		t.Errorf("%d Updates, want 3", n)
	}
}

// The branch may be missing from the cache for a moment; only when the cluster does not have it
// either is it gone.
func TestRestoreWaitCacheMissAndDeletion(t *testing.T) {
	var written atomic.Bool
	var misses atomic.Int32
	srv, c, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				err := c.Update(ctx, obj, opts...)
				written.Store(err == nil)
				return err
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*v1alpha1.Branch); ok && key.Name == "dev" && written.Load() && misses.Add(1) <= 2 {
					return apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("branches").GroupResource(), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	}, restoreObjs()...)
	operatorAnswers(t, c, "dev", 100*time.Millisecond, v1alpha1.RestoreStatus{Phase: v1alpha1.RestoreDone, RestorePointID: rpDev})
	resp, body := call(t, srv, http.MethodPost, resetURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a cache miss is not a 404); body %s", resp.StatusCode, body)
	}
	if n := misses.Load(); n < 3 {
		t.Errorf("only %d Gets: the cache miss was not exercised", n)
	}

	// Deleted in between: that is a 404.
	srv, c, _ = newTestServer(t, restoreObjs()...)
	afterDelay(100*time.Millisecond, func() {
		b := getBranch(t, c, "dev")
		if err := c.Delete(context.Background(), &b); err != nil {
			t.Error(err)
		}
	})
	resp, body = call(t, srv, http.MethodPost, resetURL, nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
}

func TestRestoreOfDeletingBranchIsConflict(t *testing.T) {
	objs := append(readyProjectObjs(testProject), branchObj("dev", "main", v1alpha1.BranchDeleting))
	srv, _, _ := newTestServer(t, objs...)
	resp, body := call(t, srv, http.MethodPost, resetURL, nil)
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
}
