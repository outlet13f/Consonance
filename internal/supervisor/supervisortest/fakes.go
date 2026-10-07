// Package supervisortest provides in-memory fakes of the supervisor's dependencies.
package supervisortest

import (
	"context"
	"sync"
	"time"

	"consonance/internal/pgctl"
)

// FakePostgres implements supervisor.Postgres in memory.
type FakePostgres struct {
	mu         sync.Mutex
	state      pgctl.State
	stopCtxErr error // ctx.Err() as the latest Stop call saw it
	StartErr   error
	StopErr    error
}

// NewFakePostgres returns a FakePostgres in the given state.
func NewFakePostgres(state pgctl.State) *FakePostgres {
	return &FakePostgres{state: state}
}

func (f *FakePostgres) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	f.state = pgctl.StateRunning
	return nil
}

func (f *FakePostgres) Stop(ctx context.Context, grace time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCtxErr = ctx.Err()
	if f.StopErr != nil {
		return f.StopErr
	}
	f.state = pgctl.StateStopped
	return nil
}

// StopCtxErr returns ctx.Err() as the latest Stop call saw it, nil if that context was still
// live (or Stop has not been called). pgctl.Manager.Stop escalates to an immediate shutdown as
// soon as its context ends, so a caller must not pass Stop a context its own client can end.
func (f *FakePostgres) StopCtxErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCtxErr
}

func (f *FakePostgres) State() (pgctl.State, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, time.Unix(0, 0).UTC()
}

func (f *FakePostgres) Ready(ctx context.Context) bool {
	s, _ := f.State()
	return s == pgctl.StateRunning
}

// FakeBackups implements supervisor.Backups in memory.
type FakeBackups struct {
	mu       sync.Mutex
	open     bool
	label    string
	BeginErr error
}

func (f *FakeBackups) Begin(ctx context.Context, label string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.BeginErr != nil {
		return "", f.BeginErr
	}
	if f.open {
		return "", pgctl.ErrBackupInProgress
	}
	f.open, f.label = true, label
	return "0/2000028", nil
}

func (f *FakeBackups) End(ctx context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		return "", "", pgctl.ErrNoBackup
	}
	f.open = false
	return "START WAL LOCATION: 0/2000028\nLABEL: " + f.label + "\n", "0/2000100", nil
}
