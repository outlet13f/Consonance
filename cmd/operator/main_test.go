package main_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

func buildOperator(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "consonance-operator")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// operatorUser is an API server user bound to the ClusterRole that make generate writes, and its
// kubeconfig: the operator runs with exactly the permissions it will have in a cluster.
func operatorUser(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	b, err := os.ReadFile(filepath.Join(kubetest.RepoRoot(), "config", "rbac", "role.yaml"))
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
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "consonance-operator"}},
	}
	if err := env.Client.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	kubeconfig, err := env.AddUser("consonance-operator")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOperatorReconcilesWithItsOwnPermissions(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	bin := buildOperator(t)
	kubeconfig := operatorUser(t)
	// The API server enforces RBAC: the user may not do what the role does not grant.
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	as, err := client.New(cfg, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := as.List(ctx, &corev1.NamespaceList{}); !apierrors.IsForbidden(err) {
		t.Fatalf("listing namespaces as the operator: %v, want Forbidden", err)
	}
	probe := fmt.Sprintf("127.0.0.1:%d", pgtest.FreePort(t))
	logPath := filepath.Join(t.TempDir(), "operator.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(bin, "--kubeconfig", kubeconfig, "--metrics-bind-address", "0", "--health-probe-bind-address", probe,
		"--agent-image", "consonance/agent:test")
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
			t.Logf("operator output:\n%s", logs())
		}
	}()

	wait := func(what string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !check() {
			select {
			case err := <-exited:
				t.Fatalf("the operator exited: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %s after 20s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait("healthz", func() bool {
		resp, err := http.Get("http://" + probe + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	ns := env.Namespace(t)
	p := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: controller.ProjectName, Namespace: ns}, Spec: v1alpha1.ProjectSpec{DisplayName: "smoke"}}
	if err := env.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	// The agent's service name does not resolve from the test host, so the project stops at
	// AgentReady=False; everything before it is made, and the status is written.
	wait("project status", func() bool {
		var got v1alpha1.Project
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
			return false
		}
		c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAgentReady)
		return c != nil && c.Reason == "AgentUnavailable" && got.Status.Phase == v1alpha1.ProjectProvisioning
	})
	for name, obj := range map[string]client.Object{
		controller.DBCredentialsSecret: &corev1.Secret{}, controller.InternalTokenSecret: &corev1.Secret{},
		controller.PVCName: &corev1.PersistentVolumeClaim{}, controller.AgentName: &corev1.Pod{},
	} {
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	var pod corev1.Pod
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: controller.AgentName}, &pod); err == nil && pod.Spec.Containers[0].Image != "consonance/agent:test" {
		t.Errorf("agent image %s", pod.Spec.Containers[0].Image)
	}
	if !strings.Contains(logs(), `"msg":"operator starting"`) {
		t.Errorf("no JSON start log:\n%s", logs())
	}

	// The paths past this point, each under the operator's own role. What the agent and the
	// supervisor would report is set by hand.
	// An ended agent pod is deleted and made again (pods delete).
	agentKey := client.ObjectKey{Namespace: ns, Name: controller.AgentName}
	if err := env.Client.Get(ctx, agentKey, &pod); err != nil {
		t.Fatal(err)
	}
	oldUID := pod.UID
	pod.Status.Phase = corev1.PodFailed
	if err := env.Client.Status().Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	wait("a new agent pod", func() bool {
		var np corev1.Pod
		return env.Client.Get(ctx, agentKey, &np) == nil && np.UID != oldUID
	})
	// A branch gets its finalizer (branches update) and, deleted, loses it once its service is
	// deleted (services delete, branches/status update).
	feature := &v1alpha1.Branch{ObjectMeta: metav1.ObjectMeta{Name: "feature", Namespace: ns}, Spec: v1alpha1.BranchSpec{Parent: v1alpha1.MainBranch}}
	if err := env.Client.Create(ctx, feature); err != nil {
		t.Fatal(err)
	}
	wait("the branch finalizer", func() bool {
		var got v1alpha1.Branch
		return env.Client.Get(ctx, client.ObjectKeyFromObject(feature), &got) == nil && slices.Contains(got.Finalizers, controller.BranchFinalizer)
	})
	if err := env.Client.Delete(ctx, feature); err != nil {
		t.Fatal(err)
	}
	wait("the branch deleted", func() bool {
		return apierrors.IsNotFound(env.Client.Get(ctx, client.ObjectKeyFromObject(feature), &v1alpha1.Branch{}))
	})
	// Once reflink is known to work, the project makes main (branches create).
	wait("reflink marked supported", func() bool {
		var got v1alpha1.Project
		if env.Client.Get(ctx, client.ObjectKeyFromObject(p), &got) != nil {
			return false
		}
		meta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionReflinkSupported, Status: metav1.ConditionTrue, Reason: "Supported"})
		return env.Client.Status().Update(ctx, &got) == nil
	})
	mainKey := client.ObjectKey{Namespace: ns, Name: v1alpha1.MainBranch}
	wait("main made by the operator", func() bool { return env.Client.Get(ctx, mainKey, &v1alpha1.Branch{}) == nil })
	// A branch with data makes its service and pod, which it owns (branches/finalizers update
	// under owner-reference enforcement).
	wait("main marked as having data", func() bool {
		var got v1alpha1.Branch
		if env.Client.Get(ctx, mainKey, &got) != nil {
			return false
		}
		got.Status.DataGeneration = 1
		return env.Client.Status().Update(ctx, &got) == nil
	})
	brKey := client.ObjectKey{Namespace: ns, Name: controller.BranchObjectName(v1alpha1.MainBranch)}
	wait("main's service and pod", func() bool {
		return env.Client.Get(ctx, brKey, &corev1.Service{}) == nil && env.Client.Get(ctx, brKey, &corev1.Pod{}) == nil
	})

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the operator did not exit within 15s of SIGTERM")
	}
}

func TestOperatorRefusesStrayArguments(t *testing.T) {
	bin := buildOperator(t)
	out, err := exec.Command(bin, "extra").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unexpected arguments") {
		t.Fatalf("stray argument: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, "--help").CombinedOutput(); err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
}
