package pgctl_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"consonance/internal/pgctl"
)

func TestBootstrapCreatesRoleAndDatabaseOnce(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)

	if err := pgctl.Bootstrap(ctx, m.ConnString(), pgctl.TenantRole, pgctl.TenantDatabase, "first-pw"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := pgctl.Bootstrap(ctx, m.ConnString(), pgctl.TenantRole, pgctl.TenantDatabase, "second-pw"); err != nil {
		t.Fatalf("second bootstrap must be a no-op: %v", err)
	}

	conn, err := pgx.Connect(ctx, m.ConnString())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	var owner string
	if err := conn.QueryRow(ctx,
		"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1", pgctl.TenantDatabase).Scan(&owner); err != nil {
		t.Fatalf("query owner: %v", err)
	}
	if owner != pgctl.TenantRole {
		t.Fatalf("database owner = %q, want %q", owner, pgctl.TenantRole)
	}
	var super, login bool
	if err := conn.QueryRow(ctx,
		"SELECT rolsuper, rolcanlogin FROM pg_roles WHERE rolname = $1", pgctl.TenantRole).Scan(&super, &login); err != nil {
		t.Fatalf("query role: %v", err)
	}
	if super || !login {
		t.Fatalf("role flags: superuser=%v login=%v, want superuser=false login=true", super, login)
	}
}

// The tenant password must not reach the server log, whatever logging the cluster runs with.
// Statement logging (log_statement) would log CREATE ROLE ... PASSWORD '<plain>' and the bind
// parameters of the format() call that builds it; log_min_error_statement (error by default)
// would log the statement when it fails.
func TestBootstrapKeepsPasswordOutOfServerLog(t *testing.T) {
	m := startedManager(t)
	ctx := testCtx(t)

	conn, err := pgx.Connect(ctx, m.ConnString())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var dataDir string
	if err := conn.QueryRow(ctx, "SHOW data_directory").Scan(&dataDir); err != nil {
		t.Fatalf("data_directory: %v", err)
	}
	for _, stmt := range []string{
		"ALTER SYSTEM SET logging_collector = on", // the log goes to files the test can read
		"ALTER SYSTEM SET log_statement = 'all'",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn.Close(ctx)
	if err := m.Stop(ctx, 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}

	const password = "pw-that-must-not-be-logged"
	if err := pgctl.Bootstrap(ctx, m.ConnString(), pgctl.TenantRole, pgctl.TenantDatabase, password); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// Role names starting with pg_ are reserved, so this CREATE ROLE fails.
	if err := pgctl.Bootstrap(ctx, m.ConnString(), "pg_reserved", "unused", password); err == nil ||
		!strings.Contains(err.Error(), "create role") || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("bootstrap with a reserved role name: err = %v, want CREATE ROLE to fail on the reserved name", err)
	}

	// Logged after both bootstraps: once it is in the log, so is everything they had logged.
	conn, err = pgx.Connect(ctx, m.ConnString())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "SELECT 'log-canary'"); err != nil {
		t.Fatalf("canary: %v", err)
	}
	var logs string
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs, "log-canary") {
		if time.Now().After(deadline) {
			t.Fatalf("the canary statement never reached the server log, so the log proves nothing:\n%s", logs)
		}
		time.Sleep(50 * time.Millisecond)
		files, _ := filepath.Glob(filepath.Join(dataDir, "log", "*"))
		logs = ""
		for _, f := range files {
			b, _ := os.ReadFile(f)
			logs += string(b)
		}
	}
	var leaks []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, password) {
			leaks = append(leaks, line)
		}
	}
	if len(leaks) > 0 {
		t.Fatalf("the tenant password is in the server log:\n%s", strings.Join(leaks, "\n"))
	}
}
