package main_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
	"consonance/internal/testutil/pgtest"
)

func buildSupervisor(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "consonance-supervisor")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// supervisorProc is a supervisor binary started by a test, with its output in a log file.
type supervisorProc struct {
	cmd     *exec.Cmd
	exited  chan struct{} // closed once the process has exited; waitErr is set by then
	waitErr error
	logPath string
}

// startSupervisor runs the supervisor binary bin with args. The process is killed when the test
// ends, and its output is logged if the test failed.
func startSupervisor(t *testing.T, bin string, args ...string) *supervisorProc {
	t.Helper()
	p := &supervisorProc{exited: make(chan struct{}), logPath: filepath.Join(t.TempDir(), "supervisor.log")}
	logFile, err := os.Create(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	p.cmd = exec.Command(bin, args...)
	p.cmd.Stdout, p.cmd.Stderr = logFile, logFile
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.waitErr = p.cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill() // no-op once it has exited
		<-p.exited
		if t.Failed() {
			t.Logf("supervisor output:\n%s", p.logs())
		}
	})
	return p
}

// exitedWithin waits up to d for the process to exit and reports whether it did.
func (p *supervisorProc) exitedWithin(d time.Duration) bool {
	select {
	case <-p.exited:
		return true
	case <-time.After(d):
		return false
	}
}

func (p *supervisorProc) logs() string {
	b, err := os.ReadFile(p.logPath)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func TestSupervisorLifecycle(t *testing.T) {
	bin := pgtest.BinDir(t)
	root := t.TempDir()
	pgtest.InitDBAt(t, bin, filepath.Join(root, "data-1"))
	current := filepath.Join(root, "current")
	if err := os.Symlink("data-1", current); err != nil {
		t.Fatal(err)
	}
	tokenFile := writeFile(t, filepath.Join(root, "token"), "tok\n")
	pwFile := writeFile(t, filepath.Join(root, "password"), "pw\n")
	apiPort := pgtest.FreePort(t)

	cmd := exec.Command(buildSupervisor(t),
		"--postgres-bin", filepath.Join(bin, "postgres"),
		"--data-dir", current,
		"--socket-dir", pgtest.SocketDir(t),
		"--port", strconv.Itoa(pgtest.FreePort(t)),
		"--listen", "127.0.0.1:"+strconv.Itoa(apiPort),
		"--token-file", tokenFile,
		"--db-password-file", pwFile,
		"--autostart=true",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := supervisorclient.New("http://127.0.0.1:"+strconv.Itoa(apiPort), "tok", nil)

	// autostart brings postgres up on the generation that current points to.
	var st supervisorapi.Status
	eventually(t, 30*time.Second, func() bool {
		var err error
		st, err = c.Status(ctx)
		return err == nil && st.PG == supervisorapi.PGRunning
	})
	if st.Generation != "data-1" {
		t.Fatalf("generation = %q, want data-1", st.Generation)
	}
	if _, err := c.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := c.BackupStart(ctx, "it"); err != nil {
		t.Fatalf("backup start: %v", err)
	}
	stop, err := c.BackupStop(ctx)
	if err != nil || !strings.Contains(stop.LabelFile, "LABEL: it") {
		t.Fatalf("backup stop = %+v, %v", stop, err)
	}
	if st, err = c.Stop(ctx); err != nil || st.PG != supervisorapi.PGStopped {
		t.Fatalf("stop = %+v, %v", st, err)
	}
	if st, err = c.Start(ctx); err != nil || st.PG != supervisorapi.PGRunning {
		t.Fatalf("start = %+v, %v", st, err)
	}

	// SIGTERM: fast shutdown, then a clean exit.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("supervisor exit: %v", err)
	}
	out, err := exec.Command(filepath.Join(bin, "pg_controldata"), filepath.Join(root, "data-1")).CombinedOutput()
	if err != nil {
		t.Fatalf("pg_controldata: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "shut down") {
		t.Fatalf("cluster was not shut down cleanly:\n%s", out)
	}
}

// fakePostmaster stands in for a postmaster that is stuck in crash recovery: it records its pid
// and never accepts connections. Like postgres it shuts down cleanly on SIGINT (fast shutdown)
// and dies at once on SIGQUIT (immediate shutdown). The marker files show which of the two the
// supervisor asked for.
const fakePostmaster = `#!/bin/sh
trap ': > "$FAKE_PG_DIR/got-int"; fast=1' INT
trap ': > "$FAKE_PG_DIR/got-quit"; exit 3' QUIT
echo $$ > "$FAKE_PG_DIR/pid.tmp" && mv "$FAKE_PG_DIR/pid.tmp" "$FAKE_PG_DIR/pid"
while [ -z "$fast" ]; do sleep 0.1; done
sleep 0.3
exit 0
`

// TestSupervisorShutsDownWhileAutostartWaits covers a pod that is deleted while postgres is still
// starting (slow crash recovery). The postmaster never becomes ready, so autostart waits out its
// whole --start-timeout unless the signal cuts the wait short. SIGTERM must reach the shutdown
// path at once: the supervisor asks the starting postmaster for a fast shutdown (not an immediate
// one) and exits 0 within seconds instead of after 60s. It needs no PostgreSQL, so it runs on
// every host.
func TestSupervisorShutsDownWhileAutostartWaits(t *testing.T) {
	root := t.TempDir()
	fakePG := writeFile(t, filepath.Join(root, "fake-postgres"), fakePostmaster)
	if err := os.Chmod(fakePG, 0o700); err != nil {
		t.Fatal(err)
	}
	// Start refuses a data directory that does not exist; the fake postmaster needs nothing in it.
	if err := os.Mkdir(filepath.Join(root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "supervisor.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	apiPort := pgtest.FreePort(t)

	cmd := exec.Command(buildSupervisor(t),
		"--postgres-bin", fakePG,
		"--data-dir", filepath.Join(root, "data"),
		"--socket-dir", pgtest.SocketDir(t),
		"--port", strconv.Itoa(pgtest.FreePort(t)),
		"--listen", "127.0.0.1:"+strconv.Itoa(apiPort),
		"--token-file", writeFile(t, filepath.Join(root, "token"), "tok\n"),
		"--autostart=true",
		"--start-timeout=60s",
	)
	cmd.Env = append(os.Environ(), "FAKE_PG_DIR="+root)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	var pid int // the fake postmaster
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // no-op once it has exited
		<-exited
		if t.Failed() {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL) // never leave a postmaster behind
			}
			if out, err := os.ReadFile(logPath); err == nil {
				t.Logf("supervisor output:\n%s", out)
			}
		}
	})

	// Wait until autostart is blocked on a postmaster that will not become ready.
	eventually(t, 15*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(root, "pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := supervisorclient.New("http://127.0.0.1:"+strconv.Itoa(apiPort), "tok", nil)
	eventually(t, 10*time.Second, func() bool {
		st, err := c.Status(ctx)
		return err == nil && st.PG == supervisorapi.PGStarting
	})

	signalled := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor still running 10s after SIGTERM: the signal did not cut short the autostart wait (--start-timeout is 60s)")
	}
	t.Logf("supervisor exited %s after SIGTERM", time.Since(signalled).Round(time.Millisecond))
	if waitErr != nil {
		t.Fatalf("supervisor exit: %v", waitErr)
	}

	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("postmaster %d outlived the supervisor (kill -0: %v)", pid, err)
	}
	if _, err := os.Stat(filepath.Join(root, "got-int")); err != nil {
		t.Errorf("the starting postmaster never got a fast shutdown (SIGINT): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "got-quit")); err == nil {
		t.Error("the starting postmaster got an immediate shutdown (SIGQUIT): Stop must run on a context that outlives the signal")
	}
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logs), "autostart failed") {
		t.Errorf("a signal that cancels autostart is not an autostart failure:\n%s", logs)
	}
}

// "Reloading" a postgres container with SIGHUP is a common habit. Go's default for SIGHUP is to
// exit, and in the image tini would then exit too and take postgres down with SIGKILL. The
// supervisor must neither die nor start a shutdown, and must keep serving.
func TestSupervisorSurvivesSIGHUP(t *testing.T) {
	root := t.TempDir()
	apiPort := pgtest.FreePort(t)
	p := startSupervisor(t, buildSupervisor(t),
		"--postgres-bin", filepath.Join(root, "no-postgres"), // never run: --autostart=false
		"--data-dir", filepath.Join(root, "data"),
		"--listen", "127.0.0.1:"+strconv.Itoa(apiPort),
		"--token-file", writeFile(t, filepath.Join(root, "token"), "tok\n"),
		"--autostart=false",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := supervisorclient.New("http://127.0.0.1:"+strconv.Itoa(apiPort), "tok", nil)
	eventually(t, 10*time.Second, func() bool {
		_, err := c.Status(ctx)
		return err == nil
	})

	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	// Dying of SIGHUP, or shutting down on it, takes milliseconds.
	if p.exitedWithin(500 * time.Millisecond) {
		t.Fatalf("supervisor exited on SIGHUP: %v", p.waitErr)
	}
	if _, err := c.Status(ctx); err != nil {
		t.Fatalf("status after SIGHUP: %v", err)
	}

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if !p.exitedWithin(10 * time.Second) {
		t.Fatal("supervisor still running 10s after SIGTERM")
	}
	if p.waitErr != nil {
		t.Fatalf("supervisor exit after SIGTERM: %v", p.waitErr)
	}
}

// An empty secret is a deployment error, so the supervisor refuses to start: with an empty token
// its API would answer 401 to everyone, and an empty tenant password would create a login role
// without a password.
func TestSupervisorRefusesEmptySecrets(t *testing.T) {
	bin := buildSupervisor(t)
	refuses := func(t *testing.T, secretArgs ...string) {
		t.Helper()
		root := t.TempDir()
		p := startSupervisor(t, bin, append([]string{
			"--postgres-bin", filepath.Join(root, "no-postgres"), // never run: --autostart=false
			"--data-dir", filepath.Join(root, "data"),
			"--listen", "127.0.0.1:" + strconv.Itoa(pgtest.FreePort(t)),
			"--autostart=false",
		}, secretArgs...)...)
		if !p.exitedWithin(10 * time.Second) {
			t.Fatal("supervisor still running 10s after it started: an empty secret must stop it at startup")
		}
		var exitErr *exec.ExitError
		if !errors.As(p.waitErr, &exitErr) || exitErr.ExitCode() <= 0 {
			t.Fatalf("supervisor exit: %v, want a non-zero exit code", p.waitErr)
		}
		if logs := p.logs(); !strings.Contains(logs, "is empty") {
			t.Fatalf("output does not say the secret is empty:\n%s", logs)
		}
	}
	t.Run("empty token file", func(t *testing.T) {
		refuses(t, "--token-file", writeFile(t, filepath.Join(t.TempDir(), "token"), " \n"))
	})
	t.Run("empty tenant password file", func(t *testing.T) {
		dir := t.TempDir()
		refuses(t,
			"--token-file", writeFile(t, filepath.Join(dir, "token"), "tok\n"),
			"--db-password-file", writeFile(t, filepath.Join(dir, "password"), "\n"))
	})
}
