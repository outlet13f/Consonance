package pgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// State is the lifecycle state of the managed postmaster.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
)

// ErrBusy is returned when a start is requested while a start or stop is in progress.
var ErrBusy = errors.New("postgres is starting or stopping")

// ErrGenerationMismatch is returned by Start when postgres runs but the data directory link
// now points to another generation than the one postgres was started on.
var ErrGenerationMismatch = errors.New("postgres runs a different generation than the data directory points to")

// Manager runs one postmaster as a child process.
type Manager struct {
	postgresBin string
	opts        Options

	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{} // closed after the current postmaster exits and state is updated
	exitErr  error
	state    State
	since    time.Time
	launched string // directory the current postmaster runs in: opts.DataDir resolved at launch
}

// NewManager returns a Manager in the stopped state.
func NewManager(postgresBin string, opts Options) *Manager {
	return &Manager{postgresBin: postgresBin, opts: opts, state: StateStopped, since: time.Now()}
}

// LocalConnString returns a connection string for the superuser over the unix socket.
func LocalConnString(socketDir string, port int) string {
	return fmt.Sprintf("host=%s port=%d user=postgres dbname=postgres sslmode=disable connect_timeout=2", socketDir, port)
}

// ConnString returns the local superuser connection string for this server.
func (m *Manager) ConnString() string {
	return LocalConnString(m.opts.SocketDir, m.opts.Port)
}

func (m *Manager) setStateLocked(s State) {
	m.state = s
	m.since = time.Now()
}

// State reports the current state and when it began.
func (m *Manager) State() (State, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.since
}

// PID returns the postmaster's process id, or 0 when it is not running.
func (m *Manager) PID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil {
		return 0
	}
	return m.cmd.Process.Pid
}

// Ready reports whether postgres accepts connections right now.
func (m *Manager) Ready(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, m.ConnString())
	if err != nil {
		return false
	}
	defer conn.Close(context.Background())
	return conn.Ping(ctx) == nil
}

// Generation returns the generation postgres runs, e.g. "data-3": the base name of the
// directory the data directory link pointed to when postgres was started. While postgres is
// stopped it returns the generation the link points to now, which the next start runs, or ""
// when the link does not resolve.
func (m *Manager) Generation() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == StateStopped {
		return generationOf(resolveDir(m.opts.DataDir))
	}
	return generationOf(m.launched)
}

// resolveDir returns dir with every symbolic link resolved, or "" when it does not resolve.
func resolveDir(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	return resolved
}

// generationOf names the generation stored in dir, e.g. "data-3" for /pgdata/data-3.
func generationOf(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Base(dir)
}

// Start launches postgres if needed and waits until it accepts connections.
// It is a no-op when postgres already runs the generation the data directory link points to,
// returns ErrGenerationMismatch when postgres runs another one, and returns ErrBusy while
// another start or a stop is in progress.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	switch m.state {
	case StateRunning:
		running, linked := m.launched, resolveDir(m.opts.DataDir)
		m.mu.Unlock()
		if linked != running {
			return fmt.Errorf("%w: it runs %q, but %s points to %q; stop postgres before starting another generation",
				ErrGenerationMismatch, generationOf(running), m.opts.DataDir, generationOf(linked))
		}
		return nil
	case StateStarting, StateStopping:
		m.mu.Unlock()
		return ErrBusy
	}
	// Start postgres on the directory the link points to now, not through the link: for as long
	// as it runs, postgres opens some files (pg_control) by their path under its data directory,
	// so given the link, a switch made while it runs would send those writes into the other
	// generation. When the link does not resolve, postgres gets it as is and reports the error.
	dir := resolveDir(m.opts.DataDir)
	opts := m.opts
	if dir != "" {
		opts.DataDir = dir
	}
	cmd := exec.Command(m.postgresBin, Args(opts)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group, so signals aimed at the supervisor never reach postgres directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("start postgres: %w", err)
	}
	done := make(chan struct{})
	m.cmd, m.done, m.exitErr, m.launched = cmd, done, nil, dir
	m.setStateLocked(StateStarting)
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		if m.cmd == cmd {
			m.cmd, m.exitErr = nil, err
			m.setStateLocked(StateStopped)
		}
		m.mu.Unlock()
		close(done)
	}()

	ready := make(chan struct{})
	go m.awaitReady(cmd, done, ready)

	select {
	case <-ready:
		return nil
	case <-done:
		m.mu.Lock()
		err := m.exitErr
		m.mu.Unlock()
		return fmt.Errorf("postgres exited during startup: %v", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitReady polls until cmd accepts connections, then marks it running.
// It keeps going after Start's caller gives up, so the state never sticks at "starting".
func (m *Manager) awaitReady(cmd *exec.Cmd, done, ready chan struct{}) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		if !m.Ready(context.Background()) {
			continue
		}
		m.mu.Lock()
		if m.cmd == cmd && m.state == StateStarting {
			m.setStateLocked(StateRunning)
		}
		m.mu.Unlock()
		close(ready)
		return
	}
}

// Stop shuts postgres down with a fast shutdown (SIGINT). If it has not exited
// after grace, or ctx ends first, it escalates to an immediate shutdown (SIGQUIT).
// It is a no-op when postgres is not running.
func (m *Manager) Stop(ctx context.Context, grace time.Duration) error {
	m.mu.Lock()
	cmd, done := m.cmd, m.done
	if cmd == nil {
		m.mu.Unlock()
		return nil
	}
	m.setStateLocked(StateStopping)
	m.mu.Unlock()

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal postgres: %w", err)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
	case <-ctx.Done():
	}
	_ = cmd.Process.Signal(syscall.SIGQUIT)
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("postgres did not exit after immediate shutdown")
	}
}
