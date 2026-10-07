package main_test

import (
	"context"
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
