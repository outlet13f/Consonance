package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// authorized reports whether r carries the admin key as "Authorization: Bearer <key>". Both
// sides are hashed first so the comparison takes the same time whatever the length of the key
// sent. A server without a key refuses everything.
func (s *server) authorized(r *http.Request) bool {
	if s.adminKey == "" {
		return false
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(s.adminKey))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// needsAuth is every path under /v1; /healthz is open so probes need no key.
func needsAuth(path string) bool { return path == "/v1" || strings.HasPrefix(path, "/v1/") }

func unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, r, &apiError{Status: http.StatusUnauthorized, Type: typeUnauthorized, Detail: "a valid admin key is required: Authorization: Bearer <key>"})
}
