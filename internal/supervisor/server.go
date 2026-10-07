// Package supervisor serves the branch supervisor's internal HTTP API (spec 7.3).
package supervisor

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/supervisorapi"
)

// Postgres is the process control the server needs (implemented by *pgctl.Manager).
type Postgres interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context, grace time.Duration) error
	State() (pgctl.State, time.Time)
	Ready(ctx context.Context) bool
}

// Backups is the backup-session control the server needs (implemented by *pgctl.Backups).
type Backups interface {
	Begin(ctx context.Context, label string) (string, error)
	End(ctx context.Context) (labelFile, stopLSN string, err error)
}

// Server serves the supervisor API. Build it as a plain struct literal; the unexported
// fields are ready to use at their zero value.
type Server struct {
	Token        string
	PG           Postgres
	Backups      Backups
	Bootstrap    func(ctx context.Context) error // nil unless this pod runs main
	Generation   func() string                   // current generation, e.g. "data-3"
	StopGrace    time.Duration
	StartTimeout time.Duration

	bootstrapMu sync.Mutex // one Bootstrap call at a time
}

// Handler returns the HTTP handler. /healthz and /readyz need no token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /v1/status", s.auth(s.status))
	mux.Handle("POST /v1/start", s.auth(s.start))
	mux.Handle("POST /v1/stop", s.auth(s.stop))
	mux.Handle("POST /v1/backup/start", s.auth(s.backupStart))
	mux.Handle("POST /v1/backup/stop", s.auth(s.backupStop))
	mux.Handle("POST /v1/bootstrap", s.auth(s.bootstrap))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(supervisorapi.TokenHeader)
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r)
	})
}

func (s *Server) currentStatus() supervisorapi.Status {
	st, since := s.PG.State()
	return supervisorapi.Status{PG: supervisorapi.PGState(st), Generation: s.Generation(), Since: since}
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.PG.Ready(r.Context()) {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentStatus())
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.StartTimeout)
	defer cancel()
	if err := s.PG.Start(ctx); err != nil {
		if errors.Is(err, pgctl.ErrBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.currentStatus())
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	// Manager.Stop escalates to an immediate shutdown as soon as its context ends. A client
	// that drops or times out must not turn the 30s fast shutdown into that (the next start
	// would need crash recovery), so Stop is bounded by StopGrace, not by the request.
	if err := s.PG.Stop(context.WithoutCancel(r.Context()), s.StopGrace); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.currentStatus())
}

func (s *Server) backupStart(w http.ResponseWriter, r *http.Request) {
	var req supervisorapi.BackupStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}
	if st, _ := s.PG.State(); st != pgctl.StateRunning {
		writeError(w, http.StatusConflict, "postgres is not running")
		return
	}
	lsn, err := s.Backups.Begin(r.Context(), req.Label)
	switch {
	case errors.Is(err, pgctl.ErrBackupInProgress):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, supervisorapi.BackupStartResponse{StartLSN: lsn})
	}
}

func (s *Server) backupStop(w http.ResponseWriter, r *http.Request) {
	label, lsn, err := s.Backups.End(r.Context())
	switch {
	case errors.Is(err, pgctl.ErrNoBackup):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, supervisorapi.BackupStopResponse{LabelFile: label, StopLSN: lsn})
	}
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if s.Bootstrap == nil {
		writeError(w, http.StatusNotFound, "bootstrap is only available on main")
		return
	}
	if err := s.runBootstrap(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.currentStatus())
}

// runBootstrap runs s.Bootstrap one call at a time. pgctl.Bootstrap checks for the tenant
// role and database and then creates what is missing, which is not atomic: overlapping calls
// (an operator retry racing the first) could both try to create the same object and one would
// fail. Serialized, the later call finds what the first created and is a no-op, as spec 7.3
// requires. The lock covers the call only, not writing the response.
func (s *Server) runBootstrap(ctx context.Context) error {
	s.bootstrapMu.Lock()
	defer s.bootstrapMu.Unlock()
	return s.Bootstrap(ctx)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, supervisorapi.Error{Error: msg})
}
