// Package controller reconciles Projects and Branches (spec 7.1): it makes each project's
// secrets, network policies, quota, volume and agent pod, and each branch's data, pod and
// service, and drives starts, stops, rollbacks, resets and deletions through the volume agent
// and the branch supervisors.
package controller

import (
	"strings"

	"consonance/api/v1alpha1"
)

// Names of the objects in a project namespace (spec 4.1, 7.1).
const (
	ProjectName         = "project"        // the one Project of a project namespace
	NamespacePrefix     = "cns-"           // a project namespace is cns-<projectId>
	PVCName             = "project-volume" // the project volume
	AgentName           = "volume-agent"   // the agent pod and its service
	DBCredentialsSecret = "db-credentials" // app_owner's password, for main's bootstrap
	InternalTokenSecret = "internal-token" // the token of the agent and supervisor APIs
	QuotaName           = "project-quota"
	BranchFinalizer     = "consonance.io/branch-cleanup"
)

// Secret keys.
const (
	TokenKey    = "token"    // internal-token
	UsernameKey = "username" // db-credentials
	PasswordKey = "password" // db-credentials
	DatabaseKey = "database" // db-credentials
)

// The tenant role and database that main's supervisor creates (pgctl.TenantRole and
// pgctl.TenantDatabase, spec 4.5).
const (
	TenantRole     = "app_owner"
	TenantDatabase = "app"
)

// Labels and annotations.
const (
	LabelManagedBy     = "app.kubernetes.io/managed-by"
	ManagedBy          = "consonance-operator" // on every object the operator makes
	LabelComponent     = "app.kubernetes.io/component"
	ComponentAgent     = "volume-agent"
	ComponentBranch    = "branch"
	LabelBranch        = "consonance.io/branch"    // a branch pod or service: its branch
	LabelBackupOf      = "consonance.io/backup-of" // a backup branch: the branch rolled back
	AnnotationSpecHash = "consonance.io/spec-hash" // the agent pod: the hash of its container spec
)

// The system namespace and the pod labels the network policies admit from it (spec 8). The
// operator and API server deployments must carry these labels (plan 5).
const (
	SystemNamespace  = "consonance-system"
	LabelAppName     = "app.kubernetes.io/name"
	OperatorAppName  = "consonance-operator"
	APIServerAppName = "consonance-apiserver"
)

// BranchObjectName is the name of a branch's pod and service: br-<branch>.
func BranchObjectName(branch string) string { return "br-" + branch }

// ProjectID returns the project ID of a project namespace cns-<projectId>.
func ProjectID(namespace string) (string, bool) {
	id, ok := strings.CutPrefix(namespace, NamespacePrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// managedLabels are the labels of every object the operator makes.
func managedLabels(component string) map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelComponent: component}
}

// isMain reports whether b is the project's main branch.
func isMain(b *v1alpha1.Branch) bool { return b.Name == v1alpha1.MainBranch }
