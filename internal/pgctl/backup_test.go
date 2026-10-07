package pgctl_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"consonance/internal/pgctl"
)

func TestBackupBeginEnd(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)
	b := pgctl.NewBackups(m.ConnString(), 30*time.Second)

	lsn, err := b.Begin(ctx, "test-label")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if !strings.Contains(lsn, "/") {
		t.Fatalf("start LSN = %q, want an LSN like 0/2000028", lsn)
	}
	if _, err := b.Begin(ctx, "again"); !errors.Is(err, pgctl.ErrBackupInProgress) {
		t.Fatalf("second begin: err = %v, want ErrBackupInProgress", err)
	}
	label, stopLSN, err := b.End(ctx)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if !strings.Contains(label, "LABEL: test-label") {
		t.Fatalf("label file does not name the backup:\n%s", label)
	}
	if stopLSN == "" {
		t.Fatal("empty stop LSN")
	}
	if _, _, err := b.End(ctx); !errors.Is(err, pgctl.ErrNoBackup) {
		t.Fatalf("second end: err = %v, want ErrNoBackup", err)
	}
}

func TestBackupTimeoutAbortsSession(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)
	b := pgctl.NewBackups(m.ConnString(), 200*time.Millisecond)

	if _, err := b.Begin(ctx, "will-time-out"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if b.InProgress() {
		t.Fatal("session still open after the timeout")
	}
	// PostgreSQL aborted the backup when the session closed, so a new one starts at once.
	if _, err := b.Begin(ctx, "after-timeout"); err != nil {
		t.Fatalf("begin after timeout: %v", err)
	}
	if _, _, err := b.End(ctx); err != nil {
		t.Fatalf("end: %v", err)
	}
}

func TestBackupBeginFailsWhenPostgresIsDown(t *testing.T) {
	m := newTestManager(t) // never started
	b := pgctl.NewBackups(m.ConnString(), time.Second)
	if _, err := b.Begin(testCtx(t), "x"); err == nil {
		t.Fatal("begin succeeded with postgres down")
	}
	if b.InProgress() {
		t.Fatal("failed begin left a session open")
	}
}
