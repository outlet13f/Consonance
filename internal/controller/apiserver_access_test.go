package controller

import (
	"context"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAPIServerRoleBinding(t *testing.T) {
	rb := desiredAPIServerRoleBinding("cns-abc")
	if rb.Name != APIServerAccount || rb.Namespace != "cns-abc" {
		t.Errorf("name %s/%s", rb.Namespace, rb.Name)
	}
	if rb.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: APIServerProjectRole}) {
		t.Errorf("roleRef %+v", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0] != (rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: APIServerAccount, Namespace: SystemNamespace}) {
		t.Errorf("subjects %+v", rb.Subjects)
	}
	if rb.Labels[LabelManagedBy] != ManagedBy || rb.Labels[LabelComponent] != "apiserver-access" {
		t.Errorf("labels %v", rb.Labels)
	}
}

func TestProjectGivesTheAPIServerItsSecrets(t *testing.T) {
	ns, _ := createProject(t, nil)
	ctx := context.Background()
	key := client.ObjectKey{Namespace: ns, Name: APIServerAccount}
	var got rbacv1.RoleBinding
	eventually(t, 10*time.Second, func() string {
		if err := kenv.Client.Get(ctx, key, &got); err != nil {
			return err.Error()
		}
		return ""
	})
	want := desiredAPIServerRoleBinding(ns)
	if got.RoleRef != want.RoleRef || len(got.Subjects) != 1 || got.Subjects[0] != want.Subjects[0] {
		t.Errorf("roleRef %+v subjects %+v", got.RoleRef, got.Subjects)
	}
	if owner := controllerName(&got); owner != ProjectName {
		t.Errorf("owner %q, want the Project", owner)
	}

	// Drift is repaired: other subjects are put back, and a deleted binding is made again.
	got.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "intruder", Namespace: "elsewhere"}}
	if err := kenv.Client.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		var rb rbacv1.RoleBinding
		if err := kenv.Client.Get(ctx, key, &rb); err != nil {
			return err.Error()
		}
		if len(rb.Subjects) != 1 || rb.Subjects[0] != want.Subjects[0] {
			return "subjects not repaired"
		}
		return ""
	})
	if err := kenv.Client.Delete(ctx, &got); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		return errString(kenv.Client.Get(ctx, key, &rbacv1.RoleBinding{}))
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func controllerName(o client.Object) string {
	for _, r := range o.GetOwnerReferences() {
		if r.Controller != nil && *r.Controller {
			return r.Name
		}
	}
	return ""
}
