// Package kubetest runs a Kubernetes API server (envtest: kube-apiserver and etcd, no kubelet and
// no controllers) with the Consonance CRDs for the tests of a package.
package kubetest

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"consonance/api/v1alpha1"
)

// Env is a running test API server.
type Env struct {
	Config *rest.Config
	Scheme *k8sruntime.Scheme
	Client client.Client // uncached
	env    *envtest.Environment
}

// Main starts the API server, runs the package's tests and stops it. Call it from TestMain:
//
//	var env *kubetest.Env
//	func TestMain(m *testing.M) { os.Exit(kubetest.Main(m, &env)) }
//
// Without KUBEBUILDER_ASSETS (make test-envtest sets it) it leaves *envp nil and the tests that
// call Need skip.
func Main(m *testing.M, envp **Env) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run()
	}
	e, err := Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubetest:", err)
		return 1
	}
	defer e.Stop()
	*envp = e
	return m.Run()
}

// Need skips the test when no API server runs.
func Need(t testing.TB, e *Env) {
	t.Helper()
	if e == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-envtest")
	}
}

// Scheme returns a scheme with the Kubernetes types and the Consonance types.
func Scheme() *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// RepoRoot is the repository root, found from this file's path.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// Start starts an API server with the CRDs of config/crd/bases. Main does it for packages that
// need nothing else before their tests run.
func Start() (*Env, error) {
	scheme := Scheme()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(RepoRoot(), "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, fmt.Errorf("start envtest: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		return nil, err
	}
	return &Env{Config: cfg, Scheme: scheme, Client: c, env: env}, nil
}

// Stop stops the API server.
func (e *Env) Stop() {
	if err := e.env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "kubetest: stop:", err)
	}
}

// Namespace creates a project namespace cns-<12 random characters> for one test. envtest runs no
// namespace controller, so the namespace and its objects stay until the API server stops.
func (e *Env) Namespace(t testing.TB) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cns-" + randomID()}}
	if err := e.Client.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

// AddUser adds an API server user and returns its kubeconfig (for running the operator binary
// with its own permissions).
func (e *Env) AddUser(name string, groups ...string) ([]byte, error) {
	u, err := e.env.AddUser(envtest.User{Name: name, Groups: groups}, nil)
	if err != nil {
		return nil, err
	}
	return u.KubeConfig()
}

// randomID is 12 lowercase letters and digits, like a projectId (spec 4.1).
func randomID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}
