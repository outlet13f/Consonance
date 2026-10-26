package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Leader election and events of the manager. client-go's lease lock only gets, creates and updates
// the lease.
//
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=consonance-system,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups="",namespace=consonance-system,resources=events,verbs=create;patch

// CacheOptions limit the manager's cache to the objects the operator makes, all labelled
// managed-by=consonance-operator: it then neither holds every secret of the cluster nor watches
// every pod.
func CacheOptions() cache.Options {
	mine := labels.SelectorFromSet(labels.Set{LabelManagedBy: ManagedBy})
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Secret{}:                {Label: mine},
		&corev1.Pod{}:                   {Label: mine},
		&corev1.Service{}:               {Label: mine},
		&corev1.PersistentVolumeClaim{}: {Label: mine},
		&corev1.ResourceQuota{}:         {Label: mine},
		&networkingv1.NetworkPolicy{}:   {Label: mine},
	}}
}

// rateLimiter retries failed reconciles from 100ms, doubling up to 30s: the default goes up to
// 16 minutes, too slow for an agent or supervisor that is back after a restart (spec 10).
func rateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](100*time.Millisecond, 30*time.Second)
}
