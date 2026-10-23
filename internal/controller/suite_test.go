package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	if err := (&ProjectReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, AgentImage: testAgentImage}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&BranchReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, PostgresImage: testPostgresImage}).SetupWithManager(mgr); err != nil {
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
