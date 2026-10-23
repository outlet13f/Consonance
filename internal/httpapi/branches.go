package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
)

// branchRetryAfter is the Retry-After of a project that is at its branch limit: a branch has to
// be deleted first, so there is no short time to wait for.
const branchRetryAfter = 60 * time.Second

// branchView is a branch as the API shows it.
type branchView struct {
	Name        string           `json:"name"`
	Parent      string           `json:"parent"`
	Phase       string           `json:"phase"`
	State       string           `json:"state"`
	Protected   bool             `json:"protected"`
	BackupOf    string           `json:"backupOf,omitempty"` // set on the backup branch of a rollback
	Reason      string           `json:"reason,omitempty"`   // why a Failed branch failed
	Message     string           `json:"message,omitempty"`
	Endpoint    *endpointView    `json:"endpoint,omitempty"`
	CreatedFrom *createdFromView `json:"createdFrom,omitempty"`
	LastRestore *restoreView     `json:"lastRestore,omitempty"`
	DataReadyAt *time.Time       `json:"dataReadyAt,omitempty"`
	RunningAt   *time.Time       `json:"runningAt,omitempty"`
	CreatedAt   time.Time        `json:"createdAt"`
}

type endpointView struct {
	Host string `json:"host"`
	Port int32  `json:"port"`
}

type createdFromView struct {
	Branch         string `json:"branch,omitempty"`
	RestorePointID string `json:"restorePointId,omitempty"`
}

type restoreView struct {
	RequestID      string `json:"requestId"`
	Phase          string `json:"phase"`
	BackupBranch   string `json:"backupBranch,omitempty"`
	RestorePointID string `json:"restorePointId,omitempty"`
	Message        string `json:"message,omitempty"`
}

// createBranchRequest is the body of POST .../branches.
type createBranchRequest struct {
	Name           string `json:"name"`
	Parent         string `json:"parent"`
	RestorePointID string `json:"restorePointId,omitempty"`
	State          string `json:"state,omitempty"`
}

func (s *server) branchEndpoints() []endpoint {
	const base = "/v1/projects/{projectId}/branches"
	return []endpoint{
		{route{http.MethodPost, base, "branch.create"}, s.createBranch},
		{route{http.MethodGet, base, "branch.list"}, s.listBranches},
		{route{http.MethodGet, base + "/{branch}", "branch.get"}, s.getBranch},
		{route{http.MethodDelete, base + "/{branch}", "branch.delete"}, s.deleteBranch},
	}
}

// isBackup reports whether b is the backup branch of a rollback.
func isBackup(b *v1alpha1.Branch) bool {
	_, ok := b.Labels[controller.LabelBackupOf]
	return ok
}

// falseCondition is the first of the types that is false, in the order given.
func falseCondition(b *v1alpha1.Branch, types ...string) *metav1.Condition {
	for _, t := range types {
		for i := range b.Status.Conditions {
			if c := &b.Status.Conditions[i]; c.Type == t && c.Status == metav1.ConditionFalse {
				return c
			}
		}
	}
	return nil
}

func branchPhase(b *v1alpha1.Branch) string {
	if b.Status.Phase == "" {
		return string(v1alpha1.BranchCreating)
	}
	return string(b.Status.Phase)
}

func branchViewOf(b *v1alpha1.Branch) branchView {
	v := branchView{
		Name:      b.Name,
		Parent:    b.Spec.Parent,
		Phase:     branchPhase(b),
		State:     string(b.Spec.State),
		Protected: b.Spec.Protected,
		BackupOf:  b.Labels[controller.LabelBackupOf],
		CreatedAt: b.CreationTimestamp.Time,
	}
	if b.Status.Phase == v1alpha1.BranchFailed {
		if c := falseCondition(b, v1alpha1.ConditionDataReady, v1alpha1.ConditionReady); c != nil {
			v.Reason, v.Message = c.Reason, c.Message
		}
	}
	if e := b.Status.Endpoint; e != nil {
		v.Endpoint = &endpointView{Host: e.Host, Port: e.Port}
	}
	if f := b.Status.CreatedFrom; f != nil {
		v.CreatedFrom = &createdFromView{Branch: f.Branch, RestorePointID: f.RestorePointID}
	}
	if r := b.Status.LastRestore; r != nil {
		v.LastRestore = &restoreView{
			RequestID: r.RequestID, Phase: string(r.Phase), BackupBranch: r.BackupBranch,
			RestorePointID: r.RestorePointID, Message: r.Message,
		}
	}
	if t := b.Status.Timings.DataReadyAt; t != nil {
		v.DataReadyAt = &t.Time
	}
	if t := b.Status.Timings.RunningAt; t != nil {
		v.RunningAt = &t.Time
	}
	return v
}

// validBranchRef checks a branch name that names an existing branch, which may be a backup
// branch: only the form of a name, as the CRD has it.
func validBranchRef(name string) bool {
	return name != "" && len(name) <= maxBranchNameLen && dns1123Label.MatchString(name)
}

// branch reads the Branch name of namespace ns. A name of the wrong form is a 404 without
// asking the cluster.
func (s *server) branch(ctx context.Context, ns, name string) (*v1alpha1.Branch, error) {
	if !validBranchRef(name) {
		return nil, notFound("no branch %q", name)
	}
	var b v1alpha1.Branch
	err := s.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &b)
	switch {
	case apierrors.IsNotFound(err):
		return nil, notFound("no branch %q", name)
	case err != nil:
		return nil, fromKube(err)
	}
	return &b, nil
}

// countBranches counts the branches of a project: those a user made (main included) and the
// backup branches of rollbacks, which have their own limit (decision 1).
func (s *server) countBranches(ctx context.Context, ns string) (user, backups int, err error) {
	var list v1alpha1.BranchList
	if err := s.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return 0, 0, fromKube(err)
	}
	for i := range list.Items {
		if isBackup(&list.Items[i]) {
			backups++
		} else {
			user++
		}
	}
	return user, backups, nil
}

// spec checks the request and returns the Branch spec it asks for, with the defaults filled in.
func (req *createBranchRequest) spec() (v1alpha1.BranchSpec, error) {
	spec := v1alpha1.BranchSpec{Parent: req.Parent, State: v1alpha1.StateRunning}
	if err := validBranchName(req.Name); err != nil {
		return spec, err
	}
	if spec.Parent == "" {
		spec.Parent = v1alpha1.MainBranch
	}
	if !validBranchRef(spec.Parent) {
		return spec, badRequest("parent %q is not the name of a branch", req.Parent)
	}
	switch req.State {
	case "", string(v1alpha1.StateRunning):
	case string(v1alpha1.StateStopped):
		spec.State = v1alpha1.StateStopped
	default:
		return spec, badRequest("state must be Running or Stopped, not %q", req.State)
	}
	spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest}
	if req.RestorePointID != "" {
		spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: req.RestorePointID}
	}
	return spec, nil
}

// permanentDataError is the problem for a branch whose data could not be made and will not be
// (spec 10). The branch stays, in phase Failed, for its owner to delete.
func permanentDataError(b *v1alpha1.Branch, c *metav1.Condition) error {
	switch c.Reason {
	case v1alpha1.ReasonSourceNotFound:
		return notFound("branch %s failed: %s", b.Name, c.Message)
	case v1alpha1.ReasonInsufficientStorage:
		return &apiError{Status: http.StatusInsufficientStorage, Type: typeInsufficientStorage, Detail: "branch " + b.Name + " failed: " + c.Message}
	}
	// ReflinkUnsupported and Invalid are about the project, not the request.
	return conflict("branch %s failed (%s): %s", b.Name, c.Reason, c.Message)
}

func (s *server) createBranch(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id := r.PathValue("projectId")
	logAttrs(ctx, "projectId", id)
	var req createBranchRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	spec, err := req.spec()
	if err != nil {
		return err
	}
	logAttrs(ctx, "branch", req.Name)
	if req.Name == v1alpha1.MainBranch {
		return conflict("branch %s exists already", req.Name)
	}
	p, err := s.readyProject(ctx, id)
	if err != nil {
		return err
	}
	ns := controller.NamespacePrefix + id

	var existing v1alpha1.Branch
	switch err := s.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: req.Name}, &existing); {
	case err == nil:
		return conflict("branch %s exists already", req.Name)
	case !apierrors.IsNotFound(err):
		return fromKube(err)
	}

	parent, err := s.branch(ctx, ns, spec.Parent)
	if ae, ok := err.(*apiError); ok && ae.Status == http.StatusNotFound {
		return notFound("no parent branch %q", spec.Parent)
	} else if err != nil {
		return err
	}
	// A copy of the parent's current data is made from its running database; a restore point
	// does not need the parent to run.
	if spec.Source.Type == v1alpha1.SourceParentLatest && parent.Status.Phase != v1alpha1.BranchRunning {
		return conflict("parent %s is %s", parent.Name, branchPhase(parent))
	}

	user, _, err := s.countBranches(ctx, ns)
	if err != nil {
		return err
	}
	if limit := int(p.Spec.Limits.MaxBranches); user >= limit {
		return &apiError{
			Status: http.StatusTooManyRequests, Type: typeRateLimited, RetryAfter: branchRetryAfter,
			Detail: fmt.Sprintf("the project has %d of %d branches", user, limit),
		}
	}

	b := &v1alpha1.Branch{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: req.Name}, Spec: spec}
	if err := s.client.Create(ctx, b); err != nil {
		return fromKube(err)
	}

	var failure error
	ready, err := waitFor(ctx, s.waits.Create, func(ctx context.Context) (bool, error) {
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
			return false, fromKube(err)
		}
		if b.Status.DataGeneration > 0 {
			return true, nil
		}
		if b.Status.Phase == v1alpha1.BranchFailed {
			if c := falseCondition(b, v1alpha1.ConditionDataReady); c != nil && v1alpha1.PermanentDataFailure(c.Reason) {
				failure = permanentDataError(b, c)
				return true, nil
			}
		}
		return false, nil
	})
	switch {
	case err != nil:
		return err
	case failure != nil:
		return failure
	case ready:
		writeJSON(w, http.StatusCreated, branchViewOf(b))
	default:
		writeJSON(w, http.StatusAccepted, branchViewOf(b))
	}
	return nil
}

func (s *server) listBranches(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("projectId")
	logAttrs(r.Context(), "projectId", id)
	if _, err := s.project(r.Context(), id); err != nil {
		return err
	}
	var list v1alpha1.BranchList
	if err := s.client.List(r.Context(), &list, client.InNamespace(controller.NamespacePrefix+id)); err != nil {
		return fromKube(err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	views := make([]branchView, len(list.Items))
	for i := range list.Items {
		views[i] = branchViewOf(&list.Items[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": views})
	return nil
}

func (s *server) getBranch(w http.ResponseWriter, r *http.Request) error {
	id, name := r.PathValue("projectId"), r.PathValue("branch")
	logAttrs(r.Context(), "projectId", id, "branch", name)
	if _, err := s.project(r.Context(), id); err != nil {
		return err
	}
	b, err := s.branch(r.Context(), controller.NamespacePrefix+id, name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, branchViewOf(b))
	return nil
}

// deleteBranch asks for the deletion and does not wait for the operator to finish it. main
// cannot be deleted: deleting the project is how it goes.
func (s *server) deleteBranch(w http.ResponseWriter, r *http.Request) error {
	id, name := r.PathValue("projectId"), r.PathValue("branch")
	logAttrs(r.Context(), "projectId", id, "branch", name)
	if _, err := s.project(r.Context(), id); err != nil {
		return err
	}
	b, err := s.branch(r.Context(), controller.NamespacePrefix+id, name)
	if err != nil {
		return err
	}
	if b.Name == v1alpha1.MainBranch {
		return conflict("branch main cannot be deleted; delete the project")
	}
	// A branch that is being deleted answers 202 too: Delete of it is no error.
	if err := s.client.Delete(r.Context(), b); err != nil && !apierrors.IsNotFound(err) {
		return fromKube(err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"name": name, "phase": "Deleting"})
	return nil
}
