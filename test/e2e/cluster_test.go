//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"consonance/test/internal/e2eutil"
)

// readyProject creates a project for the test and waits until it is Ready.
func readyProject(t *testing.T, c *e2eutil.Client, name string) string {
	t.Helper()
	id := e2eutil.CreateProject(t, c, name)
	e2eutil.WaitProjectReady(t, c, id, 5*time.Minute)
	return id
}

// TestICUCollation checks that the branch image's PostgreSQL has ICU and sorts by it (spec 7.6).
func TestICUCollation(t *testing.T) {
	c := e2eutil.API(t)
	id := readyProject(t, c, "e2e-icu")
	db := e2eutil.SQL(t, e2eutil.GetConnection(t, c, id, "main"))

	got := e2eutil.QueryStrings(t, db,
		`SELECT collname::text FROM pg_collation WHERE collprovider = 'i' AND collname IN ('ko-x-icu', 'und-x-icu') ORDER BY collname`)
	if want := []string{"ko-x-icu", "und-x-icu"}; !slices.Equal(got, want) {
		t.Errorf("ICU collations %v, want %v", got, want)
	}

	got = e2eutil.QueryStrings(t, db,
		`SELECT w FROM (VALUES ('zebra'), ('나비'), ('éclair'), ('apple'), ('가방')) AS v(w) ORDER BY w COLLATE "und-x-icu"`)
	if want := []string{"apple", "éclair", "zebra", "가방", "나비"}; !slices.Equal(got, want) {
		t.Errorf("ORDER BY und-x-icu: %v, want %v", got, want)
	}
}

// TestPodsAreRestricted checks that the project namespace and consonance-system enforce Pod
// Security restricted, that the enforcement is live, and that every pod of a project (the
// agent, main and a branch) and of the system was admitted and runs (plan 5 Review Focus 3).
func TestPodsAreRestricted(t *testing.T) {
	c := e2eutil.API(t)
	id := readyProject(t, c, "e2e-pss")
	ns := e2eutil.Namespace(id)
	c.Expect(t, http.MethodPost, e2eutil.ProjectPath(id)+"/branches", map[string]any{"name": "feature"}, nil,
		http.StatusCreated, http.StatusAccepted)
	e2eutil.WaitBranchPhase(t, c, id, "feature", "Running", 3*time.Minute)

	for _, n := range []string{ns, e2eutil.SystemNamespace} {
		level := e2eutil.Kubectl(t, "get", "namespace", n, "-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`)
		if level != "restricted" {
			t.Errorf("namespace %s: pod-security.kubernetes.io/enforce=%q, want restricted", n, level)
		}
	}

	// A privileged pod is refused: the label is enforced, not only written.
	privileged := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: pss-check
  namespace: %s
spec:
  containers:
  - name: c
    image: consonance/postgres:17-dev
    imagePullPolicy: Never
    securityContext:
      privileged: true
`, ns)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := e2eutil.KubectlInput(ctx, []byte(privileged), "apply", "--dry-run=server", "-f", "-")
	if err == nil || !strings.Contains(err.Error(), "violates PodSecurity") {
		t.Errorf("privileged pod in %s: want a PodSecurity refusal, got %q, %v", ns, out, err)
	}

	pods := map[string][]string{
		ns:                      {"volume-agent", "br-main", "br-feature"},
		e2eutil.SystemNamespace: nil, // whatever runs there
	}
	for n, want := range pods {
		// A pod being deleted (an old ReplicaSet's, after deploy-dev restarts the deployments)
		// is left out: it was admitted, and is on its way out.
		out := e2eutil.Kubectl(t, "-n", n, "get", "pods", "-o",
			`jsonpath={range .items[*]}{.metadata.name}{"|"}{.status.phase}{"|"}{.status.conditions[?(@.type=="Ready")].status}{"|"}{.metadata.deletionTimestamp}{"\n"}{end}`)
		running := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(line, "|")
			if len(f) != 4 || f[3] != "" { // an empty line, or a pod being deleted
				continue
			}
			if f[1] != "Running" || f[2] != "True" {
				t.Errorf("pod %s/%s is not running and ready: %q", n, f[0], line)
				continue
			}
			running[f[0]] = true
		}
		if len(running) == 0 {
			t.Errorf("namespace %s has no running pod", n)
		}
		for _, p := range want {
			if !running[p] {
				t.Errorf("pod %s/%s is not running and ready; pods: %q", n, p, out)
			}
		}
	}
}

// TestNetworkPolicies checks spec 8's policies in practice: the probes of the agent and the
// branch get through, the agent resolves names, the agent reaches the branch's PostgreSQL, and a
// pod of another project does not.
func TestNetworkPolicies(t *testing.T) {
	c := e2eutil.API(t)
	id := readyProject(t, c, "e2e-netpol")
	other := readyProject(t, c, "e2e-netpol-other")
	ns, otherNS := e2eutil.Namespace(id), e2eutil.Namespace(other)
	host := "br-main." + ns + ".svc"

	// The kubelet's probes are not blocked by default-deny-ingress.
	e2eutil.Kubectl(t, "-n", ns, "wait", "--for=condition=Ready", "pod/volume-agent", "pod/br-main", "--timeout=2m")

	// The agent's egress allows DNS and its own namespace.
	if out := e2eutil.Kubectl(t, "-n", ns, "exec", "volume-agent", "--", "getent", "hosts", "kubernetes.default.svc"); out == "" {
		t.Errorf("agent: getent hosts kubernetes.default.svc printed nothing")
	}
	e2eutil.Kubectl(t, "-n", ns, "exec", "volume-agent", "--", "pg_isready", "-h", host, "-p", "5432", "-t", "5")

	// A pod in another project's namespace, Pod Security restricted, resolves the branch and
	// reaches the API server (so its network works) but not the branch's 5432.
	script := fmt.Sprintf(`getent hosts %[1]s >/dev/null || exit 3
timeout 5 bash -c 'exec 3<>/dev/tcp/consonance-apiserver.consonance-system.svc/8080' || exit 4
if timeout 5 bash -c 'exec 3<>/dev/tcp/%[1]s/5432'; then exit 0; fi
exit 1`, host)
	code := runProbe(t, otherNS, "netpol-probe", script)
	switch code {
	case "1":
	case "0":
		t.Errorf("a pod in %s connected to %s:5432", otherNS, host)
	case "3":
		t.Errorf("the probe pod in %s could not resolve %s", otherNS, host)
	case "4":
		t.Errorf("the probe pod in %s could not reach the API server: its network does not work", otherNS)
	default:
		t.Errorf("probe pod exit code %q", code)
	}
}

// runProbe runs script with bash in a one-off pod of the branch image, under Pod Security
// restricted as a non-root user, and returns its exit code.
func runProbe(t *testing.T, ns, name, script string) string {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    runAsGroup: 65534
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: probe
    image: consonance/postgres:17-dev
    imagePullPolicy: Never
    command: ["bash", "-c", %q]
    securityContext:
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
`, name, ns, script)
	e2eutil.KubectlApply(t, manifest)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := e2eutil.KubectlContext(ctx, "-n", ns, "delete", "pod", name, "--ignore-not-found", "--wait=false"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	var code string
	e2eutil.Eventually(t, 2*time.Minute, time.Second, "probe pod "+ns+"/"+name+" done", func() (bool, string) {
		out := e2eutil.Kubectl(t, "-n", ns, "get", "pod", name, "-o",
			`jsonpath={.status.phase} {.status.containerStatuses[0].state.terminated.exitCode}`)
		f := strings.Fields(out)
		if len(f) == 2 && (f[0] == "Succeeded" || f[0] == "Failed") {
			code = f[1]
			return true, out
		}
		return false, out
	})
	logs, _ := e2eutil.KubectlContext(context.Background(), "-n", ns, "logs", name)
	t.Logf("probe pod %s/%s exited %s; output: %q", ns, name, code, logs)
	return code
}
