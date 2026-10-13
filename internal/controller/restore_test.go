package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

const testRP = "rp-20261007T031500Z-ab12"

// requestRestore asks for a rollback or reset, as the API server does (spec 9.4).
func requestRestore(t *testing.T, ns, name, requestID string, source v1alpha1.BranchSource) {
	t.Helper()
	eventually(t, 10*time.Second, func() string {
		b := getBranch(t, ns, name)
		b.Spec.Restore = &v1alpha1.RestoreRequest{RequestID: requestID, Source: source}
		if err := kenv.Client.Update(context.Background(), &b); err != nil {
			return err.Error()
		}
		return ""
	})
}

// waitRestore waits for a request to finish and returns the branch.
func waitRestore(t *testing.T, ns, name, requestID string, phase v1alpha1.RestorePhase) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	eventually(t, 15*time.Second, func() string {
		b = getBranch(t, ns, name)
		lr := b.Status.LastRestore
		if lr == nil || lr.RequestID != requestID || lr.Phase != phase {
			return fmt.Sprintf("lastRestore %+v", lr)
		}
		return ""
	})
	return b
}

// inOrder reports whether calls holds want in this order, other calls between them allowed.
func inOrder(calls, want []string) bool {
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	return i == len(want)
}

func runningDev(t *testing.T) (string, *fakeProject) {
	t.Helper()
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	return ns, fp
}

func TestRollback(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	b := waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	lr := b.Status.LastRestore
	if lr.FromGeneration != 1 || lr.ToGeneration != 2 || lr.FinishedAt == nil || lr.Message != "" {
		t.Errorf("lastRestore %+v", lr)
	}
	if !strings.HasPrefix(lr.BackupBranch, "dev-before-") || !agentapi.ValidBranchName(lr.BackupBranch) {
		t.Errorf("backup branch %q", lr.BackupBranch)
	}
	want := []string{
		"CreateGeneration(dev,RestorePoint)", "Stop(dev)", "ActivateGeneration(dev,2)", "Start(dev)",
		fmt.Sprintf("DetachGeneration(dev,1,%s,false)", lr.BackupBranch),
	}
	if !inOrder(fp.callLog(), want) {
		t.Errorf("calls %q, want in order %q", fp.callLog(), want)
	}
	b = waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if b.Status.DataGeneration != 2 {
		t.Errorf("dataGeneration %d", b.Status.DataGeneration)
	}
	if cur, gens, _ := fp.generations("dev"); cur != 2 || !slices.Equal(gens, []int{2}) {
		t.Errorf("dev on the volume: current %d of %v", cur, gens)
	}
	// The replaced data is a stopped backup branch.
	backup := waitPhase(t, ns, lr.BackupBranch, v1alpha1.BranchStopped)
	if backup.Labels[LabelBackupOf] != "dev" || backup.Spec.Parent != "dev" || backup.Spec.State != v1alpha1.StateStopped {
		t.Errorf("backup %+v %+v", backup.Labels, backup.Spec)
	}
	if n := countCalls(fp, "Start("+lr.BackupBranch+")"); n != 0 {
		t.Errorf("the backup branch was started %d times", n)
	}
}

func TestReset(t *testing.T) {
	ns, fp := runningDev(t)
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest})
	b := waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	lr := b.Status.LastRestore
	if lr.BackupBranch != "" || !agentapi.ValidRestorePointID(lr.RestorePointID) {
		t.Errorf("lastRestore %+v", lr)
	}
	if !inOrder(fp.callLog(), []string{"CreateGeneration(dev,ParentLatest)", "ActivateGeneration(dev,2)", "DetachGeneration(dev,1,,true)"}) {
		t.Errorf("calls %q", fp.callLog())
	}
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestResetOfMainFails(t *testing.T) {
	ns, fp := readyProject(t)
	requestRestore(t, ns, "main", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest})
	b := waitRestore(t, ns, "main", "r1", v1alpha1.RestoreFailed)
	if !strings.Contains(b.Status.LastRestore.Message, "without a parent") {
		t.Errorf("message %q", b.Status.LastRestore.Message)
	}
	waitPhase(t, ns, "main", v1alpha1.BranchRunning)
	if n := countCalls(fp, "CreateGeneration"); n != 0 {
		t.Errorf("%d generations made", n)
	}
}

func TestARestoreThatDoesNotStartGoesBack(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	fp.failToStart("dev", 2)
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	b := waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreFailed)
	lr := b.Status.LastRestore
	if !lr.Reverting || !strings.Contains(lr.Message, "did not start") {
		t.Errorf("lastRestore %+v", lr)
	}
	if !inOrder(fp.callLog(), []string{"ActivateGeneration(dev,2)", "Start(dev)", "ActivateGeneration(dev,1)", "Start(dev)", "DeleteGeneration(dev,2)"}) {
		t.Errorf("calls %q", fp.callLog())
	}
	b = waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if b.Status.DataGeneration != 1 {
		t.Errorf("dataGeneration %d", b.Status.DataGeneration)
	}
	if cur, gens, _ := fp.generations("dev"); cur != 1 || !slices.Equal(gens, []int{1}) {
		t.Errorf("dev on the volume: current %d of %v", cur, gens)
	}
	if state, gen, _ := fp.postgres("dev"); state != supervisorapi.PGRunning || gen != 1 {
		t.Errorf("postgres %s on %d", state, gen)
	}
}

func TestARestoreClearsGenerationsEarlierTriesLeft(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	// An earlier try made data-2 and lost the answer.
	if _, err := conn.Agent(ns, "").CreateGeneration(context.Background(), "dev", agentapi.CreateGenerationRequest{
		Source: agentapi.Source{Type: agentapi.SourceRestorePoint, RestorePointID: testRP},
	}); err != nil {
		t.Fatal(err)
	}
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	// data-2 was deleted and made anew: the agent numbers from the highest generation left.
	if !inOrder(fp.callLog(), []string{"ListGenerations(dev)", "DeleteGeneration(dev,2)", "CreateGeneration(dev,RestorePoint)", "ActivateGeneration(dev,2)"}) {
		t.Errorf("calls %q", fp.callLog())
	}
	if cur, gens, _ := fp.generations("dev"); cur != 2 || !slices.Equal(gens, []int{2}) {
		t.Errorf("dev on the volume: current %d of %v", cur, gens)
	}
}

func TestARestoreGoesOnAfterAFailedStep(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	fp.failNext("ActivateGeneration", agentErr(502, agentapi.CodeSupervisorUnavailable, "no answer"))
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	if n := countCalls(fp, "CreateGeneration"); n != 1 {
		t.Errorf("%d generations made; the restore did not go on from where it was", n)
	}
	if n := countCalls(fp, "ActivateGeneration(dev,2)"); n < 2 {
		t.Errorf("%d activations", n)
	}
}

func TestEachRequestRunsOnce(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	rp := v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP}
	requestRestore(t, ns, "dev", "r1", rp)
	waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	setState(t, ns, "dev", v1alpha1.StateStopped) // another reconcile
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
	if n := countCalls(fp, "CreateGeneration"); n != 1 {
		t.Fatalf("%d generations made for one request", n)
	}
	requestRestore(t, ns, "dev", "r2", rp)
	waitRestore(t, ns, "dev", "r2", v1alpha1.RestoreDone)
	if n := countCalls(fp, "CreateGeneration"); n != 2 {
		t.Errorf("%d generations made for two requests", n)
	}
	// Both rollbacks ran within a second or two: their backup branches still got two names.
	first, second := getBranch(t, ns, "dev").Status.LastRestore.BackupBranch, ""
	var list v1alpha1.BranchList
	if err := kenv.Client.List(context.Background(), &list, client.InNamespace(ns), client.MatchingLabels{LabelBackupOf: "dev"}); err != nil || len(list.Items) != 2 {
		t.Fatalf("backup branches: %v, %d", err, len(list.Items))
	}
	for _, it := range list.Items {
		if it.Name != first {
			second = it.Name
		}
	}
	if second == "" || second == first {
		t.Errorf("backup branches %q and %q", first, second)
	}
	// A stopped branch is started to check the restored data, then stopped again.
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
}
