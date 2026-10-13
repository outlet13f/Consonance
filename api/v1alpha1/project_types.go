package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectSpec is the project the API server asks for (spec 4.2). Omitted fields get the spec 14
// defaults from the API server.
type ProjectSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	DisplayName string `json:"displayName"`

	// +optional
	// +kubebuilder:default="17"
	// +kubebuilder:validation:Enum="17"
	PostgresVersion string `json:"postgresVersion,omitempty"`

	// +optional
	// +kubebuilder:default={}
	Storage StorageSpec `json:"storage,omitzero"`

	// +optional
	// +kubebuilder:default={cpu: "500m", memory: "1Gi"}
	DefaultCompute ComputeSpec `json:"defaultCompute,omitzero"`

	// +optional
	// +kubebuilder:default={}
	Limits ProjectLimits `json:"limits,omitzero"`

	// +optional
	// +kubebuilder:default={}
	RestorePoints RestorePointPolicy `json:"restorePoints,omitzero"`
}

// StorageSpec is the project volume. It must be XFS with reflink (spec 2).
type StorageSpec struct {
	// +optional
	// +kubebuilder:default="10Gi"
	Size resource.Quantity `json:"size,omitzero"`

	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// ComputeSpec is the CPU and memory of a branch pod: its requests and its limits.
type ComputeSpec struct {
	CPU    resource.Quantity `json:"cpu"`
	Memory resource.Quantity `json:"memory"`
}

// ProjectLimits are the project's limits (spec 4.2).
type ProjectLimits struct {
	// MaxBranches counts main. The API server enforces it.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	MaxBranches int32 `json:"maxBranches,omitempty"`

	// MinCopyInterval is the shortest time between two copies of one source branch.
	// +optional
	// +kubebuilder:default="5s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	MinCopyInterval metav1.Duration `json:"minCopyInterval,omitzero"`
}

// RestorePointPolicy is the restore point retention (spec 4.2).
type RestorePointPolicy struct {
	// +optional
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=1
	MaxPerBranch int32 `json:"maxPerBranch,omitempty"`

	// +optional
	// +kubebuilder:default="24h"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	MaxAge metav1.Duration `json:"maxAge,omitzero"`
}

// ProjectPhase is where a project is in its life (spec 4.2).
type ProjectPhase string

const (
	ProjectPending      ProjectPhase = "Pending"
	ProjectProvisioning ProjectPhase = "Provisioning"
	ProjectReady        ProjectPhase = "Ready"
	ProjectFailed       ProjectPhase = "Failed"
)

// Project condition types (spec 4.2), and Ready, which sums them up.
const (
	ConditionVolumeReady      = "VolumeReady"
	ConditionAgentReady       = "AgentReady"
	ConditionReflinkSupported = "ReflinkSupported"
	ConditionMainReady        = "MainReady"
	ConditionReady            = "Ready"
)

// ProjectStatus is what the operator reports.
type ProjectStatus struct {
	// +optional
	Phase ProjectPhase `json:"phase,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Volume VolumeStatus `json:"volume,omitzero"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VolumeStatus is the project volume as the agent last saw it.
type VolumeStatus struct {
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// +optional
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
	// +optional
	UsedBytes int64 `json:"usedBytes,omitempty"`
}

// Project is one project: its namespace cns-<projectId> holds this Project, named project, and
// its branches (spec 4.1).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'project'",message="the Project of a project namespace is named project"
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ProjectSpec `json:"spec"`

	// +optional
	Status ProjectStatus `json:"status,omitzero"`
}

// ProjectList is a list of projects.
//
// +kubebuilder:object:root=true
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}
