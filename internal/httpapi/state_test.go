package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"consonance/api/v1alpha1"
)

const (
	stopURL  = branchesURL + "/dev/stop"
	startURL = branchesURL + "/dev/start"
)

// stateObjs is a ready project with the branch dev in the state and phase given.
func stateObjs(state v1alpha1.DesiredState, phase v1alpha1.BranchPhase) []client.Object {
	dev := branchObj("dev", "main", phase)
	dev.Spec.State = state
	return append(readyProjectObjs(testProject), dev)
}

func TestStopWaits(t *testing.T) {
	srv, c, _ := newTestServer(t, stateObjs(v1alpha1.StateRunning, v1alpha1.BranchRunning)...)
	afterDelay(200*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) { s.Phase = v1alpha1.BranchStopped })
	})
	start := time.Now()
	resp, body := call(t, srv, http.MethodPost, stopURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("answered after %v, before the branch was Stopped", elapsed)
	}
	if v := decode[branchView](t, body); v.Name != "dev" || v.Phase != "Stopped" || v.State != "Stopped" {
		t.Errorf("view = %+v", v)
	}
	if b := getBranch(t, c, "dev"); b.Spec.State != v1alpha1.StateStopped {
		t.Errorf("spec.state = %q, want Stopped", b.Spec.State)
	}
}

func TestStartWaits(t *testing.T) {
	srv, c, _ := newTestServer(t, stateObjs(v1alpha1.StateStopped, v1alpha1.BranchStopped)...)
	afterDelay(200*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) { s.Phase = v1alpha1.BranchRunning })
	})
	resp, body := call(t, srv, http.MethodPost, startURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if v := decode[branchView](t, body); v.Phase != "Running" || v.State != "Running" {
		t.Errorf("view = %+v", v)
	}
	if b := getBranch(t, c, "dev"); b.Spec.State != v1alpha1.StateRunning {
		t.Errorf("spec.state = %q, want Running", b.Spec.State)
	}
}

// The wait is Waits.State, 10s by default (spec 14); the test shortens it.
func TestStartAfterTenSecondsIsAccepted(t *testing.T) {
	if got := (Waits{}).withDefaults().State; got != 10*time.Second {
		t.Fatalf("default state wait = %v, want 10s", got)
	}
	srv, c, _ := newTestServerWith(t, func(cfg *Config) { cfg.Waits.State = 200 * time.Millisecond },
		stateObjs(v1alpha1.StateStopped, v1alpha1.BranchStopped)...)
	start := time.Now()
	resp, body := call(t, srv, http.MethodPost, startURL, nil)
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	if elapsed < 200*time.Millisecond || elapsed >= 2*time.Second {
		t.Errorf("elapsed = %v, want the wait of 200ms", elapsed)
	}
	if v := decode[branchView](t, body); v.Phase != "Stopped" || v.State != "Running" {
		t.Errorf("view = %+v, want what is true now: asked to run, still Stopped", v)
	}
	if b := getBranch(t, c, "dev"); b.Spec.State != v1alpha1.StateRunning {
		t.Errorf("spec.state = %q, want Running: the request stands after the 202", b.Spec.State)
	}
}

func TestStartFailure(t *testing.T) {
	srv, c, _ := newTestServer(t, stateObjs(v1alpha1.StateStopped, v1alpha1.BranchStopped)...)
	afterDelay(200*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) {
			s.Phase = v1alpha1.BranchFailed
			s.Conditions = []metav1.Condition{{
				Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonStartFailed, Message: "postgres exited",
			}}
		})
	})
	resp, body := call(t, srv, http.MethodPost, startURL, nil)
	p := wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)
	if want := "postgres exited"; !strings.Contains(p.Detail, want) {
		t.Errorf("detail = %q, want the condition message %q in it", p.Detail, want)
	}
}

func TestStopWhileRestoringIsConflict(t *testing.T) {
	open := stateObjs(v1alpha1.StateRunning, v1alpha1.BranchRestoring)
	open[len(open)-1].(*v1alpha1.Branch).Status.LastRestore = &v1alpha1.RestoreStatus{RequestID: "r1", Phase: v1alpha1.RestoreStopping}
	asked := stateObjs(v1alpha1.StateRunning, v1alpha1.BranchRunning)
	asked[len(asked)-1].(*v1alpha1.Branch).Spec.Restore = &v1alpha1.RestoreRequest{RequestID: "r2", Source: v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest}}
	for name, objs := range map[string][]client.Object{"under way": open, "asked for": asked} {
		for _, url := range []string{stopURL, startURL} {
			srv, c, _ := newTestServer(t, objs...)
			resp, body := call(t, srv, http.MethodPost, url, nil)
			wantProblem(t, resp, body, http.StatusConflict, typeConflict)
			if b := getBranch(t, c, "dev"); b.Spec.State != v1alpha1.StateRunning {
				t.Errorf("%s %s: spec.state = %q, want it untouched", name, url, b.Spec.State)
			}
		}
	}
}

func TestStopAlreadyStoppedDoesNotWrite(t *testing.T) {
	var updates atomic.Int32
	srv, _, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates.Add(1)
				return c.Update(ctx, obj, opts...)
			},
		})
	}, stateObjs(v1alpha1.StateStopped, v1alpha1.BranchStopped)...)
	resp, body := call(t, srv, http.MethodPost, stopURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if n := updates.Load(); n != 0 {
		t.Errorf("%d Updates, want none", n)
	}
}

func TestStateUpdateRetriesConflict(t *testing.T) {
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
	}, stateObjs(v1alpha1.StateRunning, v1alpha1.BranchRunning)...)
	afterDelay(300*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) { s.Phase = v1alpha1.BranchStopped })
	})
	resp, body := call(t, srv, http.MethodPost, stopURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if n := updates.Load(); n != 2 {
		t.Errorf("%d Updates, want 2 (the conflict and the retry)", n)
	}
	if b := getBranch(t, c, "dev"); b.Spec.State != v1alpha1.StateStopped {
		t.Errorf("spec.state = %q", b.Spec.State)
	}
}

func TestStateUpdateGivesUpAfterThreeConflicts(t *testing.T) {
	var updates atomic.Int32
	srv, _, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates.Add(1)
				return apierrors.NewConflict(v1alpha1.GroupVersion.WithResource("branches").GroupResource(), obj.GetName(), nil)
			},
		})
	}, stateObjs(v1alpha1.StateRunning, v1alpha1.BranchRunning)...)
	resp, body := call(t, srv, http.MethodPost, stopURL, nil)
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
	if n := updates.Load(); n != 3 {
		t.Errorf("%d Updates, want 3", n)
	}
}

// The cache may hold the Branch as it was before the write: a phase that was the wanted one
// before the request says nothing, so the request waits for the operator to observe the spec.
func TestStateWaitIgnoresAStalePhase(t *testing.T) {
	objs := stateObjs(v1alpha1.StateRunning, v1alpha1.BranchStopped) // a stop is under way, the phase is old
	objs[len(objs)-1].(*v1alpha1.Branch).Status.ObservedGeneration = 0
	srv, c, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Waits.State = 300 * time.Millisecond
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				obj.SetGeneration(obj.GetGeneration() + 1) // what a real API server does to a changed spec
				return c.Update(ctx, obj, opts...)
			},
		})
	}, objs...)
	_ = c
	// Stopped is the wanted phase of the stop and the Branch already has it, but from before.
	resp, body := call(t, srv, http.MethodPost, stopURL, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: the operator has not looked at the new spec; body %s", resp.StatusCode, body)
	}
}

// The branch may be missing from the cache for a moment; only when the cluster does not have it
// either is it gone.
func TestStateWaitCacheMissAndDeletion(t *testing.T) {
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
	}, stateObjs(v1alpha1.StateRunning, v1alpha1.BranchRunning)...)
	afterDelay(100*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) { s.Phase = v1alpha1.BranchStopped })
	})
	resp, body := call(t, srv, http.MethodPost, stopURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a cache miss is not a 404); body %s", resp.StatusCode, body)
	}
	if n := misses.Load(); n < 3 {
		t.Errorf("only %d Gets: the cache miss was not exercised", n)
	}

	// Deleted in between: that is a 404.
	srv, c, _ = newTestServer(t, stateObjs(v1alpha1.StateRunning, v1alpha1.BranchRunning)...)
	afterDelay(100*time.Millisecond, func() {
		b := getBranch(t, c, "dev")
		if err := c.Delete(context.Background(), &b); err != nil {
			t.Error(err)
		}
	})
	resp, body = call(t, srv, http.MethodPost, stopURL, nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
}

func TestStateErrors(t *testing.T) {
	srv, _, _ := newTestServer(t, stateObjs(v1alpha1.StateRunning, v1alpha1.BranchRunning)...)
	for _, url := range []string{branchesURL + "/nope/stop", branchesURL + "/NOPE/start", "/v1/projects/bbbbbbbbbbbb/branches/dev/stop"} {
		resp, body := call(t, srv, http.MethodPost, url, nil)
		wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
	}
	if resp, body := callWith(t, srv, http.MethodPost, stopURL, nil, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a key: %d %s", resp.StatusCode, body)
	}
	if resp, _ := call(t, srv, http.MethodGet, stopURL, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET stop: %d, want 405", resp.StatusCode)
	}
}

// A project that is not Ready may still stop its branches, but not start them.
func TestStopNeedsNoReadyProjectButStartDoes(t *testing.T) {
	objs := projectObjs(testProject, "p", v1alpha1.ProjectStatus{Phase: v1alpha1.ProjectFailed})
	dev := branchObj("dev", "main", v1alpha1.BranchRunning)
	objs = append(objs, dev)
	srv, c, _ := newTestServerWith(t, func(cfg *Config) { cfg.Waits.State = 200 * time.Millisecond }, objs...)
	resp, body := call(t, srv, http.MethodPost, startURL, nil)
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
	resp, body = call(t, srv, http.MethodPost, stopURL, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: status = %d, want 202 (written, not yet Stopped); body %s", resp.StatusCode, body)
	}
	if b := getBranch(t, c, "dev"); b.Spec.State != v1alpha1.StateStopped {
		t.Errorf("spec.state = %q, want Stopped", b.Spec.State)
	}
}

func TestStateOfDeletingBranchIsConflict(t *testing.T) {
	objs := stateObjs(v1alpha1.StateRunning, v1alpha1.BranchDeleting)
	srv, _, _ := newTestServer(t, objs...)
	resp, body := call(t, srv, http.MethodPost, stopURL, nil)
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
}

func TestRestoreOpen(t *testing.T) {
	req := func(id string) *v1alpha1.RestoreRequest { return &v1alpha1.RestoreRequest{RequestID: id} }
	last := func(id string, p v1alpha1.RestorePhase) *v1alpha1.RestoreStatus {
		return &v1alpha1.RestoreStatus{RequestID: id, Phase: p}
	}
	tests := []struct {
		name string
		spec *v1alpha1.RestoreRequest
		last *v1alpha1.RestoreStatus
		want bool
	}{
		{"empty", nil, nil, false},
		{"under way", req("r1"), last("r1", v1alpha1.RestoreStopping), true},
		{"under way without a request in the spec", nil, last("r1", v1alpha1.RestorePreparing), true},
		{"done", req("r1"), last("r1", v1alpha1.RestoreDone), false},
		{"failed", req("r1"), last("r1", v1alpha1.RestoreFailed), false},
		{"done, spec cleared", nil, last("r1", v1alpha1.RestoreDone), false},
		{"new request id", req("r2"), last("r1", v1alpha1.RestoreDone), true},
		{"first request", req("r1"), nil, true},
		{"empty request id", req(""), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &v1alpha1.Branch{Spec: v1alpha1.BranchSpec{Restore: tt.spec}, Status: v1alpha1.BranchStatus{LastRestore: tt.last}}
			if got := restoreOpen(b); got != tt.want {
				t.Errorf("restoreOpen = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartOfABranchWhoseDataFailedIsConflict(t *testing.T) {
	srv, _, updates := dataFailedServer(t, dataFailedObjs(v1alpha1.StateStopped))
	resp, body := call(t, srv, http.MethodPost, startURL, nil)
	wantDataFailed(t, resp, body, updates)
}
