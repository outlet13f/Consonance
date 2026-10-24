package httpapi

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
)

const (
	connURL      = branchesURL + "/main/connection"
	connPassword = "p@ss/w:rd"
)

// dbCredentials is the db-credentials Secret of the test project.
func dbCredentials() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: controller.DBCredentialsSecret},
		Data: map[string][]byte{
			controller.UsernameKey: []byte("app_owner"),
			controller.PasswordKey: []byte(connPassword),
			controller.DatabaseKey: []byte("app"),
		},
	}
}

// connectionObjs is a ready project whose main branch has an endpoint, and its credentials.
func connectionObjs() []client.Object {
	objs := readyProjectObjs(testProject)
	for _, o := range objs {
		if b, ok := o.(*v1alpha1.Branch); ok {
			b.Status.Endpoint = &v1alpha1.Endpoint{Host: "main.cns-" + testProject + ".svc", Port: 5432}
		}
	}
	return append(objs, dbCredentials())
}

func TestConnection(t *testing.T) {
	srv, _, _ := newTestServer(t, connectionObjs()...)
	resp, body := call(t, srv, http.MethodGet, connURL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	v := decode[connectionView](t, body)
	if v.Host != "main.cns-"+testProject+".svc" || v.Port != 5432 || v.Database != "app" || v.User != "app_owner" || v.Password != connPassword {
		t.Errorf("view = %+v", v)
	}
	if strings.Contains(v.URI, connPassword) {
		t.Errorf("uri %q has the password unescaped", v.URI)
	}
	u, err := url.Parse(v.URI)
	if err != nil {
		t.Fatalf("uri %q: %v", v.URI, err)
	}
	pw, _ := u.User.Password()
	if u.Scheme != "postgresql" || u.User.Username() != "app_owner" || pw != connPassword || u.Hostname() != v.Host || u.Port() != "5432" || u.Path != "/app" {
		t.Errorf("uri %q parses to %+v, password %q", v.URI, u, pw)
	}
	// The six fields of the brief, and no more.
	fields := decode[map[string]any](t, body)
	for _, k := range []string{"host", "port", "database", "user", "password", "uri"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("no field %q in %s", k, body)
		}
	}
	if len(fields) != 6 {
		t.Errorf("fields = %v, want 6", fields)
	}
}

func TestConnectionWithoutEndpointIsConflict(t *testing.T) {
	objs := append(readyProjectObjs(testProject), dbCredentials())
	srv, _, _ := newTestServer(t, objs...)
	resp, body := call(t, srv, http.MethodGet, connURL, nil)
	p := wantProblem(t, resp, body, http.StatusConflict, typeConflict)
	if p.Detail != "branch main has no endpoint yet" {
		t.Errorf("detail = %q", p.Detail)
	}
}

func TestConnectionRejects(t *testing.T) {
	objs := connectionObjs()
	objs = append(objs, projectObjs("bbbbbbbbbbbb", "provisioning", v1alpha1.ProjectStatus{})...)
	srv, _, _ := newTestServer(t, objs...)
	for name, tc := range map[string]struct {
		path   string
		status int
		typ    problemType
	}{
		"no branch":            {branchesURL + "/nope/connection", 404, typeNotFound},
		"branch of a bad form": {branchesURL + "/Main/connection", 404, typeNotFound},
		"no project":           {"/v1/projects/cccccccccccc/branches/main/connection", 404, typeNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			resp, b := call(t, srv, http.MethodGet, tc.path, nil)
			wantProblem(t, resp, b, tc.status, tc.typ)
		})
	}
}

func TestConnectionSecretProblemsAreInternal(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Secret) *corev1.Secret{
		"no secret":   func(*corev1.Secret) *corev1.Secret { return nil },
		"no password": func(s *corev1.Secret) *corev1.Secret { delete(s.Data, controller.PasswordKey); return s },
		"no user":     func(s *corev1.Secret) *corev1.Secret { delete(s.Data, controller.UsernameKey); return s },
		"no database": func(s *corev1.Secret) *corev1.Secret { delete(s.Data, controller.DatabaseKey); return s },
	} {
		t.Run(name, func(t *testing.T) {
			objs := readyProjectObjs(testProject)
			for _, o := range objs {
				if b, ok := o.(*v1alpha1.Branch); ok {
					b.Status.Endpoint = &v1alpha1.Endpoint{Host: "h", Port: 5432}
				}
			}
			if s := mutate(dbCredentials()); s != nil {
				objs = append(objs, s)
			}
			srv, _, _ := newTestServer(t, objs...)
			resp, body := call(t, srv, http.MethodGet, connURL, nil)
			p := wantProblem(t, resp, body, http.StatusInternalServerError, typeInternal)
			if strings.Contains(p.Detail, connPassword) {
				t.Errorf("detail %q has the password", p.Detail)
			}
		})
	}
}

func TestConnectionIsNotLogged(t *testing.T) {
	var buf syncBuffer
	srv, _, _ := newTestServerWith(t, func(c *Config) { c.Log = slog.New(slog.NewJSONHandler(&buf, nil)) }, connectionObjs()...)
	resp, body := call(t, srv, http.MethodGet, connURL, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "p@ss") {
		t.Fatalf("status = %d; body %s", resp.StatusCode, body)
	}
	firstLogLine(t, &buf) // the request line is written after the response
	for _, secret := range []string{connPassword, url.QueryEscape(connPassword), url.PathEscape(connPassword), "p@ss"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("the log has %q: %s", secret, buf.String())
		}
	}
}
