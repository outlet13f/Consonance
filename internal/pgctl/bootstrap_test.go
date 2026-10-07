package pgctl_test

import (
	"context"
	"testing"

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
