package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/controller"
)

// maxLabelBytes is the longest label of a restore point (carry-over from plan 2).
const maxLabelBytes = 256

// restorePointView is a restore point as the API shows it.
type restorePointView struct {
	ID             string    `json:"id"`
	Branch         string    `json:"branch"`
	CreatedAt      time.Time `json:"createdAt"`
	Reason         string    `json:"reason"`
	Label          string    `json:"label"`
	BackupStartLSN string    `json:"backupStartLSN"`
}

func restorePointViewOf(rp agentapi.RestorePoint) restorePointView {
	return restorePointView(rp)
}

// createRestorePointRequest is the body of POST .../restore-points.
type createRestorePointRequest struct {
	Label string `json:"label,omitempty"`
}

func (s *server) restorePointEndpoints() []endpoint {
	const base = "/v1/projects/{projectId}/branches/{branch}/restore-points"
	return []endpoint{
		{route{http.MethodPost, base, "restorepoint.create"}, s.createRestorePoint},
		{route{http.MethodGet, base, "restorepoint.list"}, s.listRestorePoints},
		{route{http.MethodDelete, base + "/{restorePointId}", "restorepoint.delete"}, s.deleteRestorePoint},
	}
}

// agent returns the client of the agent of namespace ns, with the token of the project's
// internal-token Secret. The Secret is read uncached: the cache does not watch Secrets. A
// Secret that is missing or empty is the operator's to have made, so it is a 500.
func (s *server) agent(ctx context.Context, ns string) (AgentAPI, error) {
	var secret corev1.Secret
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: controller.InternalTokenSecret}, &secret); err != nil {
		return nil, internal(fmt.Errorf("secret %s: %w", controller.InternalTokenSecret, err))
	}
	token := string(secret.Data[controller.TokenKey])
	if token == "" {
		return nil, internal(fmt.Errorf("secret %s has no %s", controller.InternalTokenSecret, controller.TokenKey))
	}
	return s.agents.Agent(ns, token), nil
}

// restorePointOf finds restore point id among those of branch. An id of the wrong form, or
// one the agent does not list (or a branch the agent has no data for yet), is a 404.
func (s *server) restorePointOf(ctx context.Context, ns, branch, id string) (agentapi.RestorePoint, error) {
	if !agentapi.ValidRestorePointID(id) {
		return agentapi.RestorePoint{}, notFound("no restore point %q", id)
	}
	a, err := s.agent(ctx, ns)
	if err != nil {
		return agentapi.RestorePoint{}, err
	}
	list, err := a.ListRestorePoints(ctx, branch)
	if err != nil {
		if agentclient.CodeOf(err) == agentapi.CodeNotFound {
			return agentapi.RestorePoint{}, notFound("branch %s has no restore point %q", branch, id)
		}
		return agentapi.RestorePoint{}, fromAgent(err)
	}
	for _, rp := range list {
		if rp.ID == id {
			return rp, nil
		}
	}
	return agentapi.RestorePoint{}, notFound("branch %s has no restore point %q", branch, id)
}

// restorePointBranch is the common start of the three endpoints: the project is ready and the
// branch is there. It returns the branch and the project's namespace.
func (s *server) restorePointBranch(r *http.Request) (*v1alpha1.Branch, string, error) {
	id, name := r.PathValue("projectId"), r.PathValue("branch")
	logAttrs(r.Context(), "projectId", id, "branch", name)
	if _, err := s.readyProject(r.Context(), id); err != nil {
		return nil, "", err
	}
	ns := controller.NamespacePrefix + id
	b, err := s.branch(r.Context(), ns, name)
	return b, ns, err
}

func (s *server) createRestorePoint(w http.ResponseWriter, r *http.Request) error {
	var req createRestorePointRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if len(req.Label) > maxLabelBytes {
		return badRequest("label is %d bytes, at most %d", len(req.Label), maxLabelBytes)
	}
	b, ns, err := s.restorePointBranch(r)
	if err != nil {
		return err
	}
	// A restore point is a backup-mode copy of the running database.
	if b.Status.Phase != v1alpha1.BranchRunning {
		return conflict("branch %s is %s; a restore point is made from a running branch", b.Name, branchPhase(b))
	}
	a, err := s.agent(r.Context(), ns)
	if err != nil {
		return err
	}
	rp, err := a.CreateRestorePoint(r.Context(), b.Name, agentapi.CreateRestorePointRequest{Label: req.Label, Reason: agentapi.ReasonAPI})
	if err != nil {
		return fromAgent(err)
	}
	writeJSON(w, http.StatusCreated, restorePointViewOf(rp))
	return nil
}

func (s *server) listRestorePoints(w http.ResponseWriter, r *http.Request) error {
	b, ns, err := s.restorePointBranch(r)
	if err != nil {
		return err
	}
	a, err := s.agent(r.Context(), ns)
	if err != nil {
		return err
	}
	list, err := a.ListRestorePoints(r.Context(), b.Name)
	// A branch whose data is not made yet has no restore points: not an error.
	if err != nil && agentclient.CodeOf(err) != agentapi.CodeNotFound {
		return fromAgent(err)
	}
	views := make([]restorePointView, len(list))
	for i, rp := range list {
		views[i] = restorePointViewOf(rp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"restorePoints": views})
	return nil
}

func (s *server) deleteRestorePoint(w http.ResponseWriter, r *http.Request) error {
	rpID := r.PathValue("restorePointId")
	if !agentapi.ValidRestorePointID(rpID) {
		return notFound("no restore point %q", rpID)
	}
	b, ns, err := s.restorePointBranch(r)
	if err != nil {
		return err
	}
	a, err := s.agent(r.Context(), ns)
	if err != nil {
		return err
	}
	if err := a.DeleteRestorePoint(r.Context(), b.Name, rpID); err != nil {
		return fromAgent(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
