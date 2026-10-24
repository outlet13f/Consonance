package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/internal/controller"
)

// connectionView is how to connect to a branch. It holds the database password, so the
// response is never cached and nothing here may reach the request log (which records neither
// bodies nor Secret values).
type connectionView struct {
	Host     string `json:"host"`
	Port     int32  `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	URI      string `json:"uri"`
}

func (s *server) connectionEndpoints() []endpoint {
	return []endpoint{
		{route{http.MethodGet, "/v1/projects/{projectId}/branches/{branch}/connection", "branch.connection"}, s.getConnection},
	}
}

// getConnection answers with the endpoint of the branch (from its status) and the login of the
// project's db-credentials Secret. The Secret is read uncached, like the agent token: the cache
// does not watch Secrets. A Secret the operator should have made but did not is a 500.
func (s *server) getConnection(w http.ResponseWriter, r *http.Request) error {
	id, name := r.PathValue("projectId"), r.PathValue("branch")
	logAttrs(r.Context(), "projectId", id, "branch", name)
	if _, err := s.project(r.Context(), id); err != nil {
		return err
	}
	ns := controller.NamespacePrefix + id
	b, err := s.branch(r.Context(), ns, name)
	if err != nil {
		return err
	}
	e := b.Status.Endpoint
	if e == nil || e.Host == "" || e.Port == 0 {
		return conflict("branch %s has no endpoint yet", b.Name)
	}

	var secret corev1.Secret
	if err := s.reader.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: controller.DBCredentialsSecret}, &secret); err != nil {
		return internal(fmt.Errorf("secret %s: %w", controller.DBCredentialsSecret, err))
	}
	user := string(secret.Data[controller.UsernameKey])
	password := string(secret.Data[controller.PasswordKey])
	database := string(secret.Data[controller.DatabaseKey])
	for key, val := range map[string]string{controller.UsernameKey: user, controller.PasswordKey: password, controller.DatabaseKey: database} {
		if val == "" {
			return internal(fmt.Errorf("secret %s has no %s", controller.DBCredentialsSecret, key))
		}
	}

	// url.URL escapes the password and the database name, and JoinHostPort brackets an IPv6 host.
	uri := url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(e.Host, strconv.Itoa(int(e.Port))),
		Path:   "/" + database,
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, connectionView{
		Host: e.Host, Port: e.Port, Database: database, User: user, Password: password, URI: uri.String(),
	})
	return nil
}
