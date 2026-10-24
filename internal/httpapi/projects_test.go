package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
	"consonance/internal/testutil/kubetest"
)

// projectObjs is the namespace and Project of project id, as the API server makes them, with
// the status the operator would report.
func projectObjs(id, displayName string, status v1alpha1.ProjectStatus) []client.Object {
	ns := "cns-" + id
	return []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "consonance-apiserver"}}},
		&v1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: controller.ProjectName},
			Spec: v1alpha1.ProjectSpec{
				DisplayName: displayName,
				Storage:     v1alpha1.StorageSpec{Size: resource.MustParse("10Gi"), StorageClassName: testStorageClass},
				DefaultCompute: v1alpha1.ComputeSpec{
					CPU: resource.MustParse("500m"), Memory: resource.MustParse("1Gi"),
				},
				Limits: v1alpha1.ProjectLimits{MaxBranches: 10, MinCopyInterval: metav1.Duration{Duration: 5 * time.Second}},
			},
			Status: status,
		},
	}
}

func readyStatus() v1alpha1.ProjectStatus {
	return v1alpha1.ProjectStatus{
		Phase:  v1alpha1.ProjectReady,
		Volume: v1alpha1.VolumeStatus{PVCName: "data", CapacityBytes: 10 << 30, UsedBytes: 12345},
		Conditions: []metav1.Condition{{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", Message: "all good",
		}},
	}
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return v
}

func TestCreateProject(t *testing.T) {
	srv, c, _ := newTestServer(t)
	resp, body := call(t, srv, http.MethodPost, "/v1/projects", map[string]any{"displayName": "my-app"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	v := decode[projectView](t, body)
	if !regexp.MustCompile(`^[a-z0-9]{12}$`).MatchString(v.ID) || v.Phase != "Provisioning" || v.DisplayName != "my-app" {
		t.Fatalf("view = %+v, want a 12 character id, phase Provisioning, displayName my-app", v)
	}

	var ns corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: "cns-" + v.ID}, &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Labels["app.kubernetes.io/managed-by"] != "consonance-apiserver" || ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("namespace labels = %v", ns.Labels)
	}
	var p v1alpha1.Project
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "cns-" + v.ID, Name: "project"}, &p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Storage.StorageClassName != "consonance-xfs" || p.Spec.DisplayName != "my-app" {
		t.Errorf("spec = %+v", p.Spec)
	}
	if !p.Spec.Storage.Size.IsZero() || !p.Spec.DefaultCompute.CPU.IsZero() {
		t.Errorf("size and compute were not asked for and are left to the CRD defaults: %+v", p.Spec)
	}
}

func TestCreateProjectWithSizeAndCompute(t *testing.T) {
	srv, c, _ := newTestServer(t)
	resp, body := call(t, srv, http.MethodPost, "/v1/projects", map[string]any{
		"displayName": "big", "storageSize": "20Gi", "compute": map[string]string{"cpu": "1", "memory": "2Gi"},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	v := decode[projectView](t, body)
	if v.Storage.Size != "20Gi" || v.DefaultCompute.CPU != "1" || v.DefaultCompute.Memory != "2Gi" {
		t.Errorf("view = %+v", v)
	}
	var p v1alpha1.Project
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "cns-" + v.ID, Name: "project"}, &p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.Storage.Size.Cmp(resource.MustParse("20Gi")) != 0 ||
		p.Spec.DefaultCompute.CPU.Cmp(resource.MustParse("1")) != 0 ||
		p.Spec.DefaultCompute.Memory.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("spec = %+v", p.Spec)
	}
}

func TestCreateProjectRejects(t *testing.T) {
	long := make([]byte, 101)
	for i := range long {
		long[i] = 'a'
	}
	srv, c, _ := newTestServer(t)
	for name, body := range map[string]any{
		"no displayName":    map[string]any{},
		"empty displayName": map[string]any{"displayName": ""},
		"101 characters":    map[string]any{"displayName": string(long)},
		"size not a number": map[string]any{"displayName": "x", "storageSize": "abc"},
		"size under 1Gi":    map[string]any{"displayName": "x", "storageSize": "512Mi"},
		"no memory":         map[string]any{"displayName": "x", "compute": map[string]string{"cpu": "1"}},
		"no cpu":            map[string]any{"displayName": "x", "compute": map[string]string{"memory": "1Gi"}},
		"zero cpu":          map[string]any{"displayName": "x", "compute": map[string]string{"cpu": "0", "memory": "1Gi"}},
		"negative memory":   map[string]any{"displayName": "x", "compute": map[string]string{"cpu": "1", "memory": "-1Gi"}},
		"unknown field":     map[string]any{"displayName": "x", "storageClassName": "fast"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, b := call(t, srv, http.MethodPost, "/v1/projects", body)
			wantProblem(t, resp, b, http.StatusBadRequest, typeValidation)
		})
	}
	var list corev1.NamespaceList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d namespaces were made by refused requests", len(list.Items))
	}
}

func TestCreateProjectRetriesAnIDCollisionOnce(t *testing.T) {
	var made atomic.Int32
	cfg, _, _ := testConfig()
	c := fake.NewClientBuilder().WithScheme(kubetest.Scheme()).WithStatusSubresource(&v1alpha1.Project{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Namespace); ok && made.Add(1) <= 1 {
				return apiAlreadyExists(obj)
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	cfg.Client, cfg.Reader = c, c
	srv := newServerWithRoutes(t, cfg)
	resp, body := call(t, srv, http.MethodPost, "/v1/projects", map[string]any{"displayName": "x"})
	if resp.StatusCode != http.StatusAccepted || made.Load() != 2 {
		t.Fatalf("status %d after %d namespace creates, want 202 after 2; body %s", resp.StatusCode, made.Load(), body)
	}

	// Two collisions in a row are a 500.
	made.Store(-100)
	resp, body = call(t, srv, http.MethodPost, "/v1/projects", map[string]any{"displayName": "x"})
	wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)
}

func TestCreateProjectRemovesTheNamespaceWhenTheProjectFails(t *testing.T) {
	cfg, _, _ := testConfig()
	c := fake.NewClientBuilder().WithScheme(kubetest.Scheme()).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1alpha1.Project); ok {
				return apiInvalid(obj)
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	cfg.Client, cfg.Reader = c, c
	srv := newServerWithRoutes(t, cfg)
	resp, body := call(t, srv, http.MethodPost, "/v1/projects", map[string]any{"displayName": "x"})
	wantProblem(t, resp, body, http.StatusBadRequest, typeValidation)
	var list corev1.NamespaceList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("namespace left behind: %v", list.Items)
	}
}

func TestGetProject(t *testing.T) {
	failed := v1alpha1.ProjectStatus{
		Phase: v1alpha1.ProjectFailed,
		Conditions: []metav1.Condition{{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonReflinkUnsupported, Message: "the volume has no reflink",
		}},
	}
	objs := append(projectObjs("aaaaaaaaaaaa", "ready one", readyStatus()), projectObjs("bbbbbbbbbbbb", "failed one", failed)...)
	srv, _, _ := newTestServer(t, objs...)

	resp, body := call(t, srv, http.MethodGet, "/v1/projects/aaaaaaaaaaaa", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	v := decode[projectView](t, body)
	if v.ID != "aaaaaaaaaaaa" || v.Phase != "Ready" || v.DisplayName != "ready one" || v.Storage.UsedBytes != 12345 ||
		v.Storage.CapacityBytes != 10<<30 || v.Storage.Size != "10Gi" || v.Storage.StorageClassName != testStorageClass ||
		v.DefaultCompute.CPU != "500m" || v.DefaultCompute.Memory != "1Gi" || v.Limits.MaxBranches != 10 || v.Limits.MinCopyInterval != "5s" {
		t.Errorf("view = %+v", v)
	}
	if v.Reason != "" || v.Message != "" {
		t.Errorf("a Ready project has reason %q message %q", v.Reason, v.Message)
	}

	resp, body = call(t, srv, http.MethodGet, "/v1/projects/bbbbbbbbbbbb", nil)
	v = decode[projectView](t, body)
	if resp.StatusCode != http.StatusOK || v.Phase != "Failed" || v.Reason != "ReflinkUnsupported" || v.Message != "the volume has no reflink" {
		t.Errorf("status %d view = %+v", resp.StatusCode, v)
	}

	resp, body = call(t, srv, http.MethodGet, "/v1/projects/cccccccccccc", nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
}

// A replica whose cache is behind does not have the Project yet when the create's 202 has
// gone out: the cluster has it, so the answer is not a 404.
func TestGetProjectRightAfterCreateIsNotNotFound(t *testing.T) {
	srv, _, _ := newTestServerWith(t, func(cfg *Config) {
		cfg.Client = interceptor.NewClient(cfg.Client.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*v1alpha1.Project); ok {
					return apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("projects").GroupResource(), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	})
	resp, body := call(t, srv, http.MethodPost, "/v1/projects", map[string]any{"displayName": "my-app"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: status = %d, want 202; body %s", resp.StatusCode, body)
	}
	id := decode[projectView](t, body).ID
	resp, body = call(t, srv, http.MethodGet, "/v1/projects/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if v := decode[projectView](t, body); v.ID != id || v.DisplayName != "my-app" {
		t.Errorf("view = %+v", v)
	}
}

func TestMalformedProjectIDIsNotFound(t *testing.T) {
	var gets atomic.Int32
	cfg, _, _ := testConfig()
	c := fake.NewClientBuilder().WithScheme(kubetest.Scheme()).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets.Add(1)
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	cfg.Client, cfg.Reader = c, c
	srv := newServerWithRoutes(t, cfg)
	for _, id := range []string{"Abc", "cns-a1b2c3d4e5f6", "a1b2c3d4e5f", "a1b2c3d4e5f67"} {
		resp, body := call(t, srv, http.MethodGet, "/v1/projects/"+id, nil)
		wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
	}
	if n := gets.Load(); n != 0 {
		t.Errorf("%d Gets reached the client, want none", n)
	}
}

func TestReadyProject(t *testing.T) {
	failed := v1alpha1.ProjectStatus{
		Phase: v1alpha1.ProjectFailed,
		Conditions: []metav1.Condition{{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonReflinkUnsupported, Message: "no reflink",
		}},
	}
	objs := append(projectObjs("aaaaaaaaaaaa", "a", readyStatus()), projectObjs("bbbbbbbbbbbb", "b", failed)...)
	objs = append(objs, projectObjs("cccccccccccc", "c", v1alpha1.ProjectStatus{})...)
	cfg, _, _ := testConfig(objs...)
	s := newServer(cfg)
	ctx := context.Background()

	if p, err := s.readyProject(ctx, "aaaaaaaaaaaa"); err != nil || p.Spec.DisplayName != "a" {
		t.Fatalf("ready project: %v, %v", p, err)
	}
	_, err := s.readyProject(ctx, "bbbbbbbbbbbb")
	if ae, ok := err.(*apiError); !ok || ae.Status != http.StatusConflict || ae.Type != typeConflict || ae.Detail != "project bbbbbbbbbbbb is Failed: no reflink" {
		t.Errorf("failed project: %v", err)
	}
	_, err = s.readyProject(ctx, "cccccccccccc")
	if ae, ok := err.(*apiError); !ok || ae.Status != http.StatusConflict || ae.Detail != "project cccccccccccc is Provisioning: " {
		t.Errorf("project without a phase: %v", err)
	}
	_, err = s.readyProject(ctx, "dddddddddddd")
	if ae, ok := err.(*apiError); !ok || ae.Status != http.StatusNotFound {
		t.Errorf("missing project: %v", err)
	}
}

func TestListProjects(t *testing.T) {
	objs := append(projectObjs("bbbbbbbbbbbb", "second", readyStatus()), projectObjs("aaaaaaaaaaaa", "first", v1alpha1.ProjectStatus{})...)
	// A Project outside a project namespace is not the API's.
	objs = append(objs, &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "project"},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "stray"},
	}, &v1alpha1.Project{
		// get refuses these ids, so list must not show them.
		ObjectMeta: metav1.ObjectMeta{Namespace: "cns-foo", Name: "project"},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "bad id"},
	}, &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cns-cccccccccccc", Name: "other"},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "bad name"},
	})
	srv, _, _ := newTestServer(t, objs...)
	resp, body := call(t, srv, http.MethodGet, "/v1/projects", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	got := decode[struct {
		Projects []projectView `json:"projects"`
	}](t, body)
	if len(got.Projects) != 2 || got.Projects[0].ID != "aaaaaaaaaaaa" || got.Projects[1].ID != "bbbbbbbbbbbb" {
		t.Fatalf("projects = %+v, want aaaaaaaaaaaa then bbbbbbbbbbbb", got.Projects)
	}
	if got.Projects[0].Phase != "Provisioning" || got.Projects[1].Phase != "Ready" {
		t.Errorf("phases = %s, %s", got.Projects[0].Phase, got.Projects[1].Phase)
	}

	// No projects is an empty list, not null.
	empty, _, _ := newTestServer(t)
	_, body = call(t, empty, http.MethodGet, "/v1/projects", nil)
	if string(body) != "{\"projects\":[]}\n" {
		t.Errorf("empty list = %q", body)
	}
}

func TestDeleteProject(t *testing.T) {
	srv, c, _ := newTestServer(t, projectObjs("aaaaaaaaaaaa", "a", readyStatus())...)
	resp, body := call(t, srv, http.MethodDelete, "/v1/projects/aaaaaaaaaaaa", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	got := decode[map[string]string](t, body)
	if got["id"] != "aaaaaaaaaaaa" || got["phase"] != "Deleting" {
		t.Errorf("body = %v", got)
	}
	// The fake client has no namespace controller: a deleted namespace is gone at once.
	var ns corev1.Namespace
	if err := c.Get(context.Background(), client.ObjectKey{Name: "cns-aaaaaaaaaaaa"}, &ns); err == nil {
		t.Errorf("namespace still there: %+v", ns)
	}

	resp, body = call(t, srv, http.MethodDelete, "/v1/projects/aaaaaaaaaaaa", nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
	resp, body = call(t, srv, http.MethodDelete, "/v1/projects/Abc", nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
}

func TestDeleteProjectAlreadyTerminating(t *testing.T) {
	now := metav1.Now()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "cns-aaaaaaaaaaaa", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
		Labels: map[string]string{"app.kubernetes.io/managed-by": "consonance-apiserver"},
	}}
	srv, _, _ := newTestServer(t, ns)
	resp, body := call(t, srv, http.MethodDelete, "/v1/projects/aaaaaaaaaaaa", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
}

func apiAlreadyExists(obj client.Object) error {
	return apierrors.NewAlreadyExists(corev1.Resource("namespaces"), obj.GetName())
}

func apiInvalid(obj client.Object) error {
	return apierrors.NewBadRequest("the Project was refused")
}

func TestDeleteRefusesANamespaceTheAPIServerDidNotMake(t *testing.T) {
	objs := projectObjs("aaaaaaaaaaaa", "a", readyStatus())
	objs[0].SetLabels(nil)
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cns-bbbbbbbbbbbb", Labels: map[string]string{"app.kubernetes.io/managed-by": "someone-else"}}})
	srv, c, _ := newTestServer(t, objs...)
	for _, id := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"} {
		resp, body := call(t, srv, http.MethodDelete, "/v1/projects/"+id, nil)
		wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)
		var ns corev1.Namespace
		if err := c.Get(context.Background(), client.ObjectKey{Name: "cns-" + id}, &ns); err != nil {
			t.Errorf("namespace cns-%s was deleted: %v", id, err)
		}
	}
}
