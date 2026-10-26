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
	"slices"
	"testing"
	"time"

	"consonance/test/internal/e2eutil"
)

func TestMain(m *testing.M) {
	if err := e2eutil.CheckCluster(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: the development cluster is not ready: %v\n", err)
		os.Exit(1)
	}
	before, err := projectNamespaces()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: list project namespaces: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if !leavesNothingBehind(before) {
		code = 1
	}
	os.Exit(code)
}

func projectNamespaces() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return e2eutil.ProjectNamespaces(ctx)
}

// leavesNothingBehind is TestE2ELeavesNothingBehind: after every test, whether it passed or not,
// no cns- namespace made during the run is left (plan 5 Review Focus 2). Namespaces that were
// there before (another user's projects, a performance run) are not this run's to judge. It runs
// in TestMain because it has to come after the other tests and their cleanups.
func leavesNothingBehind(before []string) bool {
	const name = "TestE2ELeavesNothingBehind"
	fmt.Printf("=== RUN   %s\n", name)
	start := time.Now()
	now, err := projectNamespaces()
	elapsed := time.Since(start).Seconds()
	if err != nil {
		fmt.Printf("    %v\n--- FAIL: %s (%.2fs)\n", err, name, elapsed)
		return false
	}
	var left []string
	for _, ns := range now {
		if !slices.Contains(before, ns) {
			left = append(left, ns)
		}
	}
	if len(left) > 0 {
		fmt.Printf("    project namespaces left behind: %v\n--- FAIL: %s (%.2fs)\n", left, name, elapsed)
		return false
	}
	if len(before) > 0 {
		fmt.Printf("    not judged, there before the run: %v\n", before)
	}
	fmt.Printf("--- PASS: %s (%.2fs)\n", name, elapsed)
	return true
}
