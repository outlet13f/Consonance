package httpapi

import (
	"context"
	"crypto/rand"
	"net/http"
	"sort"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
)

const (
	maxDisplayNameLen = 100
	projectIDLen      = 12
	// apiServerManager is the managed-by label of what the API server makes (decision 6); the
	// operator's own objects carry controller.ManagedBy.
	apiServerManager = "consonance-apiserver"
	labelPodSecurity = "pod-security.kubernetes.io/enforce"
)

var minStorageSize = resource.MustParse("1Gi")

// projectView is a project as the API shows it.
type projectView struct {
	ID             string      `json:"id"`
	DisplayName    string      `json:"displayName"`
	Phase          string      `json:"phase"`
	Reason         string      `json:"reason,omitempty"`  // why Ready is false
	Message        string      `json:"message,omitempty"` // and what the operator says about it
	Storage        storageView `json:"storage"`
	DefaultCompute computeView `json:"defaultCompute"`
	Limits         limitsView  `json:"limits"`
	CreatedAt      time.Time   `json:"createdAt"`
}

type storageView struct {
	Size             string `json:"size"`
	StorageClassName string `json:"storageClassName"`
	CapacityBytes    int64  `json:"capacityBytes"`
	UsedBytes        int64  `json:"usedBytes"`
}

type computeView struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type limitsView struct {
	MaxBranches     int32  `json:"maxBranches"`
	MinCopyInterval string `json:"minCopyInterval"`
}

type createProjectRequest struct {
	DisplayName string          `json:"displayName"`
	StorageSize string          `json:"storageSize,omitempty"`
	Compute     *computeRequest `json:"compute,omitempty"`
}

type computeRequest struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

func (s *server) projectEndpoints() []endpoint {
	return []endpoint{
		{route{http.MethodPost, "/v1/projects", "project.create"}, s.createProject},
		{route{http.MethodGet, "/v1/projects", "project.list"}, s.listProjects},
		{route{http.MethodGet, "/v1/projects/{projectId}", "project.get"}, s.getProject},
		{route{http.MethodDelete, "/v1/projects/{projectId}", "project.delete"}, s.deleteProject},
	}
}

// phaseOf is the phase of p; a project the operator has not looked at yet is Provisioning.
func phaseOf(p *v1alpha1.Project) string {
	if p.Status.Phase == "" {
		return string(v1alpha1.ProjectProvisioning)
	}
	return string(p.Status.Phase)
}

// readyCondition is the Ready condition of p, or nil.
func readyCondition(p *v1alpha1.Project) *metav1.Condition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == v1alpha1.ConditionReady {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

func viewOf(id string, p *v1alpha1.Project) projectView {
	v := projectView{
		ID:          id,
		DisplayName: p.Spec.DisplayName,
		Phase:       phaseOf(p),
		Storage: storageView{
			Size:             p.Spec.Storage.Size.String(),
			StorageClassName: p.Spec.Storage.StorageClassName,
			CapacityBytes:    p.Status.Volume.CapacityBytes,
			UsedBytes:        p.Status.Volume.UsedBytes,
		},
		DefaultCompute: computeView{CPU: p.Spec.DefaultCompute.CPU.String(), Memory: p.Spec.DefaultCompute.Memory.String()},
		Limits:         limitsView{MaxBranches: p.Spec.Limits.MaxBranches, MinCopyInterval: p.Spec.Limits.MinCopyInterval.Duration.String()},
		CreatedAt:      p.CreationTimestamp.Time,
	}
	if c := readyCondition(p); c != nil && c.Status == metav1.ConditionFalse {
		v.Reason, v.Message = c.Reason, c.Message
	}
	return v
}

// project reads the Project of project id. An id of the wrong form is a 404 without asking the
// cluster, so a request cannot make the API server look up arbitrary names.
func (s *server) project(ctx context.Context, id string) (*v1alpha1.Project, error) {
	if !validProjectID(id) {
		return nil, notFound("no project %q", id)
	}
	var p v1alpha1.Project
	err := s.client.Get(ctx, client.ObjectKey{Namespace: controller.NamespacePrefix + id, Name: controller.ProjectName}, &p)
	switch {
	case apierrors.IsNotFound(err):
		return nil, notFound("no project %q", id)
	case err != nil:
		return nil, fromKube(err)
	}
	return &p, nil
}

// readyProject is project for the requests that need a working project: any other phase is a
// 409 that says why.
func (s *server) readyProject(ctx context.Context, id string) (*v1alpha1.Project, error) {
	p, err := s.project(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Status.Phase == v1alpha1.ProjectReady {
		return p, nil
	}
	msg := ""
	if c := readyCondition(p); c != nil {
		msg = c.Message
	}
	return nil, conflict("project %s is %s: %s", id, phaseOf(p), msg)
}

// newProjectID is 12 random lowercase letters and digits. Bytes that would favour some
// characters (252 and up) are dropped, so every character is equally likely.
func newProjectID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	id := make([]byte, 0, projectIDLen)
	buf := make([]byte, 2*projectIDLen)
	for len(id) < projectIDLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < 252 && len(id) < projectIDLen {
				id = append(id, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(id), nil
}

// positiveQuantity parses a CPU or memory amount that must be above zero.
func positiveQuantity(field, s string) (resource.Quantity, error) {
	if s == "" {
		return resource.Quantity{}, badRequest("compute needs both cpu and memory; %s is missing", field)
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return q, badRequest("compute.%s %q is not a quantity: %v", field, s, err)
	}
	if q.Sign() <= 0 {
		return q, badRequest("compute.%s must be above zero, not %q", field, s)
	}
	return q, nil
}

// spec checks the request and returns the Project spec it asks for. Fields left out stay empty
// for the CRD's defaults (spec 14).
func (req *createProjectRequest) spec(storageClass string) (v1alpha1.ProjectSpec, error) {
	var spec v1alpha1.ProjectSpec
	if n := utf8.RuneCountInString(req.DisplayName); n < 1 || n > maxDisplayNameLen {
		return spec, badRequest("displayName must be 1 to %d characters, not %d", maxDisplayNameLen, n)
	}
	spec.DisplayName = req.DisplayName
	spec.Storage.StorageClassName = storageClass
	if req.StorageSize != "" {
		q, err := resource.ParseQuantity(req.StorageSize)
		if err != nil {
			return spec, badRequest("storageSize %q is not a quantity: %v", req.StorageSize, err)
		}
		if q.Cmp(minStorageSize) < 0 {
			return spec, badRequest("storageSize must be at least 1Gi, not %q", req.StorageSize)
		}
		spec.Storage.Size = q
	}
	if req.Compute != nil {
		cpu, err := positiveQuantity("cpu", req.Compute.CPU)
		if err != nil {
			return spec, err
		}
		mem, err := positiveQuantity("memory", req.Compute.Memory)
		if err != nil {
			return spec, err
		}
		spec.DefaultCompute = v1alpha1.ComputeSpec{CPU: cpu, Memory: mem}
	}
	return spec, nil
}

// createNamespace makes the namespace of a new project under a fresh id. A namespace that
// exists already (a collision of random ids, which is unlikely) gets one more try.
func (s *server) createNamespace(ctx context.Context) (string, error) {
	for attempt := 0; ; attempt++ {
		id, err := newProjectID()
		if err != nil {
			return "", internal(err)
		}
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: controller.NamespacePrefix + id,
			Labels: map[string]string{
				controller.LabelManagedBy: apiServerManager,
				labelPodSecurity:          "restricted",
			},
		}}
		err = s.client.Create(ctx, ns)
		switch {
		case err == nil:
			return id, nil
		case apierrors.IsAlreadyExists(err) && attempt == 0:
		case apierrors.IsAlreadyExists(err):
			return "", internal(err)
		default:
			return "", fromKube(err)
		}
	}
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) error {
	var req createProjectRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	spec, err := req.spec(s.storageClass)
	if err != nil {
		return err
	}
	ctx := r.Context()
	id, err := s.createNamespace(ctx)
	if err != nil {
		return err
	}
	logAttrs(ctx, "projectId", id)
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Namespace: controller.NamespacePrefix + id, Name: controller.ProjectName},
		Spec:       spec,
	}
	if err := s.client.Create(ctx, p); err != nil {
		// A namespace without its Project would stay for ever; take it back. This runs even if
		// the caller has gone, hence a context of its own.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if derr := s.client.Delete(cleanup, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controller.NamespacePrefix + id}}); derr != nil && !apierrors.IsNotFound(derr) {
			logAttrs(ctx, "cleanupError", derr.Error())
		}
		return fromKube(err)
	}
	writeJSON(w, http.StatusAccepted, viewOf(id, p))
	return nil
}

func (s *server) listProjects(w http.ResponseWriter, r *http.Request) error {
	var list v1alpha1.ProjectList
	if err := s.client.List(r.Context(), &list); err != nil {
		return fromKube(err)
	}
	views := []projectView{}
	for i := range list.Items {
		p := &list.Items[i]
		if id, ok := controller.ProjectID(p.Namespace); ok {
			views = append(views, viewOf(id, p))
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"projects": views})
	return nil
}

func (s *server) getProject(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("projectId")
	logAttrs(r.Context(), "projectId", id)
	p, err := s.project(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, viewOf(id, p))
	return nil
}

// deleteProject deletes the namespace; Kubernetes removes what is in it, volume included, and
// the answer does not wait for that. A namespace that is going already is not an error.
func (s *server) deleteProject(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("projectId")
	logAttrs(r.Context(), "projectId", id)
	if !validProjectID(id) {
		return notFound("no project %q", id)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controller.NamespacePrefix + id}}
	err := s.client.Delete(r.Context(), ns, client.PropagationPolicy(metav1.DeletePropagationBackground))
	switch {
	case apierrors.IsNotFound(err):
		return notFound("no project %q", id)
	case err != nil:
		return fromKube(err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "phase": "Deleting"})
	return nil
}
