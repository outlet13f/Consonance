//go:build e2e

// Package e2e runs the branching engine end to end on the development k3s (make dev-cluster
// images load-images deploy-dev): spec 12's scenario, ICU collations, Pod Security restricted
// and the network policies. Run it with make e2e. Without the cluster it fails rather than
// skips, because nobody runs it by accident.
package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"consonance/test/internal/e2eutil"
)

func TestMain(m *testing.M) {
	if err := e2eutil.CheckCluster(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: the development cluster is not ready: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if !leavesNothingBehind() {
		code = 1
	}
	os.Exit(code)
}

// leavesNothingBehind is TestE2ELeavesNothingBehind: after every test, whether it passed or not,
// no cns- namespace is left (plan 5 Review Focus 2). It runs in TestMain because it has to come
// after the other tests and their cleanups.
func leavesNothingBehind() bool {
	const name = "TestE2ELeavesNothingBehind"
	fmt.Printf("=== RUN   %s\n", name)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	left, err := e2eutil.ProjectNamespaces(ctx)
	elapsed := time.Since(start).Seconds()
	switch {
	case err != nil:
		fmt.Printf("    %v\n--- FAIL: %s (%.2fs)\n", err, name, elapsed)
		return false
	case len(left) > 0:
		fmt.Printf("    project namespaces left behind: %v\n--- FAIL: %s (%.2fs)\n", left, name, elapsed)
		return false
	}
	fmt.Printf("--- PASS: %s (%.2fs)\n", name, elapsed)
	return true
}
