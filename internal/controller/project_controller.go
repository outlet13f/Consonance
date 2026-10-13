package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"consonance/api/v1alpha1"
)

const (
	bootstrapTimeout = 30 * time.Second
	// usageEvery is how often a ready project's volume usage is refreshed.
	usageEvery = time.Minute
)

// The Project reconcile's permissions (config/rbac/role.yaml, make generate).
//
// +kubebuilder:rbac:groups=consonance.io,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups=consonance.io,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=consonance.io,resources=projects/finalizers,verbs=update
// +kubebuilder:rbac:groups=consonance.io,resources=branches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;persistentvolumeclaims;pods;services;resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// ProjectReconciler carries out spec 7.1's Project steps.
type ProjectReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Connector  Connector
	AgentImage string
}

// SetupWithManager registers the reconciler. A project is looked at again when its spec changes
// (not on its own status updates), when one of its objects changes, when its volume binds, and
// when main changes (main's first run leads to the bootstrap).
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Project{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(projectOf)).
		Watches(&v1alpha1.Branch{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			if o.GetName() != v1alpha1.MainBranch {
				return nil
			}
			return projectOf(ctx, o)
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4, RateLimiter: rateLimiter()}).
		Complete(r)
}

// projectOf maps an object to the Project of its namespace.
func projectOf(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: o.GetNamespace(), Name: ProjectName}}}
}

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Project
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // deleting a project deletes its namespace and everything in it (spec 4.1)
	}
	id, _ := ProjectID(p.Namespace)
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithValues("projectId", id))

	before := p.Status.DeepCopy()
	res, err := r.reconcile(ctx, &p)
	p.Status.ObservedGeneration = p.Generation
	summarize(&p)
	if !equality.Semantic.DeepEqual(*before, p.Status) {
		if uerr := r.Status().Update(ctx, &p); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
	}
	return res, err
}

// reconcile takes the project as far as it can; the conditions it sets say how far it got.
func (r *ProjectReconciler) reconcile(ctx context.Context, p *v1alpha1.Project) (ctrl.Result, error) {
	id, ok := ProjectID(p.Namespace)
	if !ok {
		return ctrl.Result{}, nil // summarize reports it; nothing else is made
	}

	// Step 1: secrets, network policies, quota.
	if _, err := r.ensureSecret(ctx, newDBCredentialsSecret, p.Namespace); err != nil {
		return ctrl.Result{}, err
	}
	tokenSecret, err := r.ensureSecret(ctx, newInternalTokenSecret, p.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	token := string(tokenSecret.Data[TokenKey])
	if token == "" {
		return ctrl.Result{}, fmt.Errorf("secret %s has no %s", InternalTokenSecret, TokenKey)
	}
	for _, want := range desiredNetworkPolicies(p.Namespace) {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
			np.Labels, np.Spec = want.Labels, want.Spec
			return controllerutil.SetControllerReference(p, np, r.Scheme)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	wantQuota := desiredQuota(p)
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: wantQuota.Name, Namespace: wantQuota.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Labels, quota.Spec = wantQuota.Labels, wantQuota.Spec
		return controllerutil.SetControllerReference(p, quota, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Step 2: the volume. A WaitForFirstConsumer class binds it only once the agent pod is
	// scheduled, so nothing below waits for VolumeReady.
	pvc, err := r.ensurePVC(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	p.Status.Volume.PVCName = PVCName
	if pvc.Status.Phase == corev1.ClaimBound {
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionVolumeReady, true, "Bound", "")
	} else {
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionVolumeReady, false, "NotBound", "the volume is "+string(pvc.Status.Phase))
	}

	// Step 3: the agent.
	if err := r.ensureAgent(ctx, p, id); err != nil {
		return ctrl.Result{}, err
	}
	agent := r.Connector.Agent(p.Namespace, token)

	// Step 4: reflink. A volume without it stays failed (spec 10); a probe that failed for
	// another reason (500) is tried again.
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReflinkSupported); c == nil || c.Status != metav1.ConditionTrue {
		if c != nil && c.Status == metav1.ConditionFalse {
			return ctrl.Result{}, nil
		}
		pre, err := call(ctx, callTimeout, agent.Preflight)
		if err != nil {
			setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionAgentReady, false, "AgentUnavailable", err.Error())
			return ctrl.Result{RequeueAfter: waitShort}, nil
		}
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionAgentReady, true, "Answers", "")
		if !pre.Reflink {
			setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionReflinkSupported, false, ReasonReflinkUnsupported, pre.Error)
			return ctrl.Result{}, nil
		}
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionReflinkSupported, true, "Supported", "kernel "+pre.Kernel)
	}

	// Step 5: main. Its Branch reconcile makes its data with init-main.
	main, err := r.ensureMainBranch(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Step 6: the tenant login, once main first runs. Bootstrap skips what exists.
	if !meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionMainReady) {
		if main.Status.Phase != v1alpha1.BranchRunning {
			setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionMainReady, false, "MainNotRunning", "main is "+phaseOrNone(main.Status.Phase))
			return ctrl.Result{}, nil // the main Branch watch brings the project back
		}
		sup := r.Connector.Supervisor(p.Namespace, v1alpha1.MainBranch, token)
		if _, err := call(ctx, bootstrapTimeout, sup.Bootstrap); err != nil {
			setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionMainReady, false, "BootstrapFailed", err.Error())
			return ctrl.Result{}, err
		}
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionMainReady, true, "Bootstrapped", "")
	}

	// The volume's usage, refreshed while the project lives; it also tells whether the agent
	// still answers.
	u, err := call(ctx, callTimeout, agent.Usage)
	if err != nil {
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionAgentReady, false, "AgentUnavailable", err.Error())
		return ctrl.Result{RequeueAfter: waitShort}, nil
	}
	setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionAgentReady, true, "Answers", "")
	p.Status.Volume.CapacityBytes, p.Status.Volume.UsedBytes = u.CapacityBytes, u.UsedBytes
	return ctrl.Result{RequeueAfter: usageEvery}, nil
}

// ensureSecret returns the secret, made by newSecret if it does not exist. An existing secret is
// never changed: the pods and main's database hold its values.
func (r *ProjectReconciler) ensureSecret(ctx context.Context, newSecret func(string) (*corev1.Secret, error), ns string) (*corev1.Secret, error) {
	want, err := newSecret(ns)
	if err != nil {
		return nil, err
	}
	var got corev1.Secret
	err = r.Get(ctx, client.ObjectKeyFromObject(want), &got)
	if err == nil {
		return &got, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	// No owner: the secrets, like the volume, go only with the namespace.
	if err := r.Create(ctx, want); err != nil {
		return nil, err
	}
	return want, nil
}

// ensurePVC makes the project volume once. It has no owner: deleting the Project object alone
// must not delete the data; deleting the namespace does (spec 4.1).
func (r *ProjectReconciler) ensurePVC(ctx context.Context, p *v1alpha1.Project) (*corev1.PersistentVolumeClaim, error) {
	want := desiredPVC(p)
	var got corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &got)
	if err == nil {
		return &got, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err := r.Create(ctx, want); err != nil {
		return nil, err
	}
	return want, nil
}

// ensureAgent keeps the agent's service and pod. A pod that ended, or that runs old settings, is
// deleted, and the next reconcile makes it anew.
func (r *ProjectReconciler) ensureAgent(ctx context.Context, p *v1alpha1.Project, projectID string) error {
	wantSvc := desiredAgentService(p.Namespace)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: wantSvc.Name, Namespace: wantSvc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = wantSvc.Labels
		svc.Spec.Selector, svc.Spec.Ports = wantSvc.Spec.Selector, wantSvc.Spec.Ports
		return controllerutil.SetControllerReference(p, svc, r.Scheme)
	}); err != nil {
		return err
	}
	want := desiredAgentPod(p, projectID, r.AgentImage)
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &pod)
	switch {
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(p, want, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, want)
	case err != nil:
		return err
	case pod.DeletionTimestamp != nil:
		return nil // on its way out; its deletion brings the project back
	case pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded ||
		pod.Annotations[AnnotationSpecHash] != want.Annotations[AnnotationSpecHash]:
		logf.FromContext(ctx).Info("replacing the agent pod", "phase", pod.Status.Phase)
		return client.IgnoreNotFound(r.Delete(ctx, &pod))
	}
	return nil
}

// ensureMainBranch makes the main Branch once (spec 7.1 step 5). The Project owns it.
func (r *ProjectReconciler) ensureMainBranch(ctx context.Context, p *v1alpha1.Project) (*v1alpha1.Branch, error) {
	var main v1alpha1.Branch
	err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: v1alpha1.MainBranch}, &main)
	if err == nil {
		return &main, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	main = v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.MainBranch, Namespace: p.Namespace},
		Spec: v1alpha1.BranchSpec{
			Source:    v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest},
			Protected: true,
			State:     v1alpha1.StateRunning,
		},
	}
	if err := controllerutil.SetControllerReference(p, &main, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, &main); err != nil {
		return nil, err
	}
	return &main, nil
}

// summarize sets the phase and the Ready condition from the four conditions of spec 4.2.
func summarize(p *v1alpha1.Project) {
	ready := func(status bool, reason, msg string) {
		setCondition(&p.Status.Conditions, p.Generation, v1alpha1.ConditionReady, status, reason, msg)
	}
	if _, ok := ProjectID(p.Namespace); !ok {
		p.Status.Phase = v1alpha1.ProjectFailed
		ready(false, "InvalidNamespace", "a Project lives in a namespace named cns-<projectId>")
		return
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReflinkSupported); c != nil && c.Status == metav1.ConditionFalse {
		p.Status.Phase = v1alpha1.ProjectFailed
		ready(false, ReasonReflinkUnsupported, c.Message)
		return
	}
	var waiting []string
	for _, t := range []string{v1alpha1.ConditionVolumeReady, v1alpha1.ConditionAgentReady, v1alpha1.ConditionReflinkSupported, v1alpha1.ConditionMainReady} {
		if !meta.IsStatusConditionTrue(p.Status.Conditions, t) {
			waiting = append(waiting, t)
		}
	}
	if len(waiting) == 0 {
		p.Status.Phase = v1alpha1.ProjectReady
		ready(true, "Ready", "")
		return
	}
	p.Status.Phase = v1alpha1.ProjectProvisioning
	ready(false, "Provisioning", "waiting for "+strings.Join(waiting, ", "))
}

// setCondition sets one condition; its transition time changes only with its status.
func setCondition(conds *[]metav1.Condition, generation int64, typ string, status bool, reason, msg string) {
	s := metav1.ConditionFalse
	if status {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conds, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: msg, ObservedGeneration: generation})
}

func phaseOrNone(p v1alpha1.BranchPhase) string {
	if p == "" {
		return "not reconciled yet"
	}
	return string(p)
}
