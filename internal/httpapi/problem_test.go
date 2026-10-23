package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
)

func asAPIError(t *testing.T, err error) *apiError {
	t.Helper()
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("err = %#v, want an *apiError", err)
	}
	return ae
}

func TestFromAgent(t *testing.T) {
	for _, tc := range []struct {
		code   agentapi.ErrorCode
		status int
		typ    problemType
	}{
		{agentapi.CodeNotFound, http.StatusNotFound, typeNotFound},
		{agentapi.CodeRateLimited, http.StatusTooManyRequests, typeRateLimited},
		{agentapi.CodeInsufficientStorage, http.StatusInsufficientStorage, typeInsufficientStorage},
		{agentapi.CodeNotRunning, http.StatusConflict, typeConflict},
		{agentapi.CodeConflict, http.StatusConflict, typeConflict},
		{agentapi.CodeBusy, http.StatusConflict, typeConflict},
		{agentapi.CodeInvalid, http.StatusBadRequest, typeValidation},
		{agentapi.CodeInternal, http.StatusInternalServerError, typeInternal},
		{agentapi.CodeSupervisorUnavailable, http.StatusInternalServerError, typeInternal},
		{agentapi.CodeUnauthorized, http.StatusInternalServerError, typeInternal},
		{"", http.StatusInternalServerError, typeInternal},
	} {
		ae := asAPIError(t, fromAgent(&agentclient.APIError{StatusCode: 418, Code: tc.code, Message: "the agent says"}))
		if ae.Status != tc.status || ae.Type != tc.typ || ae.Detail == "" {
			t.Errorf("code %q: got %d %s %q, want %d %s", tc.code, ae.Status, ae.Type, ae.Detail, tc.status, tc.typ)
		}
	}

	// The agent's message is the detail.
	ae := asAPIError(t, fromAgent(&agentclient.APIError{StatusCode: 404, Code: agentapi.CodeNotFound, Message: "no such restore point"}))
	if ae.Detail != "no such restore point" {
		t.Errorf("detail = %q", ae.Detail)
	}
	// An error that wraps the agent's is still the agent's.
	ae = asAPIError(t, fromAgent(fmt.Errorf("creating: %w", &agentclient.APIError{StatusCode: 409, Code: agentapi.CodeBusy, Message: "busy"})))
	if ae.Status != http.StatusConflict {
		t.Errorf("wrapped: status = %d", ae.Status)
	}
	// A body that was not the agent's (no code) is a 500, whatever its status was.
	ae = asAPIError(t, fromAgent(&agentclient.APIError{StatusCode: 404, Message: "upstream connect error"}))
	if ae.Status != http.StatusInternalServerError || ae.Type != typeInternal {
		t.Errorf("no code: got %d %s", ae.Status, ae.Type)
	}
	// A network error is a 500 with the cause as the detail.
	ae = asAPIError(t, fromAgent(errors.New("dial tcp: connection refused")))
	if ae.Status != http.StatusInternalServerError || ae.Type != typeInternal || ae.Detail != "dial tcp: connection refused" {
		t.Errorf("network: got %d %s %q", ae.Status, ae.Type, ae.Detail)
	}
}

func TestFromAgentRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		retryAfter time.Duration
		want       string
	}{
		{1500 * time.Millisecond, "2"},
		{3 * time.Second, "3"},
		{0, "1"},
		{200 * time.Millisecond, "1"},
	} {
		err := fromAgent(&agentclient.APIError{StatusCode: 429, Code: agentapi.CodeRateLimited, Message: "slow down", RetryAfter: tc.retryAfter})
		rec := httptest.NewRecorder()
		writeProblem(rec, httptest.NewRequest(http.MethodGet, "/", nil), err)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != tc.want {
			t.Errorf("RetryAfter %v: status %d, Retry-After %q, want 429 and %q", tc.retryAfter, rec.Code, rec.Header().Get("Retry-After"), tc.want)
		}
	}
}

func TestFromKube(t *testing.T) {
	gr := schema.GroupResource{Group: "consonance.io", Resource: "branches"}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		typ    problemType
	}{
		{"not found", apierrors.NewNotFound(gr, "dev"), http.StatusNotFound, typeNotFound},
		{"already exists", apierrors.NewAlreadyExists(gr, "dev"), http.StatusConflict, typeConflict},
		{"conflict", apierrors.NewConflict(gr, "dev", errors.New("the object has been modified")), http.StatusConflict, typeConflict},
		{"invalid", apierrors.NewBadRequest("bad"), http.StatusBadRequest, typeValidation},
		{"forbidden", apierrors.NewForbidden(gr, "dev", errors.New("no")), http.StatusInternalServerError, typeInternal},
		{"other", errors.New("boom"), http.StatusInternalServerError, typeInternal},
		{"wrapped not found", fmt.Errorf("getting: %w", apierrors.NewNotFound(gr, "dev")), http.StatusNotFound, typeNotFound},
	} {
		ae := asAPIError(t, fromKube(tc.err))
		if ae.Status != tc.status || ae.Type != tc.typ || ae.Detail == "" {
			t.Errorf("%s: got %d %s %q, want %d %s", tc.name, ae.Status, ae.Type, ae.Detail, tc.status, tc.typ)
		}
	}
}
