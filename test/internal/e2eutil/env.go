//go:build e2e || perf

// Package e2eutil is what the E2E tests (test/e2e) and the performance measurements (test/perf)
// share: the paths of the development cluster, kubectl, port-forwards, an API client, the
// project lifecycle and SQL through a port-forward to a branch. Both run against the k3s that
// make dev-cluster starts and make deploy-dev fills; neither skips when it is missing, because
// they only run when asked for.
package e2eutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// SystemNamespace is where the operator and the API server run.
const SystemNamespace = "consonance-system"

// paths of the development cluster, found once from the working directory (the package's).
var env struct {
	once       sync.Once
	root       string
	kubeconfig string
	adminKey   string
	err        error
}

func load() error {
	env.once.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			env.err = err
			return
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				env.err = errors.New("no go.mod above the working directory")
				return
			}
			dir = parent
		}
		env.root = dir
		// Always the development kubeconfig, never an ambient KUBECONFIG: these tests create and
		// delete namespaces (the same rule as hack/deploy-dev.sh).
		env.kubeconfig = filepath.Join(dir, "bin", "dev", "kubeconfig")
		if _, err := os.Stat(env.kubeconfig); err != nil {
			env.err = fmt.Errorf("no development kubeconfig (make dev-cluster): %w", err)
			return
		}
		key, err := os.ReadFile(filepath.Join(dir, "bin", "dev", "admin-key"))
		if err != nil {
			env.err = fmt.Errorf("no admin key (make deploy-dev): %w", err)
			return
		}
		env.adminKey = strings.TrimSpace(string(key))
		if env.adminKey == "" {
			env.err = errors.New("bin/dev/admin-key is empty")
		}
	})
	return env.err
}

func mustLoad(t testing.TB) {
	t.Helper()
	if err := load(); err != nil {
		t.Fatal(err)
	}
}

// CheckCluster tells whether the development cluster is there with the API server and the
// operator deployed. TestMain calls it so that a missing cluster fails the run instead of every
// test failing on its own.
func CheckCluster() error {
	if err := load(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, d := range []string{"consonance-apiserver", "consonance-operator"} {
		out, err := KubectlContext(ctx, "-n", SystemNamespace, "get", "deployment", d,
			"-o", "jsonpath={.status.availableReplicas}")
		if err != nil {
			return fmt.Errorf("deployment %s/%s (make dev-cluster images load-images deploy-dev): %w", SystemNamespace, d, err)
		}
		if out == "" || out == "0" {
			return fmt.Errorf("deployment %s/%s has no available replica", SystemNamespace, d)
		}
	}
	return nil
}

// KubectlContext runs kubectl against the development cluster and returns its trimmed standard
// output; the error carries standard error.
func KubectlContext(ctx context.Context, args ...string) (string, error) {
	return KubectlInput(ctx, nil, args...)
}

// KubectlInput is KubectlContext with stdin as kubectl's standard input.
func KubectlInput(ctx context.Context, stdin []byte, args ...string) (string, error) {
	if err := load(); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", env.kubeconfig}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+env.kubeconfig)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Kubectl runs kubectl with a two-minute limit and fails the test when it fails.
func Kubectl(t testing.TB, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := KubectlContext(ctx, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// KubectlApply applies a manifest from memory.
func KubectlApply(t testing.TB, manifest string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := KubectlInput(ctx, []byte(manifest), "apply", "-f", "-"); err != nil {
		t.Fatal(err)
	}
}

// ProjectNamespaces lists the cns- namespaces, those that are terminating included.
func ProjectNamespaces(ctx context.Context) ([]string, error) {
	out, err := KubectlContext(ctx, "get", "namespaces", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Fields(out) {
		if strings.HasPrefix(n, NamespacePrefix) {
			names = append(names, n)
		}
	}
	return names, nil
}

// Eventually polls cond every interval until it is true or timeout passes, and then fails the
// test with what describes the last state.
func Eventually(t testing.TB, timeout, interval time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, state := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached in %s; last: %s", what, timeout, state)
		}
		time.Sleep(interval)
	}
}
