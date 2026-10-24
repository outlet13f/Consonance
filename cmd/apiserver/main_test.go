package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
	"consonance/internal/testutil/kubetest"
	"consonance/internal/testutil/pgtest"
)

var env *kubetest.Env

func TestMain(m *testing.M) { os.Exit(kubetest.Main(m, &env)) }

func buildAPIServer(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "consonance-apiserver")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// apiServerUser is an API server user bound to the ClusterRole that make generate writes, and its
// kubeconfig: the API server runs with exactly the permissions it will have in a cluster.
func apiServerUser(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	b, err := os.ReadFile(filepath.Join(kubetest.RepoRoot(), "config", "rbac", "apiserver", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(b, &role); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Create(ctx, &role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), &role) })
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "consonance-apiserver"}},
	}
	if err := env.Client.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), binding) })
	kubeconfig, err := env.AddUser("consonance-apiserver")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeKey(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin-key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// adminKey is as long as the API server asks for: 32 bytes.
const adminKey = "test-admin-key-0123456789abcdefg"

// httpClient has a timeout, so a hung server fails the test instead of leaving the child behind.
var httpClient = &http.Client{Timeout: 15 * time.Second}

func TestAPIServerWithItsOwnPermissions(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	bin := buildAPIServer(t)
	kubeconfig := apiServerUser(t)
	// The API server enforces RBAC: the user may not do what the role does not grant.
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	as, err := client.New(cfg, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := as.List(ctx, &corev1.PodList{}); !apierrors.IsForbidden(err) {
		t.Fatalf("listing pods as the API server: %v, want Forbidden", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", pgtest.FreePort(t))
	logPath := filepath.Join(t.TempDir(), "apiserver.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(bin, "--kubeconfig", kubeconfig, "--listen-address", addr, "--admin-key-file", writeKey(t, adminKey+"\n"))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	logs := func() string { b, _ := os.ReadFile(logPath); return string(b) }
	defer func() {
		_ = cmd.Process.Kill()
		if t.Failed() {
			t.Logf("API server output:\n%s", logs())
		}
	}()

	// limit is how long wait waits: the start itself may take as long as the cache sync (30s)
	// when a permission is missing, and then the process exits, which wait reports.
	limit := 45 * time.Second
	wait := func(what string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !check() {
			select {
			case err := <-exited:
				t.Fatalf("the API server exited: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %s after %v", what, limit)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// call sends a request with the admin key and returns the status and the body.
	call := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, "http://"+addr+path, r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+adminKey)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	// expect is a request that must answer status.
	expect := func(status int, method, path string, body any) []byte {
		t.Helper()
		got, b := call(method, path, body)
		if got != status {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, got, status, b)
		}
		return b
	}
	wait("healthz", func() bool {
		resp, err := httpClient.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	limit = 20 * time.Second

	// A project: its namespace and Project are made (namespaces create, projects create).
	var created struct{ ID string }
	if err := json.Unmarshal(expect(http.StatusAccepted, http.MethodPost, "/v1/projects", map[string]string{"displayName": "smoke"}), &created); err != nil {
		t.Fatal(err)
	}
	id := created.ID
	ns := controller.NamespacePrefix + id
	var gotNS corev1.Namespace
	if err := env.Client.Get(ctx, client.ObjectKey{Name: ns}, &gotNS); err != nil {
		t.Fatal(err)
	}
	if gotNS.Labels[controller.LabelManagedBy] != "consonance-apiserver" || gotNS.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("namespace labels %v", gotNS.Labels)
	}
	projectKey := client.ObjectKey{Namespace: ns, Name: controller.ProjectName}
	var project v1alpha1.Project
	if err := env.Client.Get(ctx, projectKey, &project); err != nil {
		t.Fatal(err)
	}
	if project.Spec.Storage.StorageClassName != "consonance-xfs" {
		t.Errorf("storage class %q", project.Spec.Storage.StorageClassName)
	}
	// Read through the cache (projects get, list, watch).
	wait("the project in the cache", func() bool {
		code, _ := call(http.MethodGet, "/v1/projects/"+id, nil)
		return code == http.StatusOK
	})
	expect(http.StatusOK, http.MethodGet, "/v1/projects", nil)

	// What the operator would report is set by hand: the project is Ready, and main runs.
	project.Status.Phase = v1alpha1.ProjectReady
	if err := env.Client.Status().Update(ctx, &project); err != nil {
		t.Fatal(err)
	}
	mainBranch := &v1alpha1.Branch{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.MainBranch, Namespace: ns}}
	if err := env.Client.Create(ctx, mainBranch); err != nil {
		t.Fatal(err)
	}
	mainBranch.Status.Phase = v1alpha1.BranchRunning
	mainBranch.Status.DataGeneration = 1
	if err := env.Client.Status().Update(ctx, mainBranch); err != nil {
		t.Fatal(err)
	}
	wait("main in the cache", func() bool {
		code, _ := call(http.MethodGet, "/v1/projects/"+id+"/branches/main", nil)
		return code == http.StatusOK
	})

	// A branch is created (branches create). There is no operator, so it has no data and the
	// answer is 202 after the wait.
	expect(http.StatusAccepted, http.MethodPost, "/v1/projects/"+id+"/branches", map[string]string{"name": "feature", "parent": "main"})
	var feature v1alpha1.Branch
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "feature"}, &feature); err != nil {
		t.Fatal(err)
	}
	if feature.Spec.Parent != v1alpha1.MainBranch {
		t.Errorf("parent %q", feature.Spec.Parent)
	}

	// Stopping it writes the Branch (branches update); a stand-in for the operator answers.
	stopped := make(chan error, 1)
	go func() {
		stopped <- func() error {
			key := client.ObjectKeyFromObject(&feature)
			for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				var b v1alpha1.Branch
				if err := env.Client.Get(ctx, key, &b); err != nil {
					return err
				}
				if b.Spec.State != v1alpha1.StateStopped {
					continue
				}
				b.Status.Phase, b.Status.ObservedGeneration = v1alpha1.BranchStopped, b.Generation
				return env.Client.Status().Update(ctx, &b)
			}
			return errors.New("the branch was not asked to stop")
		}()
	}()
	expect(http.StatusOK, http.MethodPost, "/v1/projects/"+id+"/branches/feature/stop", nil)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}

	// No endpoint yet: 409. With the endpoint and db-credentials, the connection (secrets get).
	expect(http.StatusConflict, http.MethodGet, "/v1/projects/"+id+"/branches/main/connection", nil)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: controller.DBCredentialsSecret},
		Data: map[string][]byte{
			controller.UsernameKey: []byte("app_owner"), controller.PasswordKey: []byte("pw"), controller.DatabaseKey: []byte("app"),
		},
	}
	if err := env.Client.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(mainBranch), mainBranch); err != nil {
		t.Fatal(err)
	}
	mainBranch.Status.Endpoint = &v1alpha1.Endpoint{Host: "main.example", Port: 5432}
	if err := env.Client.Status().Update(ctx, mainBranch); err != nil {
		t.Fatal(err)
	}
	var conn struct{ URI string }
	wait("the connection", func() bool {
		code, b := call(http.MethodGet, "/v1/projects/"+id+"/branches/main/connection", nil)
		return code == http.StatusOK && json.Unmarshal(b, &conn) == nil
	})
	if conn.URI != "postgresql://app_owner:pw@main.example:5432/app" {
		t.Errorf("uri %q", conn.URI)
	}

	// A branch is deleted (branches delete); there is no operator, so its finalizer-less object
	// is gone, or going.
	expect(http.StatusAccepted, http.MethodDelete, "/v1/projects/"+id+"/branches/feature", nil)
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(&feature), &feature); err == nil && feature.DeletionTimestamp == nil {
		t.Errorf("the branch is not deleted")
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}

	// Deleting the project deletes its namespace (namespaces get, delete).
	expect(http.StatusAccepted, http.MethodDelete, "/v1/projects/"+id, nil)
	if err := env.Client.Get(ctx, client.ObjectKey{Name: ns}, &gotNS); err != nil || gotNS.DeletionTimestamp == nil {
		t.Errorf("namespace after delete: %v, %v", gotNS.DeletionTimestamp, err)
	}

	if strings.Contains(strings.ToLower(logs()), "forbidden") {
		t.Errorf("the API server was refused something:\n%s", logs())
	}
	if !strings.Contains(logs(), `"msg":"API server starting"`) {
		t.Errorf("no JSON start log:\n%s", logs())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the API server did not exit within 15s of SIGTERM")
	}
}

func TestRefusesAnEmptyAdminKey(t *testing.T) {
	bin := buildAPIServer(t)
	out, err := exec.Command(bin, "--admin-key-file", writeKey(t, " \n")).CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(string(out), "is empty") {
		t.Fatalf("empty key: %v\n%s", err, out)
	}
}

func TestRefusesAShortAdminKey(t *testing.T) {
	bin := buildAPIServer(t)
	out, err := exec.Command(bin, "--admin-key-file", writeKey(t, adminKey[:31]+"\n")).CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(string(out), "at least 32 bytes") {
		t.Fatalf("31 byte key: %v\n%s", err, out)
	}
}

func TestRefusesAMissingAdminKeyFile(t *testing.T) {
	bin := buildAPIServer(t)
	out, err := exec.Command(bin).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "admin-key-file") {
		t.Fatalf("no key file: %v\n%s", err, out)
	}
}

func TestRefusesStrayArguments(t *testing.T) {
	bin := buildAPIServer(t)
	out, err := exec.Command(bin, "--admin-key-file", writeKey(t, adminKey), "extra").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unexpected arguments") {
		t.Fatalf("stray argument: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, "--help").CombinedOutput(); err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
}
