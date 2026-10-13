package v1alpha1_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/testutil/kubetest"
)

var env *kubetest.Env

func TestMain(m *testing.M) { os.Exit(kubetest.Main(m, &env)) }

func TestProjectGetsTheSpecDefaults(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	ns := env.Namespace(t)
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: ns},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "my-app"},
	}
	if err := env.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Project
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatal(err)
	}
	s := got.Spec
	checks := []struct {
		name      string
		got, want string
	}{
		{"postgresVersion", s.PostgresVersion, "17"},
		{"storage.size", s.Storage.Size.String(), "10Gi"},
		{"defaultCompute.cpu", s.DefaultCompute.CPU.String(), "500m"},
		{"defaultCompute.memory", s.DefaultCompute.Memory.String(), "1Gi"},
		{"limits.maxBranches", itoa(s.Limits.MaxBranches), "10"},
		{"limits.minCopyInterval", s.Limits.MinCopyInterval.Duration.String(), (5 * time.Second).String()},
		{"restorePoints.maxPerBranch", itoa(s.RestorePoints.MaxPerBranch), "20"},
		{"restorePoints.maxAge", s.RestorePoints.MaxAge.Duration.String(), (24 * time.Hour).String()},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestProjectKeepsGivenValues(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	ns := env.Namespace(t)
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: ns},
		Spec: v1alpha1.ProjectSpec{
			DisplayName: "my-app",
			Storage:     v1alpha1.StorageSpec{Size: resource.MustParse("20Gi"), StorageClassName: "consonance-xfs"},
			Limits:      v1alpha1.ProjectLimits{MaxBranches: 3},
		},
	}
	if err := env.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Project
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(p), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Storage.Size.String() != "20Gi" || got.Spec.Storage.StorageClassName != "consonance-xfs" ||
		got.Spec.Limits.MaxBranches != 3 || got.Spec.Limits.MinCopyInterval.Duration != 5*time.Second {
		t.Fatalf("spec = %+v", got.Spec)
	}
}

func TestProjectMustBeNamedProject(t *testing.T) {
	kubetest.Need(t, env)
	ns := env.Namespace(t)
	p := &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: ns},
		Spec:       v1alpha1.ProjectSpec{DisplayName: "my-app"},
	}
	if err := env.Client.Create(context.Background(), p); !apierrors.IsInvalid(err) {
		t.Fatalf("create Project named other: %v, want Invalid", err)
	}
}

func TestBranchGetsTheSpecDefaults(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	ns := env.Namespace(t)
	b := branch(ns, "dev", "main")
	if err := env.Client.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Branch
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(b), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Source.Type != v1alpha1.SourceParentLatest || got.Spec.State != v1alpha1.StateRunning || got.Spec.Protected {
		t.Fatalf("spec = %+v", got.Spec)
	}
}

func TestBranchValidation(t *testing.T) {
	kubetest.Need(t, env)
	ns := env.Namespace(t)
	rp := v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-ab12"}
	cases := []struct {
		name   string
		branch *v1alpha1.Branch
		valid  bool
	}{
		{"main without a parent", branch(ns, "main", ""), true},
		{"main with a parent", branch(ns, "main", "dev"), false},
		{"another branch without a parent", branch(ns, "dev", ""), false},
		{"40 characters", branch(ns, strings.Repeat("a", 40), "main"), true},
		{"41 characters", branch(ns, strings.Repeat("a", 41), "main"), false},
		{"a dot", branch(ns, "a.b", "main"), false},
		{"a parent with a capital", branch(ns, "cap", "Main"), false},
		{"from a restore point", with(branch(ns, "from-rp", "main"), func(b *v1alpha1.Branch) { b.Spec.Source = rp }), true},
		{"a restore point source without an id", with(branch(ns, "no-id", "main"), func(b *v1alpha1.Branch) {
			b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint}
		}), false},
		{"a malformed restore point id", with(branch(ns, "bad-id", "main"), func(b *v1alpha1.Branch) {
			b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-1"}
		}), false},
		{"an unknown state", with(branch(ns, "paused", "main"), func(b *v1alpha1.Branch) { b.Spec.State = "Paused" }), false},
		{"a restore without a requestId", with(branch(ns, "no-request", "main"), func(b *v1alpha1.Branch) {
			b.Spec.Restore = &v1alpha1.RestoreRequest{Source: rp}
		}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := env.Client.Create(context.Background(), c.branch)
			if c.valid && err != nil {
				t.Fatalf("create: %v, want success", err)
			}
			if !c.valid && !apierrors.IsInvalid(err) {
				t.Fatalf("create: %v, want Invalid", err)
			}
		})
	}
}

func TestBranchParentAndSourceAreImmutable(t *testing.T) {
	kubetest.Need(t, env)
	ctx := context.Background()
	ns := env.Namespace(t)
	b := branch(ns, "dev", "main")
	if err := env.Client.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	update := func(change func(*v1alpha1.Branch)) error {
		var cur v1alpha1.Branch
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(b), &cur); err != nil {
			t.Fatal(err)
		}
		change(&cur)
		return env.Client.Update(ctx, &cur)
	}
	if err := update(func(b *v1alpha1.Branch) { b.Spec.Parent = "other" }); !apierrors.IsInvalid(err) {
		t.Fatalf("change parent: %v, want Invalid", err)
	}
	if err := update(func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-ab12"}
	}); !apierrors.IsInvalid(err) {
		t.Fatalf("change source: %v, want Invalid", err)
	}
	if err := update(func(b *v1alpha1.Branch) { b.Spec.State = v1alpha1.StateStopped }); err != nil {
		t.Fatalf("change state: %v", err)
	}
}

func branch(ns, name, parent string) *v1alpha1.Branch {
	return &v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.BranchSpec{Parent: parent},
	}
}

func with(b *v1alpha1.Branch, change func(*v1alpha1.Branch)) *v1alpha1.Branch {
	change(b)
	return b
}

func itoa(n int32) string { return resource.NewQuantity(int64(n), resource.DecimalSI).String() }
