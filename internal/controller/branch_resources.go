package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"consonance/api/v1alpha1"
	"consonance/internal/supervisorapi"
)

const (
	postgresPort  = 5432
	dbSecretDir   = "/etc/consonance/db" // main's supervisor reads the tenant password here
	containerName = "postgres"
)

// branchLabels select a branch's pod.
func branchLabels(branch string) map[string]string {
	l := managedLabels(ComponentBranch)
	l[LabelBranch] = branch
	return l
}

// branchCompute is the branch's own compute, or the project's default (spec 4.3).
func branchCompute(p *v1alpha1.Project, b *v1alpha1.Branch) v1alpha1.ComputeSpec {
	if b.Spec.Compute != nil {
		return *b.Spec.Compute
	}
	return p.Spec.DefaultCompute
}

// desiredBranchPod runs a branch: the supervisor, which runs PostgreSQL on /pgdata/current
// (spec 7.3, 7.5).
func desiredBranchPod(p *v1alpha1.Project, b *v1alpha1.Branch, image string) *corev1.Pod {
	compute := branchCompute(p, b)
	res := corev1.ResourceList{corev1.ResourceCPU: compute.CPU, corev1.ResourceMemory: compute.Memory}
	tokenVol, tokenMount := tokenVolume()
	autostart := "true"
	if b.Spec.State == v1alpha1.StateStopped {
		autostart = "false"
	}
	c := corev1.Container{
		Name:            containerName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			{Name: "postgres", ContainerPort: postgresPort},
			{Name: "supervisor", ContainerPort: int32(supervisorapi.DefaultPort)},
		},
		Env: []corev1.EnvVar{
			// shared_buffers is a quarter of this (spec 7.3); the limit is always set, or the
			// Downward API would pass the node's memory (carry-over from plan 1).
			{Name: "MEMORY_LIMIT_BYTES", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
				ContainerName: containerName, Resource: "limits.memory", Divisor: resource.MustParse("1"),
			}}},
			// The state at pod creation; later changes go through /v1/start and /v1/stop.
			{Name: "AUTOSTART", Value: autostart},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "pgdata", MountPath: "/pgdata", SubPath: "branches/" + b.Name},
			{Name: "run", MountPath: "/var/run/postgresql"},
			tokenMount,
		},
		Resources:       corev1.ResourceRequirements{Requests: res, Limits: res},
		ReadinessProbe:  httpProbe("/readyz", supervisorapi.DefaultPort, 2),
		LivenessProbe:   httpProbe("/healthz", supervisorapi.DefaultPort, 10),
		SecurityContext: restrictedContainer(),
	}
	volumes := []corev1.Volume{
		{Name: "pgdata", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: PVCName}}},
		{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		tokenVol,
	}
	if isMain(b) {
		// Only main bootstraps the tenant login (spec 4.5, 7.5).
		c.Args = []string{"--db-password-file=" + dbSecretDir + "/" + PasswordKey}
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "db", MountPath: dbSecretDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "db", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: DBCredentialsSecret, Items: []corev1.KeyToPath{{Key: PasswordKey, Path: PasswordKey}},
		}}})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: BranchObjectName(b.Name), Namespace: b.Namespace, Labels: branchLabels(b.Name)},
		Spec: corev1.PodSpec{
			Containers:      []corev1.Container{c},
			Volumes:         volumes,
			SecurityContext: restrictedPod(),
			// The volume is RWO: the branch runs where the agent mounted it (spec 7.5).
			Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: ComponentAgent}},
					TopologyKey:   "kubernetes.io/hostname",
				}},
			}},
			TerminationGracePeriodSeconds: ptr.To(terminationGrace),
			AutomountServiceAccountToken:  ptr.To(false),
		},
	}
}

// desiredBranchService exposes PostgreSQL and the supervisor (spec 7.1 Branch step 2). It
// publishes the pod while it is not ready: the readiness probe fails while PostgreSQL is
// stopped, and the operator and the agent must still reach the supervisor to start it or to
// check that it stopped.
func desiredBranchService(b *v1alpha1.Branch) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: BranchObjectName(b.Name), Namespace: b.Namespace, Labels: branchLabels(b.Name)},
		Spec: corev1.ServiceSpec{
			Selector:                 map[string]string{LabelComponent: ComponentBranch, LabelBranch: b.Name},
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: "postgres", Port: postgresPort, TargetPort: intstr.FromInt(postgresPort), Protocol: corev1.ProtocolTCP},
				{Name: "supervisor", Port: int32(supervisorapi.DefaultPort), TargetPort: intstr.FromInt(supervisorapi.DefaultPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// branchEndpoint is where clients reach a branch's PostgreSQL.
func branchEndpoint(b *v1alpha1.Branch) *v1alpha1.Endpoint {
	return &v1alpha1.Endpoint{Host: fmt.Sprintf("%s.%s.svc", BranchObjectName(b.Name), b.Namespace), Port: postgresPort}
}

// backupBranchName names the branch that keeps the generation a rollback replaced:
// <first 18 characters of the branch>-before-<UTC yyyymmddhhmmss>, at most 40 characters
// (spec 9.4).
func backupBranchName(branch string, at time.Time) string {
	prefix := branch
	if len(prefix) > 18 {
		prefix = prefix[:18]
	}
	return prefix + "-before-" + at.UTC().Format("20060102150405")
}
