package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"consonance/api/v1alpha1"
	"consonance/internal/testutil/kubetest"
)

// terminate deletes a namespace. envtest runs no namespace controller, so it stays Terminating
// with everything in it, and the API server refuses creates in it, as during a project deletion
// before the namespace controller is done.
func terminate(t *testing.T, ns string) {
	t.Helper()
	ctx := context.Background()
	if err := kenv.Client.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		var n corev1.Namespace
		if err := kenv.Client.Get(ctx, client.ObjectKey{Name: ns}, &n); err != nil {
			return err.Error()
		}
		if n.Status.Phase != corev1.NamespaceTerminating {
			return "namespace " + string(n.Status.Phase)
		}
		return ""
	})
}

// deleteNow deletes an object as the namespace controller does, and waits until it is gone.
func deleteNow(t *testing.T, obj client.Object) {
	t.Helper()
	if err := client.IgnoreNotFound(kenv.Client.Delete(context.Background(), obj)); err != nil {
		t.Fatal(err)
	}
	waitGone(t, obj.GetNamespace(), obj.GetName(), obj)
}

func TestBeingDeleted(t *testing.T) {
	kubetest.Need(t, kenv)
	ctx := context.Background()
	ns := kenv.Namespace(t)
	if beingDeleted(ctx, kenv.Client, ns) {
		t.Error("an active namespace is reported as being deleted")
	}
	var s corev1.Secret
	if err := kenv.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: InternalTokenSecret}, &s); err == nil {
		t.Error("the dry run made the secret")
	}
	terminate(t, ns)
	if !beingDeleted(ctx, kenv.Client, ns) {
		t.Error("a terminating namespace is not reported as being deleted")
	}
}

// While a project's namespace is deleted, the namespace controller deletes the pods, services
// and secrets under the Branches and the Project. Their reconciles end quietly instead of
// failing on what they cannot make again, and a deleted branch lets go of its finalizer without
// the agent.
func TestReconcilesAreQuietWhileTheNamespaceIsDeleted(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	terminate(t, ns)

	ctx := context.Background()
	br := &BranchReconciler{Client: kenv.Client, Scheme: kenv.Scheme, Connector: conn, PostgresImage: testPostgresImage}
	pr := &ProjectReconciler{Client: kenv.Client, Scheme: kenv.Scheme, Connector: conn, AgentImage: testAgentImage}
	reconcileQuietly := func(r interface {
		Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
	}, name string) {
		t.Helper()
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: name}})
		if err != nil || res != (ctrl.Result{}) {
			t.Errorf("reconcile %s = %+v, %v; want nothing", name, res, err)
		}
	}

	// The pod and service went: making them again is refused.
	deleteNow(t, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: BranchObjectName("dev")}})
	deleteNow(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: BranchObjectName("dev")}})
	reconcileQuietly(br, "dev")

	// The agent pod went: the Project's reconcile cannot make it again.
	deleteNow(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: AgentName}})
	reconcileQuietly(pr, ProjectName)

	// The internal token went.
	deleteNow(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: InternalTokenSecret}})
	reconcileQuietly(br, "dev")
	reconcileQuietly(pr, ProjectName)

	// The branch is deleted: its finalizer goes without the token or the agent; the volume
	// goes with the namespace.
	deleteBranch(t, ns, "dev")
	waitGone(t, ns, "dev", &v1alpha1.Branch{})
	if slices.Contains(fp.callLog(), "DeleteBranchData(dev)") {
		t.Errorf("the agent was asked to delete dev's data: %q", fp.callLog())
	}
}

// goneOnWrite is a fake client whose object vanishes just before the reconcile writes it: the
// write then fails with the API server's NotFound.
func goneOnWrite(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	vanish := func(ctx context.Context, c client.WithWatch, obj client.Object) {
		cur := obj.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), cur); err != nil {
			t.Fatal(err)
		}
		cur.SetFinalizers(nil)
		if err := c.Update(ctx, cur); err != nil {
			t.Fatal(err)
		}
		if err := client.IgnoreNotFound(c.Delete(ctx, cur)); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(kubetest.Scheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Branch{}, &v1alpha1.Project{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				vanish(ctx, c.(client.WithWatch), obj)
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*v1alpha1.Branch); ok {
					vanish(ctx, c, obj)
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
}

func tokenSecret(ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: InternalTokenSecret, Namespace: ns},
		Data:       map[string][]byte{TokenKey: []byte("t")},
	}
}

// A branch deleted between the read and the status write: the write's NotFound is success.
func TestStatusWriteOfAGoneBranchIsSuccess(t *testing.T) {
	const ns = "cns-x"
	p := testProject(ns)
	p.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionReflinkSupported, Status: metav1.ConditionTrue, Reason: "Supported", LastTransitionTime: metav1.Now()}}
	b := testBranch(ns, v1alpha1.MainBranch, "")
	b.Finalizers = []string{BranchFinalizer}
	c := goneOnWrite(t, p, b, tokenSecret(ns))
	r := &BranchReconciler{Client: c, Scheme: c.Scheme(), Connector: newFakeConnector(c), PostgresImage: testPostgresImage}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); err != nil {
		t.Errorf("reconcile = %v, want nil", err)
	}
}

// A deleted branch whose finalizer someone else removed first: the update's NotFound is
// success.
func TestFinalizerRemovalOfAGoneBranchIsSuccess(t *testing.T) {
	const ns = "cns-x"
	b := testBranch(ns, v1alpha1.MainBranch, "")
	b.Finalizers = []string{BranchFinalizer}
	b.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	b.Status.Phase = v1alpha1.BranchDeleting // no status write first: the Update is what vanishes
	c := goneOnWrite(t, b)
	r := &BranchReconciler{Client: c, Scheme: c.Scheme(), Connector: newFakeConnector(c), PostgresImage: testPostgresImage}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); err != nil {
		t.Errorf("reconcile = %v, want nil", err)
	}
}

// A Project deleted between the read and the status write: the write's NotFound is success.
func TestStatusWriteOfAGoneProjectIsSuccess(t *testing.T) {
	const ns = "cns-x"
	p := testProject(ns)
	c := goneOnWrite(t, p, tokenSecret(ns))
	r := &ProjectReconciler{Client: c, Scheme: c.Scheme(), Connector: newFakeConnector(c), AgentImage: testAgentImage}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
		t.Errorf("reconcile = %v, want nil", err)
	}
}
