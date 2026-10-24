package httpapi

import (
	_ "embed"
	"net/http"
)

// openAPIPath is where the API document is served, without a key (see isPublic).
const openAPIPath = "/v1/openapi.json"

// openAPIDocument is the OpenAPI 3.1 document of the API. TestOpenAPICoversEveryRoute and
// TestOpenAPISchemasMatchTheViews keep it in step with the routes and the views.
//
//go:embed openapi.json
var openAPIDocument []byte

func (s *server) openAPI(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openAPIDocument)
	return nil
}
