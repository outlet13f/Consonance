// Package v1alpha1 holds the Project and Branch custom resources (spec 4).
//
// +kubebuilder:object:generate=true
// +groupName=consonance.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is consonance.io/v1alpha1, a working name until the product is named (spec 4).
	GroupVersion = schema.GroupVersion{Group: "consonance.io", Version: "v1alpha1"}

	// SchemeBuilder registers the types of this group.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types of this group to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
