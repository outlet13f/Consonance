package pgctl_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// waitFor polls cond until it holds and fails the test once timeout has passed.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logBuffer collects what the default slog logger writes during a test.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// line returns the first logged line that contains s, or "" when there is none.
func (b *logBuffer) line(s string) string {
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}

// captureLogs sends the default slog logger to a buffer until the test ends.
func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	b := &logBuffer{}
	prev, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut) // SetDefault pointed the log package at b as well
		log.SetFlags(prevFlags)
	})
	return b
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

// A postmaster that exits without a stop request (a crash, an OOM kill) does so behind every
// caller's back, so the log has to say it, with the exit error. A requested stop is no news.
func TestUnrequestedExitIsLogged(t *testing.T) {
	logs := captureLogs(t)
	m := startedManager(t)
	ctx := testCtx(t)

	if err := m.Stop(ctx, 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if l := logs.line("without a stop request"); l != "" {
		t.Fatalf("a requested stop was logged as an exit without one: %s", l)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := m.PID()
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	var l string
	waitFor(t, 5*time.Second, "the exit to be logged", func() bool {
		l = logs.line("postgres exited without a stop request")
		return l != ""
	})
	for _, want := range []string{fmt.Sprintf("pid=%d", pid), "state=running", `err="signal: killed"`} {
		if !strings.Contains(l, want) {
			t.Errorf("log line lacks %s: %s", want, l)
		}
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

// Only the supervisor knows which generation postgres runs: the one the data directory link
// pointed to when it started. Plan 3's rollback switches the link while postgres is stopped
// (stop, activate, start, detach, spec 9.4). A link switched while postgres runs must not change
// the generation reported, must not let a start pass as a no-op (detaching would then move the
// directory postgres runs in), and must not reach the running postgres: stopping it has to leave
// the new generation intact, so the next start runs it.
func TestGenerationIsTheOnePostgresRuns(t *testing.T) {
	bin := pgtest.BinDir(t)
	root := t.TempDir()
	data1 := pgtest.InitDBAt(t, bin, filepath.Join(root, "data-1"))
	data2 := filepath.Join(root, "data-2")
	if out, err := exec.Command("cp", "-a", data1, data2).CombinedOutput(); err != nil {
		t.Fatalf("copy data-1 to data-2: %v\n%s", err, out)
	}
	current := filepath.Join(root, "current")
	link := func(target string) { // atomic switch, as the volume agent does it (spec 6)
		t.Helper()
		if err := os.Symlink(target, current+".tmp"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(current+".tmp", current); err != nil {
			t.Fatal(err)
		}
	}
	link("data-1")
	m := pgctl.NewManager(filepath.Join(bin, "postgres"), pgctl.Options{
		DataDir:        current,
		SocketDir:      pgtest.SocketDir(t),
		Port:           pgtest.FreePort(t),
		MaxConnections: 20,
	})
	t.Cleanup(func() { _ = m.Stop(context.Background(), 5*time.Second) })
	ctx := testCtx(t)

	if g := m.Generation(); g != "data-1" {
		t.Fatalf("stopped: generation = %q, want data-1 (the link target)", g)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	assertRunsIn(t, m, data1)

	link("data-2") // reaches the link, not the running postgres
	if g := m.Generation(); g != "data-1" {
		t.Errorf("running: generation = %q after the link switch, want data-1 (the generation postgres runs)", g)
	}
	if err := m.Start(ctx); !errors.Is(err, pgctl.ErrGenerationMismatch) {
		t.Errorf("start with the link switched: err = %v, want ErrGenerationMismatch", err)
	}
	if s, _ := m.State(); s != pgctl.StateRunning {
		t.Fatalf("state = %s: a refused start must leave postgres running", s)
	}
	assertRunsIn(t, m, data1)

	if err := m.Stop(ctx, 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if g := m.Generation(); g != "data-2" {
		t.Fatalf("stopped: generation = %q, want data-2 (the next start runs it)", g)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start on data-2: %v", err)
	}
	if g := m.Generation(); g != "data-2" {
		t.Fatalf("running: generation = %q, want data-2", g)
	}
	assertRunsIn(t, m, data2)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start while running the generation the link points to must be a no-op: %v", err)
	}
}

// assertRunsIn fails the test unless m's postmaster runs in dir: a postmaster writes its pid to
// the first line of postmaster.pid in its data directory.
func assertRunsIn(t *testing.T, m *pgctl.Manager, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "postmaster.pid"))
	if err != nil {
		t.Fatalf("postgres does not run in %s: %v", dir, err)
	}
	pid, _ := strconv.Atoi(strings.SplitN(string(b), "\n", 2)[0])
	if pid == 0 || pid != m.PID() {
		t.Fatalf("%s/postmaster.pid names pid %d, want the postmaster %d", dir, pid, m.PID())
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
