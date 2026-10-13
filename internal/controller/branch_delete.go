package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"consonance/api/v1alpha1"
)

// finalize deletes a branch (spec 7.1 step 5, 9.6): its service and pod, then, once the pod
// object is gone, its data, then the finalizer. The agent removes a branch's data when its
// supervisor does not answer, taking that for "the pod is gone", so the pod must really be gone
// first (carry-over from plan 2).
func (r *BranchReconciler) finalize(ctx context.Context, b *v1alpha1.Branch) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, BranchFinalizer) {
		return ctrl.Result{}, nil
	}
	if b.Status.Phase != v1alpha1.BranchDeleting {
		b.Status.Phase = v1alpha1.BranchDeleting
		if err := r.Status().Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}
	key := client.ObjectKey{Namespace: b.Namespace, Name: BranchObjectName(b.Name)}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if err := client.IgnoreNotFound(r.Delete(ctx, svc)); err != nil {
		return ctrl.Result{}, err
	}
	var pod corev1.Pod
	err := r.Get(ctx, key, &pod)
	switch {
	case err == nil:
		if pod.DeletionTimestamp == nil {
			if err := client.IgnoreNotFound(r.Delete(ctx, &pod)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: waitShort}, nil // the pod's deletion also brings us back
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	deleteData, err := r.dataToDelete(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}
	if deleteData {
		token, err := r.token(ctx, b.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		agent := r.Connector.Agent(b.Namespace, token)
		if _, err := call(ctx, callTimeout, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, agent.DeleteBranchData(ctx, b.Name) // a branch without data: success
		}); err != nil {
			return retryLater(err)
		}
		logf.FromContext(ctx).Info("branch data deleted", "op", "delete")
	}
	controllerutil.RemoveFinalizer(b, BranchFinalizer)
	return ctrl.Result{}, r.Update(ctx, b)
}

// dataToDelete reports whether the branch's data must be deleted through the agent. Not main's:
// it is the project's data, which the agent refuses to delete; deleting the main Branch object
// only makes the Project reconcile make it again, on the same data. Not when the project goes:
// deleting the namespace takes the volume, and the agent may be gone already. Not when the
// project never had a working agent: no data can exist.
func (r *BranchReconciler) dataToDelete(ctx context.Context, b *v1alpha1.Branch) (bool, error) {
	if isMain(b) {
		return false, nil
	}
	var p v1alpha1.Project
	err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ProjectName}, &p)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !p.DeletionTimestamp.IsZero() || !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReflinkSupported) {
		return false, nil
	}
	return true, nil
}
