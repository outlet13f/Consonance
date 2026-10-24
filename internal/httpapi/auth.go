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

// isPublic is the whole set of requests that need no key: the probe and the API document. It is
// a list and not a prefix rule, so a path that is not named here, under /v1 or outside it, is
// behind the key, and a route added later is too. It is GET only: HEAD, which the mux serves
// for a GET route, needs the key like everything else.
func isPublic(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	return path == "/healthz" || path == openAPIPath
}

func unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, r, &apiError{Status: http.StatusUnauthorized, Type: typeUnauthorized, Detail: "a valid admin key is required: Authorization: Bearer <key>"})
}
