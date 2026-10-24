// Package httpapi is the REST API server (spec chapter 5): stateless, in front of the operator.
// It reads and writes Project and Branch objects, and calls a project's volume agent only for
// restore points; the operator does the rest.
package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Config is what a server needs.
type Config struct {
	Client       client.Client  // cached reads, and writes
	Reader       client.Reader  // uncached reads, for Secrets (the cache does not watch them)
	Agents       AgentConnector // the volume agents of the projects
	AdminKey     string         // the key of the Authorization header
	StorageClass string         // the storageClassName of new projects
	Waits        Waits          // how long a request waits for the operator
	Now          func() time.Time
	Log          *slog.Logger
}

// Waits is how long a request holds its response for the operator to finish (spec 14): after
// that the answer is 202 and the caller polls. A zero field means the spec's value.
type Waits struct {
	Create  time.Duration // branch creation, 201 or 202
	State   time.Duration // stop and start, 200 or 202
	Restore time.Duration // rollback and reset, 200 or 202
}

func (w Waits) withDefaults() Waits {
	if w.Create == 0 {
		w.Create = 5 * time.Second
	}
	if w.State == 0 {
		w.State = 10 * time.Second
	}
	if w.Restore == 0 {
		w.Restore = 30 * time.Second
	}
	return w
}

type server struct {
	client       client.Client
	reader       client.Reader
	agents       AgentConnector
	adminKey     string
	storageClass string
	waits        Waits
	now          func() time.Time
	log          *slog.Logger
}

// route is one endpoint of the API. The table of them is also what the OpenAPI document is
// checked against.
type route struct {
	Method  string
	Pattern string // a http.ServeMux path pattern, without the method
	Op      string // the name in the request log, for example project.get
}

// endpoint is a route with its handler.
type endpoint struct {
	route
	handle handlerFunc
}

func newServer(cfg Config) *server {
	s := &server{
		client:       cfg.Client,
		reader:       cfg.Reader,
		agents:       cfg.Agents,
		adminKey:     cfg.AdminKey,
		storageClass: cfg.StorageClass,
		waits:        cfg.Waits.withDefaults(),
		now:          cfg.Now,
		log:          cfg.Log,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// New returns the API's handler.
func New(cfg Config) http.Handler {
	s := newServer(cfg)
	return s.handler(s.endpoints())
}

// endpoints is every route with its handler; a task that adds endpoints adds them here.
func (s *server) endpoints() []endpoint {
	eps := []endpoint{
		{route{http.MethodGet, "/healthz", "healthz"}, s.healthz},
	}
	eps = append(eps, s.projectEndpoints()...)
	eps = append(eps, s.branchEndpoints()...)
	eps = append(eps, s.connectionEndpoints()...)
	eps = append(eps, s.stateEndpoints()...)
	return append(eps, s.restorePointEndpoints()...)
}

// routes lists the endpoints without their handlers.
func (s *server) routes() []route {
	eps := s.endpoints()
	out := make([]route, len(eps))
	for i, e := range eps {
		out[i] = e.route
	}
	return out
}

// methods are the ones a 405 looks for, in the order of the Allow header.
var methods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// handler builds the router: the log and request id around the key check around a ServeMux.
// The mux answers neither 404 nor 405 with a problem, so a catch-all route does it.
func (s *server) handler(eps []endpoint) http.Handler {
	mux := http.NewServeMux()
	for _, e := range eps {
		mux.Handle(e.Method+" "+e.Pattern, s.route(e))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var allow []string
		for _, m := range methods {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := mux.Handler(probe); pattern != "" && pattern != "/" {
				allow = append(allow, m)
			}
		}
		if len(allow) == 0 {
			setOp(r.Context(), "not-found")
			writeProblem(w, r, notFound("no such route: %s %s", r.Method, r.URL.Path))
			return
		}
		setOp(r.Context(), "method-not-allowed")
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeProblem(w, r, &apiError{Status: http.StatusMethodNotAllowed, Type: typeValidation, Detail: r.Method + " is not allowed here; use " + strings.Join(allow, ", ")})
	})
	return s.logged(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if needsAuth(r.URL.Path) && !s.authorized(r) {
			setOp(r.Context(), "unauthorized")
			unauthorized(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// route names the request for the log and turns a returned error into a problem.
func (s *server) route(e endpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setOp(r.Context(), e.Op)
		if err := e.handle(w, r); err != nil {
			writeProblem(w, r, err)
		}
	})
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
	return nil
}
