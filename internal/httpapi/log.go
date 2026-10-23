package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// reqState is what one request collects for its log line: the route's name and the fields the
// handler added with logAttrs.
type reqState struct {
	mu    sync.Mutex
	op    string
	attrs []any
}

type reqStateKey struct{}

func stateOf(ctx context.Context) *reqState {
	st, _ := ctx.Value(reqStateKey{}).(*reqState)
	return st
}

// logAttrs adds fields (key, value, ...) to the request's log line, for example
// logAttrs(ctx, "projectId", id, "branch", name). Outside a request it does nothing.
func logAttrs(ctx context.Context, kv ...any) {
	if st := stateOf(ctx); st != nil {
		st.mu.Lock()
		st.attrs = append(st.attrs, kv...)
		st.mu.Unlock()
	}
}

func setOp(ctx context.Context, op string) {
	if st := stateOf(ctx); st != nil {
		st.mu.Lock()
		st.op = op
		st.mu.Unlock()
	}
}

// validRequestID accepts the X-Request-Id of a caller: 1 to 64 letters, digits, '-' and '_',
// so a value cannot forge a log line or a header.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system's random source is gone
	}
	return hex.EncodeToString(b[:])
}

// statusWriter remembers the status code for the log line.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logged gives the request an id (echoed in X-Request-Id) and a log state, and writes one line
// when next returns: requestId, op, method, path, status, durationMs and what logAttrs added.
func (s *server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		st := &reqState{op: "unknown"}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), reqStateKey{}, st)))

		st.mu.Lock()
		defer st.mu.Unlock()
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		attrs := append([]any{
			"requestId", id,
			"op", st.op,
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"durationMs", time.Since(start).Milliseconds(),
		}, st.attrs...)
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		}
		s.log.Log(r.Context(), level, "request", attrs...)
	})
}
