package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"consonance/api/v1alpha1"
)

// The operator's metrics (spec 11). The operator sees every object's lifetime, so it times them
// all. Each is observed once, in the reconcile whose status update first records the moment, and
// never from an old timestamp again: a restarted operator does not count a branch twice.
var (
	// buckets run from 50ms to 102s, the span of a branch coming up and of a project's first start.
	buckets = prometheus.ExponentialBuckets(0.05, 2, 12)

	branchCreateSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "consonance_branch_create_seconds",
		Help:    "Time from a Branch's creation until its data exists. main is left out: its init-main is part of the project's creation.",
		Buckets: buckets,
	})
	branchFirstReadySeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "consonance_branch_first_ready_seconds",
		Help:    "Time from a Branch's creation until PostgreSQL first runs.",
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
		Help: "Failed data copies (CreateBranchData, CreateGeneration), by the agent's error code, or transport when none answered.",
	}, []string{"reason"})
)

func init() {
	metrics.Registry.MustRegister(branchCreateSeconds, branchFirstReadySeconds, restoreSeconds,
		startSeconds, projectCreateSeconds, copyErrors)
}

// countCopyError counts a failed copy by the agent's error code; an error with none (the agent
// did not answer) is a transport error.
func countCopyError(err error) {
	reason := string(agentCode(err))
	if reason == "" {
		reason = "transport"
	}
	copyErrors.WithLabelValues(reason).Inc()
}

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
