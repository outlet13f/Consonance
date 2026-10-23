package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/testutil/kubetest"
)

func TestProjectMakesItsObjects(t *testing.T) {
	ns, fp := createProject(t, nil)
	ctx := context.Background()
	key := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: ns, Name: name} }
	eventually(t, 10*time.Second, func() string {
		p := getProject(t, ns)
		if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionAgentReady) ||
			!meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReflinkSupported) {
			return fmt.Sprintf("AgentReady %s, ReflinkSupported %s", cond(p.Status.Conditions, v1alpha1.ConditionAgentReady), cond(p.Status.Conditions, v1alpha1.ConditionReflinkSupported))
		}
		var main v1alpha1.Branch
		if err := kenv.Client.Get(ctx, key(v1alpha1.MainBranch), &main); err != nil {
			return "main: " + err.Error()
		}
		return ""
	})

	var creds, token corev1.Secret
	if err := kenv.Client.Get(ctx, key(DBCredentialsSecret), &creds); err != nil || len(creds.Data[PasswordKey]) != 32 {
		t.Fatalf("db-credentials: %v %v", err, creds.Data)
	}
	if err := kenv.Client.Get(ctx, key(InternalTokenSecret), &token); err != nil {
		t.Fatal(err)
	}
	if got := fp.seenTokens(); !slices.Equal(got, []string{string(token.Data[TokenKey])}) {
		t.Errorf("the agent saw tokens %q", got)
	}
	var pols networkingv1.NetworkPolicyList
	if err := kenv.Client.List(ctx, &pols, client.InNamespace(ns)); err != nil || len(pols.Items) != 5 {
		t.Fatalf("network policies: %v, %d", err, len(pols.Items))
	}
	var quota corev1.ResourceQuota
	var pvc corev1.PersistentVolumeClaim
	var svc corev1.Service
	var pod corev1.Pod
	for name, obj := range map[string]client.Object{QuotaName: &quota, PVCName: &pvc, AgentName: &svc} {
		if err := kenv.Client.Get(ctx, key(name), obj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := kenv.Client.Get(ctx, key(AgentName), &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Image != testAgentImage || !metav1.IsControlledBy(&pod, ptrProject(getProject(t, ns))) {
		t.Errorf("agent pod image %s owners %+v", pod.Spec.Containers[0].Image, pod.OwnerReferences)
	}
	if len(pvc.OwnerReferences) != 0 || len(creds.OwnerReferences) != 0 {
		t.Errorf("the volume and the secrets must go only with the namespace: %+v %+v", pvc.OwnerReferences, creds.OwnerReferences)
	}
	var main v1alpha1.Branch
	if err := kenv.Client.Get(ctx, key(v1alpha1.MainBranch), &main); err != nil || !main.Spec.Protected || main.Spec.Parent != "" {
		t.Fatalf("main: %v %+v", err, main.Spec)
	}
	if p := getProject(t, ns); p.Status.Phase != v1alpha1.ProjectProvisioning || p.Status.Volume.PVCName != PVCName {
		t.Errorf("phase %s volume %+v", p.Status.Phase, p.Status.Volume)
	}

	// Reconciles that change nothing write nothing: no update loop.
	versions := func() []string {
		var out []string
		for name, obj := range map[string]client.Object{QuotaName: &corev1.ResourceQuota{}, AgentName: &corev1.Service{}, "branch-ingress": &networkingv1.NetworkPolicy{}} {
			if err := kenv.Client.Get(ctx, key(name), obj); err != nil {
				t.Fatal(err)
			}
			out = append(out, name+"@"+obj.GetResourceVersion())
		}
		slices.Sort(out)
		return out
	}
	before := versions()
	bindVolume(t, ns) // triggers a reconcile
	time.Sleep(time.Second)
	if after := versions(); !slices.Equal(before, after) {
		t.Errorf("objects changed with nothing to change: %v then %v", before, after)
	}
}

func ptrProject(p v1alpha1.Project) *v1alpha1.Project { return &p }

// runMain does what main's Branch reconcile does (task 7): make its data and pod and report it
// running. With that reconciler registered it may have done so already.
func runMain(t *testing.T, ns string) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Agent(ns, "").InitMain(ctx); err != nil {
		t.Fatal(err)
	}
	main := &v1alpha1.Branch{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.MainBranch}}
	eventually(t, 10*time.Second, func() string {
		if err := kenv.Client.Get(ctx, client.ObjectKeyFromObject(main), main); err != nil {
			return "main: " + err.Error()
		}
		return ""
	})
	pod := desiredBranchPod(ptrProject(getProject(t, ns)), main, testPostgresImage)
	if err := ignoreExists(kenv.Client.Create(ctx, pod)); err != nil {
		t.Fatal(err)
	}
	updateStatus(t, main, func(b *v1alpha1.Branch) { b.Status.Phase = v1alpha1.BranchRunning })
}

func TestProjectBecomesReady(t *testing.T) {
	ns, fp := createProject(t, nil)
	bindVolume(t, ns)
	runMain(t, ns)
	eventually(t, 15*time.Second, func() string {
		p := getProject(t, ns)
		if p.Status.Phase != v1alpha1.ProjectReady {
			return fmt.Sprintf("phase %s, Ready %s: %s", p.Status.Phase, cond(p.Status.Conditions, v1alpha1.ConditionReady),
				meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady).Message)
		}
		return ""
	})
	p := getProject(t, ns)
	if p.Status.Volume.CapacityBytes != 10<<30 || p.Status.Volume.UsedBytes != 1<<30 {
		t.Errorf("volume %+v", p.Status.Volume)
	}
	if _, _, boot := fp.postgres(v1alpha1.MainBranch); !boot {
		t.Error("main was not bootstrapped")
	}
	if !slices.Contains(fp.callLog(), "Bootstrap(main)") {
		t.Errorf("calls %q", fp.callLog())
	}
}

func TestProjectFailsWithoutReflink(t *testing.T) {
	ns, fp := createProject(t, func(fp *fakeProject) {
		fp.setPreflight(agentapi.Preflight{Reflink: false, Kernel: "6.8.0", Error: "reflink copies are not supported here: operation not supported"})
	})
	eventually(t, 10*time.Second, func() string {
		p := getProject(t, ns)
		if p.Status.Phase != v1alpha1.ProjectFailed {
			return "phase " + string(p.Status.Phase)
		}
		return ""
	})
	p := getProject(t, ns)
	c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReflinkSupported)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonReflinkUnsupported || !strings.Contains(c.Message, "not supported") {
		t.Fatalf("ReflinkSupported %+v", c)
	}
	if got := cond(p.Status.Conditions, v1alpha1.ConditionReady); got != "False/"+v1alpha1.ReasonReflinkUnsupported {
		t.Errorf("Ready %s", got)
	}
	// A failed project is not probed again. (A reconcile that read the Project before its
	// status update reached the cache may have probed once more before this point.)
	n := countCalls(fp, "Preflight")
	time.Sleep(time.Second)
	// A reconcile on a stale cache may make one more call; a retry loop makes many.
	if m := countCalls(fp, "Preflight"); m > n+1 {
		t.Errorf("%d preflights, then %d", n, m)
	}
	var main v1alpha1.Branch
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: v1alpha1.MainBranch}, &main); !apierrors.IsNotFound(err) {
		t.Errorf("main of a failed project: %v", err)
	}
}

func countCalls(fp *fakeProject, prefix string) int {
	n := 0
	for _, c := range fp.callLog() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestProjectWaitsForItsAgent(t *testing.T) {
	refused := errors.New("dial tcp 10.43.0.10:8443: connect: connection refused")
	ns, fp := createProject(t, func(fp *fakeProject) { fp.failNext("Preflight", refused, refused) })
	eventually(t, 15*time.Second, func() string {
		p := getProject(t, ns)
		if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReflinkSupported) {
			return "ReflinkSupported " + cond(p.Status.Conditions, v1alpha1.ConditionReflinkSupported)
		}
		return ""
	})
	if n := countCalls(fp, "Preflight"); n < 3 {
		t.Errorf("%d preflights, want 2 refused and then one answered", n)
	}
}

// An agent pod the scheduler finds no node for is reported, and the reconcile stops there:
// nothing after it works without the agent.
func TestAnUnschedulableAgentPodIsReported(t *testing.T) {
	ns, _ := createProject(t, nil)
	ctx := context.Background()
	// Main bootstrapped first: a reconcile that went on past the agent would then reach the usage
	// call, which the fake agent answers, and set AgentReady back to true.
	eventually(t, 15*time.Second, func() string {
		var pod corev1.Pod
		if err := kenv.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: AgentName}, &pod); err != nil {
			return "agent pod: " + err.Error()
		}
		if p := getProject(t, ns); !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionMainReady) {
			return "MainReady " + cond(p.Status.Conditions, v1alpha1.ConditionMainReady)
		}
		return ""
	})
	const msg = "0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector."
	unschedule(t, ns, AgentName, msg)
	eventually(t, 10*time.Second, func() string {
		if c := cond(getProject(t, ns).Status.Conditions, v1alpha1.ConditionAgentReady); c != "False/Unschedulable" {
			return "AgentReady " + c
		}
		return ""
	})
	if c := meta.FindStatusCondition(getProject(t, ns).Status.Conditions, v1alpha1.ConditionAgentReady); c == nil || c.Message != msg {
		t.Errorf("AgentReady %+v, want the scheduler's message", c)
	}
}

func TestAgentPodFollowsTheProjectSettings(t *testing.T) {
	ns, _ := createProject(t, nil)
	ctx := context.Background()
	podArgs := func() []string {
		var pod corev1.Pod
		if err := kenv.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: AgentName}, &pod); err != nil {
			return nil
		}
		return pod.Spec.Containers[0].Args
	}
	eventually(t, 10*time.Second, func() string {
		if !slices.Contains(podArgs(), "--min-copy-interval=5s") {
			return fmt.Sprintf("args %q", podArgs())
		}
		return ""
	})
	eventually(t, 10*time.Second, func() string { // the reconciler writes the Project too
		p := getProject(t, ns)
		p.Spec.Limits.MinCopyInterval = metav1.Duration{Duration: time.Minute}
		if err := kenv.Client.Update(ctx, &p); err != nil {
			return err.Error()
		}
		return ""
	})
	eventually(t, 10*time.Second, func() string {
		if !slices.Contains(podArgs(), "--min-copy-interval=1m0s") {
			return fmt.Sprintf("args %q", podArgs())
		}
		return ""
	})
}

func TestProjectOutsideAProjectNamespaceFails(t *testing.T) {
	kubetest.Need(t, kenv)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "plain-"}}
	if err := kenv.Client.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	p := &v1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: ProjectName, Namespace: ns.Name}, Spec: v1alpha1.ProjectSpec{DisplayName: "x"}}
	if err := kenv.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		got := getProject(t, ns.Name)
		if got.Status.Phase != v1alpha1.ProjectFailed || cond(got.Status.Conditions, v1alpha1.ConditionReady) != "False/InvalidNamespace" {
			return fmt.Sprintf("phase %s Ready %s", got.Status.Phase, cond(got.Status.Conditions, v1alpha1.ConditionReady))
		}
		return ""
	})
	var s corev1.Secret
	if err := kenv.Client.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: DBCredentialsSecret}, &s); !apierrors.IsNotFound(err) {
		t.Errorf("secret in a plain namespace: %v", err)
	}
}
