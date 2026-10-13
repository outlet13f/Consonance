package controller

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
)

func deleteBranch(t *testing.T, ns, name string) {
	t.Helper()
	b := getBranch(t, ns, name)
	if err := kenv.Client.Delete(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
}

func waitGone(t *testing.T, ns, name string, obj client.Object) {
	t.Helper()
	eventually(t, 15*time.Second, func() string {
		err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, obj)
		if !apierrors.IsNotFound(err) {
			return fmt.Sprintf("%s still there: %v", name, err)
		}
		return ""
	})
}

func TestDeletingABranchRemovesItsPodAndData(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	b := waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if !slices.Contains(b.Finalizers, BranchFinalizer) {
		t.Fatalf("finalizers %q", b.Finalizers)
	}
	deleteBranch(t, ns, "dev")
	waitGone(t, ns, "dev", &v1alpha1.Branch{})
	waitGone(t, ns, "br-dev", &corev1.Pod{})
	waitGone(t, ns, "br-dev", &corev1.Service{})
	if _, _, ok := fp.generations("dev"); ok {
		t.Error("dev's data is still on the volume")
	}
	if !slices.Contains(fp.callLog(), "DeleteBranchData(dev)") {
		t.Errorf("calls %q", fp.callLog())
	}
	if _, _, ok := fp.generations("main"); !ok {
		t.Error("main's data went too")
	}
}

func TestDeletingMainKeepsItsData(t *testing.T) {
	ns, fp := readyProject(t)
	main := getBranch(t, ns, v1alpha1.MainBranch)
	deleteBranch(t, ns, v1alpha1.MainBranch)
	// The Project makes main again, on the same data.
	eventually(t, 15*time.Second, func() string {
		var b v1alpha1.Branch
		if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: v1alpha1.MainBranch}, &b); err != nil {
			return err.Error()
		}
		if b.UID == main.UID || b.Status.Phase != v1alpha1.BranchRunning {
			return fmt.Sprintf("main %s is %s", b.UID, b.Status.Phase)
		}
		return ""
	})
	if slices.Contains(fp.callLog(), "DeleteBranchData(main)") {
		t.Errorf("calls %q", fp.callLog())
	}
	if cur, _, ok := fp.generations("main"); !ok || cur != 1 {
		t.Errorf("main's data: %d %v", cur, ok)
	}
}

func TestDeletingABranchOfAGoneProjectLeavesTheAgentAlone(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	p := getProject(t, ns)
	if err := kenv.Client.Delete(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	waitGone(t, ns, ProjectName, &v1alpha1.Project{})
	deleteBranch(t, ns, "dev")
	waitGone(t, ns, "dev", &v1alpha1.Branch{})
	if slices.Contains(fp.callLog(), "DeleteBranchData(dev)") {
		t.Errorf("the agent was called for a project that is gone: %q", fp.callLog())
	}
}

func TestDeletingRetriesAnAgentThatDoesNotAnswer(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	fp.failNext("DeleteBranchData", agentErr(502, agentapi.CodeSupervisorUnavailable, "no answer"))
	deleteBranch(t, ns, "dev")
	waitGone(t, ns, "dev", &v1alpha1.Branch{})
	if n := countCalls(fp, "DeleteBranchData(dev)"); n < 2 {
		t.Errorf("%d deletes", n)
	}
}

func TestAFailedBranchCanBeDeleted(t *testing.T) {
	ns, _ := readyProject(t)
	createBranch(t, ns, "lost", "main", func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-zzzz"}
	})
	waitPhase(t, ns, "lost", v1alpha1.BranchFailed)
	deleteBranch(t, ns, "lost")
	waitGone(t, ns, "lost", &v1alpha1.Branch{})
}
