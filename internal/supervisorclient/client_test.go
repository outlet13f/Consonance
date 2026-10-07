package supervisorclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/supervisor"
	"consonance/internal/supervisor/supervisortest"
	"consonance/internal/supervisorapi"
	"consonance/internal/supervisorclient"
)

func newClient(t *testing.T, pg *supervisortest.FakePostgres, tok string) *supervisorclient.Client {
	t.Helper()
	srv := httptest.NewServer((&supervisor.Server{
		Token:        "t0k",
		PG:           pg,
		Backups:      &supervisortest.FakeBackups{},
		Generation:   func() string { return "data-2" },
		StopGrace:    time.Second,
		StartTimeout: time.Second,
	}).Handler())
	t.Cleanup(srv.Close)
	return supervisorclient.New(srv.URL, tok, nil)
}

func TestClientStartStopStatus(t *testing.T) {
	c := newClient(t, supervisortest.NewFakePostgres(pgctl.StateStopped), "t0k")
	ctx := context.Background()

	st, err := c.Start(ctx)
	if err != nil || st.PG != supervisorapi.PGRunning || st.Generation != "data-2" {
		t.Fatalf("Start() = %+v, %v", st, err)
	}
	if st, err = c.Status(ctx); err != nil || st.PG != supervisorapi.PGRunning {
		t.Fatalf("Status() = %+v, %v", st, err)
	}
	if st, err = c.Stop(ctx); err != nil || st.PG != supervisorapi.PGStopped {
		t.Fatalf("Stop() = %+v, %v", st, err)
	}
}

func TestClientBackupRoundTrip(t *testing.T) {
	c := newClient(t, supervisortest.NewFakePostgres(pgctl.StateRunning), "t0k")
	ctx := context.Background()

	if _, err := c.BackupStart(ctx, "rp-1"); err != nil {
		t.Fatalf("BackupStart: %v", err)
	}
	stop, err := c.BackupStop(ctx)
	if err != nil {
		t.Fatalf("BackupStop: %v", err)
	}
	if !strings.Contains(stop.LabelFile, "LABEL: rp-1") {
		t.Fatalf("label file = %q", stop.LabelFile)
	}
}

func TestClientSurfacesConflict(t *testing.T) {
	c := newClient(t, supervisortest.NewFakePostgres(pgctl.StateRunning), "t0k")
	_, err := c.BackupStop(context.Background())
	if !supervisorclient.IsConflict(err) {
		t.Fatalf("BackupStop without session: err = %v, want a 409 APIError", err)
	}
}

func TestClientWrongTokenIsUnauthorized(t *testing.T) {
	c := newClient(t, supervisortest.NewFakePostgres(pgctl.StateRunning), "wrong")
	_, err := c.Status(context.Background())
	var apiErr *supervisorclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Status with wrong token: err = %v, want 401 APIError", err)
	}
}
