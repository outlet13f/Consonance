package controller

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// desiredAPIServerRoleBinding lets the API server read the two secrets of a project, and nothing
// else in its namespace. The API server has no cluster-wide right to read secrets: each project
// namespace binds it to the ClusterRole consonance-apiserver-project, whose one rule names
// db-credentials and internal-token (config/rbac/apiserver/project-role.yaml).
//
// The operator does hold `secrets get` cluster-wide today, so RBAC escalation prevention would
// allow this binding even without `bind`. Its role still has `bind` on that ClusterRole by name,
// so the binding stays allowed if the operator's own secret read is ever narrowed.
func desiredAPIServerRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: APIServerAccount, Namespace: ns, Labels: managedLabels("apiserver-access")},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: APIServerProjectRole},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: APIServerAccount, Namespace: SystemNamespace,
		}},
	}
}
