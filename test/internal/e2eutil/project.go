//go:build e2e || perf

package e2eutil

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// NamespacePrefix is the prefix of project namespaces (spec 4.1).
const NamespacePrefix = "cns-"

// Namespace returns the namespace of a project.
func Namespace(id string) string { return NamespacePrefix + id }

// CreateProject creates a project and returns its ID once the API has accepted it (202); it does
// not wait for Ready (WaitProjectReady). When the test ends it deletes the project, whatever the
// test did, and waits until the namespace is gone, so a failed run leaves nothing for the next
// (plan 5 Review Focus 2). When the test failed it first logs what the namespace holds.
func CreateProject(t testing.TB, c *Client, displayName string) string {
	t.Helper()
	var p Project
	c.Expect(t, http.MethodPost, "/v1/projects", map[string]any{"displayName": displayName}, &p, http.StatusAccepted)
	if p.ID == "" || p.Phase != "Provisioning" {
		t.Fatalf("create project: want an ID and phase Provisioning, got %+v", p)
	}
	t.Cleanup(func() { deleteProject(t, c, p.ID) })
	t.Cleanup(func() {
		if t.Failed() {
			Diagnose(t, Namespace(p.ID))
		}
	})
	return p.ID
}

// deleteProject deletes the project through the API and waits for its namespace to go. When the
// API cannot be reached (its port-forward died with the test) it deletes the namespace with
// kubectl, which the development kubeconfig may do.
func deleteProject(t testing.TB, c *Client, id string) {
	ns := Namespace(id)
	r, err := c.Call(http.MethodDelete, ProjectPath(id), nil)
	switch {
	case err != nil:
		t.Logf("cleanup: delete project %s through the API: %v; deleting namespace %s with kubectl", id, err, ns)
		deleteNamespace(t, ns)
	case r.Status == http.StatusAccepted, r.Status == http.StatusNotFound:
	default:
		t.Errorf("cleanup: delete project %s: %s; deleting namespace %s with kubectl", id, r, ns)
		deleteNamespace(t, ns)
	}
	if err := waitNamespaceGone(ns, 5*time.Minute); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}

func deleteNamespace(t testing.TB, ns string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := KubectlContext(ctx, "delete", "namespace", ns, "--ignore-not-found", "--wait=false"); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}

// WaitNamespaceGone waits until the namespace no longer exists.
func WaitNamespaceGone(t testing.TB, ns string, timeout time.Duration) {
	t.Helper()
	if err := waitNamespaceGone(ns, timeout); err != nil {
		t.Fatal(err)
	}
}

func waitNamespaceGone(ns string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := KubectlContext(ctx, "get", "namespace", ns, "--ignore-not-found", "-o", "jsonpath={.status.phase}")
		cancel()
		if err == nil && out == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %s still there after %s (phase %q, err %v)", ns, timeout, out, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// GetProject reads a project.
func GetProject(t testing.TB, c *Client, id string) Project {
	t.Helper()
	var p Project
	c.Expect(t, http.MethodGet, ProjectPath(id), nil, &p, http.StatusOK)
	return p
}

// WaitProjectReady polls the project until it is Ready and fails the test when it fails or
// timeout passes.
func WaitProjectReady(t testing.TB, c *Client, id string, timeout time.Duration) Project {
	t.Helper()
	var p Project
	Eventually(t, timeout, time.Second, "project "+id+" Ready", func() (bool, string) {
		p = GetProject(t, c, id)
		if p.Phase == "Failed" {
			t.Fatalf("project %s failed: %s: %s", id, p.Reason, p.Message)
		}
		return p.Phase == "Ready", fmt.Sprintf("%s %s %s", p.Phase, p.Reason, p.Message)
	})
	return p
}

// GetBranch reads a branch.
func GetBranch(t testing.TB, c *Client, id, branch string) Branch {
	t.Helper()
	var b Branch
	c.Expect(t, http.MethodGet, BranchPath(id, branch), nil, &b, http.StatusOK)
	return b
}

// WaitBranchPhase polls the branch until its phase is phase and fails the test when it turns
// Failed instead or timeout passes.
func WaitBranchPhase(t testing.TB, c *Client, id, branch, phase string, timeout time.Duration) Branch {
	t.Helper()
	var b Branch
	Eventually(t, timeout, 250*time.Millisecond, fmt.Sprintf("branch %s/%s %s", id, branch, phase), func() (bool, string) {
		b = GetBranch(t, c, id, branch)
		if b.Phase == "Failed" && phase != "Failed" {
			t.Fatalf("branch %s/%s failed: %s: %s", id, branch, b.Reason, b.Message)
		}
		return b.Phase == phase, fmt.Sprintf("%s %s %s", b.Phase, b.Reason, b.Message)
	})
	return b
}

// WaitBranchGone polls the branch until the API answers 404.
func WaitBranchGone(t testing.TB, c *Client, id, branch string, timeout time.Duration) {
	t.Helper()
	Eventually(t, timeout, 500*time.Millisecond, fmt.Sprintf("branch %s/%s gone", id, branch), func() (bool, string) {
		r := c.Do(t, http.MethodGet, BranchPath(id, branch), nil)
		return r.Status == http.StatusNotFound, r.String()
	})
}

// GetConnection reads the connection of a branch.
func GetConnection(t testing.TB, c *Client, id, branch string) Connection {
	t.Helper()
	var conn Connection
	c.Expect(t, http.MethodGet, BranchPath(id, branch)+"/connection", nil, &conn, http.StatusOK)
	return conn
}

// Diagnose logs what a namespace holds and the logs of its pods and of the operator, for a
// failed test, before its cleanup deletes the project.
func Diagnose(t testing.TB, ns string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := func(args ...string) {
		out, err := KubectlContext(ctx, args...)
		if err != nil {
			t.Logf("diagnose: %v", err)
			return
		}
		t.Logf("diagnose: kubectl %s\n%s", strings.Join(args, " "), out)
	}
	run("-n", ns, "get", "projects,branches,pods,services,pvc,networkpolicies", "-o", "wide")
	run("-n", ns, "get", "events", "--sort-by=.lastTimestamp")
	run("-n", ns, "get", "branches", "-o", "yaml")
	pods, _ := KubectlContext(ctx, "-n", ns, "get", "pods", "-o", "jsonpath={.items[*].metadata.name}")
	for _, pod := range strings.Fields(pods) {
		run("-n", ns, "logs", pod, "--tail=60")
	}
	run("-n", SystemNamespace, "logs", "deployment/consonance-operator", "--tail=100")
	run("-n", SystemNamespace, "logs", "-l", "app.kubernetes.io/name=consonance-apiserver", "--tail=40", "--prefix")
}
