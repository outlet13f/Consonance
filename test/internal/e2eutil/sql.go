//go:build e2e || perf

package e2eutil

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ServiceOf splits a branch endpoint host br-<branch>.cns-<projectId>.svc into the Service name
// and its namespace.
func ServiceOf(host string) (svc, ns string, err error) {
	parts := strings.Split(host, ".")
	if len(parts) < 3 || parts[0] == "" || !strings.HasPrefix(parts[1], NamespacePrefix) {
		return "", "", fmt.Errorf("host %q is not <service>.cns-<projectId>.svc", host)
	}
	return parts[0], parts[1], nil
}

// SQL connects to a branch as the tenant role of conn through a port-forward to the branch's
// Service, retrying for a minute, and closes the connection when the test ends. Each try forwards
// afresh (Dial): kubectl usually ends a forward whose connection inside the pod was refused, so a
// retry over the old one would hit a dead forward. The forward targets the pod running now: after
// a stop and start, a rollback or a reset, connect again.
func SQL(t testing.TB, conn Connection) *pgx.Conn {
	t.Helper()
	mustLoad(t)
	if _, _, err := ServiceOf(conn.Host); err != nil {
		t.Fatal(err)
	}

	var (
		db      *pgx.Conn
		release func()
	)
	Eventually(t, time.Minute, time.Second, "connect to "+conn.Host, func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var err error
		db, release, _, err = Dial(ctx, conn)
		if err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	t.Cleanup(release)
	return db
}

// Dial is one try of SQL: it forwards a fresh local port to the branch's Service, connects
// through it as the tenant role of conn, and returns the connection, a release that ends the
// connection and then the forward, and how long the forward took to listen. It neither retries
// nor registers cleanups, so a caller can time a try and tell the forward's share of it from
// PostgreSQL's (test/perf).
func Dial(ctx context.Context, conn Connection) (db *pgx.Conn, release func(), forward time.Duration, err error) {
	if err := load(); err != nil {
		return nil, nil, 0, err
	}
	svc, ns, err := ServiceOf(conn.Host)
	if err != nil {
		return nil, nil, 0, err
	}
	start := time.Now()
	local, stopForward, err := startPortForward(ns, "svc/"+svc, conn.Port)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("port-forward: %w", err)
	}
	forward = time.Since(start)
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d sslmode=disable connect_timeout=10", local))
	if err != nil {
		stopForward()
		return nil, nil, forward, err
	}
	cfg.User, cfg.Password, cfg.Database = conn.User, conn.Password, conn.Database
	db, err = pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		stopForward()
		return nil, nil, forward, err
	}
	return db, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.Close(ctx)
		stopForward()
	}, forward, nil
}

// QueryStrings runs a query whose rows have one text column and returns them.
func QueryStrings(t testing.TB, db *pgx.Conn, query string, args ...any) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// Exec runs statements and fails the test on an error.
func Exec(t testing.TB, db *pgx.Conn, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
