package supervisorclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"consonance/internal/agentapi"
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

func TestClientSendsTheRequestIDOfItsContext(t *testing.T) {
	var got []string // the X-Request-Id of each call, "" when absent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(agentapi.RequestIDHeader))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	c := supervisorclient.New(srv.URL, "t0k", nil)

	for _, ctx := range []context.Context{
		agentapi.WithRequestID(context.Background(), "r1"),
		context.Background(),
		agentapi.WithRequestID(context.Background(), "bad id\nX-Evil: 1"), // never sent
	} {
		if _, err := c.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"r1", "", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("X-Request-Id of the calls = %q, want %q", got, want)
	}
}
