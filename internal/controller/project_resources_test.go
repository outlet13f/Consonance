package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"consonance/api/v1alpha1"
)

// testProject is a Project with the defaults the API server fills in (spec 14).
func testProject(ns string) *v1alpha1.Project {
	return &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: ProjectName, Namespace: ns},
		Spec: v1alpha1.ProjectSpec{
			DisplayName:     "my-app",
			PostgresVersion: "17",
			Storage:         v1alpha1.StorageSpec{Size: resource.MustParse("10Gi")},
			DefaultCompute:  v1alpha1.ComputeSpec{CPU: resource.MustParse("500m"), Memory: resource.MustParse("1Gi")},
			Limits:          v1alpha1.ProjectLimits{MaxBranches: 10, MinCopyInterval: metav1.Duration{Duration: 5 * time.Second}},
			RestorePoints:   v1alpha1.RestorePointPolicy{MaxPerBranch: 20, MaxAge: metav1.Duration{Duration: 24 * time.Hour}},
		},
	}
}

func TestProjectID(t *testing.T) {
	for ns, want := range map[string]string{"cns-a1b2c3d4e5f6": "a1b2c3d4e5f6", "cns-": "", "default": ""} {
		got, ok := ProjectID(ns)
		if got != want || ok != (want != "") {
			t.Errorf("ProjectID(%q) = %q, %v", ns, got, ok)
		}
	}
}

func TestSecretsAreRandom(t *testing.T) {
	a, err := newDBCredentialsSecret("cns-x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newDBCredentialsSecret("cns-x")
	pw := a.StringData[PasswordKey]
	if len(pw) != 32 || pw == b.StringData[PasswordKey] || strings.Trim(pw, alphanumeric) != "" {
		t.Fatalf("password %q", pw)
	}
	if a.StringData[UsernameKey] != "app_owner" || a.StringData[DatabaseKey] != "app" || a.Name != "db-credentials" {
		t.Fatalf("secret %+v", a)
	}
	tok, err := newInternalTokenSecret("cns-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.StringData[TokenKey]) != 48 || tok.Name != "internal-token" || tok.Labels[LabelManagedBy] != ManagedBy {
		t.Fatalf("token secret %+v", tok)
	}
}

func TestNetworkPolicies(t *testing.T) {
	pols := desiredNetworkPolicies("cns-x")
	byName := map[string]*networkingv1.NetworkPolicy{}
	for _, p := range pols {
		byName[p.Name] = p
		if p.Namespace != "cns-x" || p.Labels[LabelManagedBy] != ManagedBy {
			t.Errorf("%s: namespace %q labels %v", p.Name, p.Namespace, p.Labels)
		}
	}
	deny := byName["default-deny-ingress"]
	if deny == nil || len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.Ingress) != 0 {
		t.Fatalf("default deny: %+v", deny)
	}
	branchIn := byName["branch-ingress"]
	if branchIn == nil || len(branchIn.Spec.Ingress) != 2 {
		t.Fatalf("branch ingress: %+v", branchIn)
	}
	if got := ruleFor(branchIn, 5432); got == nil || len(got.From) != 2 {
		t.Fatalf("5432 rule: %+v", got)
	}
	sup := ruleFor(branchIn, 8008)
	if sup == nil || !admitsApp(sup.From, OperatorAppName) || admitsApp(sup.From, APIServerAppName) {
		t.Fatalf("8008 rule: %+v", sup)
	}
	agentIn := ruleFor(byName["agent-ingress"], 8443)
	if agentIn == nil || !admitsApp(agentIn.From, OperatorAppName) || !admitsApp(agentIn.From, APIServerAppName) {
		t.Fatalf("8443 rule: %+v", agentIn)
	}
	branchOut := byName["branch-egress"]
	if branchOut == nil || !slices.Contains(branchOut.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) || len(branchOut.Spec.Egress) != 0 {
		t.Fatalf("branch egress must deny all: %+v", branchOut)
	}
	if agentOut := byName["agent-egress"]; agentOut == nil || len(agentOut.Spec.Egress) != 2 {
		t.Fatalf("agent egress: %+v", agentOut)
	}
}

func ruleFor(p *networkingv1.NetworkPolicy, port int32) *networkingv1.NetworkPolicyIngressRule {
	for i, r := range p.Spec.Ingress {
		for _, pp := range r.Ports {
			if pp.Port.IntVal == port {
				return &p.Spec.Ingress[i]
			}
		}
	}
	return nil
}

// admitsApp reports whether a peer admits pods named app in consonance-system.
func admitsApp(peers []networkingv1.NetworkPolicyPeer, app string) bool {
	for _, peer := range peers {
		if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != SystemNamespace || peer.PodSelector == nil {
			continue
		}
		for _, e := range peer.PodSelector.MatchExpressions {
			if e.Key == LabelAppName && slices.Contains(e.Values, app) {
				return true
			}
		}
	}
	return false
}

func TestQuotaAndPVC(t *testing.T) {
	p := testProject("cns-x")
	q := desiredQuota(p)
	if got := q.Spec.Hard[corev1.ResourcePods]; got.Value() != 21 {
		t.Errorf("pods quota %s, want 21 (10 branches, 10 backups, the agent)", got.String())
	}
	if got := q.Spec.Hard[corev1.ResourceRequestsStorage]; got.String() != "10Gi" {
		t.Errorf("storage quota %s", got.String())
	}
	pvc := desiredPVC(p)
	if pvc.Name != "project-volume" || pvc.Spec.StorageClassName != nil || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("pvc %+v", pvc.Spec)
	}
	p.Spec.Storage.StorageClassName = "consonance-xfs"
	if sc := desiredPVC(p).Spec.StorageClassName; sc == nil || *sc != "consonance-xfs" {
		t.Fatalf("storage class %v", sc)
	}
}

func TestAgentPod(t *testing.T) {
	p := testProject("cns-a1b2c3d4e5f6")
	pod := desiredAgentPod(p, "a1b2c3d4e5f6", "consonance/agent:dev")
	c := pod.Spec.Containers[0]
	wantArgs := []string{"--project-id=a1b2c3d4e5f6", "--min-copy-interval=5s", "--restore-points-per-branch=20", "--restore-point-max-age=24h0m0s"}
	if !slices.Equal(c.Args, wantArgs) {
		t.Errorf("args %q, want %q", c.Args, wantArgs)
	}
	if pod.Name != "volume-agent" || pod.Labels[LabelComponent] != ComponentAgent || c.Image != "consonance/agent:dev" {
		t.Errorf("pod %s labels %v image %s", pod.Name, pod.Labels, c.Image)
	}
	checkRestricted(t, pod)
	sc := pod.Spec.SecurityContext
	if sc.FSGroup == nil || *sc.FSGroup != 999 || sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroup %v policy %v", sc.FSGroup, sc.FSGroupChangePolicy)
	}
	if mem := c.Resources.Limits[corev1.ResourceMemory]; mem.String() != "128Mi" {
		t.Errorf("memory limit %s", mem.String())
	}
	if !mounts(c, "volume", "/volume") || !mounts(c, "token", "/etc/consonance/token") {
		t.Errorf("mounts %+v", c.VolumeMounts)
	}
	if *pod.Spec.TerminationGracePeriodSeconds < 45 {
		t.Errorf("grace %d", *pod.Spec.TerminationGracePeriodSeconds)
	}

	// A setting change changes the hash, so the operator replaces the pod.
	h := pod.Annotations[AnnotationSpecHash]
	p.Spec.Limits.MinCopyInterval.Duration = time.Minute
	if h2 := desiredAgentPod(p, "a1b2c3d4e5f6", "consonance/agent:dev").Annotations[AnnotationSpecHash]; h2 == h || h == "" {
		t.Errorf("hash %q then %q", h, h2)
	}
	if svc := desiredAgentService("cns-x"); svc.Spec.Ports[0].Port != 8443 || svc.Spec.Selector[LabelComponent] != ComponentAgent {
		t.Errorf("service %+v", svc.Spec)
	}
}

// checkRestricted checks the Pod Security Standard restricted settings (spec 7.5).
func checkRestricted(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != 999 || *sc.RunAsGroup != 999 ||
		sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v", sc)
	}
	for _, c := range pod.Spec.Containers {
		s := c.SecurityContext
		if s == nil || s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation || s.Capabilities == nil ||
			!slices.Equal(s.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Errorf("container %s security context %+v", c.Name, s)
		}
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("service account token is mounted")
	}
}

func mounts(c corev1.Container, volume, path string) bool {
	return slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == volume && m.MountPath == path })
}
