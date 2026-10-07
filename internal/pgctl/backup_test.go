package pgctl_test

import (
	"errors"
	"fmt"
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

// A session closed by its own timeout ends the backup behind the caller's back (its
// backup/stop then finds no backup), so the log has to say it. ExpireSession runs what the
// timer runs, at a chosen moment; a late expiry that closes nothing logs nothing.
func TestBackupExpiryIsLogged(t *testing.T) {
	logs := captureLogs(t)
	m := startedManager(t)
	ctx := testCtx(t)
	b := pgctl.NewBackups(m.ConnString(), time.Minute) // only the test fires expiries
	t.Cleanup(b.Abort)

	if _, err := b.Begin(ctx, "ended"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	ended := b.OpenSession()
	if _, _, err := b.End(ctx); err != nil {
		t.Fatalf("end: %v", err)
	}
	b.ExpireSession(ended)
	if l := logs.line("timed out"); l != "" {
		t.Fatalf("an expiry that closed nothing was logged: %s", l)
	}

	if _, err := b.Begin(ctx, "expires"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	session := b.OpenSession()
	b.ExpireSession(session)
	l := logs.line("backup session timed out")
	if l == "" {
		t.Fatalf("the expiry that closed the session was not logged:\n%s", logs)
	}
	for _, want := range []string{"timeout=1m0s", fmt.Sprintf("backendPID=%d", session.PgConn().PID())} {
		if !strings.Contains(l, want) {
			t.Errorf("log line lacks %s: %s", want, l)
		}
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

// A session's timer can fire after its session has already ended (End or Abort raced
// with it). That late callback must not close a session that began since: a session is
// closed only once its own timeout has passed (spec 7.3). ExpireSession runs the late
// callback at a chosen moment, so the interleaving is deterministic.
func TestBackupLateExpiryDoesNotCloseNewerSession(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)

	begin := func(t *testing.T, b *pgctl.Backups, label string) {
		t.Helper()
		if _, err := b.Begin(ctx, label); err != nil {
			t.Fatalf("begin %s: %v", label, err)
		}
	}
	cases := []struct {
		name     string
		endFirst func(*pgctl.Backups) error
	}{
		{"first ended by End", func(b *pgctl.Backups) error { _, _, err := b.End(ctx); return err }},
		{"first ended by Abort", func(b *pgctl.Backups) error { b.Abort(); return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := pgctl.NewBackups(m.ConnString(), time.Minute) // only the test fires expiries
			t.Cleanup(b.Abort)

			begin(t, b, "first")
			first := b.OpenSession()
			if first == nil {
				t.Fatal("no open session after begin")
			}
			if err := tc.endFirst(b); err != nil {
				t.Fatalf("end first: %v", err)
			}
			begin(t, b, "second")

			b.ExpireSession(first) // the first session's timer fires late

			if !b.InProgress() {
				t.Fatal("a late expiry of the first session closed the second session")
			}
			if _, _, err := b.End(ctx); err != nil {
				t.Fatalf("end second: %v", err)
			}

			// The same call aimed at the session that is open does close it.
			begin(t, b, "third")
			b.ExpireSession(b.OpenSession())
			if b.InProgress() {
				t.Fatal("expiry of the open session did not close it")
			}
		})
	}
}
