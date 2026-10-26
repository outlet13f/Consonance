package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"consonance/api/v1alpha1"
	"consonance/internal/testutil/kubetest"
)

// The controller tests run the reconcilers in one manager against an envtest API server, with
// the fake agent and supervisors of fake_test.go. envtest has no kubelet: pods never run, so the
// fake decides what runs, and no controller binds volumes or deletes namespaces.
var (
	kenv *kubetest.Env
	conn *fakeConnector
)

// branchReconciler is the manager's Branch reconciler, for tests that look at what it keeps in
// memory.
var branchReconciler *BranchReconciler

// kubeletHeld holds back the branch pods of a namespace: the fake kubelet leaves them Pending
// until the test starts them (startPod). namespace -> true.
var kubeletHeld sync.Map

// fakeKubelet stands in for the kubelet, which envtest lacks: it marks every branch pod running
// with its container started, as the real one does once the container runs.
type fakeKubelet struct{ client.Client }

func (k fakeKubelet) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if _, held := kubeletHeld.Load(req.Namespace); held {
		return ctrl.Result{}, nil
	}
	var pod corev1.Pod
	if err := k.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pod.Labels[LabelComponent] != ComponentBranch || pod.DeletionTimestamp != nil || containerStarted(&pod) ||
		unschedulable(&pod) != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return ctrl.Result{}, nil
	}
	markStarted(&pod)
	return ctrl.Result{}, client.IgnoreNotFound(k.Status().Update(ctx, &pod))
}

// markStarted sets a pod's status to running with its containers started.
func markStarted(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = nil
	for _, c := range pod.Spec.Containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: c.Name, Image: c.Image, Started: ptr.To(true),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		})
	}
}

// startPod does what the fake kubelet does, for a pod it holds back.
func startPod(t *testing.T, ns, name string) {
	t.Helper()
	updateStatus(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}, markStarted)
}

// conflictReady makes the first status update that makes a project Ready fail with a conflict,
// as if the cache were stale: namespace -> armed. See TestProjectStatusConflictIsRetriedSoon.
var conflictReady sync.Map

const (
	testAgentImage    = "consonance/agent:test"
	testPostgresImage = "consonance/postgres:test"
)

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

func runSuite(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run() // the envtest tests skip; the others run
	}
	e, err := kubetest.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer e.Stop()
	kenv = e
	conn = newFakeConnector(e.Client)
	stop, err := startManager(e)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	return m.Run()
}

func startManager(e *kubetest.Env) (stop func(), err error) {
	// CONSONANCE_TEST_LOG=1 shows the reconcilers' logs.
	log := logr.Discard()
	if os.Getenv("CONSONANCE_TEST_LOG") != "" {
		log = logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, nil))
	}
	ctrl.SetLogger(log)
	mgr, err := ctrl.NewManager(e.Config, ctrl.Options{
		Scheme:                 e.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  CacheOptions(),
	})
	if err != nil {
		return nil, err
	}
	if err := (&ProjectReconciler{Client: projectConflicts{Client: mgr.GetClient()}, Scheme: mgr.GetScheme(), Connector: conn, AgentImage: testAgentImage}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	branchReconciler = &BranchReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, PostgresImage: testPostgresImage}
	if err := branchReconciler.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := ctrl.NewControllerManagedBy(mgr).Named("fake-kubelet").For(&corev1.Pod{}).Complete(fakeKubelet{mgr.GetClient()}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			fmt.Fprintln(os.Stderr, "manager:", err)
		}
	}, nil
}

// eventually calls check every 50ms until it returns "" or timeout passes; then the test fails
// with check's last message.
func eventually(t *testing.T, timeout time.Duration, check func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msg := check()
		if msg == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s", timeout, msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// createProject makes a project namespace and its Project, as the API server does (spec 9.1).
// setup, if given, prepares the project's fake agent first.
func createProject(t *testing.T, setup func(*fakeProject)) (string, *fakeProject) {
	t.Helper()
	kubetest.Need(t, kenv)
	ns := kenv.Namespace(t)
	fp := conn.project(ns)
	if setup != nil {
		setup(fp)
	}
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: ProjectName, Namespace: ns},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "test"},
	}
	if err := kenv.Client.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return ns, fp
}

func getProject(t *testing.T, ns string) v1alpha1.Project {
	t.Helper()
	var p v1alpha1.Project
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: ProjectName}, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// cond shows a condition as status/reason for test messages.
func cond(conds []metav1.Condition, typ string) string {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return "none"
	}
	return string(c.Status) + "/" + c.Reason
}

// bindVolume marks the project volume bound, which no controller does under envtest.
func bindVolume(t *testing.T, ns string) {
	t.Helper()
	ctx := context.Background()
	eventually(t, 10*time.Second, func() string {
		var pvc corev1.PersistentVolumeClaim
		if err := kenv.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: PVCName}, &pvc); err != nil {
			return "volume: " + err.Error()
		}
		pvc.Status.Phase = corev1.ClaimBound
		if err := kenv.Client.Status().Update(ctx, &pvc); err != nil {
			return "bind: " + err.Error()
		}
		return ""
	})
}

// updateStatus applies change to the latest version of obj's status, retrying on conflicts.
func updateStatus[T client.Object](t *testing.T, obj T, change func(T)) {
	t.Helper()
	ctx := context.Background()
	eventually(t, 10*time.Second, func() string {
		if err := kenv.Client.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return err.Error()
		}
		change(obj)
		if err := kenv.Client.Status().Update(ctx, obj); err != nil {
			return err.Error()
		}
		return ""
	})
}

// unschedule marks a pod as one the scheduler found no node for, which no scheduler does under
// envtest.
func unschedule(t *testing.T, ns, name, msg string) {
	t.Helper()
	updateStatus(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}, func(pod *corev1.Pod) {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: msg,
		}}
	})
}

// ignoreExists treats a create that lost a race to the operator as done.
func ignoreExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// projectConflicts is the Project reconciler's client, with a status update that conflicts once
// where a test asks for it (conflictReady).
type projectConflicts struct{ client.Client }

func (c projectConflicts) Status() client.SubResourceWriter {
	return conflictingStatus{SubResourceWriter: c.Client.Status()}
}

type conflictingStatus struct{ client.SubResourceWriter }

func (w conflictingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if p, ok := obj.(*v1alpha1.Project); ok && p.Status.Phase == v1alpha1.ProjectReady {
		if _, armed := conflictReady.LoadAndDelete(p.Namespace); armed {
			return apierrors.NewConflict(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "projects"}, p.Name,
				errors.New("the object has been modified; please apply your changes to the latest version and try again"))
		}
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}
