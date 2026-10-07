package pgctl

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Tenant role and database every project gets (spec 4.5).
const (
	TenantRole     = "app_owner"
	TenantDatabase = "app"
)

// Bootstrap creates the tenant login role and its database when they do not exist.
// It is safe to call repeatedly; an existing role keeps its password.
func Bootstrap(ctx context.Context, connString, role, database, password string) error {
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		return fmt.Errorf("check role: %w", err)
	}
	if !exists {
		if err := execFormatted(ctx, conn,
			"SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', $1::text, $2::text)", role, password); err != nil {
			return fmt.Errorf("create role: %w", err)
		}
	}
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", database).Scan(&exists); err != nil {
		return fmt.Errorf("check database: %w", err)
	}
	if !exists {
		if err := execFormatted(ctx, conn,
			"SELECT format('CREATE DATABASE %I OWNER %I', $1::text, $2::text)", database, role); err != nil {
			return fmt.Errorf("create database: %w", err)
		}
	}
	return nil
}

// execFormatted builds a statement with server-side format() quoting, then runs it.
// Identifiers and literals never pass through Go string concatenation.
func execFormatted(ctx context.Context, conn *pgx.Conn, formatQuery string, args ...any) error {
	var stmt string
	if err := conn.QueryRow(ctx, formatQuery, args...).Scan(&stmt); err != nil {
		return err
	}
	_, err := conn.Exec(ctx, stmt)
	return err
}
