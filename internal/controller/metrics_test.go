package controller

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/api/meta"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
)

// The metrics live in the registry the whole test binary shares, and earlier tests' branches may
// still be reconciling, so each test settles first and compares counts before and after.

// samples is how many observations a histogram holds.
func samples(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	var m dto.Metric
	if err := o.(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// snapshot of every count the tests watch.
type counts struct{ create, firstReady, start, project, rollbackDone, resetDone, resetFailed uint64 }

func snap(t *testing.T) counts {
	t.Helper()
	return counts{
		samples(t, branchCreateSeconds), samples(t, branchFirstReadySeconds), samples(t, startSeconds), samples(t, projectCreateSeconds),
		samples(t, restoreSeconds.WithLabelValues("rollback", "done")),
		samples(t, restoreSeconds.WithLabelValues("reset", "done")),
		samples(t, restoreSeconds.WithLabelValues("reset", "failed")),
	}
}

// settle waits until no metric has changed for a while: what earlier tests left running is done.
func settle(t *testing.T) counts {
	t.Helper()
	last, since := snap(t), time.Now()
	for time.Since(since) < 500*time.Millisecond {
		time.Sleep(50 * time.Millisecond)
		if now := snap(t); now != last {
			last, since = now, time.Now()
		}
	}
	return last
}

// expect waits for the counts to reach want and, after a pause, to be there still: nothing was
// observed twice.
func expect(t *testing.T, want counts) {
	t.Helper()
	eventually(t, 10*time.Second, func() string {
		if got := snap(t); got != want {
			return fmt.Sprintf("counts %+v, want %+v", got, want)
		}
		return ""
	})
	time.Sleep(500 * time.Millisecond)
	if got := snap(t); got != want {
		t.Errorf("counts %+v, want %+v still", got, want)
	}
}

func copyErrorCount(reason string) float64 {
	return testutil.ToFloat64(copyErrors.WithLabelValues(reason))
}

func TestBranchMetricsAreObservedOnce(t *testing.T) {
	ns, _ := readyProject(t)
	want := settle(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	want.create++
	want.firstReady++
	expect(t, want)

	// Stopping and starting it again reconciles it more, and only the start is new.
	setState(t, ns, "dev", v1alpha1.StateStopped)
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
	setState(t, ns, "dev", v1alpha1.StateRunning)
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	want.start++
	expect(t, want)
}

func TestAFirstStartIsNotAStart(t *testing.T) {
	ns, _ := readyProject(t)
	want := settle(t)
	createBranch(t, ns, "idle", "main", func(b *v1alpha1.Branch) { b.Spec.State = v1alpha1.StateStopped })
	waitPhase(t, ns, "idle", v1alpha1.BranchStopped)
	want.create++ // its data is made; it never runs
	expect(t, want)
	setState(t, ns, "idle", v1alpha1.StateRunning)
	waitPhase(t, ns, "idle", v1alpha1.BranchRunning)
	want.firstReady++
	want.start++ // stopped, then asked to run
	expect(t, want)
}

func TestMainIsLeftOutOfBranchCreate(t *testing.T) {
	before := settle(t)
	readyProject(t)
	time.Sleep(500 * time.Millisecond)
	if got := snap(t); got.create != before.create {
		t.Errorf("branch_create %d, was %d: main was counted", got.create, before.create)
	}
}

func TestProjectMetric(t *testing.T) {
	want := settle(t)
	ns, _ := createProject(t, nil)
	bindVolume(t, ns)
	eventually(t, 15*time.Second, func() string {
		if p := getProject(t, ns); p.Status.Phase != v1alpha1.ProjectReady {
			return fmt.Sprintf("phase %s: %s", p.Status.Phase, meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady).Message)
		}
		return ""
	})
	want.firstReady++ // main
	want.project++
	expect(t, want)
}

func TestRestoreMetrics(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	want := settle(t)

	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	want.rollbackDone++
	want.create++ // the backup branch's data is found, which is its data being ready
	expect(t, want)

	requestRestore(t, ns, "dev", "r2", v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest})
	waitRestore(t, ns, "dev", "r2", v1alpha1.RestoreDone)
	want.resetDone++
	expect(t, want)

	// A reset of main fails at once: it has no parent.
	requestRestore(t, ns, "main", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest})
	waitRestore(t, ns, "main", "r1", v1alpha1.RestoreFailed)
	want.resetFailed++
	expect(t, want)
}

func TestCopyErrorsAreCountedByReason(t *testing.T) {
	ns, fp := runningDev(t)
	fp.addRestorePoint(testRP, "dev")
	settle(t)
	limited, transport := copyErrorCount("rate-limited"), copyErrorCount("transport")

	// A refused branch copy, then an answer that never came.
	fp.failNext("CreateBranchData",
		&agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited, RetryAfter: time.Second},
		errors.New("connection reset"))
	createBranch(t, ns, "feat", "main")
	waitPhase(t, ns, "feat", v1alpha1.BranchRunning)
	if got := copyErrorCount("rate-limited") - limited; got != 1 {
		t.Errorf("rate-limited %v more, want 1", got)
	}
	if got := copyErrorCount("transport") - transport; got != 1 {
		t.Errorf("transport %v more, want 1", got)
	}

	// A refused generation of a restore.
	fp.failNext("CreateGeneration", &agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited, RetryAfter: time.Second})
	requestRestore(t, ns, "dev", "r1", v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: testRP})
	waitRestore(t, ns, "dev", "r1", v1alpha1.RestoreDone)
	if got := copyErrorCount("rate-limited") - limited; got != 2 {
		t.Errorf("rate-limited %v more, want 2", got)
	}
}
