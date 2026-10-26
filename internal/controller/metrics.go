package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"consonance/api/v1alpha1"
	"consonance/internal/agentapi"
)

// The operator's metrics (spec 11). The operator sees every object's lifetime, so it times them
// all. Each is observed once, in the reconcile whose status update first records the moment, and
// never from an old timestamp again: a restarted operator does not count a branch twice.
var (
	// buckets run from 50ms to 102s, the span of a branch coming up and of a project's first start.
	buckets = prometheus.ExponentialBuckets(0.05, 2, 12)

	branchCreateSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "consonance_branch_create_seconds",
		Help:    "Time from a Branch's creation until its data exists. main is left out (its init-main is part of the project's creation), and so are the backup branches a rollback makes.",
		Buckets: buckets,
	})
	branchFirstReadySeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "consonance_branch_first_ready_seconds",
		Help:    "Time from a Branch's creation until PostgreSQL first runs. Backup branches are left out.",
		Buckets: buckets,
	})
	restoreSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "consonance_restore_seconds",
		Help:    "Time a rollback (kind=rollback) or reset (kind=reset) takes from its start until it ends as done or failed.",
		Buckets: buckets,
	}, []string{"kind", "result"})
	startSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "consonance_start_seconds",
		Help:    "Time from the operator's start request to a stopped branch until it sees PostgreSQL run.",
		Buckets: buckets,
	})
	projectCreateSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "consonance_project_create_seconds",
		Help:    "Time from a Project's creation until it is first Ready.",
		Buckets: buckets,
	})
	copyErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "consonance_copy_errors_total",
		Help: "Failed data copies (CreateBranchData, CreateGeneration), by the agent's error code, transport when none answered, other for a code not known.",
	}, []string{"reason"})
)

func init() {
	metrics.Registry.MustRegister(branchCreateSeconds, branchFirstReadySeconds, restoreSeconds,
		startSeconds, projectCreateSeconds, copyErrors)
}

// copyErrorReasons are the agent's error codes. Any other code, which a newer agent may send,
// is counted as "other", so the label stays a small fixed set.
var copyErrorReasons = map[agentapi.ErrorCode]bool{
	agentapi.CodeInvalid: true, agentapi.CodeUnauthorized: true, agentapi.CodeNotFound: true,
	agentapi.CodeConflict: true, agentapi.CodeNotRunning: true, agentapi.CodeBusy: true,
	agentapi.CodeRateLimited: true, agentapi.CodeSupervisorUnavailable: true,
	agentapi.CodeInsufficientStorage: true, agentapi.CodeReflinkUnsupported: true, agentapi.CodeInternal: true,
}

// copyErrorReason is the label of a failed copy: the agent's error code, "transport" when there is
// none (the agent did not answer), "other" for a code not known.
func copyErrorReason(err error) string {
	code := agentCode(err)
	switch {
	case code == "":
		return "transport"
	case copyErrorReasons[code]:
		return string(code)
	}
	return "other"
}

// countCopyError counts a failed copy by its reason.
func countCopyError(err error) { copyErrors.WithLabelValues(copyErrorReason(err)).Inc() }

// observe records d, which a clock that stepped back may make negative, as a duration of 0 or more.
func observe(h prometheus.Observer, d time.Duration) { h.Observe(max(d, 0).Seconds()) }

// observeRestore records a finished restore, once its end is saved. A reset is a restore from the
// parent's latest data, a rollback one from a restore point.
func observeRestore(lr *v1alpha1.RestoreStatus) {
	if lr.StartedAt == nil || lr.FinishedAt == nil {
		return
	}
	kind := "rollback"
	if lr.Source.Type == v1alpha1.SourceParentLatest {
		kind = "reset"
	}
	result := "done"
	if lr.Phase == v1alpha1.RestoreFailed {
		result = "failed"
	}
	observe(restoreSeconds.WithLabelValues(kind, result), lr.FinishedAt.Sub(lr.StartedAt.Time))
}
