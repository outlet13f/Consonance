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
	"k8s.io/apimachinery/pkg/types"
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
// +kubebuilder:rbac:groups=consonance.io,resources=branches,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=consonance.io,resources=branches/status,verbs=update
// +kubebuilder:rbac:groups=consonance.io,resources=branches/finalizers,verbs=update
// +kubebuilder:rbac:groups=consonance.io,resources=projects,verbs=get;list;watch

// BranchReconciler carries out spec 7.1's Branch steps.
type BranchReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Connector     Connector
	PostgresImage string
	Now           func() time.Time // nil means time.Now

	mu         sync.Mutex
	notBefore  map[client.ObjectKey]time.Time     // a rate-limited branch is not tried again before this
	unanswered map[client.ObjectKey]time.Duration // a branch whose supervisor does not answer: the last wait
	podUIDs    map[client.ObjectKey]types.UID     // the UID of the branch's pod last seen: a new pod starts the wait afresh

	// timed holds, for a branch whose timings are not all recorded yet, which of them the API
	// server has: a metric is observed when its timing first reaches the API server, not before
	// (a status update that conflicts is made again) and not again after.
	timed map[client.ObjectKey]timedStatus
	// startedAt is when the operator asked a stopped branch to start, until it sees it run. It
	// is lost with the operator, and that start is then not timed.
	startedAt map[client.ObjectKey]time.Time
}

// timedStatus says which timings of a branch are recorded.
type timedStatus struct{ dataReady, running bool }

// startingSince notes the first request to start a stopped branch.
func (r *BranchReconciler) startingSince(key client.ObjectKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startedAt == nil {
		r.startedAt = map[client.ObjectKey]time.Time{}
	}
	if _, ok := r.startedAt[key]; !ok {
		r.startedAt[key] = r.now()
	}
}

// startEnded drops the note of a start; if the branch runs, the start's time is observed.
func (r *BranchReconciler) startEnded(key client.ObjectKey, running bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if at, ok := r.startedAt[key]; ok && running {
		observe(startSeconds, r.now().Sub(at))
	}
	delete(r.startedAt, key)
}

// recorded notes which of a branch's timings the API server has, as just read.
func (r *BranchReconciler) recorded(b *v1alpha1.Branch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := b.Status.Timings
	key := client.ObjectKeyFromObject(b)
	if t.DataReadyAt != nil && t.RunningAt != nil {
		delete(r.timed, key)
		return
	}
	if r.timed == nil {
		r.timed = map[client.ObjectKey]timedStatus{}
	}
	r.timed[key] = timedStatus{dataReady: t.DataReadyAt != nil, running: t.RunningAt != nil}
}

// saved observes the timings that b's status, which the API server now has, records for the first
// time (spec 11). main's data time is left out: its init-main belongs to the project's creation.
// Backup branches are left out of both: the operator makes them in a rollback, and their
// near-zero times would skew what a user's branch takes.
func (r *BranchReconciler) saved(b *v1alpha1.Branch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := client.ObjectKeyFromObject(b)
	was, ok := r.timed[key]
	if !ok {
		return // everything was recorded already
	}
	_, backup := b.Labels[LabelBackupOf]
	t := b.Status.Timings
	created := b.CreationTimestamp.Time
	if t.DataReadyAt != nil && !was.dataReady {
		was.dataReady = true
		if !isMain(b) && !backup {
			observe(branchCreateSeconds, t.DataReadyAt.Sub(created))
		}
	}
	if t.RunningAt != nil && !was.running {
		was.running = true
		if !backup {
			observe(branchFirstReadySeconds, t.RunningAt.Sub(created))
		}
	}
	r.timed[key] = was
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

// supervisorWait returns how long to wait before asking again a supervisor that did not answer:
// waitShort, doubled with each further failure up to 30s, so dead nodes do not keep the workers.
func (r *BranchReconciler) supervisorWait(key client.ObjectKey) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unanswered == nil {
		r.unanswered = map[client.ObjectKey]time.Duration{}
	}
	d := min(2*r.unanswered[key], 30*time.Second)
	if d == 0 {
		d = waitShort
	}
	r.unanswered[key] = d
	return d
}

// supervisorAnswered resets the wait of supervisorWait.
func (r *BranchReconciler) supervisorAnswered(key client.ObjectKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.unanswered, key)
}

// podSeen notes the pod the branch's supervisor runs in. A new pod is a fresh start: the waits
// grown on the one before do not apply to it.
func (r *BranchReconciler) podSeen(key client.ObjectKey, uid types.UID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.podUIDs == nil {
		r.podUIDs = map[client.ObjectKey]types.UID{}
	}
	if r.podUIDs[key] != uid {
		r.podUIDs[key] = uid
		delete(r.unanswered, key)
	}
}

// forget drops what is kept in memory about a deleted branch, so a new branch of the same name
// starts afresh.
func (r *BranchReconciler) forget(key client.ObjectKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.notBefore, key)
	delete(r.unanswered, key)
	delete(r.podUIDs, key)
	delete(r.timed, key)
	delete(r.startedAt, key)
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
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	id, _ := ProjectID(b.Namespace)
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithValues("projectId", id, "branch", b.Name))

	if !b.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &b)
	}
	if controllerutil.AddFinalizer(&b, BranchFinalizer) {
		if err := r.Update(ctx, &b); err != nil {
			return ctrl.Result{}, r.gone(req.NamespacedName, err)
		}
	}
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
		if apierrors.IsNotFound(err) && beingDeleted(ctx, r.Client, b.Namespace) {
			return ctrl.Result{}, nil // the namespace controller took it; the branch goes too
		}
		return ctrl.Result{}, err
	}

	r.recorded(&b)
	before := b.Status.DeepCopy()
	res, err := r.reconcile(ctx, &p, &b, token)
	if namespaceTerminating(err) {
		return ctrl.Result{}, nil // the project is being deleted: nothing it would make stays
	}
	b.Status.ObservedGeneration = b.Generation
	if !equality.Semantic.DeepEqual(*before, b.Status) {
		if uerr := r.Status().Update(ctx, &b); uerr != nil {
			if apierrors.IsConflict(uerr) {
				return res, err // a newer version exists, and its update event brings the branch back
			}
			if apierrors.IsNotFound(uerr) {
				return ctrl.Result{}, r.gone(req.NamespacedName, uerr)
			}
			return ctrl.Result{}, errors.Join(err, uerr)
		}
		r.saved(&b)
	}
	return res, err
}

// gone turns the NotFound of a write to a branch that was deleted meanwhile into success, and
// forgets the branch; any other error is returned as it is.
func (r *BranchReconciler) gone(key client.ObjectKey, err error) error {
	if apierrors.IsNotFound(err) {
		r.forget(key)
		return nil
	}
	return err
}

// saveStatus writes b's status now. It sends a copy, so b's status, and the pointers into it a
// restore holds, stay the ones in memory; only the resource version is taken back.
func (r *BranchReconciler) saveStatus(ctx context.Context, b *v1alpha1.Branch) error {
	c := b.DeepCopy()
	if err := r.Status().Update(ctx, c); err != nil {
		return err
	}
	b.ResourceVersion = c.ResourceVersion
	r.saved(b)
	return nil
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
		c.Status == metav1.ConditionFalse && v1alpha1.PermanentDataFailure(c.Reason) {
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
		return ctrl.Result{}, err // a pod being replaced or placed: its deletion or status change brings the branch back
	}

	// Step 4 (spec 9.4): a rollback or reset takes over while one is open; the state follows.
	if restorePending(b) {
		res, err := r.restore(ctx, b, agent, sup)
		if err != nil || restorePending(b) {
			return res, err
		}
	}

	// Step 3: the state.
	return r.ensureState(ctx, b, sup)
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
			return r.dataFailed(b, err, v1alpha1.ReasonInitMainFailed)
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
			countCopyError(err)
			return r.dataFailed(b, err, v1alpha1.ReasonCopyFailed)
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
		permanent = v1alpha1.ReasonSourceNotFound // no parent data, or no such restore point
	}
	if permanent != "" {
		b.Status.Phase = v1alpha1.BranchFailed
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionDataReady, false, permanent, err.Error())
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, permanent, err.Error())
		return ctrl.Result{}, nil
	}
	if agentCode(err) == agentapi.CodeNotRunning {
		reason = v1alpha1.ReasonParentNotRunning
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
// It reports whether a pod is there to talk to; a pod that cannot be made or placed sets Ready to
// say why.
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
			setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, v1alpha1.ReasonPodCreateFailed, err.Error())
			return false, err
		}
		return false, nil // its container is not started yet; the pod's status changes bring the branch back
	case err != nil:
		return false, err
	case pod.DeletionTimestamp != nil:
		return false, nil
	case pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded:
		logf.FromContext(ctx).Info("replacing the branch pod", "phase", pod.Status.Phase)
		return false, client.IgnoreNotFound(r.Delete(ctx, &pod))
	}
	if c := unschedulable(&pod); c != nil {
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, v1alpha1.ReasonUnschedulable, c.Message)
		return false, nil // no supervisor to call; the pod's status change brings the branch back
	}
	// Until its container has started there is no supervisor to call. Calling anyway fails, and
	// each failure would double the wait before the next try (supervisorWait): the pod's early
	// status changes would grow it to many seconds before the supervisor listens, and a stopped
	// branch's pod, never Ready, sends no later change to cut that wait short. The change that
	// marks the container started brings the branch back instead.
	r.podSeen(client.ObjectKeyFromObject(b), pod.UID)
	return containerStarted(&pod), nil
}

// containerStarted reports whether the pod runs and its container has started.
func containerStarted(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName {
			return cs.Started != nil && *cs.Started
		}
	}
	return false
}

// ensureState brings PostgreSQL to spec.state (spec 7.1 step 3, 9.5).
func (r *BranchReconciler) ensureState(ctx context.Context, b *v1alpha1.Branch, sup SupervisorAPI) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(b)
	st, err := call(ctx, statusTimeout, sup.Status)
	if err != nil { // the pod is not up yet, or restarts, or its node is gone
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, v1alpha1.ReasonSupervisorUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: r.supervisorWait(key)}, nil
	}
	r.supervisorAnswered(key)
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
	if b.Status.Phase == v1alpha1.BranchStopped { // asked to run again: this is a start, not the first
		r.startingSince(key)
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
		setCondition(&b.Status.Conditions, b.Generation, v1alpha1.ConditionReady, false, v1alpha1.ReasonStartFailed, err.Error())
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
	r.startEnded(client.ObjectKeyFromObject(b), phase == v1alpha1.BranchRunning)
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
