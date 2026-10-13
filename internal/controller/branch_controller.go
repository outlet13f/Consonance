package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// The Branch reconcile's permissions (config/rbac/role.yaml, make generate).
//
// +kubebuilder:rbac:groups=consonance.io,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=consonance.io,resources=branches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=consonance.io,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=consonance.io,resources=projects,verbs=get;list;watch

// BranchReconciler carries out spec 7.1's Branch steps.
type BranchReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Connector     Connector
	PostgresImage string
	Now           func() time.Time // nil means time.Now

	mu        sync.Mutex
	notBefore map[client.ObjectKey]time.Time // a rate-limited branch is not tried again before this
}

// holdOff keeps a branch from being tried again for d: a rate limit's Retry-After is honored
// even when an event wakes the branch sooner.
func (r *BranchReconciler) holdOff(key client.ObjectKey, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notBefore == nil {
		r.notBefore = map[client.ObjectKey]time.Time{}
	}
	r.notBefore[key] = r.now().Add(d)
}

// heldOff returns how long a branch is still held off.
func (r *BranchReconciler) heldOff(key client.ObjectKey) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.notBefore[key].Sub(r.now())
	if d <= 0 {
		delete(r.notBefore, key)
		return 0
	}
	return d
}

func (r *BranchReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registers the reconciler. A branch is looked at again when its pod or service
// changes, when its project changes (a branch waits for the project's agent), and when a branch
// in its namespace changes (a new branch waits for its parent to run).
func (r *BranchReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Branch{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Watches(&v1alpha1.Project{}, handler.EnqueueRequestsFromMapFunc(r.branchesOf(func(*v1alpha1.Branch) bool { return true }))).
		Watches(&v1alpha1.Branch{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			parent := o.GetName()
			return r.branchesOf(func(b *v1alpha1.Branch) bool { return b.Spec.Parent == parent && b.Status.DataGeneration == 0 })(ctx, o)
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 8, RateLimiter: rateLimiter()}).
		Complete(r)
}

// branchesOf maps an object to the branches of its namespace that match.
func (r *BranchReconciler) branchesOf(match func(*v1alpha1.Branch) bool) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list v1alpha1.BranchList
		if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			logf.FromContext(ctx).Error(err, "list branches")
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			if match(&list.Items[i]) {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
			}
		}
		return out
	}
}

func (r *BranchReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b v1alpha1.Branch
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	id, _ := ProjectID(b.Namespace)
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithValues("projectId", id, "branch", b.Name))

	if d := r.heldOff(req.NamespacedName); d > 0 {
		return ctrl.Result{RequeueAfter: d}, nil
	}
	var p v1alpha1.Project
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ProjectName}, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err) // the Project watch brings the branch back
	}
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReflinkSupported) {
		return ctrl.Result{}, nil // the project's agent is not ready; the Project watch brings us back
	}
	token, err := r.token(ctx, b.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	before := b.Status.DeepCopy()
	res, err := r.reconcile(ctx, &p, &b, token)
	b.Status.ObservedGeneration = b.Generation
	if !equality.Semantic.DeepEqual(*before, b.Status) {
		if uerr := r.Status().Update(ctx, &b); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
	}
	return res, err
}

// token is the project's internal token, which the Project reconcile made.
func (r *BranchReconciler) token(ctx context.Context, ns string) (string, error) {
	var s corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: InternalTokenSecret}, &s); err != nil {
		return "", fmt.Errorf("secret %s: %w", InternalTokenSecret, err)
	}
	t := string(s.Data[TokenKey])
	if t == "" {
		return "", fmt.Errorf("secret %s has no %s", InternalTokenSecret, TokenKey)
	}
	return t, nil
}

func (r *BranchReconciler) reconcile(ctx context.Context, p *v1alpha1.Project, b *v1alpha1.Branch, token string) (ctrl.Result, error) {
	if c := meta.FindStatusCondition(b.Status.Conditions, v1alpha1.ConditionDataReady); c != nil &&
		c.Status == metav1.ConditionFalse && permanentDataFailure(c.Reason) {
		return ctrl.Result{}, nil // spec 10: a permanent failure stays failed
	}

	agent := r.Connector.Agent(b.Namespace, token)
	sup := r.Connector.Supervisor(b.Namespace, b.Name, token)

	// Step 1: data.
	if b.Status.DataGeneration == 0 {
		if res, err := r.ensureData(ctx, b, agent); b.Status.DataGeneration == 0 {
			return res, err
		}
	}

	// Step 2: service and pod. A pod is made only once the data exists, so its subPath does.
	if err := r.ensureService(ctx, b); err != nil {
		return ctrl.Result{}, err
	}
	b.Status.Endpoint = branchEndpoint(b)
	if ready, err := r.ensurePod(ctx, p, b); !ready || err != nil {
		return ctrl.Result{}, err // a pod being replaced: its deletion brings the branch back
	}

	// Step 3: the state.
	return r.ensureState(ctx, b, sup)
}

// permanentDataFailure reports a DataReady reason that retrying cannot fix (spec 10).
func permanentDataFailure(reason string) bool {
	switch reason {
	case ReasonReflinkUnsupported, ReasonInsufficientStorage, ReasonSourceNotFound, ReasonInvalid:
		return true
	}
	return false
}

// ensureData makes the branch's data: main's with init-main (spec 7.1 step 5: main's data is
// made only there), any other's by copying its parent or a restore point. Both calls are safe to
// repeat: a retry answers with the data made already.
func (r *BranchReconciler) ensureData(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI) (ctrl.Result, error) {
	b.Status.Phase = v1alpha1.BranchCreating
	var gen int
	if isMain(b) {
		g, err := call(ctx, initTimeout, agent.InitMain)
		if err != nil {
			return r.dataFailed(b, err, "InitMainFailed")
		}
		gen = g.Generation
	} else {
		req := agentapi.CreateBranchDataRequest{
			Parent: b.Spec.Parent,
			Source: agentapi.Source{Type: agentapi.SourceType(b.Spec.Source.Type), RestorePointID: b.Spec.Source.RestorePointID},
		}
		d, err := call(ctx, copyTimeout, func(ctx context.Context) (agentapi.BranchData, error) {
			return agent.CreateBranchData(ctx, b.Name, req)
		})
		if err != nil {
			return r.dataFailed(b, err, "CopyFailed")
		}
		gen = d.Generation
		if b.Status.CreatedFrom == nil {
			b.Status.CreatedFrom = &v1alpha1.CreatedFrom{Branch: b.Spec.Parent, RestorePointID: b.Spec.Source.RestorePointID}
		}
		if d.BackupStartLSN != "" { // empty when a retry found the data made already
			b.Status.CreatedFrom.BackupStartLSN = d.BackupStartLSN
		}
	}
	b.Status.DataGeneration = int32(gen)
	b.Status.Phase = v1alpha1.BranchDataReady
	setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionDataReady, true, "Made", "")
	if b.Status.Timings.DataReadyAt == nil {
		b.Status.Timings.DataReadyAt = &metav1.Time{Time: r.now()}
	}
	logf.FromContext(ctx).Info("branch data ready", "op", "data", "generation", gen)
	return ctrl.Result{}, nil
}

// dataFailed records why the data could not be made. A permanent failure fails the branch; any
// other is retried.
func (r *BranchReconciler) dataFailed(b *v1alpha1.Branch, err error, reason string) (ctrl.Result, error) {
	permanent := permanentReason(err)
	if permanent == "" && !isMain(b) && agentCode(err) == agentapi.CodeNotFound {
		permanent = ReasonSourceNotFound // no parent data, or no such restore point
	}
	if permanent != "" {
		b.Status.Phase = v1alpha1.BranchFailed
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionDataReady, false, permanent, err.Error())
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, permanent, err.Error())
		return ctrl.Result{}, nil
	}
	if agentCode(err) == agentapi.CodeNotRunning {
		reason = "ParentNotRunning"
	}
	setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionDataReady, false, reason, err.Error())
	return r.retryLater(b, err)
}

// retryLater is the package's retryLater, which also holds the branch off for a rate limit.
func (r *BranchReconciler) retryLater(b *v1alpha1.Branch, err error) (ctrl.Result, error) {
	res, rerr := retryLater(err)
	if agentCode(err) == agentapi.CodeRateLimited {
		r.holdOff(client.ObjectKeyFromObject(b), res.RequeueAfter)
	}
	return res, rerr
}

// ensureService keeps the branch's service.
func (r *BranchReconciler) ensureService(ctx context.Context, b *v1alpha1.Branch) error {
	want := desiredBranchService(b)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = want.Labels
		svc.Spec.Selector, svc.Spec.Ports = want.Spec.Selector, want.Spec.Ports
		svc.Spec.PublishNotReadyAddresses = want.Spec.PublishNotReadyAddresses
		return controllerutil.SetControllerReference(b, svc, r.Scheme)
	})
	return err
}

// ensurePod makes the branch's pod if it is missing and replaces one that ended (spec 7.1 step 2).
// It reports whether a pod is there to talk to.
func (r *BranchReconciler) ensurePod(ctx context.Context, p *v1alpha1.Project, b *v1alpha1.Branch) (bool, error) {
	want := desiredBranchPod(p, b, r.PostgresImage)
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &pod)
	switch {
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(b, want, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, want); err != nil {
			return false, err
		}
		return true, nil
	case err != nil:
		return false, err
	case pod.DeletionTimestamp != nil:
		return false, nil
	case pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded:
		logf.FromContext(ctx).Info("replacing the branch pod", "phase", pod.Status.Phase)
		return false, client.IgnoreNotFound(r.Delete(ctx, &pod))
	}
	return true, nil
}

// ensureState brings PostgreSQL to spec.state (spec 7.1 step 3, 9.5).
func (r *BranchReconciler) ensureState(ctx context.Context, b *v1alpha1.Branch, sup SupervisorAPI) (ctrl.Result, error) {
	st, err := call(ctx, callTimeout, sup.Status)
	if err != nil { // the pod is not up yet, or restarts
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, "SupervisorUnavailable", err.Error())
		return ctrl.Result{RequeueAfter: waitShort}, nil
	}
	if n, ok := parseGeneration(st.Generation); ok {
		b.Status.DataGeneration = int32(n)
	}
	if b.Spec.State == v1alpha1.StateStopped {
		switch st.PG {
		case supervisorapi.PGStopped:
			r.settled(b, v1alpha1.BranchStopped)
			return ctrl.Result{}, nil
		case supervisorapi.PGStopping:
			b.Status.Phase = v1alpha1.BranchStopping
			return ctrl.Result{RequeueAfter: waitPoll}, nil
		}
		b.Status.Phase = v1alpha1.BranchStopping
		st, err := call(ctx, stopTimeout, sup.Stop)
		if err != nil {
			return ctrl.Result{}, err
		}
		if st.PG == supervisorapi.PGStopped {
			r.settled(b, v1alpha1.BranchStopped)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: waitPoll}, nil
	}

	switch st.PG {
	case supervisorapi.PGRunning:
		r.settled(b, v1alpha1.BranchRunning)
		return ctrl.Result{}, nil
	case supervisorapi.PGStarting:
		b.Status.Phase = v1alpha1.BranchStarting
		return ctrl.Result{RequeueAfter: waitPoll}, nil
	case supervisorapi.PGStopping:
		b.Status.Phase = v1alpha1.BranchStopping
		return ctrl.Result{RequeueAfter: waitPoll}, nil
	}
	b.Status.Phase = v1alpha1.BranchStarting
	st, err = call(ctx, startTimeout, sup.Start)
	switch {
	case err == nil && st.PG == supervisorapi.PGRunning:
		r.settled(b, v1alpha1.BranchRunning)
		return ctrl.Result{}, nil
	case err == nil, stillStarting(err), supervisorStatus(err) == 409:
		// Still starting (503), or busy with another request (409): look again.
		return ctrl.Result{RequeueAfter: waitPoll}, nil
	case supervisorStatus(err) != 0:
		// PostgreSQL did not start on this data (500). Tried again with backoff.
		b.Status.Phase = v1alpha1.BranchFailed
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, "StartFailed", err.Error())
	}
	return ctrl.Result{}, err
}

// settled records that the branch reached its desired state.
func (r *BranchReconciler) settled(b *v1alpha1.Branch, phase v1alpha1.BranchPhase) {
	b.Status.Phase = phase
	setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, true, string(phase), "")
	if phase == v1alpha1.BranchRunning && b.Status.Timings.RunningAt == nil {
		b.Status.Timings.RunningAt = &metav1.Time{Time: r.now()}
	}
}

// parseGeneration reads a supervisor's generation, "data-N".
func parseGeneration(name string) (int, bool) {
	s, ok := strings.CutPrefix(name, "data-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}
