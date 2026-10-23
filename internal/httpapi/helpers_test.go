package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
	"consonance/internal/testutil/kubetest"
)

const (
	testKey          = "test-key"
	testStorageClass = "consonance-xfs"
	testToken        = "internal-test-token" // the token in the internal-token Secret of a ready project
)

// newTestServer serves the API over a fake Kubernetes client (which is both Config.Client and
// Config.Reader) and a fake agent, with objs already in the cluster. The fake client has the
// status subresource of Project and Branch, so Status().Update works as on a real API server.
func newTestServer(t *testing.T, objs ...client.Object) (*httptest.Server, client.Client, *fakeAgents) {
	t.Helper()
	return newTestServerWith(t, nil, objs...)
}

// newTestServerWith is newTestServer after tune changed the Config, for the tests that need
// short waits or their own log.
func newTestServerWith(t *testing.T, tune func(*Config), objs ...client.Object) (*httptest.Server, client.Client, *fakeAgents) {
	t.Helper()
	cfg, c, agents := testConfig(objs...)
	if tune != nil {
		tune(&cfg)
	}
	srv := httptest.NewServer(New(cfg))
	t.Cleanup(srv.Close)
	return srv, c, agents
}

// testConfig is the Config newTestServer uses, with the fake client and the fake agents it made.
func testConfig(objs ...client.Object) (Config, client.Client, *fakeAgents) {
	c := fake.NewClientBuilder().
		WithScheme(kubetest.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Project{}, &v1alpha1.Branch{}).
		Build()
	agents := newFakeAgents()
	return Config{Client: c, Reader: c, Agents: agents, AdminKey: testKey, StorageClass: testStorageClass}, c, agents
}

// newServerWithRoutes serves the routes of the server plus extra ones, for tests of the
// skeleton that need a route the skeleton itself does not have.
func newServerWithRoutes(t *testing.T, cfg Config, extra ...endpoint) *httptest.Server {
	t.Helper()
	s := newServer(cfg)
	srv := httptest.NewServer(s.handler(append(s.endpoints(), extra...)))
	t.Cleanup(srv.Close)
	return srv
}

// call sends a request with the admin key and a JSON body (nil for none), and returns the
// response and its body.
func call(t *testing.T, srv *httptest.Server, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	return callWith(t, srv, method, path, body, "Bearer "+testKey)
}

// callWith is call with the Authorization header given ("" sends none).
func callWith(t *testing.T, srv *httptest.Server, method, path string, body any, authorization string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
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

// problem is the body of an error response.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// wantProblem fails unless resp is a problem+json response with the status and type.
func wantProblem(t *testing.T, resp *http.Response, body []byte, status int, typ problemType) problem {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, status, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p problem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if p.Type != string(typ) || p.Status != status || p.Title != http.StatusText(status) || p.Detail == "" {
		t.Fatalf("problem = %+v, want type %q status %d title %q and a detail", p, typ, status, http.StatusText(status))
	}
	return p
}

// readyProjectObjs is the namespace, the Ready Project, its internal-token Secret and the
// Running main branch of project id: what a request for a branch finds in a working project.
func readyProjectObjs(id string) []client.Object {
	objs := projectObjs(id, "ready", readyStatus())
	objs = append(objs, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cns-" + id, Name: controller.InternalTokenSecret},
		Data:       map[string][]byte{controller.TokenKey: []byte(testToken)},
	})
	return append(objs, &v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Namespace: "cns-" + id, Name: v1alpha1.MainBranch},
		Spec:       v1alpha1.BranchSpec{Protected: true, State: v1alpha1.StateRunning},
		Status:     v1alpha1.BranchStatus{Phase: v1alpha1.BranchRunning, DataGeneration: 1},
	})
}

// setBranchStatus changes the status of a branch the way the operator would. It is safe to call
// from the goroutine of afterDelay.
func setBranchStatus(t *testing.T, c client.Client, ns, name string, change func(*v1alpha1.BranchStatus)) {
	t.Helper()
	var b v1alpha1.Branch
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
		t.Errorf("get branch %s: %v", name, err)
		return
	}
	change(&b.Status)
	if err := c.Status().Update(context.Background(), &b); err != nil {
		t.Errorf("update status of branch %s: %v", name, err)
	}
}

// afterDelay runs f in a goroutine after d, for a change the operator makes while a request
// waits.
func afterDelay(d time.Duration, f func()) {
	go func() {
		time.Sleep(d)
		f()
	}()
}
