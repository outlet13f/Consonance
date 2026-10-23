package controller

import (
	"errors"
	"net/http"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
	"consonance/internal/agentclient"
	"consonance/internal/supervisorclient"
)

// agentCode is the error code of an agent error, "" for any other error.
func agentCode(err error) agentapi.ErrorCode { return agentclient.CodeOf(err) }

// supervisorStatus is the HTTP status of a supervisor error, 0 when the supervisor did not
// answer.
func supervisorStatus(err error) int {
	var e *supervisorclient.APIError
	if errors.As(err, &e) {
		return e.StatusCode
	}
	return 0
}

// stillStarting reports a /v1/start that timed out while PostgreSQL still starts (503): not a
// failure (carry-over from plan 1).
func stillStarting(err error) bool { return supervisorStatus(err) == http.StatusServiceUnavailable }

// permanentReason is the condition reason of an agent error that retrying cannot fix, or "".
// A copy's not-found (no parent data, no such restore point) is permanent too; callers that
// copy check it themselves.
func permanentReason(err error) string {
	switch agentCode(err) {
	case agentapi.CodeReflinkUnsupported:
		return v1alpha1.ReasonReflinkUnsupported
	case agentapi.CodeInsufficientStorage:
		return v1alpha1.ReasonInsufficientStorage
	case agentapi.CodeInvalid:
		return v1alpha1.ReasonInvalid
	}
	return ""
}

// retryLater turns an error worth retrying into what Reconcile returns: a rate limit comes back
// after Retry-After, a source branch that is not running after a short wait, and anything else
// (busy, supervisor-unavailable, internal, a copy's conflict, no answer) with backoff
// (carry-over from plan 2).
func retryLater(err error) (ctrl.Result, error) {
	var e *agentclient.APIError
	if errors.As(err, &e) {
		switch e.Code {
		case agentapi.CodeRateLimited:
			return ctrl.Result{RequeueAfter: max(e.RetryAfter, time.Second)}, nil
		case agentapi.CodeNotRunning:
			return ctrl.Result{RequeueAfter: waitShort}, nil
		}
	}
	return ctrl.Result{}, err
}

// How long to wait before looking again at something on its way: a pod coming up, PostgreSQL
// starting or stopping, a source branch to run.
const (
	waitShort = 2 * time.Second
	waitPoll  = 500 * time.Millisecond
)
