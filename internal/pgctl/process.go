package pgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// Manager runs one postmaster as a child process.
type Manager struct {
	postgresBin string
	opts        Options

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{} // closed after the current postmaster exits and state is updated
	exitErr error
	state   State
	since   time.Time
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

// Start launches postgres if needed and waits until it accepts connections.
// It is a no-op when postgres is already running and returns ErrBusy while
// another start or a stop is in progress.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	switch m.state {
	case StateRunning:
		m.mu.Unlock()
		return nil
	case StateStarting, StateStopping:
		m.mu.Unlock()
		return ErrBusy
	}
	cmd := exec.Command(m.postgresBin, Args(m.opts)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group, so signals aimed at the supervisor never reach postgres directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("start postgres: %w", err)
	}
	done := make(chan struct{})
	m.cmd, m.done, m.exitErr = cmd, done, nil
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
