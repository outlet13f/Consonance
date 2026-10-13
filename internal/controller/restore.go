package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

// restorePending reports whether a rollback or reset is under way or asked for. One under way is
// finished before a newer request starts.
func restorePending(b *v1alpha1.Branch) bool {
	lr := b.Status.LastRestore
	if lr != nil && !restoreFinished(lr.Phase) {
		return true
	}
	req := b.Spec.Restore
	return req != nil && req.RequestID != "" && (lr == nil || lr.RequestID != req.RequestID)
}

func restoreFinished(p v1alpha1.RestorePhase) bool {
	return p == v1alpha1.RestoreDone || p == v1alpha1.RestoreFailed
}

// step is one step of a restore. next says the step finished and the restore goes on at the
// phase it set; otherwise Reconcile returns res and err.
type step func(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, sup SupervisorAPI) (next bool, res ctrl.Result, err error)

// restore carries out a rollback (source RestorePoint) or reset (source ParentLatest) step by
// step (spec 9.4). The status is saved before each step, so an operator that restarts goes on
// from the step it was at (spec 10).
func (r *BranchReconciler) restore(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, sup SupervisorAPI) (ctrl.Result, error) {
	lr := b.Status.LastRestore
	if lr == nil || restoreFinished(lr.Phase) {
		req := b.Spec.Restore
		lr = &v1alpha1.RestoreStatus{RequestID: req.RequestID, Phase: v1alpha1.RestorePreparing, Source: req.Source, StartedAt: &metav1.Time{Time: r.now()}}
		b.Status.LastRestore = lr
		if lr.Source.Type == v1alpha1.SourceParentLatest && b.Spec.Parent == "" {
			r.restoreFailed(b, "a branch without a parent cannot be reset")
		}
	}
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithValues("op", "restore", "requestId", lr.RequestID))
	steps := map[v1alpha1.RestorePhase]step{
		v1alpha1.RestorePreparing:  r.restorePrepare,
		v1alpha1.RestoreStopping:   r.restoreStop,
		v1alpha1.RestoreActivating: r.restoreActivate,
		v1alpha1.RestoreStarting:   r.restoreStart,
		v1alpha1.RestoreDetaching:  r.restoreDetach,
	}
	for {
		if !restoreFinished(lr.Phase) {
			b.Status.Phase = v1alpha1.BranchRestoring
		}
		if err := r.saveStatus(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
		if restoreFinished(lr.Phase) {
			logf.FromContext(ctx).Info("restore finished", "phase", lr.Phase, "message", lr.Message)
			return ctrl.Result{}, nil
		}
		next, res, err := steps[lr.Phase](ctx, b, agent, sup)
		if !next {
			if err != nil && lr.Message == "" {
				lr.Message = err.Error() // kept until a step clears it, so a stuck restore says why
			}
			return res, err
		}
	}
}

// restoreFailed ends the restore as failed.
func (r *BranchReconciler) restoreFailed(b *v1alpha1.Branch, msg string) {
	lr := b.Status.LastRestore
	lr.Phase, lr.Message, lr.FinishedAt = v1alpha1.RestoreFailed, msg, &metav1.Time{Time: r.now()}
}

// restorePrepare makes the new generation (Preparing). Generations above the current one are what
// earlier tries made and never activated: they are deleted first, and again once the new one
// exists, in case a lost answer made one more (carry-over from plan 2). Generations below the
// current one may hold a user's data from before an earlier restore and are left alone.
func (r *BranchReconciler) restorePrepare(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, _ SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	gens, err := r.clearUnactivated(ctx, b, agent)
	if err != nil {
		res, err := retryLater(err)
		return false, res, err
	}
	if lr.FromGeneration == 0 {
		lr.FromGeneration = int32(gens.Current)
	}
	if lr.ToGeneration == 0 {
		req := agentapi.CreateGenerationRequest{
			Source: agentapi.Source{Type: agentapi.SourceType(lr.Source.Type), RestorePointID: lr.Source.RestorePointID},
		}
		if lr.Source.Type == v1alpha1.SourceParentLatest {
			req.Parent = b.Spec.Parent
		}
		g, err := call(ctx, copyTimeout, func(ctx context.Context) (agentapi.Generation, error) {
			return agent.CreateGeneration(ctx, b.Name, req)
		})
		if err != nil {
			if permanentReason(err) != "" || agentCode(err) == agentapi.CodeNotFound {
				r.restoreFailed(b, err.Error())
				return true, ctrl.Result{}, nil
			}
			if agentCode(err) == agentapi.CodeNotRunning {
				lr.Message = "waiting for the parent to run: " + err.Error()
			}
			res, err := r.retryLater(b, err)
			return false, res, err
		}
		lr.ToGeneration, lr.Message = int32(g.Generation), ""
		if err := r.saveStatus(ctx, b); err != nil { // remember it before anything else
			return false, ctrl.Result{}, err
		}
		if _, err := r.clearUnactivated(ctx, b, agent); err != nil {
			res, err := retryLater(err)
			return false, res, err
		}
	}
	b.Status.LastRestore.Phase = v1alpha1.RestoreStopping
	return true, ctrl.Result{}, nil
}

// clearUnactivated deletes the generations above the current one, except the one this restore
// made, and returns the generations.
func (r *BranchReconciler) clearUnactivated(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI) (agentapi.Generations, error) {
	lr := b.Status.LastRestore
	gens, err := call(ctx, callTimeout, func(ctx context.Context) (agentapi.Generations, error) {
		return agent.ListGenerations(ctx, b.Name)
	})
	if err != nil {
		return gens, err
	}
	if lr.ToGeneration != 0 && !slices.Contains(gens.Generations, int(lr.ToGeneration)) {
		lr.ToGeneration = 0 // gone; make another
	}
	for _, g := range gens.Generations {
		if g <= gens.Current || int32(g) == lr.ToGeneration {
			continue
		}
		if err := r.deleteGeneration(ctx, agent, b.Name, g); err != nil {
			return gens, err
		}
	}
	return gens, nil
}

func (r *BranchReconciler) deleteGeneration(ctx context.Context, agent AgentAPI, branch string, n int) error {
	_, err := call(ctx, callTimeout, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, agent.DeleteGeneration(ctx, branch, n)
	})
	if agentCode(err) == agentapi.CodeNotFound {
		return nil
	}
	return err
}

// restoreStop stops PostgreSQL (Stopping). The switch waits until the supervisor reports it
// stopped (carry-over from plan 1).
func (r *BranchReconciler) restoreStop(ctx context.Context, b *v1alpha1.Branch, _ AgentAPI, sup SupervisorAPI) (bool, ctrl.Result, error) {
	st, err := call(ctx, stopTimeout, sup.Stop)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if st.PG != supervisorapi.PGStopped {
		return false, ctrl.Result{RequeueAfter: waitPoll}, nil
	}
	b.Status.LastRestore.Phase = v1alpha1.RestoreActivating
	return true, ctrl.Result{}, nil
}

// restoreActivate points current at the new generation (Activating).
func (r *BranchReconciler) restoreActivate(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, _ SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	if err := r.activate(ctx, agent, b.Name, lr.ToGeneration); err != nil {
		if agentCode(err) == agentapi.CodeConflict { // PostgreSQL is not stopped after all
			lr.Phase = v1alpha1.RestoreStopping
			return true, ctrl.Result{}, nil
		}
		res, err := retryLater(err)
		return false, res, err
	}
	b.Status.DataGeneration = lr.ToGeneration
	lr.Phase = v1alpha1.RestoreStarting
	return true, ctrl.Result{}, nil
}

func (r *BranchReconciler) activate(ctx context.Context, agent AgentAPI, branch string, n int32) error {
	_, err := call(ctx, callTimeout, func(ctx context.Context) (agentapi.Generations, error) {
		return agent.ActivateGeneration(ctx, branch, int(n))
	})
	return err
}

// restoreStart starts PostgreSQL on the new generation (Starting). A 503 means it still starts,
// not that it failed (carry-over from plan 1); a 500 means it exited on the new data, and the
// branch goes back to the old generation (spec 9.4 step 4).
func (r *BranchReconciler) restoreStart(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, sup SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	if lr.Reverting {
		return r.restoreRevert(ctx, b, agent, sup)
	}
	st, err := call(ctx, startTimeout, sup.Start)
	switch {
	case err == nil && st.PG == supervisorapi.PGRunning:
		lr.Phase = v1alpha1.RestoreDetaching
		return true, ctrl.Result{}, nil
	case err == nil, stillStarting(err), supervisorStatus(err) == 409:
		return false, ctrl.Result{RequeueAfter: waitPoll}, nil
	case supervisorStatus(err) != 0:
		lr.Reverting = true
		lr.Message = "the restored data did not start: " + err.Error()
		return true, ctrl.Result{}, nil
	}
	return false, ctrl.Result{}, err
}

// restoreRevert puts the old generation back, starts it and deletes the new one; the restore then
// ends as failed.
func (r *BranchReconciler) restoreRevert(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, sup SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	why := lr.Message
	if err := r.activate(ctx, agent, b.Name, lr.FromGeneration); err != nil {
		if agentCode(err) == agentapi.CodeConflict { // not stopped: stop it, then switch
			if _, err := call(ctx, stopTimeout, sup.Stop); err != nil {
				return false, ctrl.Result{}, err
			}
			return false, ctrl.Result{RequeueAfter: waitPoll}, nil
		}
		res, err := retryLater(err)
		return false, res, err
	}
	b.Status.DataGeneration = lr.FromGeneration
	st, err := call(ctx, startTimeout, sup.Start)
	switch {
	case err == nil && st.PG == supervisorapi.PGRunning:
	case err == nil, stillStarting(err), supervisorStatus(err) == 409:
		return false, ctrl.Result{RequeueAfter: waitPoll}, nil
	case supervisorStatus(err) != 0:
		why += "; the previous data did not start either: " + err.Error()
	default:
		return false, ctrl.Result{}, err
	}
	if err := r.deleteGeneration(ctx, agent, b.Name, int(lr.ToGeneration)); err != nil {
		res, err := retryLater(err)
		return false, res, err
	}
	r.restoreFailed(b, why)
	return true, ctrl.Result{}, nil
}

// restoreDetach keeps the replaced generation (Detaching): a rollback's as a backup branch, which
// is made only after the move (carry-over from plan 2), a reset's as a restore point.
func (r *BranchReconciler) restoreDetach(ctx context.Context, b *v1alpha1.Branch, agent AgentAPI, _ SupervisorAPI) (bool, ctrl.Result, error) {
	lr := b.Status.LastRestore
	req := agentapi.DetachRequest{AsRestorePoint: true}
	if lr.Source.Type == v1alpha1.SourceRestorePoint {
		if lr.BackupBranch == "" {
			name, err := r.freeBackupName(ctx, b, lr.StartedAt.Time)
			if err != nil {
				return false, ctrl.Result{}, err
			}
			lr.BackupBranch = name
			if err := r.saveStatus(ctx, b); err != nil { // a retry must use the same name
				return false, ctrl.Result{}, err
			}
		}
		req = agentapi.DetachRequest{ToBranch: lr.BackupBranch}
	}
	resp, err := call(ctx, callTimeout, func(ctx context.Context) (agentapi.DetachResponse, error) {
		return agent.DetachGeneration(ctx, b.Name, int(lr.FromGeneration), req)
	})
	if req.ToBranch != "" && agentCode(err) == agentapi.CodeConflict {
		// The name is taken (by data whose Branch is not made yet): take a later second's.
		at, _ := time.Parse("20060102150405", lr.BackupBranch[len(lr.BackupBranch)-14:])
		if at.Sub(lr.StartedAt.Time) > time.Minute { // a conflict that a new name does not cure
			r.restoreFailed(b, fmt.Sprintf("the previous data stays on generation %d: %v", lr.FromGeneration, err))
			return true, ctrl.Result{}, nil
		}
		name, ferr := r.freeBackupName(ctx, b, at.Add(time.Second))
		if ferr != nil {
			return false, ctrl.Result{}, ferr
		}
		lr.BackupBranch = name
		return true, ctrl.Result{}, nil // saved, then tried again
	}
	switch {
	case agentCode(err) == agentapi.CodeNotFound: // nothing to keep
		lr.BackupBranch = ""
		lr.Message = "the previous data was gone; nothing was kept"
	case err != nil:
		res, err := retryLater(err)
		return false, res, err
	case req.ToBranch != "":
		if err := r.ensureBackupBranch(ctx, b, lr.BackupBranch); err != nil {
			return false, ctrl.Result{}, err
		}
		lr.Message = ""
	case resp.RestorePoint != nil:
		lr.RestorePointID, lr.Message = resp.RestorePoint.ID, ""
	}
	lr.Phase, lr.FinishedAt = v1alpha1.RestoreDone, &metav1.Time{Time: r.now()}
	return true, ctrl.Result{}, nil
}

// freeBackupName returns the first backup branch name from t on that no Branch has: two rollbacks
// of a branch within one second would otherwise want the same name (spec 9.4 names it to the
// second).
func (r *BranchReconciler) freeBackupName(ctx context.Context, b *v1alpha1.Branch, t time.Time) (string, error) {
	for i := range 60 {
		name := backupBranchName(b.Name, t.Add(time.Duration(i)*time.Second))
		var existing v1alpha1.Branch
		err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, &existing)
		if apierrors.IsNotFound(err) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("every backup branch name of a minute is taken")
}

// ensureBackupBranch makes the stopped branch that holds a rollback's replaced data. Its data is
// there already, so its own reconcile finds it instead of copying.
func (r *BranchReconciler) ensureBackupBranch(ctx context.Context, b *v1alpha1.Branch, name string) error {
	backup := &v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.Namespace, Labels: map[string]string{LabelBackupOf: b.Name}},
		Spec: v1alpha1.BranchSpec{
			Parent:  b.Name,
			Source:  v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest},
			State:   v1alpha1.StateStopped,
			Compute: b.Spec.Compute,
		},
	}
	err := r.Create(ctx, backup)
	if apierrors.IsAlreadyExists(err) {
		var got v1alpha1.Branch
		if gerr := r.Get(ctx, client.ObjectKeyFromObject(backup), &got); gerr != nil {
			return gerr
		}
		if got.Labels[LabelBackupOf] != b.Name {
			return errors.New("branch " + name + " exists and is not this rollback's backup")
		}
		return nil
	}
	return err
}
