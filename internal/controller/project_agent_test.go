package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"consonance/api/v1alpha1"
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
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 || len(terms[0].MatchExpressions) != 0 {
		t.Fatalf("node selector terms %+v, want one field term", terms)
	}
	f := terms[0].MatchFields[0]
	if f.Key != metav1.ObjectNameField || f.Operator != corev1.NodeSelectorOpIn || len(f.Values) != 1 || f.Values[0] != "node-a" {
		t.Errorf("node selector %+v, want metadata.name In [node-a]", f)
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

// An agent pod pinned to a node that no running branch pod is on any more, and that cannot be
// scheduled there, is made again; one whose branch pods still run there waits for its node.
func TestAnAgentPodPinnedToAGoneNodeIsMadeAgain(t *testing.T) {
	ctx := context.Background()
	p := testProject("cns-x")
	stuck := func() *corev1.Pod {
		pod := desiredAgentPod(p, "x", testAgentImage)
		pod.Spec.Affinity = onNode("node-gone")
		pod.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.",
		}}}
		return pod
	}
	run := func(objs ...client.Object) (client.Client, bool) {
		t.Helper()
		c := fake.NewClientBuilder().WithScheme(kubetest.Scheme()).WithObjects(objs...).Build()
		r := &ProjectReconciler{Client: c, Scheme: c.Scheme(), AgentImage: testAgentImage}
		scheduled, err := r.ensureAgent(ctx, p, "x")
		if err != nil {
			t.Fatal(err)
		}
		return c, scheduled
	}
	key := client.ObjectKey{Namespace: p.Namespace, Name: AgentName}

	c, _ := run(stuck())
	if err := c.Get(ctx, key, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("the agent pod pinned to a node with no branch pods is still there: %v", err)
	}

	c, scheduled := run(stuck(), runningBranchPod("dev", "node-gone"))
	if err := c.Get(ctx, key, &corev1.Pod{}); err != nil {
		t.Errorf("the agent pod whose branch pods still run on its node: %v", err)
	}
	if scheduled || cond(p.Status.Conditions, v1alpha1.ConditionAgentReady) != "False/Unschedulable" {
		t.Errorf("scheduled %v, AgentReady %s", scheduled, cond(p.Status.Conditions, v1alpha1.ConditionAgentReady))
	}
}
