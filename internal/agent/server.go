// Package agent serves the volume agent's internal HTTP API (spec 7.2).
package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"consonance/internal/agentapi"
	"consonance/internal/volume"
)

// Ops is the volume work behind the API (implemented by *volume.Volume).
type Ops interface {
	Preflight() (agentapi.Preflight, error)
	Usage() (agentapi.Usage, error)
	InitMain(ctx context.Context) (agentapi.Generation, error)
	CreateBranchData(ctx context.Context, name string, req agentapi.CreateBranchDataRequest) (agentapi.BranchData, error)
	DeleteBranchData(ctx context.Context, name string) error
	CreateRestorePoint(ctx context.Context, branch string, req agentapi.CreateRestorePointRequest) (agentapi.RestorePoint, error)
	ListRestorePoints(branch string) ([]agentapi.RestorePoint, error)
	DeleteRestorePoint(ctx context.Context, branch, id string) error
	ListGenerations(branch string) (agentapi.Generations, error)
	CreateGeneration(ctx context.Context, branch string, req agentapi.CreateGenerationRequest) (agentapi.Generation, error)
	ActivateGeneration(ctx context.Context, branch string, n int) (agentapi.Generations, error)
	DetachGeneration(ctx context.Context, branch string, n int, req agentapi.DetachRequest) (agentapi.DetachResponse, error)
	DeleteGeneration(ctx context.Context, branch string, n int) error
}

// Server serves the agent API. Every route but /healthz needs the token; an empty Token lets
// nobody in. Log gets one line per request; nil means slog.Default() at the time of the request.
type Server struct {
	Token string
	Ops   Ops
	Log   *slog.Logger
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// maxBody bounds request bodies.
const maxBody = 64 << 10

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /v1/preflight", s.auth(s.preflight))
	mux.Handle("GET /v1/usage", s.auth(s.usage))
	mux.Handle("POST /v1/init-main", s.auth(s.initMain))
	mux.Handle("POST /v1/branches/{name}/data", s.auth(s.createBranchData))
	mux.Handle("DELETE /v1/branches/{name}/data", s.auth(s.deleteBranchData))
	mux.Handle("POST /v1/branches/{name}/restore-points", s.auth(s.createRestorePoint))
	mux.Handle("GET /v1/branches/{name}/restore-points", s.auth(s.listRestorePoints))
	mux.Handle("DELETE /v1/branches/{name}/restore-points/{id}", s.auth(s.deleteRestorePoint))
	mux.Handle("GET /v1/branches/{name}/generations", s.auth(s.listGenerations))
	mux.Handle("POST /v1/branches/{name}/generations", s.auth(s.createGeneration))
	mux.Handle("POST /v1/branches/{name}/generations/{n}/activate", s.auth(s.activateGeneration))
	mux.Handle("POST /v1/branches/{name}/generations/{n}/detach", s.auth(s.detachGeneration))
	mux.Handle("DELETE /v1/branches/{name}/generations/{n}", s.auth(s.deleteGeneration))
	return s.logRequests(mux)
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec, ok := w.(*statusRecorder); ok { // r is the routed request here
			rec.op, rec.branch = r.Pattern, r.PathValue("name")
		}
		got := r.Header.Get(agentapi.TokenHeader)
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, agentapi.CodeUnauthorized, "invalid token")
			return
		}
		next(w, r)
	})
}

func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	p, err := s.Ops.Preflight()
	reply(w, p, err)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	u, err := s.Ops.Usage()
	reply(w, u, err)
}

func (s *Server) initMain(w http.ResponseWriter, r *http.Request) {
	g, err := s.Ops.InitMain(r.Context())
	reply(w, g, err)
}

func (s *Server) createBranchData(w http.ResponseWriter, r *http.Request) {
	var req agentapi.CreateBranchDataRequest
	if !decode(w, r, &req) {
		return
	}
	d, err := s.Ops.CreateBranchData(r.Context(), r.PathValue("name"), req)
	reply(w, d, err)
}

func (s *Server) deleteBranchData(w http.ResponseWriter, r *http.Request) {
	replyNoContent(w, s.Ops.DeleteBranchData(r.Context(), r.PathValue("name")))
}

func (s *Server) createRestorePoint(w http.ResponseWriter, r *http.Request) {
	var req agentapi.CreateRestorePointRequest
	if !decode(w, r, &req) {
		return
	}
	rp, err := s.Ops.CreateRestorePoint(r.Context(), r.PathValue("name"), req)
	reply(w, rp, err)
}

func (s *Server) listRestorePoints(w http.ResponseWriter, r *http.Request) {
	rps, err := s.Ops.ListRestorePoints(r.PathValue("name"))
	if rps == nil {
		rps = []agentapi.RestorePoint{}
	}
	reply(w, agentapi.RestorePointList{RestorePoints: rps}, err)
}

func (s *Server) deleteRestorePoint(w http.ResponseWriter, r *http.Request) {
	replyNoContent(w, s.Ops.DeleteRestorePoint(r.Context(), r.PathValue("name"), r.PathValue("id")))
}

func (s *Server) listGenerations(w http.ResponseWriter, r *http.Request) {
	g, err := s.Ops.ListGenerations(r.PathValue("name"))
	reply(w, g, err)
}

func (s *Server) createGeneration(w http.ResponseWriter, r *http.Request) {
	var req agentapi.CreateGenerationRequest
	if !decode(w, r, &req) {
		return
	}
	g, err := s.Ops.CreateGeneration(r.Context(), r.PathValue("name"), req)
	reply(w, g, err)
}

func (s *Server) activateGeneration(w http.ResponseWriter, r *http.Request) {
	n, ok := generation(w, r)
	if !ok {
		return
	}
	g, err := s.Ops.ActivateGeneration(r.Context(), r.PathValue("name"), n)
	reply(w, g, err)
}

func (s *Server) detachGeneration(w http.ResponseWriter, r *http.Request) {
	n, ok := generation(w, r)
	if !ok {
		return
	}
	var req agentapi.DetachRequest
	if !decode(w, r, &req) {
		return
	}
	d, err := s.Ops.DetachGeneration(r.Context(), r.PathValue("name"), n, req)
	reply(w, d, err)
}

func (s *Server) deleteGeneration(w http.ResponseWriter, r *http.Request) {
	n, ok := generation(w, r)
	if !ok {
		return
	}
	replyNoContent(w, s.Ops.DeleteGeneration(r.Context(), r.PathValue("name"), n))
}

// generation parses the {n} path segment.
func generation(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, agentapi.CodeInvalid, "generation must be a positive integer")
		return 0, false
	}
	return n, true
}

// decode reads a JSON body into v. An empty body leaves v at its zero value; unknown fields
// and bodies over 64 KiB are refused.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, agentapi.CodeInvalid, "request body: "+err.Error())
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeOpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func replyNoContent(w http.ResponseWriter, err error) {
	if err != nil {
		writeOpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeOpError maps a volume error to its status code and error code.
func writeOpError(w http.ResponseWriter, err error) {
	if rec, ok := w.(*statusRecorder); ok {
		rec.err = err
	}
	var rl *volume.RateLimitError
	switch {
	case errors.As(err, &rl):
		secs := int(math.Ceil(rl.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		writeError(w, http.StatusTooManyRequests, agentapi.CodeRateLimited, err.Error())
	case errors.Is(err, volume.ErrInvalid):
		writeError(w, http.StatusBadRequest, agentapi.CodeInvalid, err.Error())
	case errors.Is(err, volume.ErrNotFound):
		writeError(w, http.StatusNotFound, agentapi.CodeNotFound, err.Error())
	case errors.Is(err, volume.ErrNotRunning):
		writeError(w, http.StatusConflict, agentapi.CodeNotRunning, err.Error())
	case errors.Is(err, volume.ErrBusy):
		writeError(w, http.StatusConflict, agentapi.CodeBusy, err.Error())
	case errors.Is(err, volume.ErrConflict):
		writeError(w, http.StatusConflict, agentapi.CodeConflict, err.Error())
	case errors.Is(err, volume.ErrInsufficientStorage):
		writeError(w, http.StatusInsufficientStorage, agentapi.CodeInsufficientStorage, err.Error())
	case errors.Is(err, volume.ErrSupervisorUnavailable):
		writeError(w, http.StatusBadGateway, agentapi.CodeSupervisorUnavailable, err.Error())
	case errors.Is(err, volume.ErrReflinkUnsupported):
		writeError(w, http.StatusNotImplemented, agentapi.CodeReflinkUnsupported, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, agentapi.CodeInternal, err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code agentapi.ErrorCode, msg string) {
	writeJSON(w, status, agentapi.Error{Error: msg, Code: code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// logRequests logs one line per API request with its route, branch, status, duration and, for
// a failed volume operation, the error (spec 11). 5xx lines are logged at level Error.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		level := slog.LevelInfo
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		attrs := []any{"op", rec.op, "branch", rec.branch, "status", rec.status, "durationMs", time.Since(start).Milliseconds()}
		if rec.err != nil {
			attrs = append(attrs, "err", rec.err.Error())
		}
		s.logger().Log(r.Context(), level, "request", attrs...)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status     int
	op, branch string
	err        error
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
