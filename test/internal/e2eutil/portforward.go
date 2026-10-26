//go:build e2e || perf

package e2eutil

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// PortForward forwards a free local port to port of target (svc/<name> or pod/<name>) in ns with
// a kubectl port-forward subprocess, waits until it listens, and stops it in t.Cleanup. It
// retries for a minute: the pod behind a Service may not run yet, and another process may take
// the free port between the check and kubectl's bind.
//
// kubectl port-forward reaches the port inside the pod's network namespace through the kubelet,
// not over the pod network, so the project's NetworkPolicies do not apply to it; the E2E checks
// them with a pod instead (TestNetworkPolicies). A forward to a Service picks one pod when it
// starts and dies with it: forward again after the pod is replaced.
func PortForward(t testing.TB, ns, target string, port int) int {
	t.Helper()
	mustLoad(t)
	deadline := time.Now().Add(time.Minute)
	for {
		local, stop, err := startPortForward(ns, target, port)
		if err == nil {
			t.Cleanup(stop)
			return local
		}
		if time.Now().After(deadline) {
			t.Fatalf("port-forward %s/%s %d: %v", ns, target, port, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func startPortForward(ns, target string, port int) (int, func(), error) {
	local, err := freePort()
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", env.kubeconfig, "-n", ns, "port-forward",
		"--address", "127.0.0.1", target, fmt.Sprintf("%d:%d", local, port))
	cmd.Env = append(os.Environ(), "KUBECONFIG="+env.kubeconfig)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		cancel()
		return 0, nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop := func() { cancel(); <-exited }

	// kubectl prints "Forwarding from 127.0.0.1:<port>" once it listens.
	ready := "Forwarding from 127.0.0.1:" + strconv.Itoa(local)
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
			cancel()
			return 0, nil, fmt.Errorf("kubectl exited: %s", strings.TrimSpace(out.String()))
		case <-timer.C:
			stop()
			return 0, nil, fmt.Errorf("not listening after 30s: %s", strings.TrimSpace(out.String()))
		case <-tick.C:
			if strings.Contains(out.String(), ready) {
				return local, stop, nil
			}
		}
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// lockedBuffer is kubectl's output, written by the process and read by the waiter.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
