package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/supervisorapi"
)

const (
	componentProject = "project"
	uid              = int64(999) // the postgres user of the images (spec 7.5)
	// terminationGrace covers the supervisor's 30s fast shutdown plus 10s after SIGQUIT, and
	// the agent's 25s drain (carry-over from plans 1 and 2).
	terminationGrace = int64(45)
	tokenMountDir    = "/etc/consonance/token" // the agent and supervisors read <dir>/token
)

// randomString returns n characters from the alphabet, read from crypto/rand.
func randomString(n int, alphabet string) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

const alphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newDBCredentialsSecret is app_owner's login: a 32-character random password (spec 4.5). It is
// made once and never changed: main's database keeps the password it was bootstrapped with.
func newDBCredentialsSecret(ns string) (*corev1.Secret, error) {
	pw, err := randomString(32, alphanumeric)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: DBCredentialsSecret, Namespace: ns, Labels: managedLabels(componentProject)},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{UsernameKey: TenantRole, PasswordKey: pw, DatabaseKey: TenantDatabase},
	}, nil
}

// newInternalTokenSecret is the project's token for the agent and supervisor APIs. It is made
// once: the pods read it when they start.
func newInternalTokenSecret(ns string) (*corev1.Secret, error) {
	tok, err := randomString(48, alphanumeric)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: InternalTokenSecret, Namespace: ns, Labels: managedLabels(componentProject)},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{TokenKey: tok},
	}, nil
}

// desiredNetworkPolicies are spec 8's rules for a project namespace.
func desiredNetworkPolicies(ns string) []*networkingv1.NetworkPolicy {
	system := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": SystemNamespace}}
	fromSystemApps := func(apps ...string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: system,
			PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: LabelAppName, Operator: metav1.LabelSelectorOpIn, Values: apps},
			}},
		}
	}
	agent := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: ComponentAgent}}}
	branchPods := metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: ComponentBranch}}
	agentPods := metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: ComponentAgent}}
	port := func(p int32, proto corev1.Protocol) networkingv1.NetworkPolicyPort {
		return networkingv1.NetworkPolicyPort{Protocol: ptr.To(proto), Port: ptr.To(intstr.FromInt32(p))}
	}
	policy := func(name string, pods metav1.LabelSelector, types []networkingv1.PolicyType) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: managedLabels(componentProject)},
			Spec:       networkingv1.NetworkPolicySpec{PodSelector: pods, PolicyTypes: types},
		}
	}
	ingress := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
	egress := []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}

	defaultDeny := policy("default-deny-ingress", metav1.LabelSelector{}, ingress)

	branchIn := policy("branch-ingress", branchPods, ingress)
	branchIn.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{
		{ // PostgreSQL: the system namespace (API server, later the gateway) and the agent
			Ports: []networkingv1.NetworkPolicyPort{port(5432, corev1.ProtocolTCP)},
			From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: system}, agent},
		},
		{ // the supervisor: the operator and the agent
			Ports: []networkingv1.NetworkPolicyPort{port(int32(supervisorapi.DefaultPort), corev1.ProtocolTCP)},
			From:  []networkingv1.NetworkPolicyPeer{fromSystemApps(OperatorAppName), agent},
		},
	}

	agentIn := policy("agent-ingress", agentPods, ingress)
	agentIn.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
		Ports: []networkingv1.NetworkPolicyPort{port(int32(agentapi.DefaultPort), corev1.ProtocolTCP)},
		From:  []networkingv1.NetworkPolicyPeer{fromSystemApps(OperatorAppName, APIServerAppName)},
	}}

	// PostgreSQL has no reason to reach out (spec 8): no egress rule allows nothing.
	branchOut := policy("branch-egress", branchPods, egress)

	agentOut := policy("agent-egress", agentPods, egress)
	agentOut.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}, // this namespace
		{ // DNS
			To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
			}}},
			Ports: []networkingv1.NetworkPolicyPort{port(53, corev1.ProtocolUDP), port(53, corev1.ProtocolTCP)},
		},
	}
	return []*networkingv1.NetworkPolicy{defaultDeny, branchIn, agentIn, branchOut, agentOut}
}

// desiredQuota caps what a project can hold: one volume of the project's size, and pods for
// maxBranches branches, as many backup branches (a rollback adds one beyond the API's count)
// and the agent.
func desiredQuota(p *v1alpha1.Project) *corev1.ResourceQuota {
	pods := 2*int64(p.Spec.Limits.MaxBranches) + 1
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: QuotaName, Namespace: p.Namespace, Labels: managedLabels(componentProject)},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourcePods:                   *resource.NewQuantity(pods, resource.DecimalSI),
			corev1.ResourcePersistentVolumeClaims: *resource.NewQuantity(1, resource.DecimalSI),
			corev1.ResourceRequestsStorage:        p.Spec.Storage.Size,
		}},
	}
}

// desiredPVC is the project volume (spec 7.1 step 2).
func desiredPVC(p *v1alpha1.Project) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: PVCName, Namespace: p.Namespace, Labels: managedLabels(componentProject)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: p.Spec.Storage.Size,
			}},
		},
	}
	if p.Spec.Storage.StorageClassName != "" {
		pvc.Spec.StorageClassName = ptr.To(p.Spec.Storage.StorageClassName)
	}
	return pvc
}

// restrictedPod and restrictedContainer meet Pod Security Standard restricted (spec 7.5).
func restrictedPod() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      ptr.To(uid),
		RunAsGroup:     ptr.To(uid),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// tokenVolume mounts internal-token, which the agent and supervisors read at tokenMountDir/token.
func tokenVolume() (corev1.Volume, corev1.VolumeMount) {
	return corev1.Volume{
			Name:         "token",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: InternalTokenSecret}},
		}, corev1.VolumeMount{
			Name: "token", MountPath: tokenMountDir, ReadOnly: true,
		}
}

func httpProbe(path string, port int, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt(port)}},
		PeriodSeconds: period, TimeoutSeconds: 2, FailureThreshold: 3,
	}
}

// agentArgs are the agent's settings from the project spec (spec 14 defaults come from the CRD).
func agentArgs(p *v1alpha1.Project, projectID string) []string {
	return []string{
		"--project-id=" + projectID,
		"--min-copy-interval=" + p.Spec.Limits.MinCopyInterval.Duration.String(),
		"--restore-points-per-branch=" + strconv.Itoa(int(p.Spec.RestorePoints.MaxPerBranch)),
		"--restore-point-max-age=" + p.Spec.RestorePoints.MaxAge.Duration.String(),
	}
}

// desiredAgentPod is the volume agent (spec 7.1 step 3, 7.2, 7.5). Its spec-hash annotation
// changes with the image and the settings, so the operator replaces a pod whose settings are old:
// the agent reads them only when it starts.
func desiredAgentPod(p *v1alpha1.Project, projectID, image string) *corev1.Pod {
	tokenVol, tokenMount := tokenVolume()
	sc := restrictedPod()
	sc.FSGroup = ptr.To(uid)
	sc.FSGroupChangePolicy = ptr.To(corev1.FSGroupChangeOnRootMismatch) // no recursive chown of a large volume
	res := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")}
	c := corev1.Container{
		Name:            "agent",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            agentArgs(p, projectID),
		Ports:           []corev1.ContainerPort{{Name: "api", ContainerPort: int32(agentapi.DefaultPort)}},
		VolumeMounts:    []corev1.VolumeMount{{Name: "volume", MountPath: "/volume"}, tokenMount},
		Resources:       corev1.ResourceRequirements{Requests: res, Limits: res},
		LivenessProbe:   httpProbe("/healthz", agentapi.DefaultPort, 10),
		ReadinessProbe:  httpProbe("/healthz", agentapi.DefaultPort, 2),
		SecurityContext: restrictedContainer(),
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: AgentName, Namespace: p.Namespace,
			Labels:      managedLabels(ComponentAgent),
			Annotations: map[string]string{AnnotationSpecHash: specHash(c)},
		},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{c},
			Volumes:                       []corev1.Volume{{Name: "volume", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: PVCName}}}, tokenVol},
			SecurityContext:               sc,
			TerminationGracePeriodSeconds: ptr.To(terminationGrace),
			AutomountServiceAccountToken:  ptr.To(false),
		},
	}
}

// onNode requires a pod to run on node. It is set on the pod, outside the container spec, so it
// does not change the spec-hash.
func onNode(node string) *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
		}}},
	}}
}

// desiredAgentService gives the agent the address the operator and API server call:
// http://volume-agent.<namespace>.svc:8443.
func desiredAgentService(ns string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: AgentName, Namespace: ns, Labels: managedLabels(ComponentAgent)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{LabelComponent: ComponentAgent},
			Ports: []corev1.ServicePort{{
				Name: "api", Port: int32(agentapi.DefaultPort), TargetPort: intstr.FromInt(agentapi.DefaultPort), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// specHash is a short hash of a container spec.
func specHash(c corev1.Container) string {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // a corev1.Container always marshals
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
