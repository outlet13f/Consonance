package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthzNeedsNoKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, body := callWith(t, srv, http.MethodGet, "/healthz", nil, "")
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("GET /healthz = %d %q", resp.StatusCode, body)
	}
}

func TestAuth(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for name, header := range map[string]string{
		"no header":   "",
		"basic":       "Basic dGVzdDprZXk=",
		"wrong key":   "Bearer wrong",
		"longer key":  "Bearer " + testKey + "x",
		"shorter key": "Bearer test-ke",
		"empty key":   "Bearer ",
		"no scheme":   testKey,
		"no space":    "Bearer" + testKey,
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := callWith(t, srv, http.MethodGet, "/v1/nope", nil, header)
			wantProblem(t, resp, body, http.StatusUnauthorized, typeUnauthorized)
			if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
		})
	}
	resp, body := call(t, srv, http.MethodGet, "/v1/nope", nil)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("the right key was refused: %s", body)
	}
}

func TestAuthRefusesEverythingWithoutAConfiguredKey(t *testing.T) {
	srv, _, _ := newTestServerWith(t, func(c *Config) { c.AdminKey = "" })
	resp, body := callWith(t, srv, http.MethodGet, "/v1/nope", nil, "Bearer ")
	wantProblem(t, resp, body, http.StatusUnauthorized, typeUnauthorized)
}

// TestOnlyHealthzAndOpenAPIArePublic: the public set is explicit, so a route added later, or a
// path outside /v1, is behind the key unless it is named in the set.
func TestOnlyHealthzAndOpenAPIArePublic(t *testing.T) {
	var reached atomic.Bool
	cfg, _, _ := testConfig()
	srv := newServerWithRoutes(t, cfg,
		endpoint{route{http.MethodGet, "/internal/state", "test.internal"}, func(w http.ResponseWriter, r *http.Request) error {
			reached.Store(true)
			writeJSON(w, http.StatusOK, map[string]string{})
			return nil
		}},
		endpoint{route{http.MethodPost, "/healthz/deep", "test.deep"}, func(w http.ResponseWriter, r *http.Request) error {
			reached.Store(true)
			return nil
		}})

	// Without a key: 401 for everything but the two public routes, and the handler never runs.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/internal/state"},
		{http.MethodPost, "/healthz/deep"},
		{http.MethodGet, "/metrics"}, // outside /v1 and not a route
		{http.MethodGet, "/"},
		{http.MethodPost, "/healthz"}, // public for GET only
		{http.MethodPost, "/v1/openapi.json"},
		{http.MethodGet, "/v1/projects"},
	} {
		resp, body := callWith(t, srv, c.method, c.path, nil, "")
		wantProblem(t, resp, body, http.StatusUnauthorized, typeUnauthorized)
		if reached.Load() {
			t.Fatalf("%s %s ran its handler without a key", c.method, c.path)
		}
	}

	// With the key the routes are reached, and a path outside /v1 that is not a route is a 404.
	if resp, body := call(t, srv, http.MethodGet, "/internal/state", nil); resp.StatusCode != http.StatusOK || !reached.Load() {
		t.Errorf("GET /internal/state with the key = %d %s, reached %v", resp.StatusCode, body, reached.Load())
	}
	resp, body := call(t, srv, http.MethodGet, "/metrics", nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)

	// The two public routes need no key.
	for _, path := range []string{"/healthz", "/v1/openapi.json"} {
		if resp, body := callWith(t, srv, http.MethodGet, path, nil, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without a key = %d %s", path, resp.StatusCode, body)
		}
	}
}

func TestUnknownRouteIsProblem(t *testing.T) {
	cfg, _, _ := testConfig()
	srv := newServerWithRoutes(t, cfg, endpoint{route{http.MethodGet, "/v1/test-only", "test.list"}, func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, http.StatusOK, map[string]string{})
		return nil
	}})
	resp, body := call(t, srv, http.MethodGet, "/v1/nope", nil)
	wantProblem(t, resp, body, http.StatusNotFound, typeNotFound)

	resp, body = call(t, srv, http.MethodPut, "/v1/test-only", nil)
	wantProblem(t, resp, body, http.StatusMethodNotAllowed, typeValidation)
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", got)
	}

	// The same against the real routes.
	real, _, _ := newTestServer(t)
	resp, body = call(t, real, http.MethodPut, "/v1/projects", nil)
	wantProblem(t, resp, body, http.StatusMethodNotAllowed, typeValidation)

	// The same for a route that needs no key.
	resp, body = call(t, srv, http.MethodPost, "/healthz", nil)
	wantProblem(t, resp, body, http.StatusMethodNotAllowed, typeValidation)
	// Without the key the answer is 401 whether or not the route exists.
	resp, body = callWith(t, srv, http.MethodPut, "/v1/test-only", nil, "")
	wantProblem(t, resp, body, http.StatusUnauthorized, typeUnauthorized)
}

func TestRequestIDIsEchoed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	get := func(id string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
		if id != "" {
			req.Header.Set("X-Request-Id", id)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("X-Request-Id")
	}
	if got := get("abc-1"); got != "abc-1" {
		t.Errorf("X-Request-Id = %q, want abc-1", got)
	}
	if got := get(strings.Repeat("a", 64)); got != strings.Repeat("a", 64) {
		t.Errorf("a 64 character id was not echoed: %q", got)
	}
	for _, bad := range []string{"", "a b", strings.Repeat("a", 65), "a/b", "é"} {
		got := get(bad)
		if got == "" || got == bad {
			t.Errorf("X-Request-Id for %q = %q, want a new value", bad, got)
		}
	}
	if a, b := get(""), get(""); a == b {
		t.Errorf("two requests got the same id %q", a)
	}
}

// syncBuffer is a bytes.Buffer the log handler and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRequestLog(t *testing.T) {
	var buf syncBuffer
	srv, _, _ := newTestServerWith(t, func(c *Config) { c.Log = slog.New(slog.NewJSONHandler(&buf, nil)) })
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	req.Header.Set("X-Request-Id", "log-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := firstLogLine(t, &buf)
	if rec["requestId"] != "log-1" || rec["op"] != "healthz" || rec["status"] != float64(200) {
		t.Errorf("log line = %v, want requestId log-1, op healthz, status 200", rec)
	}
}

func TestLogAttrsReachTheRequestLine(t *testing.T) {
	var buf syncBuffer
	cfg, _, _ := testConfig()
	cfg.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	srv := newServerWithRoutes(t, cfg, endpoint{route{http.MethodGet, "/v1/things/{id}", "test.thing"}, func(w http.ResponseWriter, r *http.Request) error {
		logAttrs(r.Context(), "projectId", r.PathValue("id"), "branch", "dev")
		return notFound("no thing %s", r.PathValue("id"))
	}})
	if resp, body := call(t, srv, http.MethodGet, "/v1/things/abc123def456", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	rec := firstLogLine(t, &buf)
	if rec["projectId"] != "abc123def456" || rec["branch"] != "dev" || rec["op"] != "test.thing" || rec["status"] != float64(404) {
		t.Errorf("log line = %v", rec)
	}
}

// firstLogLine waits for the request line, which is written after the response, and decodes
// it. There must be exactly one.
func firstLogLine(t *testing.T, buf *syncBuffer) map[string]any {
	t.Helper()
	var out string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if out = buf.String(); out != "" {
			break
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1: %q", len(lines), out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("log line %q: %v", lines[0], err)
	}
	return rec
}

func TestProblemFromPlainError(t *testing.T) {
	cfg, _, _ := testConfig()
	srv := newServerWithRoutes(t, cfg,
		endpoint{route{http.MethodGet, "/v1/plain", "test.plain"}, func(w http.ResponseWriter, r *http.Request) error {
			return errors.New("disk on fire")
		}},
		endpoint{route{http.MethodGet, "/v1/slow", "test.slow"}, func(w http.ResponseWriter, r *http.Request) error {
			return &apiError{Status: http.StatusTooManyRequests, Type: typeRateLimited, Detail: "slow down", RetryAfter: 1500 * time.Millisecond}
		}},
		endpoint{route{http.MethodGet, "/v1/typed", "test.typed"}, func(w http.ResponseWriter, r *http.Request) error {
			return conflict("branch %s exists", "dev")
		}})

	resp, body := call(t, srv, http.MethodGet, "/v1/plain", nil)
	if p := wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal); p.Detail != "disk on fire" {
		t.Errorf("detail = %q", p.Detail)
	}
	resp, body = call(t, srv, http.MethodGet, "/v1/slow", nil)
	wantProblem(t, resp, body, http.StatusTooManyRequests, typeRateLimited)
	if got := resp.Header.Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2 (rounded up)", got)
	}
	resp, body = call(t, srv, http.MethodGet, "/v1/typed", nil)
	if p := wantProblem(t, resp, body, http.StatusConflict, typeConflict); p.Detail != "branch dev exists" {
		t.Errorf("detail = %q", p.Detail)
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Errorf("Retry-After on a 409: %q", resp.Header.Get("Retry-After"))
	}
}

func TestApiErrorHelpers(t *testing.T) {
	for _, c := range []struct {
		err    *apiError
		status int
		typ    problemType
	}{
		{badRequest("bad %d", 1), 400, typeValidation},
		{notFound("gone"), 404, typeNotFound},
		{conflict("busy"), 409, typeConflict},
		{internal(errors.New("x")), 500, typeInternal},
	} {
		if c.err.Status != c.status || c.err.Type != c.typ || c.err.Detail == "" || c.err.Error() == "" {
			t.Errorf("%+v, want status %d type %s", c.err, c.status, c.typ)
		}
	}
}

func TestDefaultWaits(t *testing.T) {
	if got := (Waits{}).withDefaults(); got != (Waits{Create: 5 * time.Second, State: 10 * time.Second, Restore: 30 * time.Second}) {
		t.Errorf("zero Waits = %+v, want 5s/10s/30s", got)
	}
	set := Waits{Create: time.Millisecond, State: 2 * time.Millisecond, Restore: 3 * time.Millisecond}
	if got := set.withDefaults(); got != set {
		t.Errorf("set Waits = %+v, want it kept", got)
	}
	if got := (Waits{State: time.Second}).withDefaults(); got.Create != 5*time.Second || got.State != time.Second || got.Restore != 30*time.Second {
		t.Errorf("partly set Waits = %+v", got)
	}
}
