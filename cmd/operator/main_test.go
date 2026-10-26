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

// systemNamespace is where the operator runs in a cluster; its Role for leases and events lives there.
const systemNamespace = "consonance-system"

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

// operatorRBAC reads the two documents that make generate writes to role.yaml: the ClusterRole
// of the operator and the Role (leases and events) in the system namespace.
func operatorRBAC(t *testing.T) (rbacv1.ClusterRole, rbacv1.Role) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(kubetest.RepoRoot(), "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cr rbacv1.ClusterRole
	var role rbacv1.Role
	var haveCR, haveRole bool
	for _, doc := range strings.Split(string(b), "\n---\n") {
		doc = strings.TrimPrefix(strings.TrimSpace(doc), "---")
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var tm metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &tm); err != nil {
			t.Fatal(err)
		}
		switch tm.Kind {
		case "ClusterRole":
			haveCR = true
			if err := yaml.Unmarshal([]byte(doc), &cr); err != nil {
				t.Fatal(err)
			}
		case "Role":
			haveRole = true
			if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("role.yaml has a %q document", tm.Kind)
		}
	}
	if !haveCR || !haveRole {
		t.Fatalf("role.yaml must hold a ClusterRole and a Role (got ClusterRole %v, Role %v)", haveCR, haveRole)
	}
	return cr, role
}

// operatorUser is an API server user bound to the ClusterRole and the Role that make generate
// writes, and its kubeconfig: the operator runs with exactly the permissions it will have in a
// cluster.
func operatorUser(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	cr, role := operatorRBAC(t)
	if role.Namespace != systemNamespace {
		t.Fatalf("the Role is in namespace %q, want %q", role.Namespace, systemNamespace)
	}
	if err := env.Client.Create(ctx, &cr); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: systemNamespace}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	if err := env.Client.Create(ctx, &role); err != nil {
		t.Fatal(err)
	}
	subjects := []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "consonance-operator"}}
	if err := env.Client.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: cr.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cr.Name},
		Subjects:   subjects,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: systemNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   subjects,
	}); err != nil {
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

// TestOperatorRoleHasOnlyWhatItUses holds the rendered role to the verbs the code calls: a verb
// the operator never uses is a permission a stolen token would get for nothing. Leases and events
// belong to the system namespace's Role, not to the cluster-wide one.
func TestOperatorRoleHasOnlyWhatItUses(t *testing.T) {
	cr, role := operatorRBAC(t)
	// resource -> verbs the ClusterRole must not grant.
	never := map[string][]string{
		"secrets":                {"update", "patch", "delete"},
		"persistentvolumeclaims": {"update", "patch", "delete"},
		"pods":                   {"update", "patch"},
		"services":               {"patch"},
		"resourcequotas":         {"patch", "delete"},
		"networkpolicies":        {"patch", "delete"},
		"branches":               {"patch", "delete"},
		"branches/status":        {"get", "patch"},
		"projects/status":        {"get", "patch"},
		"leases":                 {"*", "get", "list", "watch", "create", "update", "patch", "delete"},
		"events":                 {"*", "get", "list", "watch", "create", "update", "patch", "delete"},
	}
	granted := map[string]map[string]bool{}
	for _, rule := range cr.Rules {
		for _, res := range rule.Resources {
			if granted[res] == nil {
				granted[res] = map[string]bool{}
			}
			for _, v := range rule.Verbs {
				granted[res][v] = true
			}
		}
		if slices.Contains(rule.Resources, "*") || slices.Contains(rule.Verbs, "*") {
			t.Errorf("the ClusterRole has a wildcard rule: %+v", rule)
		}
	}
	for res, verbs := range never {
		for _, v := range verbs {
			if granted[res][v] {
				t.Errorf("the ClusterRole grants %s on %s, which the operator does not use", v, res)
			}
		}
	}
	// What CreateOrUpdate and the reconcilers do need must stay.
	for res, verbs := range map[string][]string{
		"secrets": {"get", "list", "watch", "create"}, "persistentvolumeclaims": {"get", "list", "watch", "create"},
		"pods": {"get", "list", "watch", "create", "delete"}, "services": {"get", "list", "watch", "create", "update", "delete"},
		"resourcequotas": {"get", "list", "watch", "create", "update"}, "networkpolicies": {"get", "list", "watch", "create", "update"},
		"branches": {"get", "list", "watch", "create", "update"}, "branches/status": {"update"}, "projects/status": {"update"},
		"branches/finalizers": {"update"}, "projects/finalizers": {"update"}, "projects": {"get", "list", "watch"},
	} {
		for _, v := range verbs {
			if !granted[res][v] {
				t.Errorf("the ClusterRole lacks %s on %s", v, res)
			}
		}
	}
	if role.Namespace != systemNamespace {
		t.Errorf("the Role is in namespace %q, want %q", role.Namespace, systemNamespace)
	}
	inRole := map[string]bool{}
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			inRole[res] = true
		}
	}
	for _, res := range []string{"leases", "events"} {
		if !inRole[res] {
			t.Errorf("the Role has no rule for %s", res)
		}
	}
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
