package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
)

// stateUpdateTries is how often a change of spec.state is tried when the write conflicts: the
// operator writes the Branch too (finalizers, labels), and the cached read may be behind.
const stateUpdateTries = 3

func (s *server) stateEndpoints() []endpoint {
	const base = "/v1/projects/{projectId}/branches/{branch}"
	return []endpoint{
		{route{http.MethodPost, base + "/stop", "branch.stop"}, s.stopBranch},
		{route{http.MethodPost, base + "/start", "branch.start"}, s.startBranch},
	}
}

// restoreOpen reports whether a rollback or reset of b is under way or asked for: the last one
// is not Done or Failed, or the request in the spec is one the operator has not carried out. It
// is the operator's own test (restorePending), and a branch for which it is true is busy.
func restoreOpen(b *v1alpha1.Branch) bool {
	lr := b.Status.LastRestore
	if lr != nil && lr.Phase != v1alpha1.RestoreDone && lr.Phase != v1alpha1.RestoreFailed {
		return true
	}
	req := b.Spec.Restore
	return req != nil && req.RequestID != "" && (lr == nil || lr.RequestID != req.RequestID)
}

// dataFailed is the 409 of a branch the operator has given up on: DataReady is false for a
// reason retrying cannot fix, or the branch is Failed without data. The operator reads neither
// spec.restore nor spec.state of it again (spec 10), so a request would wait for nothing and a
// restore would stay open for ever. It is nil for any other branch.
func dataFailed(b *v1alpha1.Branch) error {
	c := falseCondition(b, v1alpha1.ConditionDataReady)
	if c == nil || !v1alpha1.PermanentDataFailure(c.Reason) {
		if b.Status.Phase != v1alpha1.BranchFailed || b.Status.DataGeneration != 0 {
			return nil
		}
		c = falseCondition(b, v1alpha1.ConditionDataReady, v1alpha1.ConditionReady)
	}
	reason, msg := string(v1alpha1.BranchFailed), "it has no data"
	if c != nil {
		reason, msg = c.Reason, c.Message
	}
	return conflict("branch %s has failed (%s): %s; delete it", b.Name, reason, msg)
}

// stopBranch stops a branch. A project that is not Ready may be stopped: stopping must always
// work.
func (s *server) stopBranch(w http.ResponseWriter, r *http.Request) error {
	return s.changeState(w, r, v1alpha1.StateStopped, false)
}

func (s *server) startBranch(w http.ResponseWriter, r *http.Request) error {
	return s.changeState(w, r, v1alpha1.StateRunning, true)
}

// phaseOfState is the phase a branch reaches when it is in the state.
func phaseOfState(state v1alpha1.DesiredState) v1alpha1.BranchPhase {
	if state == v1alpha1.StateStopped {
		return v1alpha1.BranchStopped
	}
	return v1alpha1.BranchRunning
}

// changeState sets spec.state of the branch and waits for its phase: 200 when it is there, 202
// when the wait is over first. A branch that already has the state is not written.
func (s *server) changeState(w http.ResponseWriter, r *http.Request, want v1alpha1.DesiredState, needReady bool) error {
	ctx := r.Context()
	id, name := r.PathValue("projectId"), r.PathValue("branch")
	logAttrs(ctx, "projectId", id, "branch", name)
	var err error
	if needReady {
		_, err = s.readyProject(ctx, id)
	} else {
		_, err = s.project(ctx, id)
	}
	if err != nil {
		return err
	}
	ns := controller.NamespacePrefix + id

	b, err := s.setState(ctx, ns, name, want)
	if err != nil {
		return err
	}

	// The phase is the only evidence: the cache may still hold the Branch as it was before the
	// write, and its phase says nothing of the new state. ObservedGeneration tells that the
	// operator has looked at the spec this request wrote.
	generation := b.Generation
	wantPhase := phaseOfState(want)
	var failure error
	reached, err := waitFor(ctx, s.waits.State, func(ctx context.Context) (bool, error) {
		if visible, err := s.refresh(ctx, b); err != nil || !visible {
			return false, err
		}
		if b.Status.ObservedGeneration < generation {
			return false, nil
		}
		switch {
		case b.Status.Phase == wantPhase:
			return true, nil
		case want == v1alpha1.StateRunning && b.Status.Phase == v1alpha1.BranchFailed:
			failure = startFailed(b)
			return true, nil
		}
		return false, nil
	})
	switch {
	case err != nil:
		return err
	case failure != nil:
		return failure
	case reached:
		writeJSON(w, http.StatusOK, branchViewOf(b))
	default:
		writeJSON(w, http.StatusAccepted, branchViewOf(b))
	}
	return nil
}

// refresh reads b again from the cache, for a request that waits for the operator. A Branch the
// cache does not have is not necessarily gone: visible is false while the cluster still has it,
// and only when the cluster has not either is it a 404 (the cache may be behind).
func (s *server) refresh(ctx context.Context, b *v1alpha1.Branch) (visible bool, err error) {
	switch err := s.client.Get(ctx, client.ObjectKeyFromObject(b), b); {
	case apierrors.IsNotFound(err):
		var latest v1alpha1.Branch
		switch err := s.reader.Get(ctx, client.ObjectKeyFromObject(b), &latest); {
		case apierrors.IsNotFound(err):
			return false, notFound("branch %s was deleted", b.Name)
		case err != nil:
			return false, fromKube(err)
		}
		return false, nil
	case err != nil:
		return false, fromKube(err)
	}
	return true, nil
}

// setState writes spec.state of the branch, unless it has the state already, and returns the
// Branch as written. A conflict is retried on a fresh read, which also decides again whether the
// branch is still free of a restore: the operator wins races, and so does a restore request.
func (s *server) setState(ctx context.Context, ns, name string, want v1alpha1.DesiredState) (*v1alpha1.Branch, error) {
	var lastErr error
	for try := 0; try < stateUpdateTries; try++ {
		if try > 0 {
			// A moment for the cache to catch up with the write that conflicted.
			select {
			case <-time.After(pollInterval):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		b, err := s.branch(ctx, ns, name)
		if err != nil {
			return nil, err
		}
		if b.DeletionTimestamp != nil || b.Status.Phase == v1alpha1.BranchDeleting {
			return nil, conflict("branch %s is being deleted", name)
		}
		if err := dataFailed(b); err != nil {
			return nil, err
		}
		if restoreOpen(b) {
			return nil, conflict("branch %s has a rollback or reset under way", name)
		}
		if b.Spec.State == want {
			return b, nil
		}
		b.Spec.State = want
		switch err := s.client.Update(ctx, b); {
		case err == nil:
			return b, nil
		case apierrors.IsConflict(err):
			lastErr = err
		default:
			return nil, fromKube(err)
		}
	}
	return nil, conflict("branch %s was changed by someone else: %v", name, lastErr)
}

// startFailed is the problem for a branch that went to Failed while it was asked to start.
func startFailed(b *v1alpha1.Branch) error {
	if c := falseCondition(b, v1alpha1.ConditionReady, v1alpha1.ConditionDataReady); c != nil {
		return internal(fmt.Errorf("branch %s failed to start (%s): %s", b.Name, c.Reason, c.Message))
	}
	return internal(fmt.Errorf("branch %s failed to start", b.Name))
}
