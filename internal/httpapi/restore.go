package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/controller"
)

// restoreUpdateTries is how often spec.restore is written when the write conflicts, as for
// spec.state.
const restoreUpdateTries = stateUpdateTries

// rollbackRequest is the body of POST .../rollback.
type rollbackRequest struct {
	RestorePointID string `json:"restorePointId"`
}

// restoreResult is the answer to a rollback or reset: 200 with the backup branch of a rollback
// or the restore point of a reset, 202 with the request id to follow.
type restoreResult struct {
	Branch         branchView `json:"branch"`
	RequestID      string     `json:"requestId,omitempty"`
	BackupBranch   string     `json:"backupBranch,omitempty"`
	RestorePointID string     `json:"restorePointId,omitempty"`
}

func (s *server) restoreEndpoints() []endpoint {
	const base = "/v1/projects/{projectId}/branches/{branch}"
	return []endpoint{
		{route{http.MethodPost, base + "/reset", "branch.reset"}, s.resetBranch},
		{route{http.MethodPost, base + "/rollback", "branch.rollback"}, s.rollbackBranch},
	}
}

// restoreKind is what a request asks for.
type restoreKind struct {
	source v1alpha1.BranchSource
	reset  bool
}

func (s *server) resetBranch(w http.ResponseWriter, r *http.Request) error {
	return s.restore(w, r, restoreKind{source: v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest}, reset: true})
}

func (s *server) rollbackBranch(w http.ResponseWriter, r *http.Request) error {
	var req rollbackRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if !agentapi.ValidRestorePointID(req.RestorePointID) {
		return badRequest("restorePointId %q is not the form rp-<yyyymmddThhmmssZ>-<4 characters>", req.RestorePointID)
	}
	return s.restore(w, r, restoreKind{source: v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: req.RestorePointID}})
}

// restore asks the operator for a rollback or reset of the branch and waits for it: 200 when
// lastRestore of this request is Done and the phase is no longer Restoring, 500 when it is
// Failed, 202 when the wait is over first.
func (s *server) restore(w http.ResponseWriter, r *http.Request, kind restoreKind) error {
	ctx := r.Context()
	id, name := r.PathValue("projectId"), r.PathValue("branch")
	logAttrs(ctx, "projectId", id, "branch", name)
	p, err := s.readyProject(ctx, id)
	if err != nil {
		return err
	}
	ns := controller.NamespacePrefix + id
	reqID := requestIDOf(ctx)

	b, source, err := s.requestRestore(ctx, p, ns, name, reqID, kind)
	if err != nil {
		return err
	}

	// Only the lastRestore of this request counts: a read that is behind the write then delays
	// the answer and cannot make it wrong. The operator records the end while the phase is still
	// Restoring and moves it on right after, so the wait is also for that: the answer shows the
	// branch as it settled.
	var failed bool
	done, err := waitFor(ctx, s.waits.Restore, func(ctx context.Context) (bool, error) {
		if visible, err := s.refresh(ctx, b); err != nil || !visible {
			return false, err
		}
		lr := b.Status.LastRestore
		if lr == nil || lr.RequestID != reqID || b.Status.Phase == v1alpha1.BranchRestoring {
			return false, nil
		}
		switch lr.Phase {
		case v1alpha1.RestoreDone:
			return true, nil
		case v1alpha1.RestoreFailed:
			failed = true
			return true, nil
		}
		return false, nil
	})
	switch {
	case err != nil:
		return err
	case !done:
		writeJSON(w, http.StatusAccepted, restoreResult{Branch: branchViewOf(b), RequestID: reqID})
		return nil
	case failed:
		msg := b.Status.LastRestore.Message
		if msg == "" {
			msg = "the restore of branch " + name + " failed"
		}
		return internal(errors.New(msg))
	}
	res := restoreResult{Branch: branchViewOf(b)}
	// What the request was decides the body, and for a repeat that is the earlier request.
	if source.Type == v1alpha1.SourceRestorePoint {
		res.BackupBranch = b.Status.LastRestore.BackupBranch
	} else {
		res.RestorePointID = b.Status.LastRestore.RestorePointID
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

// requestRestore writes spec.restore of the branch, after the checks, and returns the Branch as
// written with the source of the request. A request whose id is the one of the last restore, or
// of the open one in the spec, is a repeat of that request when it asks for the same source:
// nothing is checked or written, the caller waits for the result, and the source returned is the
// earlier one. The same id for another source (another restore point, or a reset instead of a
// rollback) is a different request and a 409. A conflict on the write is retried on a fresh
// read, which decides everything again: the operator wins races, and so does another restore
// request.
func (s *server) requestRestore(ctx context.Context, p *v1alpha1.Project, ns, name, reqID string, kind restoreKind) (*v1alpha1.Branch, v1alpha1.BranchSource, error) {
	var lastErr error
	for try := 0; try < restoreUpdateTries; try++ {
		if try > 0 {
			// A moment for the cache to catch up with the write that conflicted.
			select {
			case <-time.After(pollInterval):
			case <-ctx.Done():
				return nil, kind.source, ctx.Err()
			}
		}
		b, err := s.branch(ctx, ns, name)
		if err != nil {
			return nil, kind.source, err
		}
		if earlier, ok := earlierSource(b, reqID); ok {
			// A source the operator did not record cannot be told apart: it is taken as the same.
			if earlier.Type != "" && earlier != kind.source {
				return nil, kind.source, conflict("X-Request-Id %s was used for a different request", reqID)
			}
			if earlier.Type == "" {
				earlier = kind.source
			}
			return b, earlier, nil
		}
		if err := s.checkRestore(ctx, p, ns, b, kind); err != nil {
			return nil, kind.source, err
		}
		b.Spec.Restore = &v1alpha1.RestoreRequest{RequestID: reqID, Source: kind.source}
		switch err := s.client.Update(ctx, b); {
		case err == nil:
			return b, kind.source, nil
		case apierrors.IsConflict(err):
			lastErr = err
		default:
			return nil, kind.source, fromKube(err)
		}
	}
	return nil, kind.source, conflict("branch %s was changed by someone else: %v", name, lastErr)
}

// earlierSource is the source of the restore of b that has the request id, from its status or,
// while the operator has not started it, from its spec. The type is empty when the status does
// not say.
func earlierSource(b *v1alpha1.Branch, reqID string) (v1alpha1.BranchSource, bool) {
	if lr := b.Status.LastRestore; lr != nil && lr.RequestID == reqID {
		if lr.Source.Type == "" && b.Spec.Restore != nil && b.Spec.Restore.RequestID == reqID {
			return b.Spec.Restore.Source, true
		}
		return lr.Source, true
	}
	if req := b.Spec.Restore; req != nil && req.RequestID == reqID {
		return req.Source, true
	}
	return v1alpha1.BranchSource{}, false
}

// checkRestore is what has to hold before a restore of b is asked for, so the request is
// refused here and not left to fail in the operator.
func (s *server) checkRestore(ctx context.Context, p *v1alpha1.Project, ns string, b *v1alpha1.Branch, kind restoreKind) error {
	if kind.reset && b.Name == v1alpha1.MainBranch {
		return conflict("branch main has no parent to reset to (spec chapter 5)")
	}
	if b.DeletionTimestamp != nil || b.Status.Phase == v1alpha1.BranchDeleting {
		return conflict("branch %s is being deleted", b.Name)
	}
	if err := dataFailed(b); err != nil {
		return err
	}
	if restoreOpen(b) {
		return conflict("a rollback or reset is in progress")
	}
	if kind.reset {
		// A reset copies the parent's running database.
		parent, err := s.branch(ctx, ns, b.Spec.Parent)
		var ae *apiError
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
			return conflict("parent %s of branch %s does not exist", b.Spec.Parent, b.Name)
		case err != nil:
			return err
		case parent.Status.Phase != v1alpha1.BranchRunning:
			return conflict("parent %s is %s; a reset copies a running parent", parent.Name, branchPhase(parent))
		}
		return nil
	}

	// The restore point has to be the branch's own; main's is not one to roll dev back to.
	if _, err := s.restorePointOf(ctx, ns, b.Name, kind.source.RestorePointID); err != nil {
		return err
	}
	_, backups, err := s.countBranches(ctx, ns)
	if err != nil {
		return err
	}
	if limit := int(p.Spec.Limits.MaxBranches); backups >= limit {
		return &apiError{
			Status: http.StatusTooManyRequests, Type: typeRateLimited, RetryAfter: branchRetryAfter,
			Detail: fmt.Sprintf("the project has %d of %d backup branches; delete an old backup branch (<name>-before-<time>) to make room for the one this rollback makes", backups, limit),
		}
	}
	return nil
}
