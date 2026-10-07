package pgctl_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/testutil/pgtest"
)

// newTestManager returns a stopped Manager over a fresh data directory.
func newTestManager(t *testing.T) *pgctl.Manager {
	t.Helper()
	bin := pgtest.BinDir(t)
	m := pgctl.NewManager(filepath.Join(bin, "postgres"), pgctl.Options{
		DataDir:        pgtest.InitDB(t, bin),
		SocketDir:      pgtest.SocketDir(t),
		Port:           pgtest.FreePort(t),
		MaxConnections: 20,
	})
	t.Cleanup(func() { _ = m.Stop(context.Background(), 5*time.Second) })
	return m
}

// startedManager returns a running Manager.
func startedManager(t *testing.T) *pgctl.Manager {
	t.Helper()
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	return m
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestStartStopRestart(t *testing.T) {
	m := newTestManager(t)
	ctx := testCtx(t)

	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if s, _ := m.State(); s != pgctl.StateRunning {
		t.Fatalf("state = %s, want running", s)
	}
	if !m.Ready(ctx) {
		t.Fatal("not ready after start")
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("second start must be a no-op: %v", err)
	}
	if err := m.Stop(ctx, 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if s, _ := m.State(); s != pgctl.StateStopped {
		t.Fatalf("state = %s, want stopped", s)
	}
	if m.Ready(ctx) {
		t.Fatal("ready after stop")
	}
	if err := m.Stop(ctx, time.Second); err != nil {
		t.Fatalf("second stop must be a no-op: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

func TestCrashMovesToStopped(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)

	// Kill the whole postgres process group, as an OOM kill would.
	if err := syscall.Kill(-m.PID(), syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := m.State(); s == pgctl.StateStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state did not become stopped after the crash")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.Ready(ctx) {
		t.Fatal("ready after crash")
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start after crash: %v", err)
	}
}

func TestStartFailsOnMissingDataDir(t *testing.T) {
	bin := pgtest.BinDir(t)
	m := pgctl.NewManager(filepath.Join(bin, "postgres"), pgctl.Options{
		DataDir:        filepath.Join(t.TempDir(), "current"), // nothing there
		SocketDir:      pgtest.SocketDir(t),
		Port:           pgtest.FreePort(t),
		MaxConnections: 20,
	})
	err := m.Start(testCtx(t))
	if err == nil {
		t.Fatal("Start succeeded on a missing data directory")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("unexpected error: %v", err)
	}
	if s, _ := m.State(); s != pgctl.StateStopped {
		t.Fatalf("state = %s, want stopped", s)
	}
}

func TestStopWhileStarting(t *testing.T) {
	m := newTestManager(t)
	ctx := testCtx(t)

	startErr := make(chan error, 1)
	go func() { startErr <- m.Start(ctx) }()
	for m.PID() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := m.Stop(ctx, 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	<-startErr // nil (became ready first) or an exit error; both are fine
	if s, _ := m.State(); s != pgctl.StateStopped {
		t.Fatalf("state = %s, want stopped", s)
	}
}

func TestConcurrentStartLaunchesOnePostmaster(t *testing.T) {
	m := newTestManager(t)
	ctx := testCtx(t)

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- m.Start(ctx) }()
	}
	ok := 0
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case errors.Is(err, pgctl.ErrBusy):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok == 0 {
		t.Fatal("no start succeeded")
	}
	if s, _ := m.State(); s != pgctl.StateRunning {
		t.Fatalf("state = %s, want running", s)
	}
}
