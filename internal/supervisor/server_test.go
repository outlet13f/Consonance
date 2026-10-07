package supervisor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/supervisor"
	"consonance/internal/supervisor/supervisortest"
	"consonance/internal/supervisorapi"
)

const token = "t0k"

func newHandler(pg *supervisortest.FakePostgres, b *supervisortest.FakeBackups, bootstrap func(context.Context) error) http.Handler {
	s := &supervisor.Server{
		Token:        token,
		PG:           pg,
		Backups:      b,
		Bootstrap:    bootstrap,
		Generation:   func() string { return "data-1" },
		StopGrace:    time.Second,
		StartTimeout: time.Second,
	}
	return s.Handler()
}

func call(h http.Handler, method, path, tok, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set(supervisorapi.TokenHeader, tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestHealthzNeedsNoToken(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateStopped), &supervisortest.FakeBackups{}, nil)
	if rec := call(h, "GET", "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}

func TestReadyzFollowsPostgres(t *testing.T) {
	running := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)
	if rec := call(running, "GET", "/readyz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("readyz while running = %d, want 200", rec.Code)
	}
	stopped := newHandler(supervisortest.NewFakePostgres(pgctl.StateStopped), &supervisortest.FakeBackups{}, nil)
	if rec := call(stopped, "GET", "/readyz", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while stopped = %d, want 503", rec.Code)
	}
}

func TestRejectsMissingOrWrongToken(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)
	for _, tok := range []string{"", "nope"} {
		if rec := call(h, "GET", "/v1/status", tok, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", tok, rec.Code)
		}
	}
}

func TestStartReportsRunningStatus(t *testing.T) {
	pg := supervisortest.NewFakePostgres(pgctl.StateStopped)
	h := newHandler(pg, &supervisortest.FakeBackups{}, nil)
	rec := call(h, "POST", "/v1/start", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	st := decode[supervisorapi.Status](t, rec)
	if st.PG != supervisorapi.PGRunning || st.Generation != "data-1" {
		t.Fatalf("status = %+v, want running on data-1", st)
	}
}

func TestStartWhileBusyIs409(t *testing.T) {
	pg := supervisortest.NewFakePostgres(pgctl.StateStarting)
	pg.StartErr = pgctl.ErrBusy
	h := newHandler(pg, &supervisortest.FakeBackups{}, nil)
	if rec := call(h, "POST", "/v1/start", token, ""); rec.Code != http.StatusConflict {
		t.Fatalf("start while busy = %d, want 409", rec.Code)
	}
}

// Postgres runs one generation while /pgdata/current points to another: the caller asked for the
// linked generation, so a 200 "already running" would be a lie (spec 9.4 would then detach the
// directory postgres runs in).
func TestStartOnAnotherGenerationIs409(t *testing.T) {
	pg := supervisortest.NewFakePostgres(pgctl.StateRunning)
	pg.StartErr = fmt.Errorf("%w: it runs data-1, current now points to data-2", pgctl.ErrGenerationMismatch)
	h := newHandler(pg, &supervisortest.FakeBackups{}, nil)

	rec := call(h, "POST", "/v1/start", token, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("start on another generation = %d %s, want 409", rec.Code, rec.Body)
	}
	if msg := decode[supervisorapi.Error](t, rec).Error; msg != pg.StartErr.Error() {
		t.Fatalf("error = %q, want the mismatch with both generations: %q", msg, pg.StartErr)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	pg := supervisortest.NewFakePostgres(pgctl.StateRunning)
	h := newHandler(pg, &supervisortest.FakeBackups{}, nil)
	for i := range 2 {
		rec := call(h, "POST", "/v1/stop", token, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("stop #%d = %d", i+1, rec.Code)
		}
		if st := decode[supervisorapi.Status](t, rec); st.PG != supervisorapi.PGStopped {
			t.Fatalf("stop #%d status = %s, want stopped", i+1, st.PG)
		}
	}
}

// pgctl.Manager.Stop escalates from the fast shutdown to an immediate one (SIGQUIT) as soon as
// its context ends. A client that disconnects or times out while the fast shutdown runs must
// not reach Stop's context, or the next start would have to recover from a crash.
func TestStopIgnoresClientGivingUp(t *testing.T) {
	disconnected, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, cancelTimedOut := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelTimedOut()

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"client disconnected", disconnected},
		{"client timed out", timedOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pg := supervisortest.NewFakePostgres(pgctl.StateRunning)
			h := newHandler(pg, &supervisortest.FakeBackups{}, nil)

			req := httptest.NewRequestWithContext(tc.ctx, "POST", "/v1/stop", nil)
			req.Header.Set(supervisorapi.TokenHeader, token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("stop = %d %s, want 200", rec.Code, rec.Body)
			}
			// Stop must really have run, or the context check below would pass vacuously.
			if st := decode[supervisorapi.Status](t, rec); st.PG != supervisorapi.PGStopped {
				t.Fatalf("status = %s, want stopped", st.PG)
			}
			if err := pg.StopCtxErr(); err != nil {
				t.Fatalf("PG.Stop got a finished context (%v); Manager.Stop would escalate to an immediate shutdown", err)
			}
		})
	}
}

func TestBackupStartNeedsLabel(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)
	if rec := call(h, "POST", "/v1/backup/start", token, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("backup without label = %d, want 400", rec.Code)
	}
}

func TestBackupStartNeedsRunningPostgres(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateStopped), &supervisortest.FakeBackups{}, nil)
	if rec := call(h, "POST", "/v1/backup/start", token, `{"label":"x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("backup while stopped = %d, want 409", rec.Code)
	}
}

func TestBackupRoundTrip(t *testing.T) {
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)

	rec := call(h, "POST", "/v1/backup/start", token, `{"label":"b1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup start = %d %s", rec.Code, rec.Body)
	}
	if got := decode[supervisorapi.BackupStartResponse](t, rec); got.StartLSN == "" {
		t.Fatal("empty start LSN")
	}
	if rec := call(h, "POST", "/v1/backup/start", token, `{"label":"b2"}`); rec.Code != http.StatusConflict {
		t.Fatalf("second backup start = %d, want 409", rec.Code)
	}
	rec = call(h, "POST", "/v1/backup/stop", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("backup stop = %d %s", rec.Code, rec.Body)
	}
	if got := decode[supervisorapi.BackupStopResponse](t, rec); !strings.Contains(got.LabelFile, "LABEL: b1") {
		t.Fatalf("label file = %q", got.LabelFile)
	}
	if rec := call(h, "POST", "/v1/backup/stop", token, ""); rec.Code != http.StatusConflict {
		t.Fatalf("backup stop without session = %d, want 409", rec.Code)
	}
}

func TestBootstrapOnlyOnMain(t *testing.T) {
	notMain := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, nil)
	if rec := call(notMain, "POST", "/v1/bootstrap", token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bootstrap on non-main = %d, want 404", rec.Code)
	}
	calls := 0
	main := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{},
		func(context.Context) error { calls++; return nil })
	if rec := call(main, "POST", "/v1/bootstrap", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("bootstrap on main = %d %s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("bootstrap called %d times, want 1", calls)
	}
}

// pgctl.Bootstrap looks for the tenant role and database and then creates what is missing,
// which is not atomic: two overlapping calls (an operator retry racing the first) would both
// see "missing" and one would fail on the duplicate. Spec 7.3 wants the repeat to be a no-op
// that returns 200, so the server has to run Bootstrap one call at a time.
func TestBootstrapCallsDoNotOverlap(t *testing.T) {
	var inFlight, ran atomic.Int32
	bootstrap := func(context.Context) error {
		ran.Add(1)
		if inFlight.Add(1) > 1 {
			inFlight.Add(-1)
			return errors.New("bootstrap called while another call is running")
		}
		defer inFlight.Add(-1)
		time.Sleep(50 * time.Millisecond) // long enough for a concurrent request to arrive
		return nil
	}
	h := newHandler(supervisortest.NewFakePostgres(pgctl.StateRunning), &supervisortest.FakeBackups{}, bootstrap)

	codes := make([]int, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			<-start
			codes[i] = call(h, "POST", "/v1/bootstrap", token, "").Code
		})
	}
	close(start)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("concurrent bootstrap #%d = %d, want 200", i+1, code)
		}
	}
	if got := ran.Load(); got != 2 {
		t.Errorf("bootstrap ran %d times, want 2: the second call has to wait for the first, not be dropped", got)
	}
}
