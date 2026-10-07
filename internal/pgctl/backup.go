package pgctl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrBackupInProgress = errors.New("a backup is already in progress")
	ErrNoBackup         = errors.New("no backup in progress")
)

// Backups runs at most one non-exclusive base backup at a time.
//
// PostgreSQL ties a non-exclusive backup to the session that started it, so the
// session stays open between Begin and End. When End does not arrive within the
// timeout, the session is closed and PostgreSQL aborts the backup by itself.
type Backups struct {
	connString string
	timeout    time.Duration

	mu    sync.Mutex
	conn  *pgx.Conn
	timer *time.Timer
}

// NewBackups returns a backup controller for the server at connString.
func NewBackups(connString string, timeout time.Duration) *Backups {
	return &Backups{connString: connString, timeout: timeout}
}

// Begin opens a dedicated session and runs pg_backup_start(label, fast := true),
// which forces an immediate checkpoint. It returns the backup start LSN.
func (b *Backups) Begin(ctx context.Context, label string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return "", ErrBackupInProgress
	}
	conn, err := pgx.Connect(ctx, b.connString)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	var lsn string
	if err := conn.QueryRow(ctx, "SELECT pg_backup_start($1, true)::text", label).Scan(&lsn); err != nil {
		conn.Close(context.Background())
		return "", fmt.Errorf("pg_backup_start: %w", err)
	}
	b.conn = conn
	b.timer = time.AfterFunc(b.timeout, b.Abort)
	return lsn, nil
}

// End runs pg_backup_stop(false) on the backup session and closes it.
// It returns the backup_label contents and the stop LSN.
func (b *Backups) End(ctx context.Context) (labelFile, stopLSN string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return "", "", ErrNoBackup
	}
	b.timer.Stop()
	conn := b.conn
	b.conn, b.timer = nil, nil
	defer conn.Close(context.Background())
	if err := conn.QueryRow(ctx, "SELECT lsn::text, labelfile FROM pg_backup_stop(false)").Scan(&stopLSN, &labelFile); err != nil {
		return "", "", fmt.Errorf("pg_backup_stop: %w", err)
	}
	return labelFile, stopLSN, nil
}

// Abort closes the backup session if one is open; PostgreSQL then aborts the backup.
func (b *Backups) Abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.conn.Close(context.Background())
	b.conn, b.timer = nil, nil
}

// InProgress reports whether a backup session is open.
func (b *Backups) InProgress() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil
}
