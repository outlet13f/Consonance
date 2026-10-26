package main_test

import (
	"context"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/internal/controller"
	"consonance/internal/testutil/kubetest"
)

// TestAPIServerCannotReadOtherSecrets holds the API server to the two secrets of a project
// namespace it is bound in: nothing cluster-wide, nothing else in the namespace, no list.
func TestAPIServerCannotReadOtherSecrets(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	kubeconfig := apiServerUser(t)
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	as, err := client.New(cfg, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}

	ns := env.Namespace(t)
	bound := env.Namespace(t) // the namespace the API server is bound in
	givesSecrets(t, bound)
	secret := func(ns, name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, StringData: map[string]string{"k": "v"}}
	}
	for _, s := range []*corev1.Secret{
		secret(bound, controller.DBCredentialsSecret), secret(bound, controller.InternalTokenSecret), secret(bound, "other"),
		secret(ns, controller.DBCredentialsSecret), secret("kube-system", "other"),
	} {
		if err := env.Client.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = env.Client.Delete(context.Background(), s) })
	}

	get := func(ns, name string) error {
		return as.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &corev1.Secret{})
	}
	// Where it is bound: the two secrets, and no more. The binding takes a moment to reach the
	// authorizer's cache.
	deadline := time.Now().Add(10 * time.Second)
	for get(bound, controller.DBCredentialsSecret) != nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	for _, name := range []string{controller.DBCredentialsSecret, controller.InternalTokenSecret} {
		if err := get(bound, name); err != nil {
			t.Errorf("reading %s where bound: %v", name, err)
		}
	}
	forbidden := map[string]error{
		"another secret where bound":             get(bound, "other"),
		"db-credentials of an unbound namespace": get(ns, controller.DBCredentialsSecret),
		"a secret of kube-system":                get("kube-system", "other"),
		"a list where bound":                     as.List(ctx, &corev1.SecretList{}, client.InNamespace(bound)),
		"a list of all secrets":                  as.List(ctx, &corev1.SecretList{}),
		"a create where bound":                   as.Create(ctx, secret(bound, "new")),
		"a delete where bound":                   as.Delete(ctx, secret(bound, controller.DBCredentialsSecret)),
	}
	for what, err := range forbidden {
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s: %v, want Forbidden", what, err)
		}
	}
}

// apiServerAccountUser is the user name of the API server's service account in a cluster.
const apiServerAccountUser = "system:serviceaccount:consonance-system:consonance-apiserver"

// TestNamespaceDeletePolicy applies config/policy/namespace-delete.yaml and deletes namespaces as
// the API server's service account (impersonated: the API server of envtest has no service
// accounts), which RBAC lets delete any namespace.
func TestNamespaceDeletePolicy(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()

	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	readYAMLDocs(t, "config/policy/namespace-delete.yaml", &policy, &binding)
	for _, o := range []client.Object{&policy, &binding} {
		if err := env.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = env.Client.Delete(context.Background(), o) })
	}
	role := installAPIServerRoles(t)
	bindClusterRole(t, role, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: controller.APIServerAccount, Namespace: controller.SystemNamespace})

	icfg := rest.CopyConfig(env.Config)
	icfg.Impersonate = rest.ImpersonationConfig{UserName: apiServerAccountUser}
	sa, err := client.New(icfg, client.Options{Scheme: env.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	newNS := func(labels map[string]string) *corev1.Namespace {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "policy-", Labels: labels}}
		if err := env.Client.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
		return ns
	}
	mine := map[string]string{controller.LabelManagedBy: "consonance-apiserver"}
	plain, operators, made := newNS(nil), newNS(map[string]string{controller.LabelManagedBy: controller.ManagedBy}), newNS(mine)

	// The policy and its binding are taken up by the API server's policy controller a moment
	// after they are made: until then a delete goes through. A dry run shows when it does not.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := sa.Delete(ctx, plain, client.DryRunAll)
		if apierrors.IsForbidden(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the policy does not deny after 30s: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	for what, ns := range map[string]*corev1.Namespace{"without the label": plain, "with the operator's label": operators} {
		err := sa.Delete(ctx, ns)
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "consonance-apiserver-namespace-delete") {
			t.Errorf("deleting a namespace %s: %v, want Forbidden by the policy", what, err)
		}
		var got corev1.Namespace
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(ns), &got); err != nil || got.DeletionTimestamp != nil {
			t.Errorf("the namespace %s: %v %v", what, got.DeletionTimestamp, err)
		}
	}
	if err := sa.Delete(ctx, made); err != nil {
		t.Errorf("deleting a namespace the API server made: %v", err)
	}
	var got corev1.Namespace
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(made), &got); err != nil || got.DeletionTimestamp == nil {
		t.Errorf("the namespace it made: %v %v", got.DeletionTimestamp, err)
	}
	// Anyone else is not held to the label.
	if err := env.Client.Delete(ctx, plain); err != nil {
		t.Errorf("deleting a namespace without the label as the administrator: %v", err)
	}
}
