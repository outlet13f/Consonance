package controller

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorapi"
)

// readyProject makes a project whose main branch runs.
func readyProject(t *testing.T) (string, *fakeProject) {
	t.Helper()
	ns, fp := createProject(t, nil)
	bindVolume(t, ns)
	waitPhase(t, ns, v1alpha1.MainBranch, v1alpha1.BranchRunning)
	return ns, fp
}

func getBranch(t *testing.T, ns, name string) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func waitPhase(t *testing.T, ns, name string, phase v1alpha1.BranchPhase) v1alpha1.Branch {
	t.Helper()
	var b v1alpha1.Branch
	eventually(t, 15*time.Second, func() string {
		if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &b); err != nil {
			return name + ": " + err.Error()
		}
		if b.Status.Phase != phase {
			return fmt.Sprintf("%s is %q, DataReady %s, Ready %s", name, b.Status.Phase,
				cond(b.Status.Conditions, v1alpha1.ConditionDataReady), cond(b.Status.Conditions, v1alpha1.ConditionReady))
		}
		return ""
	})
	return b
}

// createBranch makes a branch as the API server does (spec 9.2).
func createBranch(t *testing.T, ns, name, parent string, change ...func(*v1alpha1.Branch)) {
	t.Helper()
	b := &v1alpha1.Branch{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.BranchSpec{Parent: parent}}
	for _, c := range change {
		c(b)
	}
	if err := kenv.Client.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

func setState(t *testing.T, ns, name string, state v1alpha1.DesiredState) {
	t.Helper()
	eventually(t, 10*time.Second, func() string {
		b := getBranch(t, ns, name)
		b.Spec.State = state
		if err := kenv.Client.Update(context.Background(), &b); err != nil {
			return err.Error()
		}
		return ""
	})
}

func TestMainRunsAndTheProjectGetsReady(t *testing.T) {
	ns, fp := readyProject(t)
	main := getBranch(t, ns, v1alpha1.MainBranch)
	if main.Status.DataGeneration != 1 || !meta.IsStatusConditionTrue(main.Status.Conditions, v1alpha1.ConditionDataReady) ||
		main.Status.Timings.DataReadyAt == nil || main.Status.Timings.RunningAt == nil {
		t.Errorf("main status %+v", main.Status)
	}
	if e := main.Status.Endpoint; e == nil || e.Host != "br-main."+ns+".svc" || e.Port != 5432 {
		t.Errorf("endpoint %+v", e)
	}
	var pod corev1.Pod
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-main"}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Image != testPostgresImage || !metav1.IsControlledBy(&pod, &main) {
		t.Errorf("pod image %s owners %+v", pod.Spec.Containers[0].Image, pod.OwnerReferences)
	}
	var svc corev1.Service
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-main"}, &svc); err != nil || !svc.Spec.PublishNotReadyAddresses {
		t.Errorf("service: %v %+v", err, svc.Spec)
	}
	if !slices.Contains(fp.callLog(), "InitMain") {
		t.Errorf("calls %q", fp.callLog())
	}
	eventually(t, 10*time.Second, func() string {
		if p := getProject(t, ns); p.Status.Phase != v1alpha1.ProjectReady {
			return "project " + string(p.Status.Phase)
		}
		return ""
	})
}

func TestBranchFromItsRunningParent(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	b := waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if f := b.Status.CreatedFrom; f == nil || f.Branch != "main" || f.BackupStartLSN != "0/3000028" {
		t.Errorf("createdFrom %+v", f)
	}
	if !slices.Contains(fp.callLog(), "CreateBranchData(dev,main,ParentLatest)") {
		t.Errorf("calls %q", fp.callLog())
	}
	if state, gen, _ := fp.postgres("dev"); state != supervisorapi.PGRunning || gen != 1 {
		t.Errorf("dev's postgres %s on %d", state, gen)
	}
}

func TestBranchWaitsForItsParentToRun(t *testing.T) {
	ns, _ := readyProject(t)
	setState(t, ns, "main", v1alpha1.StateStopped)
	waitPhase(t, ns, "main", v1alpha1.BranchStopped)
	createBranch(t, ns, "dev", "main")
	eventually(t, 10*time.Second, func() string {
		if c := cond(getBranch(t, ns, "dev").Status.Conditions, v1alpha1.ConditionDataReady); c != "False/ParentNotRunning" {
			return "DataReady " + c
		}
		return ""
	})
	setState(t, ns, "main", v1alpha1.StateRunning)
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestBranchFromARestorePoint(t *testing.T) {
	ns, fp := readyProject(t)
	fp.addRestorePoint("rp-20261007T031500Z-ab12", "main")
	createBranch(t, ns, "fix", "main", func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-ab12"}
	})
	b := waitPhase(t, ns, "fix", v1alpha1.BranchRunning)
	if f := b.Status.CreatedFrom; f == nil || f.RestorePointID != "rp-20261007T031500Z-ab12" || f.BackupStartLSN != "" {
		t.Errorf("createdFrom %+v", f)
	}
}

func TestBranchFromAMissingRestorePointFails(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "lost", "main", func(b *v1alpha1.Branch) {
		b.Spec.Source = v1alpha1.BranchSource{Type: v1alpha1.SourceRestorePoint, RestorePointID: "rp-20261007T031500Z-zzzz"}
	})
	b := waitPhase(t, ns, "lost", v1alpha1.BranchFailed)
	if c := cond(b.Status.Conditions, v1alpha1.ConditionDataReady); c != "False/"+ReasonSourceNotFound {
		t.Errorf("DataReady %s", c)
	}
	n := countCalls(fp, "CreateBranchData(lost") // a permanent failure is not retried
	time.Sleep(time.Second)
	if m := countCalls(fp, "CreateBranchData(lost"); m != n {
		t.Errorf("%d copies, then %d", n, m)
	}
	var pod corev1.Pod
	if err := kenv.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "br-lost"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("a branch without data got a pod: %v", err)
	}
}

func TestRateLimitedCopyComesBackAfterRetryAfter(t *testing.T) {
	ns, fp := readyProject(t)
	fp.failNext("CreateBranchData", &agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited, RetryAfter: time.Second})
	start := time.Now()
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	if n := countCalls(fp, "CreateBranchData(dev"); n < 2 {
		t.Errorf("%d copies, want a refused one and then more", n)
	}
	if took := time.Since(start); took < time.Second {
		t.Errorf("retried after %s, before Retry-After", took)
	}
}

func TestStopAndStart(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	setState(t, ns, "dev", v1alpha1.StateStopped)
	waitPhase(t, ns, "dev", v1alpha1.BranchStopped)
	if state, _, _ := fp.postgres("dev"); state != supervisorapi.PGStopped {
		t.Fatalf("postgres %s", state)
	}
	setState(t, ns, "dev", v1alpha1.StateRunning)
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestAStoppedBranchIsNotStarted(t *testing.T) {
	ns, fp := readyProject(t)
	createBranch(t, ns, "idle", "main", func(b *v1alpha1.Branch) { b.Spec.State = v1alpha1.StateStopped })
	waitPhase(t, ns, "idle", v1alpha1.BranchStopped)
	time.Sleep(500 * time.Millisecond)
	if n := countCalls(fp, "Start(idle)"); n != 0 {
		t.Errorf("%d starts", n)
	}
}

func TestStartFailureIsReportedAndRetried(t *testing.T) {
	ns, fp := readyProject(t)
	fp.failToStart("dev", 1)
	createBranch(t, ns, "dev", "main")
	b := waitPhase(t, ns, "dev", v1alpha1.BranchFailed)
	if c := cond(b.Status.Conditions, v1alpha1.ConditionReady); c != "False/StartFailed" {
		t.Errorf("Ready %s", c)
	}
	eventually(t, 10*time.Second, func() string {
		if n := countCalls(fp, "Start(dev)"); n < 2 {
			return fmt.Sprintf("%d starts", n)
		}
		return ""
	})
	fp.failToStart("dev", 0) // whatever kept it from starting is fixed
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}

func TestADeletedPodIsMadeAgain(t *testing.T) {
	ns, _ := readyProject(t)
	createBranch(t, ns, "dev", "main")
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: ns, Name: "br-dev"}
	if err := kenv.Client.Get(context.Background(), key, &pod); err != nil {
		t.Fatal(err)
	}
	old := pod.UID
	if err := kenv.Client.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() string {
		var p corev1.Pod
		if err := kenv.Client.Get(context.Background(), key, &p); err != nil || p.UID == old {
			return fmt.Sprintf("pod: %v", err)
		}
		return ""
	})
	waitPhase(t, ns, "dev", v1alpha1.BranchRunning)
}
