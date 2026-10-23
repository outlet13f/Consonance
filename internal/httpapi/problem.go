package httpapi

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// problemType is the "type" of an RFC 9457 problem: the values of the table in spec chapter 5.
type problemType string

const (
	typeValidation          problemType = "validation-error"
	typeUnauthorized        problemType = "unauthorized"
	typeNotFound            problemType = "not-found"
	typeConflict            problemType = "conflict"
	typeRateLimited         problemType = "rate-limited"
	typeInsufficientStorage problemType = "insufficient-storage"
	typeInternal            problemType = "internal"
)

// apiError is an error a handler returns to be written as a problem+json response.
type apiError struct {
	Status     int
	Type       problemType
	Detail     string
	RetryAfter time.Duration // sent as Retry-After, rounded up to whole seconds
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Type, e.Detail) }

func badRequest(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Type: typeValidation, Detail: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusNotFound, Type: typeNotFound, Detail: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusConflict, Type: typeConflict, Detail: fmt.Sprintf(format, args...)}
}

func internal(err error) *apiError {
	return &apiError{Status: http.StatusInternalServerError, Type: typeInternal, Detail: err.Error()}
}

// handlerFunc is a handler that reports failure by returning it; the router writes the problem.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// writeProblem answers with err as application/problem+json. An error that is not an *apiError
// is a 500 "internal" with the error text as the detail.
func writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	ae, ok := err.(*apiError)
	if !ok {
		ae = internal(err)
	}
	logAttrs(r.Context(), "error", err.Error())
	if ae.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(ae.RetryAfter.Seconds()))))
	}
	body := struct {
		Type   problemType `json:"type"`
		Title  string      `json:"title"`
		Status int         `json:"status"`
		Detail string      `json:"detail"`
	}{ae.Type, http.StatusText(ae.Status), ae.Status, ae.Detail}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(ae.Status)
	w.Write(append(b, '\n'))
}

// writeJSON answers with v as JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// A type that cannot be encoded is a bug; do not send half a body.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}
