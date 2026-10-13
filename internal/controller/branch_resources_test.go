package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
)

func testBranch(ns, name, parent string) *v1alpha1.Branch {
	return &v1alpha1.Branch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.BranchSpec{
			Parent: parent,
			Source: v1alpha1.BranchSource{Type: v1alpha1.SourceParentLatest},
			State:  v1alpha1.StateRunning,
		},
	}
}

func envVar(c corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func TestBranchPod(t *testing.T) {
	p := testProject("cns-x")
	b := testBranch("cns-x", "dev", "main")
	pod := desiredBranchPod(p, b, "consonance/postgres:17-dev")
	c := pod.Spec.Containers[0]
	if pod.Name != "br-dev" || pod.Labels[LabelBranch] != "dev" || pod.Labels[LabelComponent] != ComponentBranch {
		t.Errorf("pod %s labels %v", pod.Name, pod.Labels)
	}
	checkRestricted(t, pod)
	if pod.Spec.SecurityContext.FSGroup != nil {
		t.Errorf("a branch pod sets no fsGroup (the agent's pod owns the volume's group)")
	}
	if !mounts(c, "pgdata", "/pgdata") || !mounts(c, "run", "/var/run/postgresql") || !mounts(c, "token", "/etc/consonance/token") {
		t.Errorf("mounts %+v", c.VolumeMounts)
	}
	for _, m := range c.VolumeMounts {
		if m.Name == "pgdata" && m.SubPath != "branches/dev" {
			t.Errorf("subPath %q", m.SubPath)
		}
	}
	if mounts(c, "db", "/etc/consonance/db") || len(c.Args) != 0 {
		t.Errorf("only main gets the tenant password: args %q", c.Args)
	}
	mem := envVar(c, "MEMORY_LIMIT_BYTES")
	if mem == nil || mem.ValueFrom == nil || mem.ValueFrom.ResourceFieldRef.Resource != "limits.memory" {
		t.Errorf("MEMORY_LIMIT_BYTES %+v", mem)
	}
	if lim := c.Resources.Limits[corev1.ResourceMemory]; lim.String() != "1Gi" {
		t.Errorf("memory limit %s, want the project default", lim.String())
	}
	if a := envVar(c, "AUTOSTART"); a == nil || a.Value != "true" {
		t.Errorf("AUTOSTART %+v", a)
	}
	aff := pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(aff) != 1 || aff[0].TopologyKey != "kubernetes.io/hostname" || aff[0].LabelSelector.MatchLabels[LabelComponent] != ComponentAgent {
		t.Errorf("affinity %+v", aff)
	}
	if *pod.Spec.TerminationGracePeriodSeconds < 45 {
		t.Errorf("grace %d", *pod.Spec.TerminationGracePeriodSeconds)
	}
}

func TestMainPodGetsThePasswordAndStoppedBranchesDoNotAutostart(t *testing.T) {
	p := testProject("cns-x")
	m := testBranch("cns-x", "main", "")
	c := desiredBranchPod(p, m, "img").Spec.Containers[0]
	if len(c.Args) != 1 || c.Args[0] != "--db-password-file=/etc/consonance/db/password" || !mounts(c, "db", "/etc/consonance/db") {
		t.Errorf("main: args %q mounts %+v", c.Args, c.VolumeMounts)
	}
	s := testBranch("cns-x", "old", "main")
	s.Spec.State = v1alpha1.StateStopped
	s.Spec.Compute = &v1alpha1.ComputeSpec{CPU: resource.MustParse("250m"), Memory: resource.MustParse("512Mi")}
	c = desiredBranchPod(p, s, "img").Spec.Containers[0]
	if a := envVar(c, "AUTOSTART"); a == nil || a.Value != "false" {
		t.Errorf("AUTOSTART %+v", a)
	}
	if lim := c.Resources.Limits[corev1.ResourceMemory]; lim.String() != "512Mi" {
		t.Errorf("memory limit %s, want the branch's own", lim.String())
	}
}

func TestBranchService(t *testing.T) {
	svc := desiredBranchService(testBranch("cns-x", "dev", "main"))
	if svc.Name != "br-dev" || !svc.Spec.PublishNotReadyAddresses || svc.Spec.Selector[LabelBranch] != "dev" {
		t.Fatalf("service %s %+v", svc.Name, svc.Spec)
	}
	ports := map[int32]bool{}
	for _, p := range svc.Spec.Ports {
		ports[p.Port] = true
	}
	if !ports[5432] || !ports[8008] {
		t.Fatalf("ports %+v", svc.Spec.Ports)
	}
	if e := branchEndpoint(testBranch("cns-x", "dev", "main")); e.Host != "br-dev.cns-x.svc" || e.Port != 5432 {
		t.Fatalf("endpoint %+v", e)
	}
}

func TestBackupBranchName(t *testing.T) {
	at := time.Date(2026, 10, 7, 3, 15, 0, 0, time.FixedZone("KST", 9*3600))
	if got := backupBranchName("main", at); got != "main-before-20261006181500" {
		t.Errorf("main: %q", got)
	}
	long := strings.Repeat("abcdefghij", 4) // 40 characters
	got := backupBranchName(long, at)
	if got != "abcdefghijabcdefgh-before-20261006181500" || !agentapi.ValidBranchName(got) {
		t.Errorf("long: %q (%d characters)", got, len(got))
	}
}
