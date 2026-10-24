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
	"consonance/internal/agentapi"
	"consonance/internal/controller"
)

const (
	testProject = "aaaaaaaaaaaa"
	testNS      = "cns-" + testProject
	branchesURL = "/v1/projects/" + testProject + "/branches"
)

// branchObj is a branch of the test project in the given phase.
func branchObj(name, parent string, phase v1alpha1.BranchPhase) *v1alpha1.Branch {
	return &v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name},
		Spec:       v1alpha1.BranchSpec{Parent: parent, State: v1alpha1.StateRunning},
		Status:     v1alpha1.BranchStatus{Phase: phase, DataGeneration: 1},
	}
}

func getBranch(t *testing.T, c client.Client, name string) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: name}, &b); err != nil {
		t.Fatalf("get branch %s: %v", name, err)
	}
	return b
}

func TestCreateBranchWaitsForData(t *testing.T) {
	srv, c, _ := newTestServer(t, readyProjectObjs(testProject)...)
	afterDelay(300*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) {
			s.Phase = v1alpha1.BranchStarting
			s.DataGeneration = 1
		})
	})
	start := time.Now()
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "dev"})
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", resp.StatusCode, body)
	}
	if elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want about 300ms", elapsed)
	}
	v := decode[branchView](t, body)
	if v.Name != "dev" || v.Parent != "main" {
		t.Errorf("view = %+v, want dev of main", v)
	}
	b := getBranch(t, c, "dev")
	if b.Spec.Source.Type != v1alpha1.SourceParentLatest || b.Spec.State != v1alpha1.StateRunning || b.Spec.Parent != "main" {
		t.Errorf("spec = %+v", b.Spec)
	}
	if len(b.OwnerReferences) != 0 || b.Spec.Protected {
		t.Errorf("a user branch has no owner and is not protected: %+v", b)
	}
}

// The API server reads through an informer cache, which may not have the Branch it just made.
func TestCreateBranchWaitsForTheCacheToSeeIt(t *testing.T) {
	var created atomic.Bool
	var misses atomic.Int32
	srv, c, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				err := c.Create(ctx, obj, opts...)
				if _, ok := obj.(*v1alpha1.Branch); ok && err == nil {
					created.Store(true)
				}
				return err
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*v1alpha1.Branch); ok && key.Name == "dev" && created.Load() && misses.Add(1) <= 2 {
					return apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("branches").GroupResource(), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	}, readyProjectObjs(testProject)...)
	setBranchStatus := func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) { s.DataGeneration = 1 })
	}
	afterDelay(100*time.Millisecond, setBranchStatus)
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "dev"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", resp.StatusCode, body)
	}
	if n := misses.Load(); n < 3 {
		t.Errorf("only %d Gets of the new branch: the cache miss was not exercised", n)
	}
}

func TestCreateBranchWithStateStopped(t *testing.T) {
	srv, c, _ := newTestServer(t, readyProjectObjs(testProject)...)
	afterDelay(100*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) { s.DataGeneration = 1 })
	})
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "dev", "state": "Stopped"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	if got := decode[branchView](t, body).State; got != "Stopped" {
		t.Errorf("state = %q", got)
	}
	if b := getBranch(t, c, "dev"); b.Spec.State != v1alpha1.StateStopped {
		t.Errorf("spec.state = %q", b.Spec.State)
	}
}

// The default wait is 5s (TestDefaultWaits); a short one stands in for it here.
func TestCreateBranchAfterFiveSecondsIsAccepted(t *testing.T) {
	srv, c, _ := newTestServerWith(t, func(cfg *Config) { cfg.Waits.Create = 200 * time.Millisecond }, readyProjectObjs(testProject)...)
	start := time.Now()
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "dev"})
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	if elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, want at least the 200ms wait", elapsed)
	}
	if v := decode[branchView](t, body); v.Name != "dev" || v.Phase != "Creating" {
		t.Errorf("view = %+v, want dev in phase Creating", v)
	}
	getBranch(t, c, "dev")
}

func TestCreateBranchFromRestorePoint(t *testing.T) {
	objs := append(readyProjectObjs(testProject), branchObj("stopped", "main", v1alpha1.BranchStopped))
	srv, c, agents := newTestServerWith(t, func(cfg *Config) { cfg.Waits.Create = 100 * time.Millisecond }, objs...)
	agents.add(testNS, agentapi.RestorePoint{ID: "rp-20261007T031500Z-ab12", Branch: "stopped"})
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{
		"name": "dev", "parent": "stopped", "restorePointId": "rp-20261007T031500Z-ab12",
	})
	if resp.StatusCode == http.StatusConflict {
		t.Fatalf("a stopped parent is no problem for a restore point: %s", body)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	b := getBranch(t, c, "dev")
	if b.Spec.Source.Type != v1alpha1.SourceRestorePoint || b.Spec.Source.RestorePointID != "rp-20261007T031500Z-ab12" || b.Spec.Parent != "stopped" {
		t.Errorf("spec = %+v", b.Spec)
	}
}

// The restore point has to be one of the parent's, or the branch would be made and then fail.
func TestCreateBranchFromAnUnknownRestorePointIsNotFound(t *testing.T) {
	objs := append(readyProjectObjs(testProject), branchObj("dev", "main", v1alpha1.BranchRunning))
	srv, c, agents := newTestServer(t, objs...)
	const id = "rp-20261007T031500Z-ab12"
	agents.addData(testNS, "main")
	agents.add(testNS, agentapi.RestorePoint{ID: id, Branch: "dev"}) // a restore point, but not main's
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "new", "parent": "main", "restorePointId": id})
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
	var list v1alpha1.BranchList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil || len(list.Items) != 2 {
		t.Errorf("branches = %d (%v), want the 2 there were", len(list.Items), err)
	}
}

func TestCreateBranchFromARestorePointOfNoDataIsNotFound(t *testing.T) {
	srv, c, _ := newTestServer(t, readyProjectObjs(testProject)...)
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "new", "restorePointId": "rp-20261007T031500Z-ab12"})
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
	var list v1alpha1.BranchList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil || len(list.Items) != 1 {
		t.Errorf("branches = %d (%v), want main only", len(list.Items), err)
	}
}

func TestCreateBranchFromARestorePointRejects(t *testing.T) {
	deleting := branchObj("going", "main", v1alpha1.BranchDeleting)
	objs := append(readyProjectObjs(testProject), deleting)
	srv, _, agents := newTestServer(t, objs...)
	const id = "rp-20261007T031500Z-ab12"
	agents.add(testNS, agentapi.RestorePoint{ID: id, Branch: "going"})
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "new", "parent": "going", "restorePointId": id})
	if p := wantProblem(t, resp, body, http.StatusConflict, typeConflict); !strings.Contains(p.Detail, "being deleted") {
		t.Errorf("detail = %q", p.Detail)
	}
	resp, body = call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "new", "restorePointId": "rp-1"})
	wantProblem(t, resp, body, http.StatusBadRequest, typeValidation)
	if got := agents.seen(); len(got) != 0 {
		t.Errorf("the agent was called %d times for requests refused before it", len(got))
	}
}

func TestCreateBranchRejects(t *testing.T) {
	objs := append(readyProjectObjs(testProject),
		branchObj("stopped", "main", v1alpha1.BranchStopped),
		branchObj("starting", "main", v1alpha1.BranchStarting),
		branchObj("taken", "main", v1alpha1.BranchRunning))
	srv, c, _ := newTestServer(t, objs...)
	for name, tc := range map[string]struct {
		body   map[string]any
		status int
		typ    problemType
	}{
		"uppercase":         {map[string]any{"name": "Dev"}, 400, typeValidation},
		"backup form":       {map[string]any{"name": "main-before-20261007031500"}, 400, typeValidation},
		"41 characters":     {map[string]any{"name": strings.Repeat("a", 41)}, 400, typeValidation},
		"no name":           {map[string]any{}, 400, typeValidation},
		"bad state":         {map[string]any{"name": "dev", "state": "Paused"}, 400, typeValidation},
		"bad parent":        {map[string]any{"name": "dev", "parent": "Main"}, 400, typeValidation},
		"unknown field":     {map[string]any{"name": "dev", "size": "1Gi"}, 400, typeValidation},
		"missing parent":    {map[string]any{"name": "dev", "parent": "nope"}, 404, typeNotFound},
		"stopped parent":    {map[string]any{"name": "dev", "parent": "stopped"}, 409, typeConflict},
		"starting parent":   {map[string]any{"name": "dev", "parent": "starting"}, 409, typeConflict},
		"same name":         {map[string]any{"name": "taken"}, 409, typeConflict},
		"main":              {map[string]any{"name": "main"}, 409, typeConflict},
		"main with a state": {map[string]any{"name": "main", "state": "Stopped"}, 409, typeConflict},
		"main, bad state":   {map[string]any{"name": "main", "state": "Paused"}, 409, typeConflict},
		"main, bad parent":  {map[string]any{"name": "main", "parent": "Main"}, 409, typeConflict},
	} {
		t.Run(name, func(t *testing.T) {
			resp, b := call(t, srv, http.MethodPost, branchesURL, tc.body)
			p := wantProblem(t, resp, b, tc.status, tc.typ)
			if name == "stopped parent" && !strings.Contains(p.Detail, "parent stopped is Stopped") {
				t.Errorf("detail = %q, want it to name the parent and its phase", p.Detail)
			}
		})
	}
	var list v1alpha1.BranchList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil || len(list.Items) != 4 {
		t.Errorf("branches = %d (%v), want the 4 there were; a refused request makes none", len(list.Items), err)
	}
}

func TestBranchInAProjectThatIsNotReadyIsConflict(t *testing.T) {
	for name, status := range map[string]v1alpha1.ProjectStatus{
		"provisioning": {},
		"failed": {Phase: v1alpha1.ProjectFailed, Conditions: []metav1.Condition{{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonBootstrapFailed, Message: "no volume",
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, c, _ := newTestServer(t, projectObjs(testProject, "p", status)...)
			resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "dev"})
			p := wantProblem(t, resp, body, http.StatusConflict, typeConflict)
			want := "Provisioning"
			if name == "failed" {
				want = "Failed"
			}
			if !strings.Contains(p.Detail, want) {
				t.Errorf("detail = %q, want the phase %s", p.Detail, want)
			}
			var list v1alpha1.BranchList
			if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil || len(list.Items) != 0 {
				t.Errorf("branches = %d (%v), want none", len(list.Items), err)
			}
		})
	}
}

func TestBranchLimit(t *testing.T) {
	backup := func(name string) *v1alpha1.Branch {
		b := branchObj(name, "main", v1alpha1.BranchStopped)
		b.Labels = map[string]string{controller.LabelBackupOf: "a"}
		return b
	}
	objs := append(readyProjectObjs(testProject), branchObj("a", "main", v1alpha1.BranchRunning))
	objs[1].(*v1alpha1.Project).Spec.Limits.MaxBranches = 2
	srv, _, _ := newTestServerWith(t, func(cfg *Config) { cfg.Waits.Create = 100 * time.Millisecond }, objs...)
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "b"})
	p := wantProblem(t, resp, body, http.StatusTooManyRequests, typeRateLimited)
	if resp.Header.Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q, want 60", resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(p.Detail, "2 of 2") || strings.Contains(p.Detail, "being deleted") {
		t.Errorf("detail = %q, want the count and the limit", p.Detail)
	}

	// A branch that is being deleted still counts, and the detail says so: room comes soon.
	objs = append(readyProjectObjs(testProject), branchObj("a", "main", v1alpha1.BranchDeleting))
	objs[1].(*v1alpha1.Project).Spec.Limits.MaxBranches = 2
	srv, _, _ = newTestServerWith(t, func(cfg *Config) { cfg.Waits.Create = 100 * time.Millisecond }, objs...)
	resp, body = call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "b"})
	p = wantProblem(t, resp, body, http.StatusTooManyRequests, typeRateLimited)
	if want := "the project has 2 of 2 branches (1 being deleted)"; p.Detail != want {
		t.Errorf("detail = %q, want %q", p.Detail, want)
	}

	// Backup branches are counted apart: main alone is one user branch.
	objs = append(readyProjectObjs(testProject), backup("a-before-20261007031500"), backup("a-before-20261007031600"))
	objs[1].(*v1alpha1.Project).Spec.Limits.MaxBranches = 2
	srv, _, _ = newTestServerWith(t, func(cfg *Config) { cfg.Waits.Create = 100 * time.Millisecond }, objs...)
	resp, body = call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "b"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (backups do not count); body %s", resp.StatusCode, body)
	}
}

func TestCreateBranchPermanentFailure(t *testing.T) {
	for reason, want := range map[string]struct {
		status int
		typ    problemType
	}{
		v1alpha1.ReasonSourceNotFound:      {404, typeNotFound},
		v1alpha1.ReasonInsufficientStorage: {507, typeInsufficientStorage},
		v1alpha1.ReasonReflinkUnsupported:  {409, typeConflict},
		v1alpha1.ReasonInvalid:             {409, typeConflict},
	} {
		t.Run(reason, func(t *testing.T) {
			srv, c, _ := newTestServer(t, readyProjectObjs(testProject)...)
			afterDelay(200*time.Millisecond, func() {
				setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) {
					s.Phase = v1alpha1.BranchFailed
					s.Conditions = []metav1.Condition{{
						Type: v1alpha1.ConditionDataReady, Status: metav1.ConditionFalse, Reason: reason, Message: "the operator says so",
					}}
				})
			})
			resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "dev"})
			p := wantProblem(t, resp, body, want.status, want.typ)
			if !strings.Contains(p.Detail, "dev") || !strings.Contains(p.Detail, "the operator says so") {
				t.Errorf("detail = %q, want the branch name and the condition message", p.Detail)
			}
			// The failed branch stays for the user to delete.
			if b := getBranch(t, c, "dev"); b.Status.Phase != v1alpha1.BranchFailed {
				t.Errorf("phase = %q", b.Status.Phase)
			}
		})
	}
}

func TestCreateBranchFailedWithoutDataIsAnError(t *testing.T) {
	srv, c, _ := newTestServer(t, readyProjectObjs(testProject)...)
	afterDelay(200*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) {
			s.Phase = v1alpha1.BranchFailed
			s.Conditions = []metav1.Condition{{
				Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonPodCreateFailed, Message: "no pod",
			}}
		})
	})
	start := time.Now()
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "dev"})
	p := wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)
	if !strings.Contains(p.Detail, "PodCreateFailed") || !strings.Contains(p.Detail, "no pod") || !strings.Contains(p.Detail, "dev") {
		t.Errorf("detail = %q, want the branch, the reason and the message", p.Detail)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v: the failure ends the wait", time.Since(start))
	}
}

func TestCreateBranchFailedToStartHasData(t *testing.T) {
	srv, c, _ := newTestServer(t, readyProjectObjs(testProject)...)
	afterDelay(200*time.Millisecond, func() {
		setBranchStatus(t, c, testNS, "dev", func(s *v1alpha1.BranchStatus) {
			s.Phase = v1alpha1.BranchFailed
			s.DataGeneration = 1
			s.Conditions = []metav1.Condition{{
				Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonStartFailed, Message: "postgres exited",
			}}
		})
	})
	resp, body := call(t, srv, http.MethodPost, branchesURL, map[string]any{"name": "dev"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (the data is there); body %s", resp.StatusCode, body)
	}
	if v := decode[branchView](t, body); v.Phase != "Failed" || v.Reason != "StartFailed" || v.Message != "postgres exited" {
		t.Errorf("view = %+v", v)
	}
}

func TestGetAndListBranches(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 10, 7, 3, 15, 0, 0, time.UTC))
	running := metav1.NewTime(time.Date(2026, 10, 7, 3, 15, 4, 0, time.UTC))
	dev := branchObj("dev", "main", v1alpha1.BranchRunning)
	dev.Status.Endpoint = &v1alpha1.Endpoint{Host: "dev.cns-aaaaaaaaaaaa.svc", Port: 5432}
	dev.Status.CreatedFrom = &v1alpha1.CreatedFrom{Branch: "main", RestorePointID: "rp-20261007T031500Z-ab12"}
	dev.Status.LastRestore = &v1alpha1.RestoreStatus{
		RequestID: "r1", Phase: v1alpha1.RestoreDone, BackupBranch: "dev-before-20261007031500", RestorePointID: "rp-20261007T031000Z-cd34", Message: "done",
	}
	dev.Status.Timings = v1alpha1.Timings{DataReadyAt: &created, RunningAt: &running}
	bad := branchObj("bad", "main", v1alpha1.BranchFailed)
	bad.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionDataReady, Status: metav1.ConditionTrue, Reason: "DataReady"},
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonStartFailed, Message: "postgres exited"},
	}
	noData := branchObj("nodata", "main", v1alpha1.BranchFailed)
	noData.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonStartFailed, Message: "not ready"},
		{Type: v1alpha1.ConditionDataReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonSourceNotFound, Message: "no such restore point"},
	}
	backup := branchObj("dev-before-20261007031500", "main", v1alpha1.BranchStopped)
	backup.Labels = map[string]string{controller.LabelBackupOf: "dev"}
	srv, _, _ := newTestServer(t, append(readyProjectObjs(testProject), dev, bad, noData, backup)...)

	resp, body := call(t, srv, http.MethodGet, branchesURL+"/dev", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	v := decode[branchView](t, body)
	if v.Name != "dev" || v.Parent != "main" || v.Phase != "Running" || v.State != "Running" || v.Protected || v.BackupOf != "" || v.Reason != "" {
		t.Errorf("view = %+v", v)
	}
	if v.Endpoint == nil || v.Endpoint.Host != "dev.cns-aaaaaaaaaaaa.svc" || v.Endpoint.Port != 5432 {
		t.Errorf("endpoint = %+v", v.Endpoint)
	}
	if v.CreatedFrom == nil || v.CreatedFrom.Branch != "main" || v.CreatedFrom.RestorePointID != "rp-20261007T031500Z-ab12" {
		t.Errorf("createdFrom = %+v", v.CreatedFrom)
	}
	if lr := v.LastRestore; lr == nil || lr.RequestID != "r1" || lr.Phase != "Done" || lr.BackupBranch != "dev-before-20261007031500" || lr.RestorePointID != "rp-20261007T031000Z-cd34" || lr.Message != "done" {
		t.Errorf("lastRestore = %+v", lr)
	}
	if v.DataReadyAt == nil || !v.DataReadyAt.Equal(created.Time) || v.RunningAt == nil || !v.RunningAt.Equal(running.Time) {
		t.Errorf("times = %v, %v", v.DataReadyAt, v.RunningAt)
	}
	// The JSON names are camelCase.
	for _, key := range []string{`"dataReadyAt"`, `"runningAt"`, `"createdAt"`, `"createdFrom"`, `"lastRestore"`, `"backupBranch"`, `"requestId"`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("body has no %s: %s", key, body)
		}
	}

	// Failed: the reason of whichever of DataReady and Ready is false, DataReady first.
	_, body = call(t, srv, http.MethodGet, branchesURL+"/bad", nil)
	if v := decode[branchView](t, body); v.Phase != "Failed" || v.Reason != "StartFailed" || v.Message != "postgres exited" {
		t.Errorf("failed view = %+v", v)
	}
	_, body = call(t, srv, http.MethodGet, branchesURL+"/nodata", nil)
	if v := decode[branchView](t, body); v.Reason != "SourceNotFound" || v.Message != "no such restore point" {
		t.Errorf("failed view without data = %+v", v)
	}

	resp, body = call(t, srv, http.MethodGet, branchesURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d; body %s", resp.StatusCode, body)
	}
	list := decode[struct {
		Branches []branchView `json:"branches"`
	}](t, body)
	var names []string
	for _, b := range list.Branches {
		names = append(names, b.Name)
		if b.Name == "dev-before-20261007031500" && b.BackupOf != "dev" {
			t.Errorf("backupOf = %q, want dev", b.BackupOf)
		}
	}
	if got := strings.Join(names, ","); got != "bad,dev,dev-before-20261007031500,main,nodata" {
		t.Errorf("names = %s, want them in name order with the backup branch", got)
	}
	if main := list.Branches[3]; !main.Protected {
		t.Errorf("main is protected: %+v", main)
	}

	for _, path := range []string{branchesURL + "/nope", branchesURL + "/Dev", "/v1/projects/bbbbbbbbbbbb/branches/main", "/v1/projects/nope/branches"} {
		resp, body = call(t, srv, http.MethodGet, path, nil)
		wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
	}
}

func TestListBranchesOfNoBranchesIsAnEmptyList(t *testing.T) {
	srv, _, _ := newTestServer(t, projectObjs(testProject, "p", readyStatus())...)
	_, body := call(t, srv, http.MethodGet, branchesURL, nil)
	if string(body) != "{\"branches\":[]}\n" {
		t.Errorf("empty list = %q", body)
	}
}

func TestDeleteBranch(t *testing.T) {
	now := metav1.Now()
	going := branchObj("going", "main", v1alpha1.BranchDeleting)
	going.DeletionTimestamp, going.Finalizers = &now, []string{"consonance.io/branch"}
	srv, c, _ := newTestServer(t, append(readyProjectObjs(testProject), branchObj("dev", "main", v1alpha1.BranchRunning), going)...)

	resp, body := call(t, srv, http.MethodDelete, branchesURL+"/main", nil)
	wantProblem(t, resp, body, http.StatusConflict, typeConflict)
	getBranch(t, c, "main")

	resp, body = call(t, srv, http.MethodDelete, branchesURL+"/dev", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	if got := decode[map[string]string](t, body); got["name"] != "dev" || got["phase"] != "Deleting" {
		t.Errorf("body = %v", got)
	}
	// The fake client has no finalizers to wait for here: the branch is gone at once.
	var b v1alpha1.Branch
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "dev"}, &b); !apierrors.IsNotFound(err) {
		t.Errorf("branch dev after delete: %v", err)
	}

	resp, body = call(t, srv, http.MethodDelete, branchesURL+"/going", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("a branch that is being deleted: status = %d, want 202; body %s", resp.StatusCode, body)
	}
	for _, path := range []string{branchesURL + "/nope", branchesURL + "/dev", branchesURL + "/Dev"} {
		resp, body = call(t, srv, http.MethodDelete, path, nil)
		wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
	}
}
