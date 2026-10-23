package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"consonance/internal/testutil/kubetest"
)

// agentPodWith runs ensureAgent on a fake API server holding objs and returns the agent pod it
// made.
func agentPodWith(t *testing.T, objs ...client.Object) corev1.Pod {
	t.Helper()
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kubetest.Scheme()).WithObjects(objs...).Build()
	r := &ProjectReconciler{Client: c, Scheme: c.Scheme(), AgentImage: testAgentImage}
	p := testProject("cns-x")
	if _, err := r.ensureAgent(ctx, p, "x"); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: AgentName}, &pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

// runningBranchPod is a branch pod that holds the volume on node.
func runningBranchPod(branch, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: BranchObjectName(branch), Namespace: "cns-x", Labels: branchLabels(branch)},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: containerName, Image: testPostgresImage}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// A new agent pod goes to the node of the running branch pods: on network storage the RWO volume
// cannot leave the node it is attached to.
func TestANewAgentPodFollowsTheRunningBranchPods(t *testing.T) {
	pod := agentPodWith(t, runningBranchPod("dev", "node-a"))
	aff := pod.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("affinity %+v, want a required node affinity", aff)
	}
	terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
		t.Fatalf("node selector terms %+v", terms)
	}
	e := terms[0].MatchExpressions[0]
	if e.Key != "kubernetes.io/hostname" || e.Operator != corev1.NodeSelectorOpIn || len(e.Values) != 1 || e.Values[0] != "node-a" {
		t.Errorf("node selector %+v, want kubernetes.io/hostname In [node-a]", e)
	}
	if aff.PodAffinity != nil || aff.PodAntiAffinity != nil {
		t.Errorf("affinity %+v", aff)
	}

	// A branch pod on its way out holds the volume for no one to follow.
	leaving := runningBranchPod("dev", "node-a")
	leaving.Finalizers = []string{"test.consonance.io/hold"}
	leaving.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	if pod := agentPodWith(t, leaving); pod.Spec.Affinity != nil {
		t.Errorf("affinity %+v with only a terminating branch pod", pod.Spec.Affinity)
	}

	if pod := agentPodWith(t); pod.Spec.Affinity != nil {
		t.Errorf("affinity %+v with no branch pods", pod.Spec.Affinity)
	}
}
