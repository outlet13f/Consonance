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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
	"consonance/internal/testutil/kubetest"
)

// readyProject makes a project whose main branch runs.
func readyProject(t *testing.T) (string, *fakeProject) {
	t.Helper()
	ns, fp := createProject(t, nil)
	bindVolume(t, ns)
	waitPhase(t, ns, v1alpha1.MainBranch, v1alpha1.BranchRunning)
	return ns, fp
}

func getBranch(t *testing.T, ns, name string) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func waitPhase(t *testing.T, ns, name string, phase v1alpha1.BranchPhase) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	eventually(t, 15*time.Second, func() string {
		if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
			return name + ": " + err.Error()
		}
		if b.Status.Phase != phase {
			return fmt.Sprintf("%s is %q, DataReady %s, Ready %s", name, b.Status.Phase,
				cond(b.Status.Conditions, v1alpha1.ConditionDataReady), cond(b.Status.Conditions, v1alpha1.ConditionReady))
		}
		return ""
	})
	return b
}

// createBranch makes a branch as the API server does (spec 9.2).
func createBranch(t *testing.T, ns, name, parent string, change ...func(*v1alpha1.Branch)) {
	t.Helper()
	b := &v1alpha1.Branch{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.BranchSpec{Parent: parent}}
	for _, c := range change {
		c(b)
	}
	if err := kenv.Client.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

func setState(t *testing.T, ns, name string, state v1alpha1.DesiredState) {
	t.Helper()
	eventually(t, 10*time.Second, func() string {
		b := getBranch(t, ns, name)
		b.Spec.State = state
		if err := kenv.Client.Update(context.Background(), &b); err != nil {
			return err.Error()
		}
		return ""
	})
}

func TestMainRunsAndTheProjectGetsReady(t *testing.T) {
	ns, fp := readyProject(t)
	main := getBranch(t, ns, v1alpha1.MainBranch)
	if main.Status.DataGeneration != 1 || !meta.IsStatusConditionTrue(main.Status.Conditions, v1alpha1.ConditionDataReady) ||
		main.Status.Timings.DataReadyAt == nil || main.Status.Timings.RunningAt == nil {
		t.Errorf("main status %+v", main.Status)
	}
	if e := main.Status.Endpoint; e == nil || e.Host != "br-main."+ns+".svc" || e.Port != 5432 {
		t.Errorf("endpoint %+v", e)
	}
	var pod corev1.Pod
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-main"}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Image != testPostgresImage || !metav1.IsControlledBy(&pod, &main) {
		t.Errorf("pod image %s owners %+v", pod.Spec.Containers[0].Image, pod.OwnerReferences)
	}
	var svc corev1.Service
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-main"}, &svc); err != nil || !svc.Spec.PublishNotReadyAddresses {
		t.Errorf("service: %v %+v", err, svc.Spec)
	}
	if !slices.Contains(fp.callLog(), "InitMain") {
		t.Errorf("calls %q", fp.callLog())
	}
	eventually(t, 10*time.Second, func() string {
		if p := getProject(t, ns); p.Status.Phase != v1alpha1.ProjectReady {
			return "project " + string(p.Status.Phase)
		}
		return ""
	})
}

func TestBranchFromItsRunningParent(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	b := waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if f := b.Status.CreatedFrom; f == nil || f.Branch != "main" || f.BackupStartLSN != "0/3000028" {
		t.Errorf("createdFrom %+v", f)
	}
	if !slices.Contains(fp.callLog(), "CreateBranchData(dev,main,ParentLatest)") {
		t.Errorf("calls %q", fp.callLog())
	}
	if state, gen, _ := fp.postgres("dev"); state != supervisorapi.PGRunning || gen != 1 {
		t.Errorf("dev's postgres %s on %d", state, gen)
	}
}

func TestBranchWaitsForItsParentToRun(t *testing.T) {
	ns, _ := readyProject(t)
	setState(t, ns, "main", v1alpha1.StateStopped)
	waitPhase(t, ns, "main", v1alpha1.BranchStopped)
	createBranch(t, ns, "dev", "main")
	eventually(t, 10*time.Second, func() string {
		if c := cond(getBranch(t, ns, "dev").Status.Conditions, v1alpha1.ConditionDataReady); c != "False/ParentNotRunning" {
			return "DataReady " + c
		}
		return ""
	})
	setState(t, ns, "main", v1alpha1.StateRunning)
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestBranchFromARestorePoint(t *testing.T) {
	ns, fp := readyProject(t)
	fp.addRestorePoint("rp-20261007T031500Z-ab12", "main")
	createBranch(t, ns, "fix", "main", func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-ab12"}
	})
	b := waitPhase(t, ns, "fix", v1alpha1.BranchRunning)
	if f := b.Status.CreatedFrom; f == nil || f.RestorePointID != "rp-20261007T031500Z-ab12" || f.BackupStartLSN != "" {
		t.Errorf("createdFrom %+v", f)
	}
}

func TestBranchFromAMissingRestorePointFails(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "lost", "main", func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-zzzz"}
	})
	b := waitPhase(t, ns, "lost", v1alpha1.BranchFailed)
	if c := cond(b.Status.Conditions, v1alpha1.ConditionDataReady); c != "False/"+v1alpha1.ReasonSourceNotFound {
		t.Errorf("DataReady %s", c)
	}
	n := countCalls(fp, "CreateBranchData(lost") // a permanent failure is not retried
	time.Sleep(time.Second)
	// A reconcile on a stale cache may make one more call; a retry loop makes many.
	if m := countCalls(fp, "CreateBranchData(lost"); m > n+1 {
		t.Errorf("%d copies, then %d", n, m)
	}
	var pod corev1.Pod
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-lost"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("a branch without data got a pod: %v", err)
	}
}

func TestRateLimitedCopyComesBackAfterRetryAfter(t *testing.T) {
	ns, fp := readyProject(t)
	fp.failNext("CreateBranchData", &agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited, RetryAfter: time.Second})
	start := time.Now()
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if n := countCalls(fp, "CreateBranchData(dev"); n < 2 {
		t.Errorf("%d copies, want a refused one and then more", n)
	}
	if took := time.Since(start); took < time.Second {
		t.Errorf("retried after %s, before Retry-After", took)
	}
}

func TestStopAndStart(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	setState(t, ns, "dev", v1alpha1.StateStopped)
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
	if state, _, _ := fp.postgres("dev"); state != supervisorapi.PGStopped {
		t.Fatalf("postgres %s", state)
	}
	setState(t, ns, "dev", v1alpha1.StateRunning)
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestAStoppedBranchIsNotStarted(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "idle", "main", func(b *v1alpha1.Branch) { b.Spec.State = v1alpha1.StateStopped })
	waitPhase(t, ns, "idle", v1alpha1.BranchStopped)
	time.Sleep(500 * time.Millisecond)
	if n := countCalls(fp, "Start(idle)"); n != 0 {
		t.Errorf("%d starts", n)
	}
}

func TestStartFailureIsReportedAndRetried(t *testing.T) {
	ns, fp := readyProject(t)
	fp.failToStart("dev", 1)
	createBranch(t, ns, "dev", "main")
	b := waitPhase(t, ns, "dev", v1alpha1.BranchFailed)
	if c := cond(b.Status.Conditions, v1alpha1.ConditionReady); c != "False/StartFailed" {
		t.Errorf("Ready %s", c)
	}
	eventually(t, 10*time.Second, func() string {
		if n := countCalls(fp, "Start(dev)"); n < 2 {
			return fmt.Sprintf("%d starts", n)
		}
		return ""
	})
	fp.failToStart("dev", 0) // whatever kept it from starting is fixed
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestADeletedPodIsMadeAgain(t *testing.T) {
	ns, _ := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: ns, Name: "br-dev"}
	if err := kenv.Client.Get(context.Background(), key, &pod); err != nil {
		t.Fatal(err)
	}
	old := pod.UID
	if err := kenv.Client.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		var p corev1.Pod
		if err := kenv.Client.Get(context.Background(), key, &p); err != nil || p.UID == old {
			return fmt.Sprintf("pod: %v", err)
		}
		return ""
	})
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

// A pod the scheduler finds no node for is reported; its supervisor cannot answer.
func TestAnUnschedulablePodIsReported(t *testing.T) {
	ns, _ := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	ctx := context.Background()
	key := client.ObjectKey{Namespace: ns, Name: "br-dev"}
	var pod corev1.Pod
	if err := kenv.Client.Get(ctx, key, &pod); err != nil {
		t.Fatal(err)
	}
	old := pod.UID
	if err := kenv.Client.Delete(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		var p corev1.Pod
		if err := kenv.Client.Get(ctx, key, &p); err != nil || p.UID == old {
			return fmt.Sprintf("pod: %v", err)
		}
		return ""
	})
	const msg = "0/3 nodes are available: 3 Insufficient memory."
	unschedule(t, ns, "br-dev", msg)
	eventually(t, 10*time.Second, func() string {
		if c := cond(getBranch(t, ns, "dev").Status.Conditions, v1alpha1.ConditionReady); c != "False/Unschedulable" {
			return "Ready " + c
		}
		return ""
	})
	if c := meta.FindStatusCondition(getBranch(t, ns, "dev").Status.Conditions, v1alpha1.ConditionReady); c == nil || c.Message != msg {
		t.Errorf("Ready %+v, want the scheduler's message", c)
	}
}

// A pod the API server refuses (a full quota, say) is reported, and the error is retried.
func TestAPodThatCannotBeMadeIsReported(t *testing.T) {
	quota := errors.New(`pods "br-dev" is forbidden: exceeded quota: project-quota`)
	c := fake.NewClientBuilder().WithScheme(kubetest.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return quota
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	r := &BranchReconciler{Client: c, Scheme: c.Scheme(), PostgresImage: testPostgresImage}
	b := testBranch("cns-x", "dev", "main")
	ready, err := r.ensurePod(context.Background(), testProject("cns-x"), b)
	if ready || !errors.Is(err, quota) {
		t.Fatalf("ensurePod = %v, %v", ready, err)
	}
	if got := cond(b.Status.Conditions, v1alpha1.ConditionReady); got != "False/PodCreateFailed" {
		t.Errorf("Ready %s", got)
	}
	if c := meta.FindStatusCondition(b.Status.Conditions, v1alpha1.ConditionReady); c == nil || c.Message != quota.Error() {
		t.Errorf("Ready %+v, want the API server's message", c)
	}
}

// A supervisor that does not answer is asked again later and later; one that answers, or a
// deleted branch, starts afresh.
func TestSupervisorWaitGrows(t *testing.T) {
	r := &BranchReconciler{}
	key := client.ObjectKey{Namespace: "cns-x", Name: "dev"}
	var got []time.Duration
	for range 6 {
		got = append(got, r.supervisorWait(key))
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	r.supervisorAnswered(key)
	if d := r.supervisorWait(key); d != 2*time.Second {
		t.Errorf("after an answer: %s", d)
	}

	r.supervisorWait(key)
	r.holdOff(key, time.Minute)
	r.forget(key)
	if d := r.heldOff(key); d != 0 {
		t.Errorf("held off %s after forget", d)
	}
	if d := r.supervisorWait(key); d != 2*time.Second {
		t.Errorf("after forget: %s", d)
	}
}

// A branch's supervisor is not called before the container of its pod has started: the calls
// would fail and grow supervisorWait. Once it has started, a stopped branch is Stopped at once,
// where it used to wait out the grown wait, about 30 seconds on a real cluster (a few seconds
// here; the bound leaves room for -race).
func TestSupervisorIsNotCalledBeforeThePodStarts(t *testing.T) {
	ns, fp := readyProject(t)
	kubeletHeld.Store(ns, true)
	t.Cleanup(func() { kubeletHeld.Delete(ns) })
	createBranch(t, ns, "dev", "main", func(b *v1alpha1.Branch) { b.Spec.State = v1alpha1.StateStopped })
	waitPhase(t, ns, "dev", v1alpha1.BranchDataReady)
	eventually(t, 10*time.Second, func() string {
		var pod corev1.Pod
		if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-dev"}, &pod); err != nil {
			return "pod: " + err.Error()
		}
		return ""
	})
	time.Sleep(time.Second) // the pod's own events reconcile the branch meanwhile

	for _, c := range fp.callLog() {
		if strings.HasSuffix(c, "(dev)") && (strings.HasPrefix(c, "Status") || strings.HasPrefix(c, "Stop") || strings.HasPrefix(c, "Start")) {
			t.Errorf("supervisor called before its container started: %q", fp.callLog())
			break
		}
	}
	key := client.ObjectKey{Namespace: ns, Name: "dev"}
	branchReconciler.mu.Lock()
	wait := branchReconciler.unanswered[key]
	branchReconciler.mu.Unlock()
	if wait != 0 {
		t.Errorf("supervisorWait grew to %s before the container started", wait)
	}

	start := time.Now()
	startPod(t, ns, "br-dev")
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Stopped %s after the container started", d)
	}
}

// A new pod of a branch is a fresh start: the wait grown on the pod before does not apply.
func TestSupervisorWaitStartsAfreshOnANewPod(t *testing.T) {
	r := &BranchReconciler{}
	key := client.ObjectKey{Namespace: "cns-x", Name: "dev"}
	r.podSeen(key, "uid-1")
	r.supervisorWait(key)
	r.supervisorWait(key)
	r.podSeen(key, "uid-1")
	if d := r.supervisorWait(key); d != 8*time.Second {
		t.Errorf("same pod: wait %s, want 8s", d)
	}
	r.podSeen(key, "uid-2")
	if d := r.supervisorWait(key); d != 2*time.Second {
		t.Errorf("new pod: wait %s, want 2s", d)
	}
	r.forget(key)
	r.supervisorWait(key)
	r.podSeen(key, "uid-2") // forgotten: a pod seen again after forget counts as new
	if d := r.supervisorWait(key); d != 2*time.Second {
		t.Errorf("after forget: wait %s, want 2s", d)
	}
}

// A running branch whose container stops (a liveness kill, an OOM, CrashLoopBackOff) is not
// Ready while it is down, and says why; it is Ready again once the container runs.
func TestARestartingContainerIsNotReady(t *testing.T) {
	ns, _ := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)

	kubeletHeld.Store(ns, true)
	t.Cleanup(func() { kubeletHeld.Delete(ns) })
	updateStatus(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "br-dev"}}, func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses[0].Started = ptr.To(false)
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	})
	eventually(t, 10*time.Second, func() string {
		c := meta.FindStatusCondition(getBranch(t, ns, "dev").Status.Conditions, v1alpha1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.ReasonContainerNotStarted || c.Message != "CrashLoopBackOff" {
			return fmt.Sprintf("Ready %+v", c)
		}
		return ""
	})
	key := client.ObjectKey{Namespace: ns, Name: "dev"}
	branchReconciler.mu.Lock()
	wait := branchReconciler.unanswered[key]
	branchReconciler.mu.Unlock()
	if wait != 0 {
		t.Errorf("supervisorWait grew to %s while the container was down", wait)
	}

	startPod(t, ns, "br-dev")
	eventually(t, 10*time.Second, func() string {
		if c := cond(getBranch(t, ns, "dev").Status.Conditions, v1alpha1.ConditionReady); c != "True/Running" {
			return "Ready " + c
		}
		return ""
	})
}

func TestContainerNotStartedMessage(t *testing.T) {
	status := func(st corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: containerName, State: st}}}}
	}
	for _, c := range []struct {
		pod  *corev1.Pod
		want string
	}{
		{status(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}), "ImagePullBackOff"},
		{status(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}), "OOMKilled"},
		{status(corev1.ContainerState{}), "container not started"},
		{&corev1.Pod{}, "container not started"},
	} {
		if got := containerNotStarted(c.pod); got != c.want {
			t.Errorf("containerNotStarted(%+v) = %q, want %q", c.pod.Status, got, c.want)
		}
	}
}
